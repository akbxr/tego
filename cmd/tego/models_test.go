package main

import "testing"

func TestSplitThinking(t *testing.T) {
	cases := []struct{ in, model, level string }{
		{"anthropic/claude-opus-4-6:high", "anthropic/claude-opus-4-6", "high"},
		{"opus:off", "opus", "off"},
		// OpenRouter variants use ":" too; only real levels are split off.
		{"openrouter/deepseek/deepseek-r1:free", "openrouter/deepseek/deepseek-r1:free", ""},
		{"sonnet", "sonnet", ""},
	}
	for _, c := range cases {
		if m, l := splitThinking(c.in); m != c.model || l != c.level {
			t.Errorf("splitThinking(%q) = %q, %q; want %q, %q", c.in, m, l, c.model, c.level)
		}
	}
}
