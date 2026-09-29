package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/log"
	"github.com/liuzhixin405/cove/internal/textutil"
)

// Self-review (config "done_self_review"): before a turn that changed files
// may end, a read-only review sub-agent reads the turn's diff and reports
// real defects; findings go back to the model, once per turn. The
// verification gate catches what fails to build or test; this catches the
// logic a test does not cover. It costs a sub-agent run per turn, so it is
// off by default, and "auto" limits it to changes of selfReviewMinLines or
// more.

// selfReviewMinLines is the smallest change "auto" reviews (added plus
// removed lines).
const selfReviewMinLines = 40

// selfReviewMaxDiffBytes caps the diff handed to the reviewer; it reads
// anything beyond with its own tools.
const selfReviewMaxDiffBytes = 40 * 1024

// selfReviewNoIssues is what the reviewer answers when it finds nothing.
const selfReviewNoIssues = "NO_ISSUES"

// selfReview runs the review for this turn when it is due and returns the
// reviewer's findings; "" when there are none or no review ran.
func (e *Engine) selfReview(ctx context.Context, request string) string {
	mode := e.config.DoneSelfReview
	if (mode != "on" && mode != "auto") || e.selfReviewed || e.runtime == nil {
		return ""
	}
	runner, ok := e.runtime.AgentRunner.(api.AgentRunner)
	if !ok {
		return ""
	}
	files := e.changedFilesThisTurn()
	if len(files) == 0 {
		return ""
	}
	diff, lines := turnDiff(ctx, e.projectCwd(), files)
	if diff == "" || (mode == "auto" && lines < selfReviewMinLines) {
		return ""
	}
	e.selfReviewed = true
	e.engineOutput(fmt.Sprintf("  \x1b[2m正在自审本轮改动（%d 个文件，%d 行）…\x1b[0m", len(files), lines))
	res, err := runner.Run(ctx, "review", selfReviewTask(request, diff))
	if err != nil || res == nil || !res.Success {
		why := "no result"
		if err != nil {
			why = err.Error()
		} else if res != nil {
			why = res.Error
		}
		log.Warnf("self-review did not finish: %s", why)
		e.engineOutput("  \x1b[2m自审未完成，已跳过\x1b[0m")
		return ""
	}
	out := strings.TrimSpace(res.Output)
	if out == "" || strings.HasPrefix(out, selfReviewNoIssues) || out == strings.Trim(selfReviewNoIssues, "`") {
		e.engineOutput("  \x1b[2m自审通过\x1b[0m")
		return ""
	}
	e.engineOutput("  \x1b[33m! 自审发现问题，已交回模型处理\x1b[0m")
	return out
}

func selfReviewTask(request, diff string) string {
	return "Review the change below, made for this user request:\n<request>\n" + textutil.ClipRunes(strings.TrimSpace(request), 2000) +
		"\n</request>\n\nLook only for real defects: wrong logic, unhandled errors or edge cases, callers or tests the change breaks, " +
		"security problems, a part of the request left undone. Read the surrounding code with your tools when the diff is not enough. " +
		"Do not comment on style or naming. If you find no real defect, reply with exactly " + selfReviewNoIssues +
		". Otherwise list each defect as: file:line — the problem — why it is one.\n\n<diff>\n" + diff + "\n</diff>"
}

// selfReviewFeedback is the message that hands the findings to the model.
func selfReviewFeedback(findings string) string {
	return "[self_review] A reviewer read this turn's diff and reported:\n" + findings +
		"\n\nFix the findings that are real defects and verify again. If a finding is wrong, do not change the code for it; say why in your final report."
}

// turnDiff returns the diff of files (absolute paths) in dir and its number
// of changed lines: git diff against HEAD for tracked files, and the content
// of new or untracked ones. Outside a git repository every file is shown in
// full.
func turnDiff(ctx context.Context, dir string, files []string) (string, int) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var sb strings.Builder
	lines := 0
	inGit := gitOutput(ctx, dir, "rev-parse", "--is-inside-work-tree") == "true"
	for _, f := range files {
		rel, err := filepath.Rel(dir, f)
		if err != nil {
			rel = f
		}
		rel = filepath.ToSlash(rel)
		if inGit && gitOutput(ctx, dir, "ls-files", "--error-unmatch", "--", rel) != "" {
			d := gitOutput(ctx, dir, "diff", "--no-color", "--no-ext-diff", "HEAD", "--", rel)
			for _, l := range strings.Split(d, "\n") {
				if (strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++")) || (strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---")) {
					lines++
				}
			}
			if d != "" {
				sb.WriteString(d + "\n")
			}
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue // deleted: git diff would have shown a tracked one
		}
		body := string(data)
		lines += strings.Count(body, "\n") + 1
		fmt.Fprintf(&sb, "=== new file %s ===\n%s\n", rel, body)
	}
	return textutil.ClipBytes(sb.String(), selfReviewMaxDiffBytes, "\n... [diff truncated; read the files for the rest]"), lines
}

// gitOutput runs git in dir and returns its trimmed output, "" on failure.
func gitOutput(ctx context.Context, dir string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
