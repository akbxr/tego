package lineedit

import (
	"errors"
	"sort"
	"strconv"
	"strings"
)

// ErrCanceled is returned by Pick when the user dismisses the list.
var ErrCanceled = errors.New("canceled")

// ErrBusy is returned by Pick and Ask while another picker or prompt is
// open: try again once it closes.
var ErrBusy = errors.New("another picker is open")

// PickItem is one selectable row: Label is matched and shown, Detail is shown
// dimmed after it and also matched.
type PickItem struct {
	Label  string
	Detail string
}

const maxPickRows = 12

// Filter returns indices of items matching every whitespace-separated
// term of query (case-insensitive), best matches first. A term matches as a
// substring (ranked by position) or, weaker, as a subsequence.
func Filter(items []PickItem, query string) []int {
	terms := strings.Fields(strings.ToLower(query))
	type hit struct{ idx, score int }
	var hits []hit
	for i, it := range items {
		hay := strings.ToLower(it.Label + " " + it.Detail)
		score, ok := 0, true
		for _, t := range terms {
			if p := strings.Index(hay, t); p >= 0 {
				score += p
			} else if span := subsequenceSpan(hay, t); span >= 0 {
				score += 1000 + span
			} else {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, hit{i, score})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score < hits[b].score })
	out := make([]int, len(hits))
	for i, h := range hits {
		out[i] = h.idx
	}
	return out
}

// subsequenceSpan is the length of the shortest prefix-greedy window of hay
// containing needle's runes in order, or -1.
func subsequenceSpan(hay, needle string) int {
	nr := []rune(needle)
	start, j := -1, 0
	for i, r := range []rune(hay) {
		if j < len(nr) && r == nr[j] {
			if j == 0 {
				start = i
			}
			j++
			if j == len(nr) {
				return i - start
			}
		}
	}
	return -1
}

type picker struct {
	items    []PickItem
	query    []rune
	filtered []int
	cursor   int // index into filtered
	offset   int // first visible row
}

func (p *picker) refilter(keepItem int) {
	p.filtered = Filter(p.items, string(p.query))
	p.cursor, p.offset = 0, 0
	for i, idx := range p.filtered {
		if idx == keepItem {
			p.cursor = i
		}
	}
}

func (p *picker) move(d, rows int) {
	n := len(p.filtered)
	if n == 0 {
		return
	}
	p.cursor = (p.cursor + d + n) % n
	p.offset = scrollOffset(p.cursor, p.offset, rows)
}

// pickResult is what a closed picker reports to the blocked Pick caller.
type pickResult struct {
	idx    int
	action PickAction
	err    error
}

// PickAction is how a PickWith picker closed.
type PickAction int

const (
	PickSelect PickAction = iota // Enter on Index
	PickDelete                   // Delete or Ctrl+D on Index (PickOptions.Delete)
	PickTab                      // Tab (PickOptions.Tab)
	PickRename                   // Ctrl+R on Index (PickOptions.Rename)
	PickSort                     // Ctrl+S (PickOptions.Sort)
)

// PickOptions configure PickWith. Without Delete and Tab it behaves like Pick.
type PickOptions struct {
	Title    string
	Query    string // prefilled filter
	Selected int    // item the cursor starts on
	Delete   bool   // Ctrl+D, Delete, or (Ctrl/Alt/Cmd+)Backspace on an empty filter close it with PickDelete
	Tab      string // non-empty: Tab closes it with PickTab; the footer shows "tab <Tab>"
	Rename   bool   // Ctrl+R closes it with PickRename
	Sort     string // non-empty: Ctrl+S closes it with PickSort; the footer shows "ctrl+s sort: <Sort>"
}

// PickResult is the closed picker: the action, the item it applies to (-1
// when none) and the filter typed so far, to reopen the picker as it was.
type PickResult struct {
	Index  int
	Action PickAction
	Query  string
}

// pickState is an open modal picker drawn in the footer.
type pickState struct {
	picker
	title    string
	selected int
	rows     int // item rows shown by the last draw
	res      chan pickResult
	del      bool
	tab      string
	rename   bool
	sort     string
	input    bool // Ask: a text field, no list
}

