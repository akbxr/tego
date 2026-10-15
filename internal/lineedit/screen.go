package lineedit

import (
	"time"
	"unicode/utf8"
)

// Rendering model. Content is written in normal flow; the footer is drawn
// starting on the row below the content cursor row. col tracks the content
// cursor column; col == width means the row is exactly full and the
// terminal's pending-wrap state applies: the next printable rune lands at
// column 0 of the following row. In that state the footer starts on that
// following row (footerStart = 0) instead of one row below (footerStart = 1),
// so the "content position" used to come back is (next row, column 0).
//
// cursorDown is the number of rows from the content position down to where
// the real cursor was left (the editor cursor row, or the last footer row
// while a picker is open).

const (
	syncBegin  = "\x1b[?2026h"
	syncEnd    = "\x1b[?2026l"
	hideCursor = "\x1b[?25l"
	showCursor = "\x1b[?25h"

	// maxCarry bounds how many bytes of an incomplete escape sequence are held
	// back between writes before they are passed through as-is.
	maxCarry = 4096
	// frameInterval is the minimum spacing of coalesced Redraw calls.
	frameInterval = 16 * time.Millisecond
	// tickInterval animates the activity line while it is non-empty.
	tickInterval = 100 * time.Millisecond
)

func (t *TUI) live() bool { return t.started && !t.suspended && !t.closed }

func (t *TUI) sgr(code string) string {
	if !t.color {
		return ""
	}
	return "\x1b[" + code + "m"
}

// appendToFooterTop moves the real cursor from where the last draw left it
// to column 0 of footer row 0 and erases from there down.
func (t *TUI) appendToFooterTop(b []byte) []byte {
	b = appendCSI(b, t.cursorDown-t.footerStart, 'A')
	return append(b, "\r\x1b[J"...)
}

// write emits content p above the footer in one synchronized update.
func (t *TUI) write(p []byte) {
	b := t.buf[:0]
	live := t.live()
	virt := false
	if live {
		b = append(b, syncBegin...)
		if t.drawn {
			b = t.appendToFooterTop(b)
			if t.footerStart == 1 {
				b = append(b, "\x1b[A"...)
				b = appendCSI(b, t.col, 'C')
			} else {
				virt = true // at column 0 of the row after the full row
			}
			t.drawn = false
		}
	}
	b, virt = t.appendContent(b, p, virt)
	if live {
		if t.col == t.width {
			if !virt {
				b = append(b, '\r', '\n')
			}
			t.footerStart = 0
		} else {
			b = append(b, '\r', '\n')
			t.footerStart = 1
		}
		b = t.appendFooter(b)
		b = append(b, syncEnd...)
	}
	t.flush(b)
}

func (t *TUI) flush(b []byte) {
	if len(b) > 0 {
		_, _ = t.out.Write(b)
	}
	t.buf = b[:0]
}

// drawLocked redraws the footer now.
func (t *TUI) drawLocked() {
	if !t.live() {
		return
	}
	b := append(t.buf[:0], syncBegin...)
	if t.drawn {
		b = appendCSI(b, t.cursorDown-t.footerStart, 'A')
		b = append(b, '\r')
	} else {
		b = append(b, '\r', '\n')
		if t.col == t.width {
			t.footerStart = 0
		} else {
			t.footerStart = 1
		}
	}
	b = t.appendFooter(b)
	b = append(b, syncEnd...)
	t.flush(b)
}

// requestDrawLocked coalesces redraws to at most one per frameInterval.
func (t *TUI) requestDrawLocked() {
	if t.drawPending || !t.live() {
		return
	}
	since := time.Since(t.lastDraw)
	if since >= frameInterval {
		t.drawLocked()
		return
	}
	t.drawPending = true
	if t.drawTimer == nil {
		t.drawTimer = time.AfterFunc(frameInterval-since, t.onDrawTimer)
	} else {
		t.drawTimer.Reset(frameInterval - since)
	}
}

func (t *TUI) onDrawTimer() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.drawPending = false
	t.drawLocked()
}

func (t *TUI) onTick() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tickArmed = false
	t.drawLocked()
}

// eraseLocked removes the footer and leaves the cursor at column 0 of a
// fresh line after the content (col = 0).
func (t *TUI) eraseLocked(b []byte) []byte {
	if t.drawn {
		b = t.appendToFooterTop(b)
		if t.footerStart == 1 && t.col == 0 {
			b = append(b, "\x1b[A"...)
		}
	} else if t.col > 0 {
		b = append(b, '\r', '\n')
	}
	t.col = 0
	t.drawn = false
	t.footerRows = 0
	return b
}

