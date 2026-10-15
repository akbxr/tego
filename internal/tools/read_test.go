package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestReadPagesLongLinesByBytes(t *testing.T) {
	dir := t.TempDir()
	env := &Env{Cwd: dir}
	line := strings.Repeat("y", 199)
	var b strings.Builder
	for range 1000 { // ~200 KB, far below the 2000-line limit
		b.WriteString(line + "\n")
	}
	_ = os.WriteFile(filepath.Join(dir, "big.txt"), []byte(b.String()), 0o644)

	out, err := run(t, &readTool{env}, map[string]any{"path": "big.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > readMaxBytes+200 {
		t.Fatalf("read returned %d bytes", len(out))
	}
	m := regexp.MustCompile(`showing lines 1-(\d+) of 1000 \(50 KB per read\); use offset=(\d+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no paging notice in %q", out[len(out)-120:])
	}
	next, _ := strconv.Atoi(m[2])
	if last, _ := strconv.Atoi(m[1]); next != last+1 {
		t.Fatalf("offset %d does not follow line %d", next, last)
	}
	// The next page starts exactly where the first stopped.
	out2, err := run(t, &readTool{env}, map[string]any{"path": "big.txt", "offset": next})
	if err != nil || !strings.HasPrefix(out2, m[2]+"\t") {
		t.Fatalf("second page: %v %q", err, out2[:20])
	}
}
