package tools

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// withSpillDir points spillDir at a fresh directory for one test.
func withSpillDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "out")
	old := spillDir
	spillDir = dir
	t.Cleanup(func() { spillDir = old })
	return dir
}

var spillPathRE = regexp.MustCompile(`are in (\S+) \(read it with offset/limit\)`)

func spillFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestCutOutputIsSavedWholeToAFile(t *testing.T) {
	dir := withSpillDir(t)
	out, err := runShell(context.Background(), t.TempDir(), "seq 1 100000; echo done >&2", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "lines omitted; all 100001 lines are in ") {
		t.Fatalf("result does not name the file:\n%.300s … %.300s", out, out[len(out)-300:])
	}
	m := spillPathRE.FindStringSubmatch(out)
	if m == nil || filepath.Dir(m[1]) != dir {
		t.Fatalf("path %v not in %s", m, dir)
	}
	data, err := os.ReadFile(m[1])
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for i := 1; i <= 100000; i++ {
		want.WriteString(strconv.Itoa(i) + "\n")
	}
	want.WriteString("done\n")
	if string(data) != want.String() {
		t.Fatalf("file has %d bytes, want the whole output (%d bytes)", len(data), want.Len())
	}
	if info, _ := os.Stat(m[1]); info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v, want 0600", info.Mode().Perm())
	}
}

func TestOutputThatFitsLeavesNoFile(t *testing.T) {
	dir := withSpillDir(t)
	for _, cmd := range []string{
		"echo short",
		"seq 1 400", // past the head, but head and tail hold all of it
	} {
		out, err := runShell(context.Background(), t.TempDir(), cmd, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "omitted") || strings.Contains(out, dir) {
			t.Fatalf("%s: cut or named a file:\n%s", cmd, out)
		}
		if files := spillFiles(t, dir); len(files) != 0 {
			t.Fatalf("%s: left %v", cmd, files)
		}
	}
}

func TestSpillFileIsBoundedAndOldOnesAreRemoved(t *testing.T) {
	dir := withSpillDir(t)
	old := spillMax
	spillMax = 100 << 10
	t.Cleanup(func() { spillMax = old })
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale, fresh := filepath.Join(dir, "bash-old.log"), filepath.Join(dir, "bash-recent.log")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-spillMaxAge - time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}

	out, err := runShell(context.Background(), t.TempDir(), "seq 1 100000", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`the first 100 KB of the output are in (\S+) `).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("result does not say the file is partial:\n…%s", out[len(out)-400:])
	}
	if info, err := os.Stat(m[1]); err != nil || info.Size() != spillMax {
		t.Fatalf("file size %v (%v), want %d", info.Size(), err, spillMax)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a file older than a day was kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("a recent file was removed")
	}
}

func TestBackgroundTextNeverSpills(t *testing.T) {
	dir := withSpillDir(t)
	capText(strings.Repeat("line\n", 100000))
	if files := spillFiles(t, dir); len(files) != 0 {
		t.Fatalf("capText wrote %v", files)
	}
}