// appendFooter draws the footer lines starting at column 0 of footer row 0
// and leaves the real cursor at the editor cursor. t.footerStart must be set.
func (t *TUI) appendFooter(b []byte) []byte {
	lines, curLine, curCol, active := t.footerLines()
	b = append(b, "\x1b[J"...)
	for i, l := range lines {
		if i > 0 {
			b = append(b, '\r', '\n')
		}
		b = append(b, l...)
	}
	last := len(lines) - 1
	if curLine < 0 {
		if !t.cursorHidden {
			b = append(b, hideCursor...)
			t.cursorHidden = true
		}
		t.cursorDown = t.footerStart + last
		t.curCol = min(visibleWidth(lines[last]), t.width-1)
	} else {
		b = appendCSI(b, last-curLine, 'A')
		b = append(b, '\r')
		b = appendCSI(b, curCol, 'C')
		if t.cursorHidden {
			b = append(b, showCursor...)
			t.cursorHidden = false
		}
		t.cursorDown = t.footerStart + curLine
		t.curCol = curCol
	}
	t.footerRows = len(lines)
	t.drawn = true
	t.lastDraw = time.Now()
	if active && !t.tickArmed {
		t.tickArmed = true
		if t.tickTimer == nil {
			t.tickTimer = time.AfterFunc(tickInterval, t.onTick)
		} else {
			t.tickTimer.Reset(tickInterval)
		}
	}
	return b
}

// footerLines builds the footer: activity, header, editor rows (or picker),
// suggestion popup, status. curLine is the editor cursor line (-1 while a
// picker is open). active reports a non-empty activity line.
func (t *TUI) footerLines() (lines []string, curLine, curCol int, active bool) {
	w, h := t.width, t.height
	fit := max(w-1, 1)
	lines = t.lines[:0]
	if t.opts.Panel != nil {
		// Panels may take up to half the screen; the rest stays for output.
		panel := t.opts.Panel(w)
		if limit := h / 2; len(panel) > limit {
			panel = panel[:limit]
		}
		for _, s := range panel {
			lines = append(lines, truncateANSI(s, fit))
		}
	}
	if t.opts.Activity != nil {
		if s := t.opts.Activity(w); s != "" {
			lines = append(lines, truncateANSI(s, fit))
			active = true
		}
	}
	if t.opts.Header != nil {
		if s := t.opts.Header(w); s != "" {
			lines = append(lines, truncateANSI(s, fit))
		}
	}
	var status []string
	if t.opts.Status != nil {
		status = t.opts.Status(w)
	}
	fixed := len(lines) + len(status)
	if ps := t.pick; ps != nil {
		rows := min(maxPickRows, (h-fixed-3)/2, len(ps.filtered))
		lines = ps.appendLines(lines, max(rows, 1), w, t.sgr)
		curLine = -1
	} else {
		sug := &t.sess.sug
		popRows := 0
		if sug.visible() {
			// Leave room for one editor row and one content row.
			popRows = max(min(sug.lineCount(w, maxSuggestRows), h-fixed-2), 0)
		}
		l := computeLayout(t.opts.Prompt, t.contPrefix, t.promptW, t.sess.buf.text, t.sess.buf.pos, w)
		rows := l.rows
		// Keep the footer within the screen: show a window around the cursor.
		if maxRows := max(h-1-fixed-popRows, 1); len(rows) > maxRows {
			start := max(l.curRow-maxRows+1, 0)
			rows = rows[start : start+maxRows]
			l.curRow -= start
		}
		curLine = len(lines) + l.curRow
		curCol = l.curCol
		lines = append(lines, rows...)
		if popRows > 0 {
			lines = sug.appendLines(lines, popRows, w, t.sgr)
		}
	}
	for _, s := range status {
		lines = append(lines, truncateANSI(s, fit))
	}
	t.lines = lines
	return lines, curLine, curCol, active
}

// leavePending normalizes the pending-wrap state before a cursor motion: the
// real cursor moves to the last cell of the full row (from the virtual
// position, column 0 of the next row, when virt) and col becomes width-1.
func (t *TUI) leavePending(b []byte, virt bool) ([]byte, bool) {
	w := max(t.width, 1)
	if t.col < w {
		return b, virt
	}
	if virt {
		b = append(b, "\x1b[A"...)
		b = appendCSI(b, w, 'G')
	}
	t.col = w - 1
	return b, false
}

