package main

import "testing"

func TestStatsArgs(t *testing.T) {
	cases := []struct {
		in    string
		cache bool
		rng   string
	}{
		{"", false, "7d"},
		{"24h", false, "24h"},
		{"cache", true, "7d"},
		{" cache  all ", true, "all"},
		{"cache 7d x", true, "7d x"}, // left for ParseRange to reject
	}
	for _, c := range cases {
		if cache, rng := statsArgs(c.in); cache != c.cache || rng != c.rng {
			t.Errorf("statsArgs(%q) = %v, %q; want %v, %q", c.in, cache, rng, c.cache, c.rng)
		}
	}
}
