// Package markdown renders assistant markdown for the terminal, either as a
// whole document (Render) or as text arriving in arbitrary deltas (Stream).
//
// Rendering is line based: paragraphs are never reflowed (the terminal soft
// wraps) and inline styles do not span lines. Tables and mermaid diagrams are
// rendered once complete.
package markdown

import (
	"strings"

	"github.com/akbxr/tego/internal/mermaid"
)

// Options configures a Stream.
type Options struct {
	Width func() int // terminal columns; nil or a result <= 0 means 80
	// Height is the number of rows the stream may redraw (e.g. terminal
	// height minus a pinned footer). nil: derived from w when it is a
	// terminal *os.File, otherwise unknown.
	Height func() int
	Color  bool // ANSI styles; false gives the same layout as plain text
}

// Render renders a whole markdown document. Every output line, including the
// last, ends in "\n".
func Render(md string, width int, color bool) string {
	var s bufSink
	m := machine{out: &s, color: color, width: func() int { return width }}
	for len(md) > 0 {
		l := md
		if i := strings.IndexByte(md, '\n'); i >= 0 {
			l, md = md[:i], md[i+1:]
		} else {
			md = ""
		}
		if strings.IndexByte(l, '\r') >= 0 {
			l = strings.ReplaceAll(l, "\r", "")
		}
		m.line(l)
	}
	m.end()
	return string(s.b)
}

// sink receives the machine's output, one complete line at a time.
type sink interface {
	// emit replaces the held lines and the current line's raw echo with b.
	emit(b []byte)
	// hold keeps the current line on screen as typed; it joins the held
	// region that a later emit replaces.
	hold()
}

type bufSink struct{ b []byte }

func (s *bufSink) emit(b []byte) { s.b = append(s.b, b...) }
func (s *bufSink) hold()         {}

type heldKind uint8

const (
	heldNone    heldKind = iota
	heldCand             // a line with '|' that may be a table header
	heldTable            // header, delimiter and body rows
	heldMermaid          // ```mermaid fence and its body
)

// machine is the block-level state shared by Render and Stream.
type machine struct {
	out   sink
	color bool
	width func() int

	kind heldKind
	held []string

	fence    byte // '`' or '~' inside a fenced block, else 0
	fenceLen int
	fenceInd int
	hl       hlState

	listInd []int // indentation of the enclosing list items
	para    bool  // previous line was not blank
	indCode bool  // inside an indented code block

	buf   []byte
	items []item
	tbl   tableState
}

func (m *machine) w() int {
	if w := m.width(); w > 0 {
		return w
	}
	return 80
}

// line processes one complete line (without its newline).
func (m *machine) line(l string) {
	switch m.kind {
	case heldMermaid:
		m.held = append(m.held, l)
		if m.closes(l) {
			m.fence = 0
			m.mermaid(true)
			m.clearHeld()
			m.commit()
			return
		}
		m.out.hold()
		return
	case heldCand:
		if isDelimRow(l, countCells(m.held[0])) {
			m.kind = heldTable
			m.held = append(m.held, l)
			m.out.hold()
			return
		}
		m.paragraph(m.held[0], 0)
		m.clearHeld()
	case heldTable:
		if hasPipe(l) && strings.TrimSpace(l) != "" {
			m.held = append(m.held, l)
			m.out.hold()
			return
		}
		m.table()
		m.clearHeld()
	}
	if m.block(l) {
		if len(m.buf) > 0 {
			m.commit()
		}
		m.out.hold()
		return
	}
	m.commit()
}

// end finalizes pending output and resets the block state.
func (m *machine) end() {
	switch m.kind {
	case heldCand:
		m.paragraph(m.held[0], 0)
	case heldTable:
		m.table()
	case heldMermaid:
		m.mermaid(false)
	}
	m.clearHeld()
	if len(m.buf) > 0 {
		m.commit()
	}
	m.fence, m.hl = 0, hlState{}
	m.listInd = m.listInd[:0]
	m.para, m.indCode = false, false
}

func (m *machine) commit() {
	m.out.emit(m.buf)
	m.buf = m.buf[:0]
}

func (m *machine) clearHeld() {
	clear(m.held)
	m.held = m.held[:0]
	m.kind = heldNone
}

