package lineedit

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// escEnd returns the index just past the escape sequence starting at s[i]
// (which must be ESC) and whether the sequence is complete. It recognises CSI
// (ESC [ ... final 0x40–0x7E), OSC (ESC ] ... BEL or ST) and nF/Fp/Fe
// escapes (intermediates 0x20–0x2F, then one final byte). An incomplete
// sequence reports len(s), false.
func escEnd[T ~string | ~[]byte](s T, i int) (int, bool) {
	if i+1 >= len(s) {
		return len(s), false
	}
	switch s[i+1] {
	case '[':
		for j := i + 2; j < len(s); j++ {
			if s[j] >= 0x40 && s[j] <= 0x7e {
				return j + 1, true
			}
		}
		return len(s), false
	case ']':
		for j := i + 2; j < len(s); j++ {
			switch s[j] {
			case 0x07:
				return j + 1, true
			case 0x1b:
				if j+1 >= len(s) {
					return len(s), false
				}
				if s[j+1] == '\\' {
					return j + 2, true
				}
				return j, true // ESC aborts the OSC and starts a new sequence
			}
		}
		return len(s), false
	default:
		j := i + 1
		for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
			j++
		}
		if j >= len(s) {
			return len(s), false
		}
		return j + 1, true
	}
}

// visibleWidth returns the display width of s ignoring ANSI escape sequences
// (CSI such as SGR, OSC such as hyperlinks, and two-byte ESC sequences).
func visibleWidth(s string) int {
	if strings.IndexByte(s, 0x1b) < 0 {
		return widthCond.StringWidth(s)
	}
	w := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i, _ = escEnd(s, i)
			continue
		}
		j := i + 1
		for j < len(s) && s[j] != 0x1b {
			j++
		}
		w += widthCond.StringWidth(s[i:j])
		i = j
	}
	return w
}

// truncateANSI shortens s to at most w display cells, keeping the escape
// sequences that precede the cut. A truncated result ends in "…", closes an
// open OSC 8 hyperlink and resets SGR attributes.
func truncateANSI(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if visibleWidth(s) <= w {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s) + 16)
	cells, link := 0, false
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			end, _ := escEnd(s, i)
			seq := s[i:end]
			if strings.HasPrefix(seq, "\x1b]8;") {
				// OSC 8 ; params ; URI ST — an empty URI closes the link.
				body := strings.TrimRight(strings.TrimSuffix(seq[4:], "\x1b\\"), "\x07")
				_, uri, _ := strings.Cut(body, ";")
				link = uri != ""
			}
			sb.WriteString(seq)
			i = end
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := widthCond.RuneWidth(r)
		if cells+rw > w-1 {
			break
		}
		sb.WriteString(s[i : i+size])
		cells += rw
		i += size
	}
	sb.WriteString("…")
	if link {
		sb.WriteString("\x1b]8;;\x1b\\")
	}
	sb.WriteString("\x1b[0m")
	return sb.String()
}

// continuationPrefix indents continuation lines by the prompt width w so
// they line up with the text after the prompt.
func continuationPrefix(w int) string {
	return strings.Repeat(" ", w)
}

// layout is the visual arrangement of a buffer: one string per terminal row
// (including prompt and continuation prefixes) plus the cursor cell.
type layout struct {
	rows   []string
	curRow int
	curCol int
}

// computeLayout wraps buf into rows of at most width cells. The prompt
// occupies promptW cells on the first row; every further logical line starts
// with prefix, also promptW cells wide. Rows are broken explicitly so the
// renderer never relies on terminal auto-wrap. When the cursor sits at the
// end of a row that is exactly full, an empty row is added for it.
func computeLayout(prompt, prefix string, promptW int, buf []rune, pos, width int) layout {
	if width < 1 {
		width = 1
	}
	var (
		l   layout
		sb  strings.Builder
		row int
		col = promptW
	)
	sb.WriteString(prompt)
	flush := func() {
		l.rows = append(l.rows, sb.String())
		sb.Reset()
		row++
		col = 0
	}
	// placeEOLCursor positions the cursor at a line end (before '\n' or EOF).
	placeEOLCursor := func() {
		if col >= width {
			flush()
		}
		l.curRow, l.curCol = row, col
	}
	for i, r := range buf {
		if r == '\n' {
			if i == pos {
				placeEOLCursor()
			}
			flush()
			sb.WriteString(prefix)
			col = promptW
			continue
		}
		cw := runeCells(r)
		if col+cw > width && col > 0 {
			flush()
		}
		if i == pos {
			l.curRow, l.curCol = row, col
		}
		if r == '\t' {
			sb.WriteString(strings.Repeat(" ", tabWidth))
		} else {
			sb.WriteRune(r)
		}
		col += cw
	}
	if pos >= len(buf) {
		placeEOLCursor()
	}
	l.rows = append(l.rows, sb.String())
	return l
}

// appendCSI appends ESC [ n final when n > 0 (cursor movements).
func appendCSI(b []byte, n int, final byte) []byte {
	if n <= 0 {
		return b
	}
	b = append(b, 0x1b, '[')
	b = strconv.AppendInt(b, int64(n), 10)
	return append(b, final)
}
