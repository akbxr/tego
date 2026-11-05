package stats

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var cachePrices = Prices{Overrides: map[string]Price{
	"m":     {Input: 4, Output: 20, CacheRead: 0.4, CacheWrite: 5},
	"other": {Input: 4, Output: 20, CacheRead: 0.4, CacheWrite: 5},
}}

// req is a request of session s at minute min with the given cache use.
func req(s string, min float64, class string, read, write int) Record {
	return Record{Kind: "request", TS: int64(min * 60_000), Session: s, Folder: "/w/" + s, Class: class,
		Provider: "p", Model: "m", Input: 2, CacheRead: read, CacheWrite: write}
}

func TestRewriteCauses(t *testing.T) {
	long := func(r Record) Record { r.LongCache = true; return r }
	reason := func(r Record, why string) Record { r.CacheMiss = why; return r }
	model := func(r Record, m string) Record { r.Model = m; return r }
	cases := []struct {
		name       string
		prev       *Record
		r          Record
		cause      string
		tokens     int
		gapMinutes float64
	}{
		{"first request", nil, req("s", 0, "main", 0, 100_000), "", 0, 0},
		{"turn growth", ptr(req("s", 0, "main", 90_000, 10_000)), req("s", 1, "main", 100_000, 3_000), "", 0, 1},
		// The API names a reason although the read was fine.
		{"reason on a good read", ptr(req("s", 0, "main", 90_000, 10_000)), reason(req("s", 1, "main", 100_000, 3_000), "previous_message_not_found"), "", 0, 1},
		{"small rewrite", ptr(req("s", 0, "main", 90_000, 10_000)), req("s", 1, "main", 99_000, 2_000), "", 0, 1},
		{"expired 5m", ptr(req("s", 0, "main", 90_000, 10_000)), req("s", 6, "main", 4_000, 98_000), CauseExpired, 96_002, 6},
		{"expired 1h", ptr(long(req("s", 0, "main", 90_000, 10_000))), long(req("s", 61, "main", 4_000, 98_000)), CauseExpired, 96_002, 61},
		{"1h cache still warm", ptr(long(req("s", 0, "main", 90_000, 10_000))), long(req("s", 30, "main", 4_000, 98_000)), CauseUnknown, 96_002, 30},
		{"keep-alive miss", ptr(long(req("s", 0, "main", 90_000, 10_000))), long(req("s", 50, "keepalive", 4_000, 96_000)), CauseKeepAlive, 96_000, 50},
		// Sent after the cache lapsed (a Mac asleep): still the keep-alive's fault.
		{"keep-alive after expiry", ptr(long(req("s", 0, "main", 90_000, 10_000))), long(req("s", 180, "keepalive", 0, 100_000)), CauseKeepAlive, 100_000, 180},
		{"keep-alive read", ptr(long(req("s", 0, "main", 90_000, 10_000))), long(req("s", 55, "keepalive", 100_000, 1_500)), "", 0, 55},
		{"keep-alive, no previous", nil, req("s", 0, "keepalive", 0, 50_000), CauseKeepAlive, 50_000, -1},
		{"shrank", ptr(req("s", 0, "main", 190_000, 10_000)), req("s", 1, "main", 5_000, 45_000), CauseShrunk, 45_000, 1},
		{"model switch", ptr(req("s", 0, "main", 90_000, 10_000)), model(req("s", 1, "main", 0, 101_000), "other"), CauseSwitch, 100_002, 1},
		{"api reason", ptr(req("s", 0, "main", 90_000, 10_000)), reason(req("s", 1, "main", 7_000, 94_000), "messages_changed"), "messages_changed", 93_002, 1},
		{"unknown", ptr(req("s", 0, "main", 90_000, 10_000)), req("s", 1, "main", 7_000, 94_000), CauseUnknown, 93_002, 1},
	}
	for _, c := range cases {
		cause, tokens, gap := rewrite(c.prev, &c.r)
		wantGap := int64(c.gapMinutes * 60_000)
		if c.prev == nil {
			wantGap = -1
		}
		if cause != c.cause || tokens != c.tokens || gap != wantGap {
			t.Errorf("%s: got %q %d gap %d; want %q %d gap %d", c.name, cause, tokens, gap, c.cause, c.tokens, wantGap)
		}
	}
}

func ptr(r Record) *Record { return &r }

