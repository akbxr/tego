package markdown

import (
	"regexp"
	"strings"
	"testing"
)

var sgrRE = regexp.MustCompile("\x1b\\[[0-9;]*m|\x1b\\]8;;[^\x1b]*\x1b\\\\")

func plainRows(s string) []string {
	return strings.Split(strings.TrimSuffix(sgrRE.ReplaceAllString(s, ""), "\n"), "\n")
}

// Paragraphs wrap at spaces, never inside a word that fits a row.
func TestParagraphsWrapAtWords(t *testing.T) {
	got := plainRows(Render("Batalkan perpanjangan itu di dashboard Polar. Email A dengan tanggal October 25 tetap berlaku.", 30, false))
	want := []string{
		"Batalkan perpanjangan itu di",
		"dashboard Polar. Email A",
		"dengan tanggal October 25",
		"tetap berlaku.",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got\n%s", strings.Join(got, "\n"))
	}
}

// List items and quotes continue under their text; a word longer than a
// row is broken inside.
func TestWrapHangsAndLongWords(t *testing.T) {
	got := plainRows(Render("- Biarkan tertagih. Periodenya bergeser ke 25 November\n  1. nested item that is long enough to wrap\n> quoted text that also needs to wrap here\n\nhttps://example.com/a/very/long/path/that/cannot/fit", 30, false))
	want := []string{
		"• Biarkan tertagih. Periodenya",
		"  bergeser ke 25 November",
		"  1. nested item that is long",
		"     enough to wrap",
		"│ quoted text that also needs",
		"│ to wrap here",
		"",
		"https://example.com/a/very/lon",
		"g/path/that/cannot/fit",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got\n%s", strings.Join(got, "\n"))
	}
	for _, r := range got {
		if w := textWidth(r); w > 30 {
			t.Fatalf("row of %d columns: %q", w, r)
		}
	}
}

// A style open at a break is closed before the newline and opened again
// after it, so the margins stay plain; the text is the same.
func TestWrapReopensStyles(t *testing.T) {
	out := Render("plain **bold words that run across the row edge** and [a link here](https://x.y) end", 24, true)
	for _, row := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if w := visWidth([]byte(row)); w > 24 {
			t.Fatalf("row of %d columns: %q", w, row)
		}
		// Nothing styled runs into the row's end.
		if i, j := strings.LastIndex(row, "\x1b[1m"), strings.LastIndex(row, sgrReset); i > j {
			t.Fatalf("bold left open at the row's end: %q", row)
		}
		if i, j := strings.LastIndex(row, "\x1b]8;;https"), strings.LastIndex(row, osc8Close); i > j {
			t.Fatalf("link left open at the row's end: %q", row)
		}
	}
	if !strings.Contains(out, "\n\x1b[1m") {
		t.Fatalf("bold not reopened after a break: %q", out)
	}
	if got := strings.Join(plainRows(out), " "); got != "plain bold words that run across the row edge and a link here (https://x.y) end" {
		t.Fatalf("text changed: %q", got)
	}
}

// Code is not wrapped: its lines stay as they are.
func TestCodeIsNotWrapped(t *testing.T) {
	src := "```\n" + strings.Repeat("x ", 30) + "\n```"
	got := plainRows(Render(src, 20, false))
	if len(got) != 3 || strings.TrimSpace(got[1]) != strings.TrimSpace(strings.Repeat("x ", 30)) {
		t.Fatalf("code wrapped: %q", got)
	}
}
