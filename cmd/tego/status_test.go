package main

import (
	"strings"
	"testing"
	"time"
)

func TestWorkingGlows(t *testing.T) {
	// Without colours the text is plain.
	b := newStatusBar(palette{}, "/p", iconSet{})
	b.update(func(s *statusInfo) { s.busy, s.busySince = true, time.Now() })
	if line := b.activity(80); !strings.Contains(line, " Working… 0.") {
		t.Fatalf("activity %q", line)
	}
	// With colours a bold spot moves across the word, one rune per frame.
	c := palette{reset: "<r>", bold: "<b>", magenta: "<m>"}
	a, z := glow(c, "Working…", 2), glow(c, "Working…", 3)
	if a == z || !strings.Contains(a, "<r><b><m>o") || !strings.Contains(z, "<r><b><m>r") {
		t.Fatalf("glow frames:\n%s\n%s", a, z)
	}
	if plain := strings.NewReplacer("<r>", "", "<b>", "", "<m>", "").Replace(a); plain != "Working…" {
		t.Fatalf("text changed: %q", plain)
	}
}