func TestBuildCache(t *testing.T) {
	failed := req("a", 2, "main", 0, 0)
	failed.Error, failed.Input = "overloaded", 0
	ka := req("a", 50, "keepalive", 4_000, 59_000)
	ka.Folder = "" // keep-alives log no folder: the session's is used
	recs := []Record{
		req("a", 0, "main", 0, 50_000),         // first: expected
		req("a", 1, "main", 50_000, 10_000),    // growth
		failed,                                 // no usage: compared past it
		req("a", 3, "main", 4_000, 58_000),     // unknown, against the request at 1
		req("a", 10, "main", 4_000, 59_000),    // expired
		ka,                                     // keep-alive miss
		req("b", 0, "subagent", 0, 30_000),     // first
		req("b", 1, "subagent", 2_000, 29_000), // unknown
		{Kind: "request", TS: 60_000, Class: "auxiliary", Provider: "p", Model: "m", Input: 100, CacheWrite: 9_000}, // no session
		{Kind: "tool", TS: 60_000, Tool: "bash"},
	}
	sortRecs(recs)
	rep, err := BuildCache(recs, "all", time.UnixMilli(3_600_000), cachePrices)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Requests != 9 || rep.KeepAlives != 1 {
		t.Errorf("requests %d keep-alives %d", rep.Requests, rep.KeepAlives)
	}
	if rep.Wasted.Requests != 3 || rep.Wasted.Tokens != 56_002+59_000+28_002 {
		t.Errorf("wasted %+v", rep.Wasted)
	}
	if rep.Expired.Requests != 1 || rep.Expired.Tokens != 58_002 {
		t.Errorf("expired %+v", rep.Expired)
	}
	// Waste costs the write price less the read price: $4.60 a million.
	if want := float64(rep.Wasted.Tokens) * 4.6 / 1e6; !near(rep.Wasted.Cost, want) {
		t.Errorf("wasted cost %v, want %v", rep.Wasted.Cost, want)
	}
	var written int64
	for _, r := range recs {
		written += int64(r.CacheWrite)
	}
	if rep.Written.Tokens != written || !near(rep.Written.Cost, float64(written)*5/1e6) {
		t.Errorf("written %+v", rep.Written)
	}
	if len(rep.ByCause) != 3 || rep.ByCause[0].Key != CauseUnknown || rep.ByCause[1].Key != CauseKeepAlive ||
		rep.ByCause[2].Key != CauseExpired || !rep.ByCause[2].Expected {
		t.Errorf("by cause %+v", rep.ByCause)
	}
	if len(rep.ByClass) != 3 || rep.ByClass[0].Key != "keepalive" {
		t.Errorf("by agent %+v", rep.ByClass)
	}
	if len(rep.Top) != 3 || rep.Top[0].Class != "keepalive" || rep.Top[0].Folder != "/w/a" || rep.Top[0].GapMs != 40*60_000 {
		t.Errorf("top %+v", rep.Top)
	}

	// A range compares its first request with the one before it.
	rep, _ = BuildCache(recs, "24h", time.UnixMilli(5*60_000+24*3_600_000), cachePrices)
	if rep.Requests != 2 || rep.Expired.Requests != 1 || rep.Wasted.Requests != 1 {
		t.Errorf("range: %+v", rep)
	}
	if _, err := BuildCache(recs, "2w", time.Now(), cachePrices); err == nil {
		t.Error("unknown range accepted")
	}
}

func mustJSON(t *testing.T, r Record) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sortRecs(recs []Record) {
	for i := 1; i < len(recs); i++ {
		for j := i; j > 0 && recs[j].TS < recs[j-1].TS; j-- {
			recs[j], recs[j-1] = recs[j-1], recs[j]
		}
	}
}

func TestCacheSummary(t *testing.T) {
	recs := []Record{
		req("a", 0, "main", 0, 50_000),
		req("a", 50, "keepalive", 4_000, 46_000),
		req("a", 60, "main", 50_000, 1_000),
	}
	rep, _ := BuildCache(recs, "all", time.UnixMilli(3_600_000), cachePrices)
	var b bytes.Buffer
	CacheSummary(&b, rep, false)
	out := b.String()
	for _, want := range []string{"tego stats cache · all time", "Wasted", "46K tokens", "Keep-alives  1 sent, 1 missed",
		"keep-alive miss", "Costliest rewrites", "/w/a", "50m"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	b.Reset()
	empty, _ := BuildCache(nil, "7d", time.Now(), cachePrices)
	CacheSummary(&b, empty, false)
	if !strings.Contains(b.String(), "No prompt-cache use") {
		t.Errorf("empty: %s", b.String())
	}
}

func TestTailAndGap(t *testing.T) {
	for in, want := range map[string]string{
		"/a/b/c":                          "/a/b/c",
		"/Users/x/Code/projects/3d-video": "…/projects/3d-video",
		"/x/a-very-long-folder-name-here": "…g-folder-name-here",
	} {
		if got := tail(in, 19); got != want {
			t.Errorf("tail(%q) = %q, want %q", in, got, want)
		}
	}
	for ms, want := range map[int64]string{-1: "–", 8_000: "8s", 4 * 60_000: "4m", 3*3_600_000 + 5*60_000: "3h05m", 50 * 3_600_000: "2d02h"} {
		if got := Gap(ms); got != want {
			t.Errorf("Gap(%d) = %q, want %q", ms, got, want)
		}
	}
}

// The dashboard's cost panel names the rewrites.
func TestDashboardCacheWaste(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var lines []string
	for i, r := range []Record{
		req("a", 0, "main", 0, 50_000),
		req("a", 1, "main", 4_000, 47_000),
	} {
		r.TS = now.Add(time.Duration(i-2) * time.Minute).UnixMilli()
		r.Model = "claude-opus-5-5"
		lines = append(lines, mustJSON(t, r))
	}
	if err := os.WriteFile(filepath.Join(dir, "stats.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&Dashboard{Dir: dir}).Handler())
	defer srv.Close()
	code, body := get(t, srv, "/?range=24h")
	if code != 200 || !strings.Contains(body, "Written to the cache again") || !strings.Contains(body, "46K tokens") {
		t.Fatalf("status %d, no waste line", code)
	}
}
