package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// Boxed tool output: edit and write results (and bash runs) are
// drawn in a frame titled with the tool and file, with numbered diff lines;
// failures get a red frame. todo and task get frames too (see panels.go).
// Other tools keep the one-line "● tool" style.

var boxTitles = map[string]string{"edit": "Edit", "write": "Write", "bash": "Bash", "todo": "Todo", "task": "Task"}

// exitCodeRe finds the "[exit code N]" note bash adds for a failing command.
var exitCodeRe = regexp.MustCompile(`\[exit code (\d+)\]`)

const (
	boxBodyMax  = 14 // body rows shown for edit/write/errors
	bashTailMax = 10 // trailing output rows shown for bash
	writePeek   = 10 // content rows previewed for write
)

func (u *termUI) boxed(name string) bool {
	return !u.quiet && u.width != nil && boxTitles[name] != ""
}

// boxCallLocked handles OnToolCall for boxed tools. Every box is drawn
// whole with the result, so a failure can colour all of its frame red.
// Meanwhile a running bash command is an animated row in the footer, where
// its box will appear (and a running task is in the Subagents panel).
func (u *termUI) boxCallLocked(name string, input json.RawMessage) {
	if name == "bash" {
		u.setRunning(u.bashLabel(input))
	}
}

// runningBox is a running bash command's box, drawn live in the footer
// where the finished box will appear: the same frame, with a spinner, a
// shimmering "running" and the time so far inside. width is the terminal's;
// the box gets the content area's width and margin so the two line up.
// Called while the TUI draws, so it must not ask the TUI anything.
func (u *termUI) runningBox(cmd string, el time.Duration, tail []string, width int) []string {
	const margin = 1 // Options.Margin
	v := &termUI{dc: u.dc, width: func() int { return max(width-2*margin, 14) }}
	frame := int(el / (100 * time.Millisecond))
	secs := fmt.Sprintf(" · %ds", int(el.Seconds()))
	var b strings.Builder
	b.WriteString(v.boxTop("Bash", cmd, "", u.dc.dim))
	for _, l := range tail {
		v.rowClip(&b, u.dc.dim+l, runewidth.StringWidth(l), u.dc.dim)
	}
	body := u.dc.cyan + bashSpinner[frame%len(bashSpinner)] + u.dc.reset + " " + shimmer(u.dc, "running", frame) + u.dc.dim + secs + u.dc.reset
	v.rowClip(&b, body, runewidth.StringWidth("⣾ running"+secs), u.dc.dim)
	b.WriteString(v.boxBottom("", u.dc.dim))
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	for i := range lines {
		lines[i] = strings.Repeat(" ", margin) + lines[i]
	}
	return lines
}

func (u *termUI) setRunning(cmd string) {
	u.runMu.Lock()
	u.running, u.runSince, u.runOut = cmd, time.Now(), u.runOut[:0]
	u.runMu.Unlock()
}

// runOutKeep bounds the output kept for the live box: plenty for its last
// lines, even with long ones.
const runOutKeep = 8 << 10

// OnToolOutput receives a running bash command's output as it arrives.
func (u *termUI) OnToolOutput(name string, chunk []byte) {
	if name != "bash" {
		return
	}
	u.runMu.Lock()
	defer u.runMu.Unlock()
	if u.running == "" {
		return
	}
	u.runOut = append(u.runOut, chunk...)
	if n := len(u.runOut) - runOutKeep; n > 0 {
		u.runOut = append(u.runOut[:0], u.runOut[n:]...)
	}
}

// runningBash is the command being run, its start ("" when none) and the
// last lines of its output so far. Safe to call from the TUI's draw goroutine.
func (u *termUI) runningBash() (string, time.Time, []string) {
	u.runMu.Lock()
	defer u.runMu.Unlock()
	return u.running, u.runSince, lastLines(u.runOut, runningTailMax)
}

// termLine is one line of command output as a terminal ends up showing it:
// escape sequences dropped, only what follows the last \r (progress bars
// redraw themselves with it), tabs as spaces and no other control bytes,
// which would break a box's frame.
func termLine(l string) string {
	l = ansiRe.ReplaceAllString(l, "")
	if i := strings.LastIndexByte(strings.TrimRight(l, "\r"), '\r'); i >= 0 {
		l = l[i+1:]
	}
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, l)
}

// runningTailMax is how many output lines the live box shows.
const runningTailMax = 5

var ansiRe = regexp.MustCompile(`\x1b(\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[@-Z\\-_])`)

