package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/skills"
)

// MEMORY lines are ignored: the per-turn extraction already saves memories,
// and the review saved the same facts a second time under other names.
func TestReviewIgnoresMemoryLines(t *testing.T) {
	eng := reviewEngine(t, &mockProvider{})
	before := len(eng.memStore.All())
	eng.applyReview("MEMORY: 用户偏好用 tab 缩进")
	if got := len(eng.memStore.All()); got != before {
		t.Fatalf("review saved a memory: %d -> %d entries", before, got)
	}
}

// A learned skill is written to ~/.cove/skills/auto-<slug>/SKILL.md with a
// description, when it was generated and the session it came from, so it
// survives the process (it lived only in memory).
func TestReviewWritesSkillToDisk(t *testing.T) {
	eng := reviewEngine(t, &mockProvider{})
	res := eng.applyReview("SKILL: Release Flow | 发布新版本 | run the tests, tag, push the tag")
	if len(res.added) != 1 || res.added[0] != "Release Flow" {
		t.Fatalf("new skills = %+v", res)
	}
	path := autoSkillFile(t, "Release Flow")
	if !strings.Contains(filepath.Base(filepath.Dir(path)), "auto-release-flow-") {
		t.Fatalf("directory %s lacks the slug plus a hash suffix", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("skill file: %v", err)
	}
	body := string(data)
	for _, want := range []string{"name: Release Flow", "description: 发布新版本", "generated: ", "source_session: " + eng.SessionID(), "run the tests, tag, push the tag"} {
		if !strings.Contains(body, want) {
			t.Errorf("SKILL.md lacks %q:\n%s", want, body)
		}
	}
	sk, ok := eng.skillMgr.Get("Release Flow")
	if !ok || sk.Description != "发布新版本" {
		t.Fatalf("registered skill = %+v, %v", sk, ok)
	}
	// Two-part lines still work; the description is the steps' start.
	if got := eng.applyReview("SKILL: 部署 | 构建镜像，推送，滚动更新"); len(got.added) != 1 {
		t.Fatalf("two-part skill not saved: %v", got)
	}
}

// The review runs with the turn-end background work, and its new skills are
// in the summary line.
func TestBackgroundSummaryReportsNewSkills(t *testing.T) {
	prov := &mockProvider{responses: append(workTurnResponses(), mockResponse{content: "SKILL: Go Release | 发布 Go 版本 | go test, tag, push"})}
	eng := workReviewEngine(t, prov)
	armReview(eng)
	rec := &summaryRecorder{}
	eng.OnBackgroundSummary = rec.record
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "question"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	eng.WaitBackground(context.Background())
	eng.waitReview()
	got := rec.all()
	if len(got) != 1 || len(got[0].NewSkills) != 1 || got[0].NewSkills[0] != "Go Release" || !got[0].Notable() {
		t.Fatalf("summaries = %+v", got)
	}
}

// autoSkillFile is where writeAutoSkill puts the skill called name.
func autoSkillFile(t *testing.T, name string) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cove", "skills", "auto-"+skillSlug(name), "SKILL.md")
}

// Names that slug the same ("K8s Deploy" and "k8s-deploy") get different
// directories: the slug carries a hash of the name.
func TestAutoSkillSlugCarriesNameHash(t *testing.T) {
	if a, b := skillSlug("K8s Deploy"), skillSlug("k8s-deploy"); a == b {
		t.Fatalf("both slug to %q", a)
	}
	eng := reviewEngine(t, &mockProvider{})
	eng.applyReview("SKILL: K8s Deploy | a | steps one")
	eng.applyReview("SKILL: k8s-deploy | b | steps two")
	for name, want := range map[string]string{"K8s Deploy": "steps one", "k8s-deploy": "steps two"} {
		data, err := os.ReadFile(autoSkillFile(t, name))
		if err != nil || !strings.Contains(string(data), want) {
			t.Fatalf("%s: %q, %v", name, data, err)
		}
	}
}

// A skill file the user edited (no "generated:" line) or one naming another
// skill is left alone.
func TestAutoSkillDoesNotOverwriteForeignFile(t *testing.T) {
	eng := reviewEngine(t, &mockProvider{})
	for _, body := range []string{
		"---\nname: Release\ndescription: mine\n---\nmy own steps\n",
		"---\nname: Other\ndescription: x\ngenerated: 2026-01-01T00:00:00Z\n---\nother steps\n",
	} {
		path := autoSkillFile(t, "Release")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		res := eng.applyReview("SKILL: Release | new | new steps")
		if len(res.added)+len(res.updated) != 0 {
			t.Fatalf("foreign file replaced: %+v", res)
		}
		if data, _ := os.ReadFile(path); string(data) != body {
			t.Fatalf("file rewritten:\n%s", data)
		}
	}
}

// Learning an auto skill again rewrites its file and counts as an update.
func TestAutoSkillRelearnedCountsAsUpdate(t *testing.T) {
	eng := reviewEngine(t, &mockProvider{})
	if res := eng.applyReview("SKILL: Release | v1 | old steps"); len(res.added) != 1 {
		t.Fatalf("first: %+v", res)
	}
	res := eng.applyReview("SKILL: Release | v2 | new steps")
	if len(res.updated) != 1 || len(res.added) != 0 {
		t.Fatalf("second: %+v", res)
	}
	if data, _ := os.ReadFile(autoSkillFile(t, "Release")); !strings.Contains(string(data), "new steps") {
		t.Fatalf("file not updated:\n%s", data)
	}
}

// A learned skill never replaces a user or built-in skill of the same name.
func TestAutoSkillDoesNotShadowExistingSkill(t *testing.T) {
	eng := reviewEngine(t, &mockProvider{})
	eng.skillMgr.Register(skills.Skill{Name: "commit", Prompt: "user's commit skill", FilePath: filepath.Join(t.TempDir(), "commit", "SKILL.md")})
	res := eng.applyReview("SKILL: commit | c | learned steps")
	if len(res.added)+len(res.updated) != 0 {
		t.Fatalf("shadowed: %+v", res)
	}
	if sk, _ := eng.skillMgr.Get("commit"); sk.Prompt != "user's commit skill" {
		t.Fatalf("skill replaced: %+v", sk)
	}
}
