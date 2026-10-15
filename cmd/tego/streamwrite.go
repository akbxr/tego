package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
)

// A long edit or write takes the model seconds to write (13% of them over
// 10 s in real sessions), and until now nothing showed what it was writing.
// While it streams, the footer shows a live box like the one of a running
// bash command: the file, the last lines being written, and how far it is.
// The finished call's box (with the real diff) replaces it.

// liveWrite is an edit or write call being streamed.
type liveWrite struct {
	id, tool string
	since    time.Time
	args     *partialArgs
	rows     int // content rows shown, fixed when the call starts
}

const (
	liveWriteDelay = time.Second // short calls finish before a box would help
	// Content rows: two fifths of the screen, within these bounds.
	liveWriteMinRows = 8
	liveWriteMaxRows = 20
)

// liveWriteRows sizes the live box for a screen of height rows (0 when
// unknown). Taken once per call: the box itself shrinks the content area,
// and a size that followed it would make the box jump.
func liveWriteRows(height int) int {
	if height <= 0 {
		return liveWriteMinRows
	}
	return min(max(height*2/5, liveWriteMinRows), liveWriteMaxRows)
}

// OnToolDelta receives the next piece of a tool call's arguments.
func (u *termUI) OnToolDelta(id, name, chunk string) {
	if name != "edit" && name != "write" || !u.boxed(name) {
		return
	}
	// Before runMu: the TUI holds its own lock while it draws and then takes
	// runMu, so asking it for the height under runMu could deadlock.
	height := 0
	if u.height != nil {
		height = u.height()
	}
	u.runMu.Lock()
	defer u.runMu.Unlock()
	if u.writing == nil || u.writing.id != id {
		u.writing = &liveWrite{id: id, tool: name, since: time.Now(), args: newPartialArgs(), rows: liveWriteRows(height)}
	}
	u.writing.args.add(chunk)
}

// endWrite drops the live box: the call is complete (its own box follows)
// or the turn ended.
func (u *termUI) endWrite() {
	u.runMu.Lock()
	u.writing = nil
	u.runMu.Unlock()
}

// writingBox is the live box of the call being written, or nil (none, or
// not yet worth one). Called while the TUI draws.
func (u *termUI) writingBox(width int) []string {
	u.runMu.Lock()
	defer u.runMu.Unlock()
	w := u.writing
	if w == nil || time.Since(w.since) < liveWriteDelay {
		return nil
	}
	el := time.Since(w.since)
	p := w.args
	var rows []string
	var color, what string
	switch w.tool {
	case "write":
		lines, total := p.tail("content", false, w.rows)
		rows, color = lines, u.dc.green
		what = fmt.Sprintf("%s", plural(total, "line"))
	default: // edit: the replacement text, the part the model invents
		item := len(p.items) > 0
		lines, total := p.tail("new_string", item, w.rows)
		rows, color = lines, u.dc.green
		what = plural(total, "line")
		if n := len(p.items); n > 1 {
			what = fmt.Sprintf("change %d · %s", n, what)
		}
	}
	return u.liveBox(boxTitles[w.tool], u.shortPath(p.value("path")), rows, color, "writing", what, el, width)
}

// liveBox draws a live box in the footer: the frame of the finished box,
// rows of content, and a spinner line. Shared by running bash commands and
// calls being written. width is the terminal's; the box gets the content
// area's width and margin so it lines up with the boxes in the scrollback.
func (u *termUI) liveBox(title, subject string, rows []string, rowColor, verb, what string, el time.Duration, width int) []string {
	const margin = 1 // Options.Margin
	v := &termUI{dc: u.dc, width: func() int { return max(width-2*margin, 14) }}
	frame := int(el / (100 * time.Millisecond))
	info := fmt.Sprintf(" · %ds", int(el.Seconds()))
	if what != "" {
		info = " · " + what + info
	}
	var b strings.Builder
	b.WriteString(v.boxTop(title, subject, "", u.dc.dim))
	for _, l := range rows {
		l = termLine(l)
		v.rowClip(&b, rowColor+l, runewidth.StringWidth(l), u.dc.dim)
	}
	body := u.dc.cyan + bashSpinner[frame%len(bashSpinner)] + u.dc.reset + " " + shimmer(u.dc, verb, frame) + u.dc.dim + info + u.dc.reset
	v.rowClip(&b, body, runewidth.StringWidth("⣾ "+verb+info), u.dc.dim)
	b.WriteString(v.boxBottom("", u.dc.dim))
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	for i := range lines {
		lines[i] = strings.Repeat(" ", margin) + lines[i]
	}
	return lines
}