// block renders l into m.buf, or starts holding it and returns true.
func (m *machine) block(l string) bool {
	if m.fence != 0 {
		if m.closes(l) {
			m.fence = 0
			m.dimLine(l)
		} else {
			m.code(l)
		}
		return false
	}
	ind, p := indentOf(l)
	if p == len(l) {
		m.buf = append(m.buf, '\n')
		m.para = false
		return false
	}
	if m.indCode {
		if ind >= 4 {
			m.plain(l)
			return false
		}
		m.indCode = false
	}
	para := m.para
	m.para = true
	if ch, n, info, ok := fenceOpen(l[p:]); ok {
		m.fence, m.fenceLen, m.fenceInd = ch, n, ind
		lang := info
		if i := strings.IndexAny(lang, " \t{,"); i >= 0 {
			lang = lang[:i]
		}
		if lang == "mermaid" {
			m.kind = heldMermaid
			m.held = append(m.held, l)
			return true
		}
		m.hl = hlState{lang: lookupLang(lang)}
		m.dimLine(l)
		return false
	}
	if ind >= 4 && !para && len(m.listInd) == 0 {
		m.indCode = true
		m.plain(l)
		return false
	}
	if m.leaf(l, ind, p, false) {
		return false
	}
	if hasPipe(l) {
		m.kind = heldCand
		m.held = append(m.held, l)
		return true
	}
	if ind == 0 {
		m.listInd = m.listInd[:0]
	}
	m.paragraph(l, 0)
	return false
}

// leaf renders headings, rules, quotes and list items.
func (m *machine) leaf(l string, ind, p int, quoted bool) bool {
	s := l[p:]
	if ind < 4 {
		if lvl, text, ok := heading(s); ok {
			m.listInd = m.listInd[:0]
			base := stBold | stAccent
			if lvl == 1 {
				base |= stUnder
			}
			if quoted {
				base |= stItalic
			}
			m.items = m.items[:0]
			if lvl >= 3 {
				m.items = append(m.items, item{text: s[:min(lvl+1, len(s))]})
			}
			m.items = parseInline(m.items, text, 0, "")
			m.buf, _ = renderItems(m.buf, m.items, base, m.color)
			m.buf = append(m.buf, '\n')
			return true
		}
		if isRule(s) {
			m.listInd = m.listInd[:0]
			m.dimText(strings.Repeat("─", min(m.w(), 80)))
			m.buf = append(m.buf, '\n')
			return true
		}
		if s[0] == '>' {
			m.quote(s)
			return true
		}
	}
	mk, rest, ok := listItem(s)
	if !ok {
		return false
	}
	depth := ind / 2
	if !quoted {
		depth = m.listDepth(ind)
	}
	m.buf = append(m.buf, l[:p]...)
	box := ""
	if len(rest) >= 3 && rest[0] == '[' && rest[2] == ']' && (len(rest) == 3 || rest[3] == ' ') {
		switch rest[1] {
		case ' ':
			box = "☐"
		case 'x', 'X':
			box = "☑"
		}
		if box != "" {
			rest = strings.TrimPrefix(rest[3:], " ")
		}
	}
	if box == "" || mk[0] >= '0' && mk[0] <= '9' {
		if mk[0] < '0' || mk[0] > '9' {
			mk = [...]string{"•", "◦", "▪"}[depth%3]
		}
		m.colored(cMarker, mk)
		m.buf = append(m.buf, ' ')
	}
	if box != "" {
		c := cMarker
		if box == "☑" {
			c = cChecked
		}
		m.colored(c, box)
		m.buf = append(m.buf, ' ')
	}
	base := style(0)
	if quoted {
		base = stItalic
	}
	m.items = parseInline(m.items[:0], rest, 0, "")
	m.buf, _ = renderItems(m.buf, m.items, base, m.color)
	m.buf = append(m.buf, '\n')
	return true
}

// quote renders a blockquote line; s starts at the first '>'.
func (m *machine) quote(s string) {
	lvl := 0
	for len(s) > 0 && s[0] == '>' {
		lvl++
		s = s[1:]
		if len(s) > 0 && s[0] == ' ' {
			s = s[1:]
		}
	}
	prefix := strings.Repeat("│ ", lvl)
	if s == "" {
		prefix = prefix[:len(prefix)-1]
	}
	m.dimText(prefix)
	if s == "" {
		m.buf = append(m.buf, '\n')
		return
	}
	ind, p := indentOf(s)
	if !m.leaf(s, ind, p, true) {
		m.paragraph(s, stItalic)
	}
}

func (m *machine) listDepth(ind int) int {
	for len(m.listInd) > 0 && m.listInd[len(m.listInd)-1] > ind {
		m.listInd = m.listInd[:len(m.listInd)-1]
	}
	if len(m.listInd) == 0 || m.listInd[len(m.listInd)-1] < ind {
		m.listInd = append(m.listInd, ind)
	}
	return len(m.listInd) - 1
}

