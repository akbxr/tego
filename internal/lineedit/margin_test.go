package lineedit

import (
	"strings"
	"testing"
)

func TestContentMargin(t *testing.T) {
	tt := newTestTUI(Options{Margin: 2, Status: func(int) []string { return []string{"st"} }}, 20, 12)
	// A long line wraps before the right margin (16 cells of text per row),
	// a styled line gets its margin before the style, and the footer is
	// left alone.
	_, _ = tt.Writer().Write([]byte(strings.Repeat("a", 20) + "\n\x1b[2mdim\x1b[0m\n\nend\n"))
	got := tt.emu().all()
	want := []string{"  " + strings.Repeat("a", 16), "  aaaa", "  dim", "", "  end", "", ">", "st"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("screen %q\nwant   %q", got, want)
	}
	if w := tt.ContentWidth(); w != 16 {
		t.Fatalf("ContentWidth = %d", w)
	}
}
