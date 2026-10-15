package stats

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Lookups normalise ids the same way for config overrides and the
// models.dev table; overrides keep the tests off the generated values.
func TestLookupPrice(t *testing.T) {
	model := Price{Input: 3, Output: 15}
	flash := Price{Input: 0.5, Output: 2}
	exact := Price{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4}
	table := map[string]Price{"claude-test-9-9": model, "gemini-test-flash": flash, "ollama/my-local-model": exact}
	cases := []struct {
		provider, model string
		want            Price
		ok              bool
	}{
		{"anthropic", "claude-test-9-9", model, true},
		{"openrouter", "anthropic/claude-test-9.9", model, true},      // vendor prefix, dotted version
		{"anthropic", "claude-test-9-9-20261001", model, true},        // date suffix
		{"google-antigravity", "gemini-test-flash-high", flash, true}, // effort suffix
		{"gemini", "gemini-test-flash-preview", flash, true},          // -preview
		{"ollama", "my-local-model", exact, true},                     // provider/model key
		{"lmstudio", "my-local-model", Price{}, false},                // another provider's key
	}
	for _, c := range cases {
		got, ok := Prices{Overrides: table}.lookup(c.provider, c.model)
		if !reflect.DeepEqual(got, c.want) || ok != c.ok {
			t.Errorf("lookupPrice(%q, %q) = %v, %v; want %v, %v", c.provider, c.model, got, ok, c.want, c.ok)
		}
	}
	// An override beats the catalog, and the catalog beats the built-in table.
	catalog := map[string]Price{"claude-opus-5-5": model}
	if got, _ := (Prices{Overrides: map[string]Price{"claude-opus-5-5": exact}, Catalog: catalog}).lookup("anthropic", "claude-opus-5-5-20261001"); !reflect.DeepEqual(got, exact) {
		t.Errorf("override lost to the catalog: %v", got)
	}
	if got, _ := (Prices{Catalog: catalog}).lookup("anthropic", "claude-opus-5-5"); !reflect.DeepEqual(got, model) {
		t.Errorf("catalog lost to the built-in table: %v", got)
	}
	if _, ok := (Prices{}).lookup("anthropic", "claude-opus-5-5"); !ok {
		t.Error("claude-opus-5-5 missing from the built-in table")
	}
}

func TestPriceRequest(t *testing.T) {
	opus := Price{Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5}
	r := Record{Input: 1e6, Output: 1e6, CacheRead: 1e6, CacheWrite: 1e6}
	s, uncached := priceRequest(opus, &r)
	if s != (Spend{Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5}) || !near(uncached, 12) {
		t.Fatalf("5m cache: %+v uncached %v", s, uncached)
	}
	r.LongCache = true // 1h writes bill at 2x input
	if s, _ := priceRequest(opus, &r); s.CacheWrite != 8 {
		t.Fatalf("1h cache write: %v, want 8", s.CacheWrite)
	}
	// No cache-write price: writes bill as plain input.
	if s, _ := priceRequest(Price{Input: 0.75, Output: 3.75, CacheRead: 0.075}, &Record{CacheWrite: 1e6, LongCache: true}); s.CacheWrite != 0.75 {
		t.Fatalf("unpriced writes: %v, want 0.75", s.CacheWrite)
	}
}

// Long prompts switch to the tier price; fields the tier leaves out keep
// the base price.
func TestPriceTiers(t *testing.T) {
	p := Price{Input: 4, Output: 20, CacheRead: 0.4, Tiers: []Tier{{Above: 272000, Input: 8, Output: 30}, {Above: 1e6, Input: 16}}}
	short, _ := priceRequest(p, &Record{Input: 100000, CacheRead: 172000, Output: 1e6})
	if !near(short.Input, 0.4) || !near(short.Output, 20) {
		t.Fatalf("at the threshold: %+v", short)
	}
	long, _ := priceRequest(p, &Record{Input: 100000, CacheRead: 200000, Output: 1e6})
	if !near(long.Input, 0.8) || !near(long.Output, 30) || !near(long.CacheRead, 0.08) {
		t.Fatalf("above 272k: %+v", long)
	}
	huge, _ := priceRequest(p, &Record{Input: 2e6, Output: 1e6})
	if !near(huge.Input, 32) || !near(huge.Output, 20) {
		t.Fatalf("above 1M: %+v", huge)
	}
}