// appendContent appends content bytes converted for raw mode (lone LF
// becomes CRLF) while tracking the cursor column. virt reports whether the
// real cursor sits on the virtual pending-wrap position. An incomplete
// trailing escape or UTF-8 sequence is held back for the next write.
func (t *TUI) appendContent(b, p []byte, virt bool) ([]byte, bool) {
	data := p
	if len(t.carry) > 0 {
		t.carry = append(t.carry, p...)
		data = t.carry
	}
	w := max(t.width, 1)
	i := 0
	for i < len(data) {
		c := data[i]
		if t.col == 0 && c != '\r' && c != '\n' {
			b = t.indent(b) // margin before anything a row shows, styles included
		}
		switch {
		case c == 0x1b:
			end, ok := escEnd(data, i)
			if !ok && len(data)-i < maxCarry {
				t.keepCarry(data[i:])
				return b, virt
			}
			b, virt = t.contentEscape(b, data[i:end], virt)
			i = end
			continue
		case c == '\r':
			if virt {
				b = append(b, "\x1b[A"...)
				virt = false
			}
			b = append(b, '\r')
			t.col = 0
		case c == '\n':
			switch {
			case virt:
				virt = false // already at column 0 of the next row
			case i > 0 && data[i-1] == '\r':
				b = append(b, '\n')
			default:
				b = append(b, '\r', '\n')
			}
			t.col = 0
		case c == '\t':
			b, virt = t.leavePending(b, virt)
			b = t.indent(b)
			t.col = min((t.col/8+1)*8, w-1)
			b = append(b, c)
		case c == 0x08:
			b, virt = t.leavePending(b, virt)
			t.col = max(t.col-1, 0)
			b = append(b, c)
		case c < 0x20 || c == 0x7f:
			b = append(b, c)
		default:
			size, rw := 1, 1
			if c >= utf8.RuneSelf {
				if !utf8.FullRune(data[i:]) {
					t.keepCarry(data[i:])
					return b, virt
				}
				var r rune
				r, size = utf8.DecodeRune(data[i:])
				if r != utf8.RuneError || size > 1 {
					rw = widthCond.RuneWidth(r)
				}
			}
			if rw > 0 {
				if m := t.opts.Margin; m > 0 && t.col+rw > w-m && t.col > m {
					b = append(b, '\r', '\n') // wrap early: keep the right margin
					t.col = 0
					b = t.indent(b)
				}
				if t.col >= w || t.col+rw > w {
					t.col = 0 // the terminal wraps before this rune
				}
				virt = false
				t.col += rw
			}
			b = append(b, data[i:i+size]...)
			i += size
			continue
		}
		i++
	}
	t.carry = t.carry[:0]
	return b, virt
}

// indent writes the left margin at the start of a content row. It keeps
// the current style so text wrapped onto the next row stays styled.
func (t *TUI) indent(b []byte) []byte {
	m := t.opts.Margin
	if m <= 0 || t.col != 0 {
		return b
	}
	for range m {
		b = append(b, ' ')
	}
	t.col = m
	return b
}

func (t *TUI) keepCarry(rest []byte) {
	n := copy(t.carry[:cap(t.carry)], rest)
	if n < len(rest) {
		t.carry = append([]byte(nil), rest...)
		return
	}
	t.carry = t.carry[:n]
}

// contentEscape passes one escape sequence through, updating col for cursor
// motions (CUU/CUD rows only, CUF, CUB, CHA, CUP) and erases.
func (t *TUI) contentEscape(b, seq []byte, virt bool) ([]byte, bool) {
	if len(seq) < 3 || seq[1] != '[' {
		return append(b, seq...), virt
	}
	params := seq[2 : len(seq)-1]
	if len(params) > 0 && params[0] >= '<' && params[0] <= '?' {
		return append(b, seq...), virt // private modes
	}
	w := max(t.width, 1)
	n1, n2 := csiArgs(params)
	switch seq[len(seq)-1] {
	case 'A', 'B':
		b, virt = t.leavePending(b, virt)
	case 'C':
		b, virt = t.leavePending(b, virt)
		t.col = min(t.col+max(n1, 1), w-1)
	case 'D':
		b, virt = t.leavePending(b, virt)
		t.col = max(t.col-max(n1, 1), 0)
	case 'G':
		b, virt = t.leavePending(b, virt)
		t.col = min(max(n1, 1), w) - 1
	case 'H', 'f':
		virt = false
		t.col = min(max(n2, 1), w) - 1
	case 'J', 'K':
		// From the virtual position the erase starts on the next row, which
		// keeps the full row intact; otherwise the terminal leaves pending wrap.
		if t.col >= w && !virt {
			t.col = w - 1
		}
	}
	return append(b, seq...), virt
}

// csiArgs parses the first two numeric CSI parameters (0 when absent).
func csiArgs(params []byte) (int, int) {
	var n [2]int
	k := 0
	for _, c := range params {
		switch {
		case c >= '0' && c <= '9':
			if n[k] < 1<<20 {
				n[k] = n[k]*10 + int(c-'0')
			}
		case c == ';':
			if k == 1 {
				return n[0], n[1]
			}
			k++
		}
	}
	return n[0], n[1]
}
