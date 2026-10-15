package markdown

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

type align uint8

const (
	alignLeft align = iota
	alignCenter
	alignRight
)

// span is the range [a, b) of a slice; w is a display width where relevant.
type span struct{ a, b, w int }

// maxMinWidth caps the longest-word width a column asks for.
const maxMinWidth = 30

// tableState holds the reusable buffers of machine.table.
type tableState struct {
	aligns  []align
	widths  []int  // final column widths
	natural []int  // widest unwrapped line per column
	minw    []int  // longest word per column, capped at maxMinWidth
	items   []item // inline items of every cell segment
	segs    []span // cell segments (split at <br>) into items
	cells   []span // row-major cells into segs
	litems  []item // wrapped line fragments of the current row
	lines   []span // wrapped lines into litems, w = display width
	row     []span // per column of the current row: range of lines
	word    []item // fragments of the word being wrapped
	split   []string
}

// hasPipe reports whether l contains an unescaped '|'.
func hasPipe(l string) bool {
	for i := 0; i < len(l); i++ {
		switch l[i] {
		case '\\':
			i++
		case '|':
			return true
		}
	}
	return false
}

// splitRow appends the trimmed cells of a table row to dst.
func splitRow(dst []string, l string) []string {
	l = strings.TrimSpace(l)
	if strings.HasPrefix(l, "|") {
		l = l[1:]
	}
	if strings.HasSuffix(l, "|") && !strings.HasSuffix(l, "\\|") {
		l = l[:len(l)-1]
	}
	start := 0
	for i := 0; i < len(l); i++ {
		switch l[i] {
		case '\\':
			i++
		case '|':
			dst = append(dst, strings.TrimSpace(l[start:i]))
			start = i + 1
		}
	}
	return append(dst, strings.TrimSpace(l[start:]))
}

// delimAligns parses a delimiter row like "|:--|:-:|--:|" into dst; ok is
// false if l is not a delimiter row.
func delimAligns(dst []align, l string) ([]align, bool) {
	if !hasPipe(l) {
		return dst, false
	}
	var cells [16]string
	for _, c := range splitRow(cells[:0], l) {
		left := strings.HasPrefix(c, ":")
		right := strings.HasSuffix(c, ":")
		d := strings.Trim(c, ":")
		if d == "" || strings.Trim(d, "-") != "" {
			return dst, false
		}
		switch {
		case left && right && len(c) > 1:
			dst = append(dst, alignCenter)
		case right:
			dst = append(dst, alignRight)
		default:
			dst = append(dst, alignLeft)
		}
	}
	return dst, true
}

// isDelimRow reports whether l is a delimiter row for a header with n cells.
func isDelimRow(l string, n int) bool {
	var buf [16]align
	a, ok := delimAligns(buf[:0], l)
	return ok && len(a) == n
}

func countCells(l string) int {
	var buf [16]string
	return len(splitRow(buf[:0], l))
}

