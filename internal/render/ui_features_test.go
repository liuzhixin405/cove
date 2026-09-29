package render

import (
	"strings"
	"testing"
	"time"
)

func TestDiff(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\n"
	newText := "a\nb\nC\nd\ne\nf\ng\nh\ni\n"
	d := Diff(old, newText)
	if d.Added != 2 || d.Removed != 1 {
		t.Fatalf("+%d -%d, want +2 -1:\n%s", d.Added, d.Removed, d.Text)
	}
	for _, want := range []string{"-c", "+C", "+i", "@@ -1,"} {
		if !strings.Contains(d.Text, want) {
			t.Errorf("diff lacks %q:\n%s", want, d.Text)
		}
	}
	if Diff("same\n", "same\n").Text != "" {
		t.Fatal("equal texts produced a diff")
	}
	created := Diff("", "x\ny\n")
	if created.Added != 2 || !strings.HasPrefix(created.Text, "@@ -0,0 +1,2 @@") {
		t.Fatalf("new file diff:\n%s", created.Text)
	}
	if d := Diff("x\n", "y\n"); d.Summary() != "+1 −1" {
		t.Fatalf("summary %q", d.Summary())
	}
}

// An expandable tool block shows its "#id" (the handle for /x) and a long
// step its duration; Expanded shows the hidden output, a diff coloured.
func TestToolBlockHandleAndExpanded(t *testing.T) {
	b := ToolBlock("7", "bash", "go test ./...", "", "line1\nline2\nline3", false, 2*time.Second)
	out := Collapsed(b, 80, Styles{})
	if !strings.Contains(out, "#7") || !strings.Contains(out, "2.0s") {
		t.Fatalf("collapsed:\n%s", out)
	}
	exp := Expanded(b, 80, 2, Styles{}, true)
	if !strings.Contains(exp, "line1") || !strings.Contains(exp, "line2") || strings.Contains(exp, "line3") || !strings.Contains(exp, "还有 1 行") {
		t.Fatalf("expanded:\n%s", exp)
	}
	b.Full, b.Diff = "@@ -1 +1 @@\n-a\n+b", true
	if exp := Expanded(b, 80, 0, Styles{}, false); !strings.Contains(exp, "\x1b[32m+b") || !strings.Contains(exp, "\x1b[31m-a") {
		t.Fatalf("diff not coloured:\n%q", exp)
	}
}

func TestPlanView(t *testing.T) {
	out := "Task list (3 items):\n[✓] todo-1. 读懂解析器 [high]\n[>] todo-2. 修复分词器 [high]\n[ ] todo-3. 补测试 []\n"
	b := ToolBlock("3", "todowrite", "", "", out, false, 0)
	s := Collapsed(b, 80, Styles{})
	for _, want := range []string{"计划", "1/3 完成", "✓ 读懂解析器", "▶ 修复分词器", "○ 补测试"} {
		if !strings.Contains(s, want) {
			t.Errorf("plan lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "[high]") || strings.Contains(s, "todo-") {
		t.Errorf("plan shows the raw markup:\n%s", s)
	}
}

func TestHighlightCode(t *testing.T) {
	got := highlightCode(`func main() { s := "hi" // done`, "go")
	if !strings.Contains(got, sgrKeyword+"func"+sgrFgOff) || !strings.Contains(got, sgrString+`"hi"`+sgrFgOff) || !strings.Contains(got, sgrComment+"// done") {
		t.Fatalf("highlight = %q", got)
	}
	if highlightCode("plain text", "unknownlang") != "plain text" {
		t.Fatal("an unknown language was coloured")
	}
}

func TestMarkdownTableAndCode(t *testing.T) {
	m := NewMarkdownStream()
	out := m.Write("| 名称 | 值 |\n|---|---|\n| a | 1 |\n| 长一些 | 22 |\n\n```go\nreturn nil\n```\n")
	out += m.Flush()
	plain := StripControls(out)
	for _, want := range []string{"┌", "│ 名称   │ 值 │", "├", "│ 长一些 │ 22 │", "└"} {
		if !strings.Contains(plain, want) {
			t.Errorf("table lacks %q:\n%s", want, plain)
		}
	}
	if !strings.Contains(out, sgrKeyword+"return"+sgrFgOff) {
		t.Errorf("code not highlighted:\n%q", out)
	}
}
