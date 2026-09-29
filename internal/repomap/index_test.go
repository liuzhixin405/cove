package repomap

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestIndexRefreshIsIncremental: an unchanged tree is not re-parsed, and a
// changed, added or removed file shows up in the next refresh.
func TestIndexRefreshIsIncremental(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.go": "package a\nfunc A() {}\n",
		"b.go": "package a\nfunc B() {}\n",
	})
	ix := IndexFor(root)
	if d := ix.Refresh(); len(d.Added) != 2 {
		t.Fatalf("first refresh added %v, want both files", d.Added)
	}
	if d := ix.Refresh(); d.Changed() {
		t.Fatalf("unchanged tree reported %+v", d)
	}

	later := time.Now().Add(2 * time.Second)
	writeTree(t, root, map[string]string{"a.go": "package a\nfunc Renamed() {}\n", "c.go": "package a\nfunc C() {}\n"})
	_ = os.Chtimes(filepath.Join(root, "a.go"), later, later)
	if err := os.Remove(filepath.Join(root, "b.go")); err != nil {
		t.Fatal(err)
	}
	d := ix.Refresh()
	if strings.Join(d.Added, ",") != "c.go" || strings.Join(d.Modified, ",") != "a.go" || strings.Join(d.Removed, ",") != "b.go" {
		t.Fatalf("diff = %+v", d)
	}
	out := FormatFileMaps(ix.Files())
	if !strings.Contains(out, "func Renamed()") || strings.Contains(out, "func A()") || strings.Contains(out, "func B()") {
		t.Fatalf("index not updated:\n%s", out)
	}
}

// TestIndexIsSharedPerRoot: the tool, the excerpt and /context reach the
// same parse state through IndexFor.
func TestIndexIsSharedPerRoot(t *testing.T) {
	root := t.TempDir()
	if IndexFor(root) != IndexFor(root+string(filepath.Separator)) {
		t.Fatal("IndexFor returned two indexes for one root")
	}
}

// TestIndexConcurrentRefresh: parallel tool calls refresh the same index.
func TestIndexConcurrentRefresh(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.go": "package a\nfunc A() {}\n"})
	ix := IndexFor(root)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ix.Refresh(); _ = ix.Files() }()
	}
	wg.Wait()
	if fms := ix.Files(); len(fms) != 1 {
		t.Fatalf("files = %+v", fms)
	}
}

// TestQueryListsRelatedFiles: a query brings the callers and callees of the
// matching file, which a plain name match misses.
func TestQueryListsRelatedFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"billing/invoice.go": "package billing\ntype Invoice struct{}\nfunc Total(i *Invoice) int { return applyTax(i) }\n",
		"billing/tax.go":     "package billing\nfunc applyTax(i *Invoice) int { return 0 }\n",
		"api/handler.go":     "package api\nfunc HandleCheckout() { _ = billing.Total(nil) }\n",
		"misc/unrelated.go":  "package misc\nfunc Unrelated() {}\n",
	})
	out := Query(root, []string{"invoice"}, 12*1024)
	i := strings.Index(out, "Related by references")
	if i < 0 {
		t.Fatalf("no related section:\n%s", out)
	}
	related := out[i:]
	for _, want := range []string{"billing/tax.go", "api/handler.go"} {
		if !strings.Contains(related, want) {
			t.Errorf("related section lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "misc/unrelated.go") {
		t.Errorf("unconnected file listed:\n%s", out)
	}
}

func TestRegexLanguages(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"Svc.cs":    "namespace App;\npublic sealed class OrderService : IOrderService\n{\n    public async Task<Order> GetOrderAsync(int id, CancellationToken ct)\n    {\n        return await _repo.FindAsync(id);\n    }\n}\npublic interface IOrderService {}\npublic record OrderDto(int Id);\n",
		"Main.java": "public class Main {\n    public static void main(String[] args) {\n        run(args);\n    }\n    private int run(String[] a) { return 0; }\n}\n",
		"lib.rs":    "pub struct Parser {}\nimpl Parser {\n    pub fn parse(&self, src: &str) -> Ast {\n    }\n}\npub(crate) async fn load(path: &Path) {}\npub trait Visitor {}\n",
		"ui.ts":     "export const useCart = (id: string) => {\n}\nexport type Cart = { id: string }\nexport enum Color { Red }\n",
	})
	out := NewGenerator(root).Generate(50)
	for _, want := range []string{
		"class OrderService", "GetOrderAsync(int id, CancellationToken ct)", "interface IOrderService", "record OrderDto",
		"class Main", "main(String[] args)", "run(String[] a)",
		"struct Parser", "fn parse(&self, src: &str)", "fn load(path: &Path)", "trait Visitor",
		"const useCart", "type Cart", "enum Color",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("map lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"return await", "run(args)"} {
		if strings.Contains(out, bad) {
			t.Errorf("statement %q taken for a definition:\n%s", bad, out)
		}
	}
}