// table renders the held header, delimiter and body rows into m.buf as a
// box drawn to fit the terminal width, wrapping cell text as needed. Tables
// too narrow for one column per cell are printed raw.
func (m *machine) table() {
	t := &m.tbl
	t.aligns, _ = delimAligns(t.aligns[:0], m.held[1])
	ncol := len(t.aligns)
	width := m.w()
	overhead := 3*ncol + 1
	avail := width - overhead
	if avail < ncol {
		for _, l := range m.held {
			m.plain(l)
		}
		return
	}
	t.natural = zeroed(t.natural, ncol)
	t.minw = zeroed(t.minw, ncol)
	t.items, t.segs, t.cells = t.items[:0], t.segs[:0], t.cells[:0]
	for r, row := range m.held {
		if r == 1 {
			continue
		}
		t.split = splitRow(t.split[:0], row)
		for c := range ncol {
			text := ""
			if c < len(t.split) {
				text = t.split[c]
			}
			if strings.Contains(text, "\\|") {
				text = strings.ReplaceAll(text, "\\|", "|")
			}
			first := len(t.segs)
			for {
				seg, rest, more := cutBreak(text)
				a := len(t.items)
				t.items = compactItems(parseInline(t.items, seg, 0, ""), a)
				nat, word := measure(t.items[a:])
				t.segs = append(t.segs, span{a, len(t.items), nat})
				t.natural[c] = max(t.natural[c], nat)
				t.minw[c] = max(t.minw[c], 1, min(word, maxMinWidth))
				if !more {
					break
				}
				text = rest
			}
			t.cells = append(t.cells, span{first, len(t.segs), 0})
		}
	}
	t.widths = columnWidths(t.widths, t.natural, t.minw, width)

	m.border("┌", "┬", "┐")
	nrows := len(t.cells) / ncol
	for r := range nrows {
		base := style(0)
		if r == 0 {
			base = stBold
		}
		t.litems, t.lines, t.row = t.litems[:0], t.lines[:0], t.row[:0]
		height := 0
		for c, ce := range t.cells[r*ncol : (r+1)*ncol] {
			first := len(t.lines)
			for _, sg := range t.segs[ce.a:ce.b] {
				t.wrap(t.items[sg.a:sg.b], t.widths[c])
			}
			for len(t.lines)-first > 1 && t.lines[len(t.lines)-1].w == 0 {
				t.lines = t.lines[:len(t.lines)-1]
			}
			t.row = append(t.row, span{first, len(t.lines), 0})
			height = max(height, len(t.lines)-first)
		}
		for k := range height {
			m.dimText("│")
			for c, rc := range t.row {
				w := t.widths[c]
				m.buf = append(m.buf, ' ')
				if rc.a+k < rc.b {
					ln := t.lines[rc.a+k]
					pad := max(0, w-ln.w)
					lp := 0
					switch t.aligns[c] {
					case alignRight:
						lp = pad
					case alignCenter:
						lp = pad / 2
					}
					m.buf = appendSpaces(m.buf, lp)
					m.buf, _ = renderItems(m.buf, t.litems[ln.a:ln.b], base, m.color)
					m.buf = appendSpaces(m.buf, pad-lp)
				} else {
					m.buf = appendSpaces(m.buf, w)
				}
				m.buf = append(m.buf, ' ')
				m.dimText("│")
			}
			m.buf = append(m.buf, '\n')
		}
		if r == 0 || r < nrows-1 {
			m.border("├", "┼", "┤")
		}
	}
	m.border("└", "┴", "┘")
}

// columnWidths appends to dst the widths of columns with the given natural
// and minimum widths in a table at most width columns wide (borders
// included), reusing dst's storage. There must be room for at least one
// cell per column.
func columnWidths(dst, natural, minw []int, width int) []int {
	n := len(natural)
	overhead := 3*n + 1
	avail := width - overhead
	base := append(dst[:0], minw...)
	sumMin := sum(minw)
	if sumMin > avail {
		// Not even the longest words fit: share the space in proportion to
		// how much each column's longest word exceeds one cell.
		extra := avail - n
		want := sumMin - n
		given := 0
		for c := range base {
			base[c] = 1
			if want > 0 {
				e := (minw[c] - 1) * extra / want
				base[c] += e
				given += e
			}
		}
		for c := 0; given < extra && c < n; c++ {
			base[c]++
			given++
		}
	}
	if sum(natural)+overhead <= width {
		for c := range base {
			base[c] = max(base[c], natural[c])
		}
		return base
	}
	deficit := 0
	for c := range base {
		deficit += max(0, natural[c]-base[c])
	}
	spare := max(0, avail-sum(base))
	if deficit > 0 {
		for c := range base {
			base[c] += max(0, natural[c]-base[c]) * spare / deficit
		}
	}
	for left := avail - sum(base); left > 0; {
		grew := false
		for c := 0; c < n && left > 0; c++ {
			if base[c] < natural[c] {
				base[c]++
				left--
				grew = true
			}
		}
		if !grew {
			break
		}
	}
	return base
}