// handle applies k. It reports done with the result when the picker closes.
func (ps *pickState) handle(k key) (pickResult, bool) {
	p := &ps.picker
	rows := max(ps.rows, 1)
	cur := -1
	if len(p.filtered) > 0 {
		cur = p.filtered[p.cursor]
	}
	switch k.kind {
	case keyEnter, keyNewline:
		if ps.input {
			return pickResult{idx: -1}, true
		}
		if len(p.filtered) > 0 {
			return pickResult{idx: p.filtered[p.cursor]}, true
		}
	case keyCtrlR:
		if ps.rename && cur >= 0 {
			return pickResult{idx: cur, action: PickRename}, true
		}
	case keyCtrlS:
		if ps.sort != "" {
			return pickResult{idx: cur, action: PickSort}, true
		}
	case keyDelete, keyEOF:
		if ps.del {
			if len(p.filtered) > 0 {
				return pickResult{idx: p.filtered[p.cursor], action: PickDelete}, true
			}
			break
		}
		if k.kind == keyEOF {
			return pickResult{idx: -1, err: ErrCanceled}, true
		}
	case keyInterrupt, keyEscape:
		return pickResult{idx: -1, err: ErrCanceled}, true
	case keyTab:
		if ps.tab != "" {
			idx := -1
			if len(p.filtered) > 0 {
				idx = p.filtered[p.cursor]
			}
			return pickResult{idx: idx, action: PickTab}, true
		}
		p.move(1, rows)
	case keyUp:
		p.move(-1, rows)
	case keyDown:
		p.move(1, rows)
	case keyBackspace:
		if len(p.query) > 0 {
			p.query = p.query[:len(p.query)-1]
			p.refilter(-1)
		} else if ps.del && len(p.filtered) > 0 {
			// On a Mac the "delete" key is Backspace: with nothing left to
			// erase in the filter it deletes the item (the caller confirms).
			return pickResult{idx: p.filtered[p.cursor], action: PickDelete}, true
		}
	case keyKillLineStart, keyKillWordBack:
		// Like pi and omp: Ctrl/Alt/Cmd+Backspace deletes the item once the
		// filter is empty (on a Mac the "delete" key is Backspace).
		if ps.del && len(p.query) == 0 {
			if len(p.filtered) > 0 {
				return pickResult{idx: p.filtered[p.cursor], action: PickDelete}, true
			}
			break
		}
		p.query = p.query[:0]
		p.refilter(ps.selected)
	case keyRune:
		p.query = append(p.query, k.r)
		p.refilter(-1)
	case keyPaste:
		p.query = append(p.query, []rune(strings.ReplaceAll(k.text, "\n", " "))...)
		p.refilter(-1)
	}
	return pickResult{}, false
}

// appendLines appends the picker rows (title, query, up to rows items, count)
// fitted to width cells.
func (ps *pickState) appendLines(lines []string, rows, width int, sgr func(string) string) []string {
	p := &ps.picker
	ps.rows = rows
	p.move(0, rows)
	fit := func(s string, w int) string { return widthCond.Truncate(s, max(w, 1), "…") }
	lines = append(lines,
		sgr("1")+fit(ps.title, width-1)+sgr("0"),
		sgr("35;1")+"› "+sgr("0")+fit(string(p.query), width-4)+sgr("7")+" "+sgr("0"))
	if ps.input {
		return append(lines, sgr("2")+fit("  enter save · esc cancel", width-1)+sgr("0"))
	}
	if len(p.filtered) == 0 {
		lines = append(lines, sgr("2")+"  no matches"+sgr("0"))
	}
	end := min(p.offset+rows, len(p.filtered))
	for i := p.offset; i < end; i++ {
		it := p.items[p.filtered[i]]
		label := fit(it.Label, width-4)
		detail := ""
		if rest := width - 5 - widthCond.StringWidth(label); it.Detail != "" && rest > 3 {
			detail = "  " + sgr("2") + fit(it.Detail, rest) + sgr("0")
		}
		if i == p.cursor {
			lines = append(lines, sgr("36;1")+"❯ "+label+sgr("0")+detail)
		} else {
			lines = append(lines, "  "+label+detail)
		}
	}
	hint := " · ↑↓ move · enter select"
	if ps.del {
		hint += " · ⌫/ctrl+d delete"
	}
	if ps.rename {
		hint += " · ctrl+r rename"
	}
	if ps.sort != "" {
		hint += " · ctrl+s sort: " + ps.sort
	}
	if ps.tab != "" {
		hint += " · tab " + ps.tab
	}
	return append(lines, sgr("2")+fit("  "+strconv.Itoa(len(p.filtered))+"/"+strconv.Itoa(len(p.items))+
		hint+" · esc cancel", width-1)+sgr("0"))
}
