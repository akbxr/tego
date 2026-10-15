package stats

import "testing"

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
