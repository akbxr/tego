package stats

import (
	"strings"
	"testing"
)

func TestFastModeCostsTwice(t *testing.T) {
	p := Price{Input: 4, Output: 20, CacheRead: 0.4, CacheWrite: 5} // Opus 5.5
	r := Record{Input: 1000, Output: 500, CacheRead: 20000, CacheWrite: 3000}
	std, _ := priceRequest(p, &r)
	r.Fast = true
	fast, _ := priceRequest(p, &r)
	if got, want := fast.Total(), 2*std.Total(); got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("fast %.6f, want twice %.6f", got, std.Total())
	}
}

func TestCacheWritePremium(t *testing.T) {
	ps := Prices{}
	for model, want := range map[string]bool{
		"claude-opus-5-5":  true,  // Anthropic: writes cost 1.25x input
		"gpt-5.6":          true,  // OpenAI GPT-5.6 too
		"gpt-5.5":          false, // writes billed as input
		"gemini-3.8-flash": false,
		"no-such-model":    false,
	} {
		if got := ps.CacheWritePremium("", model); got != want {
			t.Errorf("%s: premium=%v, want %v", model, got, want)
		}
	}
}

func TestOutputSpeedSkipsBursts(t *testing.T) {
	var tot Totals
	for _, r := range []Record{
		{Output: 100, DurationMs: 3000, TTFTMs: 2995},                   // old record: thinking hid the start, a 5 ms "window"
		{Output: 131, DurationMs: 3020, TTFTMs: 3010, GenStartMs: 740},  // new: timed from message_start
		{Output: 834, DurationMs: 8820, TTFTMs: 8800, GenStartMs: 8780}, // a buffered answer: no window
		{Output: 200, DurationMs: 2500, TTFTMs: 500, Error: "boom"},     // failed
		{Output: 154, DurationMs: 2950, TTFTMs: 1380},                   // old record that streamed
		{Output: 1305, GenOutput: 0, DurationMs: 400, GenStartMs: 100},  // too short to time
	} {
		tot.add(&r, Price{}, false)
	}
	tot.finish()
	// (131 + 154) tokens over (2.28 + 1.57) s
	if want := 285 / 3.85; tot.AvgTPS < want-0.5 || tot.AvgTPS > want+0.5 {
		t.Fatalf("AvgTPS %.1f, want %.1f", tot.AvgTPS, want)
	}
}

func TestHeatLevel(t *testing.T) {
	for _, c := range []struct{ n, peak, want int }{
		{0, 50, 0}, {1, 50, 1}, {3, 50, 1}, {13, 50, 3}, {50, 50, 4}, {49, 50, 4}, {5, 0, 0},
	} {
		if got := heatLevel(c.n, c.peak); got != c.want {
			t.Errorf("heatLevel(%d, %d) = %d, want %d", c.n, c.peak, got, c.want)
		}
	}
}

// Web search is billed per search on top of its tokens, and is in the
// totals, the terminal summary and the dashboard's cost.
func TestWebSearchFee(t *testing.T) {
	haiku := Price{Input: 0.1, Output: 0.5, CacheRead: 0.01, CacheWrite: 0.125}
	r := Record{Kind: "request", Class: "auxiliary", Input: 10000, Output: 400, WebSearches: 3}
	s, _ := priceRequest(haiku, &r)
	if s.WebSearch != 0.03 || s.Total() != 0.001+0.0002+0.03 {
		t.Fatalf("spend %+v, total %v", s, s.Total())
	}
	var tot Totals
	tot.add(&r, haiku, true)
	tot.add(&Record{Input: 100, Output: 10}, haiku, true)
	if tot.WebSearches != 3 || tot.Spend.WebSearch != 0.03 || tot.Cost != tot.Spend.Total() {
		t.Fatalf("totals %+v", tot)
	}
	rows := kindRows(tot)
	last := rows[len(rows)-1]
	if last.Class != kindSearch || last.Searches != 3 || last.Cost != 0.03 || last.Tokens != 0 {
		t.Fatalf("kind row %+v", last)
	}
	var out strings.Builder
	Summary(&out, &Report{Range: "all", Overall: tot}, false)
	if !strings.Contains(out.String(), "3 web searches $0.03") {
		t.Fatalf("summary:\n%s", out.String())
	}
	// Google grounding costs $14 per 1,000.
	if s, _ := priceRequest(haiku, &Record{Model: "gemini-3.5-flash-lite", WebSearches: 2}); s.WebSearch != 0.028 {
		t.Fatalf("gemini search fee %v", s.WebSearch)
	}
	tot.WebSearches = 1
	out.Reset()
	if Summary(&out, &Report{Range: "all", Overall: tot}, false); !strings.Contains(out.String(), "1 web search $") {
		t.Fatalf("summary, one search:\n%s", out.String())
	}
}
