package lineedit

import (
	"slices"
	"strings"
)

// Suggestion is one row of the autocomplete popup shown under the editor.
type Suggestion struct {
	Value       string // text that replaces the whole buffer when accepted, e.g. "/model", "/skill:"
	Label       string // shown in the left column, e.g. "model", "skill:"
	Description string // shown dim in the right column
	Icon        string // optional single-cell glyph shown before the label
	Final       bool   // Enter submits immediately; false = Enter/Tab only complete
}

const (
	maxSuggestRows  = 10 // popup rows (a wrapped item takes two)
	maxSuggestLabel = 32 // label cells; the column adds 2 cells of padding
	minWrapDesc     = 24 // narrower description columns stay one truncated row
	suggestAccent   = "38;5;215"
)

// suggestState is the autocomplete popup state of the editor.
type suggestState struct {
	fn     func(string) []Suggestion
	items  []Suggestion
	text   []rune // buffer the items were computed for
	pos    int    // cursor position in text; items were computed for text[:pos]
	sel    int
	off    int  // first visible item
	hidden bool // dismissed (Esc, history recall) until the buffer changes
}

func (s *suggestState) visible() bool { return !s.hidden && len(s.items) > 0 }

// sync re-queries the suggestions when the buffer or cursor changed since
// the last query, keeping the selection on the same Value when it is still
// listed. The query is the text before the cursor, so completions work
// anywhere in the prompt (including later lines), not only at its end.
func (s *suggestState) sync(buf []rune, pos int) {
	pos = min(max(pos, 0), len(buf))
	if s.fn == nil || (slices.Equal(buf, s.text) && pos == s.pos) {
		return
	}
	s.text = append(s.text[:0], buf...)
	s.pos = pos
	s.hidden = false
	var prev string
	if s.sel < len(s.items) {
		prev = s.items[s.sel].Value
	}
	s.items, s.sel, s.off = nil, 0, 0
	s.items = s.fn(string(buf[:pos]))
	for i, it := range s.items {
		if it.Value == prev {
			s.sel = i
			break
		}
	}
}

// dismiss hides the popup for the current buffer.
func (s *suggestState) dismiss(buf []rune, pos int) {
	s.text = append(s.text[:0], buf...)
	s.pos = pos
	s.items, s.sel, s.off = nil, 0, 0
	s.hidden = true
}

// move changes the selection; appendLines scrolls it into view.
func (s *suggestState) move(d int) {
	if n := len(s.items); n > 0 {
		s.sel = (s.sel + d + n) % n
	}
}

// accepted returns the new buffer and cursor for accepting the selection
// with Tab (tab true) or Enter: the selection's Value replaces the text
// before the cursor and the text after it is kept. final reports that Enter
// should also submit, which only happens with nothing after the cursor.
func (s *suggestState) accepted(tab bool) (text string, cursor int, final bool) {
	it := s.items[s.sel]
	value, after := it.Value, string(s.text[s.pos:])
	final = it.Final
	if tab && it.Final && !strings.HasSuffix(value, ":") {
		value += " "
	}
	if after != "" {
		final = false
		switch {
		case strings.HasSuffix(value, " ") && (strings.HasPrefix(after, " ") || strings.HasPrefix(after, "\n")):
			value = strings.TrimSuffix(value, " ") // the text after the cursor brings its own space
		case !strings.HasSuffix(value, " ") && !strings.HasSuffix(value, ":") &&
			!strings.HasPrefix(after, " ") && !strings.HasPrefix(after, "\n"):
			value += " "
		}
	}
	return value + after, len([]rune(value)), final && !tab
}

// suggestLayout is the column layout of the popup for a given width.
type suggestLayout struct {
	labelW, descW int
	icons         bool
}

