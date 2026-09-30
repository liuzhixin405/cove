package tool

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// asciiPrefix is more than the 8KB read used to sniff the encoding from.
func asciiPrefix() []byte {
	return bytes.Repeat([]byte("// plain ascii line, padding the sniff window\n"), 300) // ~13.8KB
}

func readInput(t *testing.T, path string, extra Input) (Result, *Runtime) {
	t.Helper()
	in := Input{"filePath": path}
	for k, v := range extra {
		in[k] = v
	}
	rt := &Runtime{}
	res, err := NewReadTool().Call(context.Background(), in, Context{Cwd: filepath.Dir(path), Runtime: rt})
	if err != nil {
		t.Fatal(err)
	}
	return res, rt
}

// A file that is ASCII for the first 8KB and GBK after it used to be taken
// for UTF-8: the GBK bytes came back as invalid UTF-8, without the note.
func TestReadDetectsGBKAfterSniffWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "late.c")
	data := append(asciiPrefix(), gbkComment...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	res, rt := readInput(t, path, nil)
	if res.IsError || !strings.Contains(res.Data, "// 中文注释") || !strings.Contains(res.Data, "GBK-encoded") {
		t.Fatalf("read = %q, want the decoded GBK line and the GBK note", res.Data[max(0, len(res.Data)-300):])
	}
	// The snapshot still covers the whole file: write may replace it.
	if err := rt.Files().Check(path, data); err != nil {
		t.Errorf("file not recorded as seen: %v", err)
	}
}

func TestReadDetectsLatin1AfterSniffWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "late.txt")
	if err := os.WriteFile(path, append(asciiPrefix(), latin1Text...), 0o644); err != nil {
		t.Fatal(err)
	}
	// Even a window that shows only ASCII lines names the file's encoding:
	// edit and write judge the whole file.
	res, _ := readInput(t, path, Input{"limit": float64(5)})
	if res.IsError || !strings.Contains(res.Data, "not valid UTF-8") {
		t.Fatalf("read = %q, want the not-UTF-8 note", res.Data)
	}
}

func TestReadDetectsNULAfterSniffWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob.dat")
	if err := os.WriteFile(path, append(asciiPrefix(), []byte("x\x00\x01\x02\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := readInput(t, path, nil)
	if !res.IsError || !strings.Contains(res.Data, "binary") {
		t.Fatalf("read = %q, want a binary-file error", res.Data[:min(len(res.Data), 200)])
	}
	// Same when the file is not UTF-8 from the start.
	path2 := filepath.Join(dir, "blob2.dat")
	if err := os.WriteFile(path2, append(append(append([]byte{}, latin1Text...), asciiPrefix()...), 0), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, _ := readInput(t, path2, nil); !res.IsError || !strings.Contains(res.Data, "binary") {
		t.Fatalf("read = %q, want a binary-file error", res.Data[:min(len(res.Data), 200)])
	}
}

// A non-UTF-8 file used to be read into memory whole (plus decoded copies)
// whatever window was asked for, so a multi-GB cp1252 log exhausted memory.
func TestReadNonUTF8FileIsStreamed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")
	line := []byte("2024-01-01 caf\xe9 cr\xe8me br\xfbl\xe9e request served in 12ms\n")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat(line, 1<<14)
	const total = 48 << 20
	written := 0
	for written < total {
		n, err := f.Write(chunk)
		if err != nil {
			t.Fatal(err)
		}
		written += n
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	res, rt := readInput(t, path, Input{"offset": float64(3), "limit": float64(10)})
	runtime.ReadMemStats(&after)
	if res.IsError || !strings.Contains(res.Data, "not valid UTF-8") || !strings.Contains(res.Data, "3: 2024-01-01") {
		t.Fatalf("read = %q", res.Data)
	}
	if !strings.Contains(res.Data, "[next: offset=13]") {
		t.Errorf("window/total lines wrong: %q", res.Data)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Errorf("reading 10 lines of a %dMB file allocated %dMB", total>>20, alloc>>20)
	}
	// The tracker still has a snapshot of the whole file.
	data, _ := os.ReadFile(path)
	if err := rt.Files().Check(path, data); err != nil {
		t.Errorf("file not recorded as seen: %v", err)
	}
}

// Streaming GBK detection shows the same text as decoding the whole file.
func TestReadGBKWindowFromLargeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gbk.log")
	var data []byte
	for i := 0; i < 5000; i++ {
		data = append(data, mustGBK(t, "日志：请求完成\r\n")...)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := readInput(t, path, Input{"offset": float64(4999), "limit": float64(5)})
	if res.IsError || !strings.Contains(res.Data, "4999: 日志：请求完成\n5000: 日志：请求完成") || !strings.Contains(res.Data, "GBK-encoded") {
		t.Fatalf("read = %q", res.Data)
	}
	if !strings.Contains(res.Data, "(5000 lines total)") {
		t.Errorf("line count wrong: %q", res.Data)
	}
}
