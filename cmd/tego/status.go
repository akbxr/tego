package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/akbxr/tego/internal/llm"
)

// statusInfo is the snapshot the footer renders. The TUI redraws from its
// own goroutine, so it never touches agent state directly.
type statusInfo struct {
	model    string // display name
	spec     string // provider/model-id
	thinking string
	// fast mode is on for this model; fastResting: the last response still
	// ran at standard speed (rate limited, or not available to the account)
	fast, fastResting bool
	ctxTokens         int
	window            int
	usage             llm.Usage
	title             string
	git               string // styled branch + counts, "" outside a repo
	busy              bool
	busySince         time.Time
	todoDone          int
	todoTotal         int
}

type statusBar struct {
	mu    sync.Mutex
	s     statusInfo
	c     palette
	icons iconSet
	cwd   string         // ~-abbreviated
	rate  func() float64 // live tok/s; set once before the first turn
	// agents counts running subagents and shells running background
	// commands; both are set before the TUI starts drawing
	agents func() int
	shells func() int
	plan   func() bool // plan mode is on
}

func newStatusBar(c palette, cwd string, ic iconSet) *statusBar {
	if home, err := os.UserHomeDir(); err == nil && (cwd == home || strings.HasPrefix(cwd, home+"/")) {
		cwd = "~" + cwd[len(home):]
	}
	return &statusBar{c: c, cwd: cwd, icons: ic}
}

func (b *statusBar) update(fn func(s *statusInfo)) {
	b.mu.Lock()
	fn(&b.s)
	b.mu.Unlock()
}

func (b *statusBar) snapshot() statusInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.s
}

// header is the rule above the input, with the number of running
// subagents and the session title at the right.
func (b *statusBar) header(width int) string {
	s := b.snapshot()
	c := b.c
	title := s.title
	if r := []rune(title); len(r) > 48 {
		title = string(r[:47]) + "…"
	}
	label, plain := "", ""
	add := func(text, color string) {
		if plain != "" {
			plain += " · "
			label += c.dim + " · " + c.reset
		}
		plain += text
		label += color + text + c.reset
	}
	if b.agents != nil {
		if n := b.agents(); n > 0 {
			add(fmt.Sprintf("⧉ %d", n), c.magenta)
		}
	}
	if b.shells != nil {
		if n := b.shells(); n > 0 {
			add(fmt.Sprintf("▶ %d bg", n), c.cyan) // background shells keep running between turns
		}
	}
	if label != "" && title != "" {
		plain += " · "
		label += c.dim + " · " + c.reset
	}
	if title == "" && label == "" {
		return c.dim + strings.Repeat("─", max(width-1, 1)) + c.reset
	}
	fill := max(width-1-runewidth.StringWidth(plain+title)-4, 1)
	return c.dim + strings.Repeat("─", fill) + " " + c.reset + label + c.blue + title + c.reset + c.dim + " ─" + c.reset
}

