package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A line that comes again and again is written once, with a count.
func TestServerLogFoldsRepeats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-x.log")
	l, err := openServerLog(path, logLimit)
	if err != nil {
		t.Fatal(err)
	}
	l.Write([]byte("start\n"))
	for range 1000 {
		l.Write([]byte("waiting for plugin...\n"))
	}
	l.Write([]byte("wait"))                  // a line in pieces
	l.Write([]byte("ing for plugin...\nco")) // ...and the next one begun
	l.Write([]byte("nnected\n"))
	l.Write([]byte("done\ndone\n"))
	l.Write([]byte("no newline"))
	l.Close()
	want := "start\nwaiting for plugin...\n(the line above came 1000 more times)\nconnected\ndone\n(the line above came 1 more time)\nno newline\n"
	if got := readLog(t, path); got != want {
		t.Fatalf("log:\n%s\nwant:\n%s", got, want)
	}
}

// Past the limit the log moves to .1, replacing the one there, so the
// two hold at most about twice the limit.
func TestServerLogRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-x.log")
	l, err := openServerLog(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 500 {
		l.Write([]byte(strings.Repeat("x", 40) + string(rune('a'+i%26)) + "\n"))
	}
	l.Write([]byte("last line\n"))
	l.Close()
	cur, old := readLog(t, path), readLog(t, path+".1")
	if len(cur) > 1100 || len(old) > 1100 {
		t.Fatalf("sizes %d and %d", len(cur), len(old))
	}
	if !strings.HasPrefix(cur, "--- earlier output: "+path+".1\n") || !strings.HasSuffix(cur, "last line\n") {
		t.Fatalf("current log:\n%s", cur)
	}
	if l.runStart() != 0 {
		t.Fatalf("run start %d after a rotation", l.runStart())
	}
}

// A log an older tego let grow is moved aside when the next run starts;
// one that is small goes on, with the run starting at its end.
func TestServerLogTooBigAtStart(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "mcp-big.log")
	os.WriteFile(big, []byte(strings.Repeat("old\n", 1000)), 0o600)
	l, err := openServerLog(big, 1000)
	if err != nil {
		t.Fatal(err)
	}
	l.Write([]byte("new run\n"))
	l.Close()
	if got := readLog(t, big); got != "--- earlier output: "+big+".1\nnew run\n" {
		t.Fatalf("log %q", got)
	}
	// .1 keeps only the old log's end, from a line start.
	if old := readLog(t, big+".1"); len(old) > 1000 || !strings.HasPrefix(old, "old\n") || !strings.HasSuffix(old, "old\n") {
		t.Fatalf(".1 has %d bytes: %q…", len(old), old[:min(len(old), 20)])
	}
	small := filepath.Join(dir, "mcp-small.log")
	os.WriteFile(small, []byte("old run\n"), 0o600)
	l, _ = openServerLog(small, 1000)
	if l.runStart() != int64(len("old run\n")) {
		t.Fatalf("run start %d", l.runStart())
	}
	l.Write([]byte("new run\n"))
	l.Close()
	if got := readLog(t, small); got != "old run\nnew run\n" {
		t.Fatalf("log %q", got)
	}
}