func (s *suggestState) layout(width int) suggestLayout {
	var l suggestLayout
	for _, it := range s.items {
		l.labelW = max(l.labelW, widthCond.StringWidth(it.Label))
		l.icons = l.icons || it.Icon != ""
	}
	l.labelW = min(l.labelW, maxSuggestLabel, max(width/3, 8))
	prefix := 2 + l.labelW + 2
	if l.icons {
		prefix += 2
	}
	l.descW = max(width-1-prefix, 0)
	return l
}

// height is how many rows an item takes: descriptions too long for one
// row wrap onto a second.
func (l suggestLayout) height(it Suggestion) int {
	if l.descW >= minWrapDesc && widthCond.StringWidth(it.Description) > l.descW {
		return 2
	}
	return 1
}

// lineCount is the number of rows all items need (capped at limit).
func (s *suggestState) lineCount(width, limit int) int {
	l := s.layout(width)
	n := 0
	for _, it := range s.items {
		if n += l.height(it); n >= limit {
			return limit
		}
	}
	return n
}

// splitDesc breaks desc at a space so the first part fits w cells; the rest
// is truncated with "…" to w cells.
func splitDesc(desc string, w int) (string, string) {
	cut, cells := 0, 0
	for i, r := range desc {
		rw := widthCond.RuneWidth(r)
		if cells+rw > w {
			break
		}
		cells += rw
		if r == ' ' {
			cut = i
		}
		if i+len(string(r)) == len(desc) {
			return desc, ""
		}
	}
	if cut == 0 { // no space to break at: hard break
		first := widthCond.Truncate(desc, w, "")
		return first, widthCond.Truncate(strings.TrimSpace(desc[len(first):]), w, "…")
	}
	return desc[:cut], widthCond.Truncate(strings.TrimSpace(desc[cut+1:]), w, "…")
}

// appendLines appends at most rows popup rows fitted to width cells,
// scrolling so the selection is visible.
func (s *suggestState) appendLines(lines []string, rows, width int, sgr func(string) string) []string {
	l := s.layout(width)
	if s.sel < s.off {
		s.off = s.sel
	}
	for s.off < s.sel {
		used := 0
		for i := s.off; i <= s.sel; i++ {
			used += l.height(s.items[i])
		}
		if used <= rows {
			break
		}
		s.off++
	}
	fit := max(width-1, 1)
	indent := strings.Repeat(" ", fit-l.descW)
	var sb strings.Builder
	used := 0
	for i := s.off; i < len(s.items) && used < rows; i++ {
		it := s.items[i]
		selected := i == s.sel
		sb.Reset()
		if selected {
			sb.WriteString(sgr(suggestAccent))
			sb.WriteString("› ")
		} else {
			sb.WriteString("  ")
		}
		if l.icons {
			if it.Icon != "" {
				sb.WriteString(it.Icon)
				sb.WriteByte(' ')
			} else {
				sb.WriteString("  ")
			}
		}
		label := widthCond.Truncate(it.Label, l.labelW, "…")
		sb.WriteString(label)
		descStyle := "2"
		if selected {
			sb.WriteString(sgr("0"))
			descStyle = suggestAccent
		}
		first, second := it.Description, ""
		if l.height(it) == 2 && rows-used >= 2 {
			first, second = splitDesc(it.Description, l.descW)
		}
		if first != "" {
			sb.WriteString(strings.Repeat(" ", l.labelW-widthCond.StringWidth(label)+2))
			sb.WriteString(sgr(descStyle))
			sb.WriteString(first)
			sb.WriteString(sgr("0"))
		}
		lines = append(lines, truncateANSI(sb.String(), fit))
		used++
		if second != "" {
			lines = append(lines, indent+sgr(descStyle)+second+sgr("0"))
			used++
		}
	}
	return lines
}

// scrollOffset returns the first visible row so cursor stays within rows
// rows starting at offset.
func scrollOffset(cursor, offset, rows int) int {
	if cursor < offset {
		return cursor
	}
	if cursor >= offset+rows {
		return cursor - rows + 1
	}
	return offset
}