func (m *machine) paragraph(l string, base style) {
	m.items = parseInline(m.items[:0], l, 0, "")
	m.buf, _ = renderItems(m.buf, m.items, base, m.color)
	m.buf = append(m.buf, '\n')
}

func (m *machine) code(l string) {
	if m.color && m.hl.lang != nil {
		m.buf = m.hl.line(m.buf, l)
	} else {
		m.buf = append(m.buf, l...)
	}
	m.buf = append(m.buf, '\n')
}

func (m *machine) plain(l string) {
	m.buf = append(m.buf, l...)
	m.buf = append(m.buf, '\n')
}

func (m *machine) dimLine(l string) {
	m.dimText(l)
	m.buf = append(m.buf, '\n')
}

func (m *machine) colored(color, s string) {
	if m.color {
		m.buf = append(m.buf, color...)
		m.buf = append(m.buf, s...)
		m.buf = append(m.buf, sgrReset...)
		return
	}
	m.buf = append(m.buf, s...)
}

// mermaid renders the held ```mermaid block as a diagram, or as a code block
// if it is unclosed or does not render.
func (m *machine) mermaid(closed bool) {
	if closed {
		var src strings.Builder
		for _, l := range m.held[1 : len(m.held)-1] {
			_, p := indentOf(l)
			src.WriteString(l[min(p, m.fenceInd):])
			src.WriteByte('\n')
		}
		if out, err := mermaid.Render(src.String(), m.w()); err == nil && strings.TrimSpace(out) != "" {
			m.buf = append(m.buf, out...)
			if !strings.HasSuffix(out, "\n") {
				m.buf = append(m.buf, '\n')
			}
			return
		}
	}
	for i, l := range m.held {
		if i == 0 || closed && i == len(m.held)-1 {
			m.dimLine(l)
		} else {
			m.plain(l)
		}
	}
}

// closes reports whether l closes the current fence.
func (m *machine) closes(l string) bool {
	_, p := indentOf(l)
	n := runLen(l, p, m.fence)
	return n >= m.fenceLen && strings.TrimSpace(l[p+n:]) == ""
}

// indentOf returns the indentation width of l (tabs count 4) and the offset
// of its first non-blank byte.
func indentOf(l string) (ind, p int) {
	for p < len(l) {
		switch l[p] {
		case ' ':
			ind++
		case '\t':
			ind += 4 - ind%4
		default:
			return ind, p
		}
		p++
	}
	return ind, p
}

// fenceOpen parses an opening code fence; s starts at the first non-blank.
func fenceOpen(s string) (ch byte, n int, info string, ok bool) {
	if len(s) < 3 || s[0] != '`' && s[0] != '~' {
		return 0, 0, "", false
	}
	ch = s[0]
	n = runLen(s, 0, ch)
	if n < 3 {
		return 0, 0, "", false
	}
	info = strings.TrimSpace(s[n:])
	if ch == '`' && strings.IndexByte(info, '`') >= 0 {
		return 0, 0, "", false
	}
	return ch, n, info, true
}

// heading parses an ATX heading; text excludes the closing #s.
func heading(s string) (lvl int, text string, ok bool) {
	lvl = runLen(s, 0, '#')
	if lvl == 0 || lvl > 6 || lvl < len(s) && s[lvl] != ' ' && s[lvl] != '\t' {
		return 0, "", false
	}
	text = strings.TrimSpace(s[lvl:])
	if t := strings.TrimRight(text, "#"); t == "" || strings.HasSuffix(t, " ") {
		text = strings.TrimSpace(t)
	}
	return lvl, text, true
}

// isRule reports whether s is a thematic break (---, ***, ___).
func isRule(s string) bool {
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	n := 0
	for i := range len(s) {
		switch s[i] {
		case c:
			n++
		case ' ', '\t':
		default:
			return false
		}
	}
	return n >= 3
}

// listItem parses a list marker; s starts at the first non-blank.
func listItem(s string) (marker, rest string, ok bool) {
	i := 0
	switch {
	case s[0] == '-' || s[0] == '*' || s[0] == '+':
		i = 1
	case s[0] >= '0' && s[0] <= '9':
		for i < len(s) && i < 9 && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == len(s) || s[i] != '.' && s[i] != ')' {
			return "", "", false
		}
		i++
	default:
		return "", "", false
	}
	if i < len(s) && s[i] != ' ' && s[i] != '\t' {
		return "", "", false
	}
	if i < len(s) {
		return s[:i], s[i+1:], true
	}
	return s, "", true
}