// lastLines returns the last n non-empty lines of out as shown on a
// terminal: escape sequences dropped, and a line rewritten with \r (a
// progress bar) shown as it ends up.
func lastLines(out []byte, n int) []string {
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = termLine(l); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// bashLabel is a bash call's command for titles: its first line, with "…"
// when there are more.
func (u *termUI) bashLabel(input json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &in)
	cmd := strings.TrimSpace(in.Command)
	if i := strings.IndexByte(cmd, '\n'); i >= 0 {
		cmd = strings.TrimSpace(cmd[:i]) + " …"
	}
	return u.shortPaths("bash", cmd)
}

func (u *termUI) boxResultLocked(name string, input json.RawMessage, output string, isErr bool) {
	switch name {
	case "todo":
		fmt.Fprint(u.diag, u.todoBox(input, output, isErr))
		return
	case "task":
		fmt.Fprint(u.diag, u.taskBox(input, output, isErr))
		return
	}
	border := u.dc.dim
	if isErr {
		border = u.dc.red
	}
	var b strings.Builder
	if name == "bash" {
		u.setRunning("")
		mark := ""
		switch m := exitCodeRe.FindStringSubmatch(output); {
		case m != nil:
			mark, border = "✗ exit "+m[1], u.dc.red // the command ran but failed
		case isErr:
			mark = "✗ failed"
		}
		b.WriteString(u.boxTop("Bash", u.bashLabel(input), "", border))
		u.bashBody(&b, output, border)
		b.WriteString(u.boxBottom(mark, border))
		fmt.Fprint(u.diag, b.String())
		return
	}
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	_ = json.Unmarshal(input, &in)
	mark := ""
	if isErr {
		mark = "✗"
	}
	b.WriteString(u.boxTop(boxTitles[name], u.shortPath(in.Path), mark, border))
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	switch {
	case isErr:
		u.plainRows(&b, lines, u.dc.red, border, boxBodyMax)
	case name == "edit":
		u.editBody(&b, lines, border)
	default:
		u.writeBody(&b, lines, in.Content, border)
	}
	b.WriteString(u.boxBottom("", border))
	fmt.Fprint(u.diag, b.String())
}

// editBody: the replacement count, the numbered diff, then any syntax-check
// findings.
func (u *termUI) editBody(b *strings.Builder, lines []string, border string) {
	if len(lines) > 0 {
		head := lines[0]
		if i := strings.Index(head, ": "); i >= 0 && strings.HasPrefix(head, "Edited ") {
			head = head[i+2:]
		}
		u.row(b, head, u.dc.dim, border)
		lines = lines[1:]
	}
	diff, rest := splitDiff(lines)
	u.diffRows(b, diff, border)
	u.noteRows(b, rest, border)
}

// writeBody: the size line, a peek at the written content, then any
// syntax-check findings.
func (u *termUI) writeBody(b *strings.Builder, lines []string, content, border string) {
	if len(lines) > 0 {
		head := lines[0]
		if i := strings.Index(head, " ("); i >= 0 {
			if sp := strings.IndexByte(head, ' '); sp > 0 && sp < i {
				head = head[:sp] + head[i:] // "Created (25 bytes, 2 lines)"
			}
		}
		u.row(b, head, u.dc.dim, border)
		lines = lines[1:]
	}
	var diff []string
	content = strings.TrimRight(content, "\n")
	src := strings.Split(content, "\n")
	for i, l := range src {
		if i == writePeek {
			diff = append(diff, fmt.Sprintf("... %d more lines", len(src)-writePeek))
			break
		}
		diff = append(diff, fmt.Sprintf("+%d\t%s", i+1, l))
	}
	if content != "" {
		u.diffRows(b, diff, border)
	}
	u.noteRows(b, lines, border)
}

// bashBody shows the end of the output, which is where results and errors
// usually are.
func (u *termUI) bashBody(b *strings.Builder, output, border string) {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if n := len(lines) - bashTailMax; n > 0 {
		u.row(b, fmt.Sprintf("… %d lines above", n), u.dc.dim, border)
		lines = lines[n:]
	}
	for _, l := range lines {
		u.row(b, termLine(l), "", border)
	}
}

// splitDiff separates diffSnippet lines (" 12\t…", "-12\t…", "+12\t…",
// "... N more diff lines") from what follows them.
func splitDiff(lines []string) (diff, rest []string) {
	for i, l := range lines {
		if isDiffLine(l) || strings.HasPrefix(l, "... ") {
			continue
		}
		return lines[:i], lines[i:]
	}
	return lines, nil
}

