package markdown

import (
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// wrapRow word-wraps one rendered line (no newline; SGR and OSC-8 escapes
// allowed) to width columns and appends it to dst. Continuation rows start
// with hang, which is hangW columns wide (a list item's indent, a quote's
// bars). Rows break at runs of spaces, which are dropped there; a word
// longer than a row is broken inside. A style or link open at a break is
// closed before the newline and opened again after hang, so the margins a
// terminal UI puts at the row's edges stay plain.
func wrapRow(dst, row, hang []byte, hangW, width int) []byte {
	if width < hangW+10 || visWidth(row) <= width { // too narrow to help, or fits
		return append(dst, row...)
	}
	// Where rows break: row[from:to] (spaces and escapes) becomes a newline.
	type cut struct{ from, to int }
	var cuts []cut
	col, wordW := 0, 0
	gap, wordStart := -1, -1 // the last run of spaces a row may break at, and the word after it
	inGap, seen := false, false
	last := -1 // the last visible character that is not a space
	for i := 0; i < len(row); {
		c := row[i]
		if c == 0x1b {
			i = skipEscape(row, i)
			continue
		}
		if c == ' ' {
			if !inGap {
				inGap = true
				gap = -1
				if seen { // a space inside the hang (a list marker's) is not a break
					gap = i
				}
			}
			col++
			i++
			continue
		}
		cw, n := runeCols(row, i, col)
		if inGap {
			inGap, wordStart, wordW = false, i, 0
		}
		if cw > 0 && col+cw > width {
			if gap >= 0 && wordStart > gap {
				cuts = append(cuts, cut{gap, wordStart})
				col = hangW + wordW
			} else { // no space on this row: break inside the word
				cuts = append(cuts, cut{i, i})
				col, wordStart, wordW = hangW, i, 0
			}
			gap, seen = -1, col > hangW
		}
		col += cw
		wordW += cw
		if col > hangW {
			seen = true
		}
		last = i
		i += n
	}

	var sgr, link []byte // the style and link open now
	track := func(seq []byte) {
		switch {
		case len(seq) > 2 && seq[1] == '[' && seq[len(seq)-1] == 'm':
			if string(seq) == sgrReset {
				sgr = nil
			} else {
				sgr = append(sgr[:0:0], seq...)
			}
		case len(seq) > 4 && seq[1] == ']' && seq[2] == '8':
			if string(seq) == osc8Close {
				link = nil
			} else {
				link = append(link[:0:0], seq...)
			}
		}
	}
	k := 0
	for i := 0; i < len(row); {
		if k < len(cuts) && i == cuts[k].from {
			for j := i; j < cuts[k].to; { // the cut's escapes still apply
				if row[j] != 0x1b {
					j++
					continue
				}
				e := skipEscape(row, j)
				track(row[j:e])
				dst = append(dst, row[j:e]...)
				j = e
			}
			if len(sgr) > 0 {
				dst = append(dst, sgrReset...)
			}
			if len(link) > 0 {
				dst = append(dst, osc8Close...)
			}
			dst = append(dst, '\n')
			dst = append(dst, hang...)
			dst = append(dst, link...)
			dst = append(dst, sgr...)
			i = cuts[k].to
			k++
			continue
		}
		c := row[i]
		if c == 0x1b {
			e := skipEscape(row, i)
			track(row[i:e])
			dst = append(dst, row[i:e]...)
			i = e
			continue
		}
		if c == ' ' && i > last { // trailing spaces would only spill over
			i++
			continue
		}
		_, n := runeCols(row, i, 0)
		dst = append(dst, row[i:i+n]...)
		i += n
	}
	return dst
}

// runeCols is the columns and bytes of the character at row[i], at column
// col (for tabs).
func runeCols(row []byte, i, col int) (cols, n int) {
	c := row[i]
	switch {
	case c == '\t':
		return (col/8+1)*8 - col, 1
	case c < 0x20 || c == 0x7f:
		return 0, 1
	case c < utf8.RuneSelf:
		return 1, 1
	}
	r, n := utf8.DecodeRune(row[i:])
	return runewidth.RuneWidth(r), n
}
