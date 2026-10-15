package main

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

func TestWrapCells(t *testing.T) {
	got := wrapCells("halo dunia ini prompt panjang\nbaris dua "+strings.Repeat("x", 25)+" 日本語日本語日本語", 12)
	for _, r := range got {
		if runewidth.StringWidth(r) > 12 {
			t.Fatalf("row %q is wider than 12 cells (%q)", r, got)
		}
	}
	if got[0] != "halo dunia" || got[2] != "panjang" || got[3] != "baris dua" {
		t.Fatalf("rows = %q", got)
	}
	// Words longer than a row are split, and nothing is lost.
	if joined := strings.Join(got, ""); strings.Count(joined, "x") != 25 || !strings.Contains(joined, "日本語日本語日本語") {
		t.Fatalf("content lost: %q", got)
	}
}

func TestShortPaths(t *testing.T) {
	u := &termUI{cwd: "/Users/me/proj", home: "/Users/me"}
	cases := map[[2]string]string{
		{"read", "read /Users/me/proj/go.mod"}:          "read go.mod",
		{"edit", "edit /Users/me/other/a.go"}:           "edit ~/other/a.go",
		{"bash", "bash cat /Users/me/proj/x | wc -l"}:   "bash cat ~/proj/x | wc -l",
		{"read", "read /etc/hosts"}:                     "read /etc/hosts",
		{"grep", `grep "a" in /Users/me/proj/internal`}: `grep "a" in ~/proj/internal`,
	}
	for in, want := range cases {
		if got := u.shortPaths(in[0], in[1]); got != want {
			t.Errorf("shortPaths(%q) = %q, want %q", in[1], got, want)
		}
	}
}