func isDiffLine(l string) bool {
	if len(l) < 3 || !strings.ContainsRune(" +-", rune(l[0])) {
		return false
	}
	tab := strings.IndexByte(l, '\t')
	if tab < 2 {
		return false
	}
	for _, c := range l[1:tab] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// diffRows renders numbered diff lines with a right-aligned gutter.
func (u *termUI) diffRows(b *strings.Builder, diff []string, border string) {
	numW := 1
	for _, l := range diff {
		if isDiffLine(l) {
			numW = max(numW, strings.IndexByte(l, '\t')-1)
		}
	}
	for _, l := range diff {
		if !isDiffLine(l) {
			u.row(b, strings.Replace(l, "... ", "… ", 1), u.dc.dim, border)
			continue
		}
		tab := strings.IndexByte(l, '\t')
		sign, num, code := l[0], l[1:tab], strings.ReplaceAll(l[tab+1:], "\t", "  ")
		color := u.dc.dim
		switch sign {
		case '+':
			color = u.dc.green
		case '-':
			color = u.dc.red
		}
		gutter := fmt.Sprintf("%c%*s │ ", sign, numW, num)
		u.rowClip(b, color+gutter+code, runewidth.StringWidth(gutter+code), border)
	}
}

// noteRows renders what an edit/write reported after the diff (syntax
// check findings) in yellow.
func (u *termUI) noteRows(b *strings.Builder, lines []string, border string) {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	u.plainRows(b, lines, u.dc.yellow, border, boxBodyMax)
}

func (u *termUI) plainRows(b *strings.Builder, lines []string, color, border string, max int) {
	for i, l := range lines {
		if i == max {
			u.row(b, fmt.Sprintf("… %d more lines", len(lines)-max), u.dc.dim, border)
			return
		}
		u.row(b, l, color, border)
	}
}

func (u *termUI) inner() int { return max(u.width()-4, 10) }

// row writes text word-wrapped inside the frame.
func (u *termUI) row(b *strings.Builder, text, color, border string) {
	for _, l := range wrapCells(strings.ReplaceAll(text, "\t", "  "), u.inner()) {
		u.rowClip(b, color+l, runewidth.StringWidth(l), border)
	}
}

// rowClip writes one row whose visible text is styled (width cells),
// truncating it to the frame.
func (u *termUI) rowClip(b *strings.Builder, styled string, width int, border string) {
	iw := u.inner()
	if width > iw {
		styled = truncateStyled(styled, iw-1) + "…"
		width = iw
	}
	fmt.Fprintf(b, "%s│%s %s%s%s %s│%s\n", border, u.dc.reset, styled, u.dc.reset, strings.Repeat(" ", iw-width), border, u.dc.reset)
}

func (u *termUI) boxTop(tool, subject, mark, border string) string {
	w := max(u.width(), 14)
	title := " " + tool + ": " + subject + " "
	if subject == "" {
		title = " " + tool + " "
	}
	if mark != "" {
		title = " " + mark + title
	}
	if tw := runewidth.StringWidth(title); tw > w-4 {
		title = runewidth.Truncate(title, w-5, "…") + " "
	}
	fill := max(w-3-runewidth.StringWidth(title), 0)
	return fmt.Sprintf("%s╭─%s%s%s╮%s\n", border, u.dc.bold+title+u.dc.reset, border, strings.Repeat("─", fill), u.dc.reset)
}

func (u *termUI) boxBottom(mark, border string) string {
	w := max(u.width(), 14)
	if mark == "" {
		return fmt.Sprintf("%s╰%s╯%s\n", border, strings.Repeat("─", w-2), u.dc.reset)
	}
	label := " " + mark + " "
	return fmt.Sprintf("%s╰─%s%s╯%s\n", border, u.dc.bold+label+u.dc.reset+border, strings.Repeat("─", max(w-3-runewidth.StringWidth(label), 0)), u.dc.reset)
}

// truncateStyled cuts s (which may contain SGR escapes) to n visible cells.
func truncateStyled(s string, n int) string {
	var b strings.Builder
	cells := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := strings.IndexByte(s[i:], 'm')
			if j < 0 {
				break
			}
			b.WriteString(s[i : i+j+1])
			i += j + 1
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := runewidth.RuneWidth(r)
		if cells+rw > n {
			break
		}
		b.WriteString(s[i : i+size])
		cells += rw
		i += size
	}
	return b.String()
}
