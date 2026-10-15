package stats

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(body)
}

// The page template only fails when it runs, so render it against data
// that takes every branch: priced, unpriced, failed, many folders, empty.
func TestDashboardRenders(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var lines []string
	add := func(r Record) {
		b, _ := json.Marshal(r)
		lines = append(lines, string(b))
	}
	for i := range 12 {
		add(Record{Kind: "request", TS: now.Add(-time.Duration(i) * time.Hour).UnixMilli(), Folder: fmt.Sprintf("/p%d", i),
			Class: "main", Provider: "anthropic", Model: "claude-opus-5-5", Input: 1000, CacheRead: 9000, Output: 500, DurationMs: 2000, TTFTMs: 400})
	}
	add(Record{Kind: "request", TS: now.UnixMilli(), Folder: "/p0", Class: "subagent", Provider: "ollama", Model: "qwen3", Input: 10, Output: 5})
	add(Record{Kind: "request", TS: now.UnixMilli(), Folder: "/p0", Class: "auxiliary", Provider: "anthropic", Model: "claude-haiku-4-5",
		Error: "429 <rate limited>"})
	add(Record{Kind: "tool", TS: now.UnixMilli(), Tool: "bash", ToolError: true})
	if err := os.WriteFile(filepath.Join(dir, "stats.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&Dashboard{Dir: dir}).Handler())
	defer srv.Close()
	for _, rng := range Ranges {
		code, body := get(t, srv, "/?range="+rng)
		if code != 200 {
			t.Fatalf("%s: status %d: %s", rng, code, body)
		}
		for _, want := range []string{`data-r="requests">14<`, "Where the money went", `class="panel model`, "Show 2 more", `"label":`,
			"1 request unpriced", "429 &lt;rate limited&gt;", "Titles and search", `class="seg s-bad"`, "Response time"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: page lacks %q", rng, want)
			}
		}
	}
	if code, _ := get(t, srv, "/?range=2w"); code != 400 {
		t.Fatalf("unknown range: status %d", code)
	}
	if code, _ := get(t, srv, "/fonts/martian-mono.woff2"); code != 200 {
		t.Fatalf("font: status %d", code)
	}

	empty := httptest.NewServer((&Dashboard{Dir: t.TempDir()}).Handler())
	defer empty.Close()
	if code, body := get(t, empty, "/?range=7d"); code != 200 || !strings.Contains(body, "No requests in the last 7 days") {
		t.Fatalf("empty: status %d", code)
	}
}