func TestBuild(t *testing.T) {
	now := time.Date(2026, 10, 13, 15, 0, 0, 0, time.Local)
	at := func(days int, hour int) int64 {
		return time.Date(2026, 10, 13-days, hour, 0, 0, 0, time.Local).UnixMilli()
	}
	recs := []Record{
		{Kind: "request", TS: at(20, 9), Provider: "anthropic", Model: "claude-opus-5-5", Input: 1e6, Folder: "/old"}, // outside 7d
		{Kind: "request", TS: at(3, 9), Provider: "anthropic", Model: "claude-opus-5-5", Class: "main", Folder: "/a",
			Input: 1e6, CacheRead: 3e6, Output: 1e5},
		{Kind: "request", TS: at(3, 11), Provider: "ollama", Model: "qwen3", Class: "subagent", Folder: "/a", Input: 500, Error: "boom 1"},
		{Kind: "tool", TS: at(3, 11), Tool: "bash"},
		{Kind: "tool", TS: at(1, 8), Tool: "bash", ToolError: true},
		{Kind: "request", TS: at(0, 14), Provider: "anthropic", Model: "claude-opus-5-5", Class: "main", Folder: "/b",
			Input: 1e6, Error: "boom 2", DurationMs: 2000, TTFTMs: 500},
	}
	// Pinned prices keep the expected costs independent of models.dev.
	prices := Prices{Catalog: map[string]Price{"claude-opus-5-5": {Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5}}}
	rep, err := Build(recs, "7d", now, prices)
	if err != nil {
		t.Fatal(err)
	}
	o := rep.Overall
	if o.Requests != 3 || o.Failed != 2 || o.Unpriced != 1 {
		t.Fatalf("requests %d failed %d unpriced %d", o.Requests, o.Failed, o.Unpriced)
	}
	// opus: 2M fresh input $8 + 3M cache reads $0.6 + 0.1M output $2.
	if !near(o.Cost, 10.6) {
		t.Fatalf("cost %v, want 10.6", o.Cost)
	}
	// Input side cost 8.6 instead of 20 uncached.
	if !near(o.CacheSavings, 1-8.6/20) {
		t.Fatalf("cache savings %v", o.CacheSavings)
	}
	if o.AvgDuration != 2000 || o.AvgTTFT != 500 {
		t.Fatalf("latency %v/%v: requests without timings must not count", o.AvgDuration, o.AvgTTFT)
	}
	if len(rep.Series) != 8 { // Oct 6 .. Oct 13, empty days included
		t.Fatalf("series has %d days, want 8", len(rep.Series))
	}
	sum := 0
	for _, b := range rep.Series {
		sum += b.Requests
	}
	if sum != 3 || rep.Series[4].Requests != 2 || rep.Series[6].Requests != 0 || rep.Series[7].Errors != 1 {
		t.Fatalf("series %+v", rep.Series)
	}
	if len(rep.Errors) != 2 || rep.Errors[0].Error != "boom 2" {
		t.Fatalf("errors not newest first: %+v", rep.Errors)
	}
	if len(rep.ByTool) != 1 || rep.ByTool[0].Calls != 2 || rep.ByTool[0].Errors != 1 {
		t.Fatalf("tools %+v", rep.ByTool)
	}
	if len(rep.ByFolder) != 2 || rep.ByFolder[0].Key != "/a" { // sorted by cost: /a $6.60, /b $4
		t.Fatalf("folders %+v", rep.ByFolder)
	}
	if _, err := Build(recs, "2w", now, prices); err == nil {
		t.Fatal("unknown range accepted")
	}
	day, _ := Build(recs, "24h", now, prices)
	if day.Bucket != "hour" || len(day.Series) != 25 || day.Overall.Requests != 1 {
		t.Fatalf("24h: bucket %s, %d buckets, %d requests", day.Bucket, len(day.Series), day.Overall.Requests)
	}
}

// Session files predate the stats log: Load imports their requests once
// (forks repeat the history) and only for the time before the log starts.
func TestLoadImportsSessions(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, lines ...string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	header := `{"type":"header","cwd":"/proj","created":"2026-10-01T10:00:00Z"}`
	call := `{"type":"message","ts":1790000000,"message":{"role":"assistant","provider":"anthropic","model":"claude-opus-5-5","usage":{"input":10,"output":5},"content":[{"type":"tool_use","id":"t1","name":"bash"}]}}`
	result := `{"type":"message","ts":1790000001,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true}]}}`
	write("sessions/proj/a.jsonl", header, call, result)
	write("sessions/proj/fork.jsonl", header, call, result,
		`{"type":"message","ts":1790000002,"message":{"role":"assistant","provider":"anthropic","model":"claude-opus-5-5","usage":{"input":20,"output":5},"content":[]}}`,
		// Logged by stats.jsonl already: must not be imported twice.
		`{"type":"message","ts":1790000100,"message":{"role":"assistant","provider":"anthropic","model":"claude-opus-5-5","usage":{"input":99,"output":1},"content":[]}}`)
	// Old format without per-message ts: dated by the header.
	write("sessions/old/b.jsonl", `{"type":"header","cwd":"/old","created":"2026-09-01T08:00:00Z"}`,
		`{"type":"message","message":{"role":"assistant","provider":"openai-codex","model":"gpt-5.6","usage":{"input":1,"output":1},"content":[]}}`)
	write("stats.jsonl", `{"kind":"request","ts":1790000100000,"provider":"anthropic","model":"claude-opus-5-5","input":99,"output":1}`)

	recs, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var inputs []int
	tools := 0
	for _, r := range recs {
		if r.Kind == "tool" {
			tools++
			if r.Tool != "bash" || !r.ToolError {
				t.Fatalf("tool record %+v", r)
			}
			continue
		}
		inputs = append(inputs, r.Input)
	}
	want := []int{1, 10, 20, 99}
	if len(inputs) != len(want) || tools != 1 {
		t.Fatalf("requests %v tools %d; want %v and 1 tool", inputs, tools, want)
	}
	for i := range want {
		if inputs[i] != want[i] {
			t.Fatalf("requests %v; want %v", inputs, want)
		}
	}
	if old := recs[0]; old.Folder != "/old" || old.TS != time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("untimestamped message: %+v", old)
	}

	// The import ran once: later loads keep the history without the
	// session files and still pick up new log records.
	if err := os.RemoveAll(filepath.Join(dir, "sessions")); err != nil {
		t.Fatal(err)
	}
	OpenLog(dir).Add(Record{Kind: "request", TS: 1790000200000, Provider: "anthropic", Model: "claude-opus-5-5", Input: 7})
	again, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(recs)+1 || again[len(again)-1].Input != 7 {
		t.Fatalf("second load: %d records, want %d", len(again), len(recs)+1)
	}
}