// wrap word-wraps items to width w, appending the fragments to t.litems and
// one span per line (at least one) to t.lines. Words wider than w are broken.
func (t *tableState) wrap(items []item, w int) {
	lineA := len(t.litems)
	cur := 0
	spaceMark, spaceW := 0, 0 // pending inter-word spaces in litems
	wordW := 0
	endLine := func() {
		t.lines = append(t.lines, span{lineA, len(t.litems), cur})
		lineA, cur = len(t.litems), 0
	}
	flushWord := func() {
		if len(t.word) == 0 {
			return
		}
		if cur > 0 && cur+spaceW+wordW > w {
			t.litems = t.litems[:spaceMark]
			endLine()
		} else {
			cur += spaceW
		}
		spaceW = 0
		if cur+wordW <= w {
			t.litems = append(t.litems, t.word...)
			cur += wordW
		} else {
			for _, f := range t.word {
				s := f.text
				from := 0
				for i, r := range s {
					rw := runewidth.RuneWidth(r)
					if cur > 0 && cur+rw > w {
						if i > from {
							t.litems = append(t.litems, item{text: s[from:i], st: f.st, url: f.url})
						}
						endLine()
						from = i
					}
					cur += rw
				}
				if from < len(s) {
					t.litems = append(t.litems, item{text: s[from:], st: f.st, url: f.url})
				}
			}
		}
		t.word, wordW = t.word[:0], 0
	}
	for _, it := range items {
		s := it.text
		for len(s) > 0 {
			n := strings.IndexAny(s, " \t")
			if n < 0 {
				n = len(s)
			}
			if n > 0 {
				t.word = append(t.word, item{text: s[:n], st: it.st, url: it.url})
				wordW += textWidth(s[:n])
				s = s[n:]
				continue
			}
			n = 0
			for n < len(s) && (s[n] == ' ' || s[n] == '\t') {
				n++
			}
			flushWord()
			if cur > 0 {
				if spaceW == 0 {
					spaceMark = len(t.litems)
				}
				t.litems = append(t.litems, item{text: s[:n], st: it.st, url: it.url})
				spaceW += n
			}
			s = s[n:]
		}
	}
	flushWord()
	if spaceW > 0 {
		t.litems = t.litems[:spaceMark]
	}
	endLine()
}

// measure returns the display width of items on one line and the width of
// their longest space-separated word.
func measure(items []item) (width, word int) {
	cw := 0
	for _, it := range items {
		s := it.text
		width += textWidth(s)
		for len(s) > 0 {
			n := strings.IndexAny(s, " \t")
			if n < 0 {
				cw += textWidth(s)
				break
			}
			cw += textWidth(s[:n])
			word, cw = max(word, cw), 0
			s = s[n+1:]
		}
	}
	return width, max(word, cw)
}

// compactItems drops the unmatched-delimiter bookkeeping of items[from:]
// (keeping only the printed characters) and removes empty items.
func compactItems(items []item, from int) []item {
	k := from
	for _, it := range items[from:] {
		if it.dch != 0 {
			it.text, it.dch = it.text[:it.n], 0
		}
		if it.text != "" {
			items[k] = it
			k++
		}
	}
	return items[:k]
}

// cutBreak splits s at its first <br>, <br/> or <br /> (any case).
func cutBreak(s string) (before, after string, found bool) {
	for i := 0; i+3 < len(s); i++ {
		if s[i] != '<' || s[i+1]|0x20 != 'b' || s[i+2]|0x20 != 'r' {
			continue
		}
		j := i + 3
		for j < len(s) && s[j] == ' ' {
			j++
		}
		if j < len(s) && s[j] == '/' {
			j++
		}
		if j < len(s) && s[j] == '>' {
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[j+1:]), true
		}
	}
	return s, "", false
}

func zeroed(s []int, n int) []int {
	s = s[:0]
	for range n {
		s = append(s, 0)
	}
	return s
}

func sum(s []int) int {
	n := 0
	for _, v := range s {
		n += v
	}
	return n
}

func (m *machine) border(l, mid, r string) {
	if m.color {
		m.buf = append(m.buf, sgrDim...)
	}
	m.buf = append(m.buf, l...)
	for c, w := range m.tbl.widths {
		if c > 0 {
			m.buf = append(m.buf, mid...)
		}
		for range w + 2 {
			m.buf = append(m.buf, "─"...)
		}
	}
	m.buf = append(m.buf, r...)
	if m.color {
		m.buf = append(m.buf, sgrReset...)
	}
	m.buf = append(m.buf, '\n')
}

func (m *machine) dimText(s string) {
	if !m.color {
		m.buf = append(m.buf, s...)
		return
	}
	m.buf = append(m.buf, sgrDim...)
	m.buf = append(m.buf, s...)
	m.buf = append(m.buf, sgrReset...)
}

func appendSpaces(dst []byte, n int) []byte {
	for range n {
		dst = append(dst, ' ')
	}
	return dst
}