// status is the rule below the input plus the segment line.
func (b *statusBar) status(width int) []string {
	s := b.snapshot()
	c := b.c
	sep := c.dim + " · " + c.reset
	var parts []string
	if b.plan != nil && b.plan() {
		parts = append(parts, c.yellow+c.bold+"⏸ plan mode"+c.reset)
	}
	parts = append(parts, c.cyan+"◆ "+s.model+c.reset)
	parts = append(parts, c.blue+b.icons.folder+" "+b.cwd+c.reset)
	if s.git != "" {
		parts = append(parts, s.git)
	}
	if s.window > 0 {
		pct := float64(s.ctxTokens) * 100 / float64(s.window)
		color := c.magenta
		switch {
		case pct >= 80:
			color = c.red
		case pct >= 60:
			color = c.yellow
		}
		parts = append(parts, fmt.Sprintf("%s◔ %.1f%%/%s%s", color, pct, fmtTokens(s.window), c.reset))
	}
	if s.thinking != "" && s.thinking != llm.ThinkingOff {
		parts = append(parts, c.dim+"✦ "+s.thinking+c.reset)
	}
	switch {
	case s.fastResting:
		parts = append(parts, c.dim+"↯ fast (standard now)"+c.reset)
	case s.fast:
		parts = append(parts, c.yellow+"↯ fast"+c.reset)
	}
	if s.todoTotal > 0 {
		parts = append(parts, fmt.Sprintf("%s☑ %d/%d%s", c.green, s.todoDone, s.todoTotal, c.reset))
	}
	if u := s.usage; u.Input+u.Output+u.CacheRead > 0 {
		parts = append(parts, fmt.Sprintf("%s↑%s ↓%s%s", c.dim, fmtTokens(u.Input+u.CacheRead+u.CacheWrite), fmtTokens(u.Output), c.reset))
	}
	return []string{
		c.dim + strings.Repeat("─", max(width-1, 1)) + c.reset,
		" " + strings.Join(parts, sep),
	}
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// activity is shown above the header while a turn runs.
func (b *statusBar) activity(int) string {
	s := b.snapshot()
	c := b.c
	if !s.busy {
		return ""
	}
	el := time.Since(s.busySince)
	// One cell in from the edge, like the status line below it.
	frame := int(el / (100 * time.Millisecond))
	line := fmt.Sprintf(" %s%s%s %s %s%.1fs", c.magenta, spinner[frame%len(spinner)], c.reset, glow(c, "Working…", frame), c.dim, el.Seconds())
	if b.rate != nil {
		if r := b.rate(); r > 0 {
			line += fmt.Sprintf(" · %.0f tok/s", r)
		}
	}
	line += " · alt+enter after turn"
	if b.agents != nil && b.agents() > 0 {
		line += " · ←← agents"
	}
	return line + " · esc to interrupt" + c.reset
}

var claudeNameRe = regexp.MustCompile(`^claude-(opus|sonnet|haiku|fable)-(\d+)(?:-(\d{1,2}))?(?:-\d{8})?$`)

// modelLabel turns ids like claude-sonnet-5-5 into "Sonnet 5.5".
func modelLabel(id string) string {
	m := claudeNameRe.FindStringSubmatch(id)
	if m == nil {
		return id
	}
	v := m[2]
	if m[3] != "" {
		v += "." + m[3]
	}
	return strings.ToUpper(m[1][:1]) + m[1][1:] + " " + v
}

// gitStatus renders "<branch icon> branch ↑1 +2 ~3 ?4" or "" outside a repository.
func gitStatus(cwd string, c palette, ic iconSet) string {
	cmd := exec.Command("git", "status", "--porcelain=v1", "--branch")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	var branch, track string
	staged, modified, untracked := 0, 0, 0
	for i, line := range strings.Split(string(out), "\n") {
		if i == 0 && strings.HasPrefix(line, "## ") {
			head := strings.TrimPrefix(line, "## ")
			if b, rest, ok := strings.Cut(head, "..."); ok {
				head = b
				if j := strings.Index(rest, "["); j >= 0 {
					t := strings.Trim(rest[j:], "[]")
					t = strings.NewReplacer("ahead ", "↑", "behind ", "↓", ", ", " ").Replace(t)
					track = t
				}
			}
			branch = strings.TrimPrefix(head, "No commits yet on ")
			continue
		}
		if len(line) < 2 {
			continue
		}
		switch {
		case line[:2] == "??":
			untracked++
		default:
			if line[0] != ' ' {
				staged++
			}
			if line[1] != ' ' {
				modified++
			}
		}
	}
	s := c.yellow + ic.branch + " " + branch
	if track != "" {
		s += " " + track
	}
	for _, p := range []struct {
		n   int
		sym string
	}{{staged, "+"}, {modified, "~"}, {untracked, "?"}} {
		if p.n > 0 {
			s += fmt.Sprintf(" %s%d", p.sym, p.n)
		}
	}
	return s + c.reset
}
