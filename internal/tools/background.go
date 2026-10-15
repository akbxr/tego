package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// Background shells: bash with background:true starts a command that keeps
// running (a dev server, a watcher, a long build) while the agent goes on.
// bash_output reads what it printed since the last read, can wait for a
// line (a server's "listening on"), and stops it. Shells live as long as
// tego: KillShells stops them all on exit.

const (
	bgMaxShells   = 8
	bgBufBytes    = 256 << 10       // output kept per shell; older bytes drop
	bgMaxWait     = 120             // seconds bash_output may wait
	bgKillGrace   = 3 * time.Second // SIGTERM, then SIGKILL
	bgOutputLimit = 20 << 10        // bytes of new output returned per read
)

// bgStartWait is how long starting a shell waits, to catch commands that
// fail at once (a var for tests).
var bgStartWait = 2 * time.Second

type bgShell struct {
	id, command string
	started     time.Time
	cmd         *exec.Cmd
	done        chan struct{} // closed when the process has exited

	mu       sync.Mutex
	buf      []byte // the last bgBufBytes of output
	total    int64  // bytes written in all
	readTo   int64  // stream offset the model has seen up to
	grew     chan struct{}
	exit     string // "exit code 0", "killed: …" once done
	ended    time.Time
	reported bool // the model knows it ended (read after the end, or told)
}

func (s *bgShell) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	if over := len(s.buf) - bgBufBytes; over > 0 {
		s.buf = append(s.buf[:0], s.buf[over:]...)
	}
	s.total += int64(len(p))
	close(s.grew)
	s.grew = make(chan struct{})
	return len(p), nil
}

// unread returns the output after readTo (and how many bytes of it were
// dropped from the buffer before they were read), and marks it read.
func (s *bgShell) unread() (text string, dropped int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	from := s.total - int64(len(s.buf)) // stream offset of buf[0]
	if s.readTo < from {
		dropped = from - s.readTo
		s.readTo = from
	}
	text = string(s.buf[s.readTo-from:])
	s.readTo = s.total
	if s.exit != "" {
		s.reported = true
	}
	return text, dropped
}

// status is "running for 34s" or "exited with exit code 1 after 12s".
func (s *bgShell) status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exit == "" {
		return "running for " + since(s.started)
	}
	return fmt.Sprintf("ended (%s) after %s", s.exit, s.ended.Sub(s.started).Round(time.Second))
}

func since(t time.Time) string { return time.Since(t).Round(time.Second).String() }

type shells struct {
	mu   sync.Mutex
	list []*bgShell
	next int
}

// startBackground starts command as a background shell and waits briefly,
// so a command that fails at once reports its error right away.
func (e *Env) startBackground(command string) (string, error) {
	e.bg.mu.Lock()
	running := 0
	for _, s := range e.bg.list {
		if !s.finished() {
			running++
		}
	}
	if running >= bgMaxShells {
		e.bg.mu.Unlock()
		return "", fmt.Errorf("%d background shells are already running; stop one with bash_output (kill: true) first", running)
	}
	e.bg.next++
	s := &bgShell{id: fmt.Sprintf("bg%d", e.bg.next), command: command, started: time.Now(), done: make(chan struct{}), grew: make(chan struct{})}
	e.bg.list = append(e.bg.list, s)
	e.bg.mu.Unlock()

	shell := "bash"
	if _, err := exec.LookPath(shell); err != nil {
		shell = "sh"
	}
	s.cmd = exec.Command(shell, "-c", command)
	s.cmd.Dir = e.Cwd
	s.cmd.Env = append(os.Environ(), "TERM=dumb", "PAGER=cat", "GIT_PAGER=cat", "NO_COLOR=1")
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s.cmd.Stdout, s.cmd.Stderr = s, s
	if err := s.cmd.Start(); err != nil {
		e.dropShell(s)
		return "", fmt.Errorf("failed to start %s: %v", shell, err)
	}
	go func() {
		err := s.cmd.Wait()
		s.mu.Lock()
		s.exit, s.ended = exitText(err), time.Now()
		close(s.grew)
		s.grew = make(chan struct{})
		s.mu.Unlock()
		close(s.done)
	}()

	select {
	case <-s.done:
	case <-time.After(bgStartWait):
	}
	out, _ := s.unread()
	out = strings.TrimRight(out, "\n")
	if s.finished() {
		res := fmt.Sprintf("Background shell %s %s at once.", s.id, s.status())
		if out != "" {
			res += " Output:\n" + capText(out)
		}
		return res, nil
	}
	res := fmt.Sprintf("Started background shell %s (pid %d), still running. Read its output with bash_output {\"id\":%q} "+
		"(wait_for a line such as a server's ready message), and stop it with kill: true. You are told when it ends.", s.id, s.cmd.Process.Pid, s.id)
	if out != "" {
		res += " Output so far:\n" + capText(out)
	}
	return res, nil
}

func (s *bgShell) finished() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (e *Env) dropShell(s *bgShell) {
	e.bg.mu.Lock()
	defer e.bg.mu.Unlock()
	for i, x := range e.bg.list {
		if x == s {
			e.bg.list = append(e.bg.list[:i], e.bg.list[i+1:]...)
			return
		}
	}
}

func (e *Env) shell(id string) (*bgShell, error) {
	e.bg.mu.Lock()
	defer e.bg.mu.Unlock()
	var ids []string
	for _, s := range e.bg.list {
		if s.id == id {
			return s, nil
		}
		ids = append(ids, s.id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no background shell %q: none were started", id)
	}
	return nil, fmt.Errorf("no background shell %q; there are %s", id, strings.Join(ids, ", "))
}

func exitText(err error) string {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return "exit code 0"
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		return fmt.Sprintf("exit code %d", ee.ExitCode())
	case errors.As(err, &ee):
		return "killed: " + ee.String()
	default:
		return err.Error()
	}
}

// stop ends the shell's whole process group: SIGTERM, then SIGKILL.
func (s *bgShell) stop() {
	if s.finished() || s.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(bgKillGrace):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.done
	}
}

// KillShells stops every running background shell (tego is exiting) and
// returns how many there were.
func (e *Env) KillShells() int {
	e.bg.mu.Lock()
	list := append([]*bgShell(nil), e.bg.list...)
	e.bg.mu.Unlock()
	n := 0
	var wg sync.WaitGroup
	for _, s := range list {
		if !s.finished() {
			n++
			wg.Add(1)
			go func() { defer wg.Done(); s.stop() }()
		}
	}
	wg.Wait()
	return n
}

// RunningShells is the number of background shells still running.
func (e *Env) RunningShells() int {
	e.bg.mu.Lock()
	defer e.bg.mu.Unlock()
	n := 0
	for _, s := range e.bg.list {
		if !s.finished() {
			n++
		}
	}
	return n
}

// ShellNotices tells the model about background shells that ended since it
// last looked, once each; "" when there is nothing new.
func (e *Env) ShellNotices() string {
	e.bg.mu.Lock()
	list := append([]*bgShell(nil), e.bg.list...)
	e.bg.mu.Unlock()
	var lines []string
	for _, s := range list {
		if !s.finished() {
			continue
		}
		s.mu.Lock()
		if !s.reported {
			s.reported = true
			unread := s.total - s.readTo
			line := fmt.Sprintf("Background shell %s (%s) ended: %s after %s", s.id, oneLine(s.command, 60), s.exit, s.ended.Sub(s.started).Round(time.Second))
			if unread > 0 {
				line += fmt.Sprintf("; %d bytes of output not read yet (bash_output)", unread)
			}
			lines = append(lines, line+".")
		}
		s.mu.Unlock()
	}
	return strings.Join(lines, "\n")
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// capText trims output for the model: the start and the end of it.
func capText(s string) string {
	w := &capWriter{}
	_, _ = w.Write([]byte(s))
	return w.String()
}

type bashOutputTool struct{ env *Env }

func (t *bashOutputTool) Def() llm.Tool {
	return llm.Tool{
		Name: "bash_output",
		Description: "Read a background shell started with bash background:true: returns the output printed since your last read and whether it is still running. " +
			"wait_for (a regular expression) waits until a new line matches it, up to wait seconds (default 30, max 120), e.g. a server's ready line; " +
			"wait alone waits for new output or the end. kill: true stops the shell and its children. Without id, lists the background shells.",
		Schema: schema(`{"type":"object","properties":{` +
			`"id":{"type":"string","description":"e.g. bg1"},` +
			`"wait_for":{"type":"string","description":"Regular expression to wait for in new output"},` +
			`"wait":{"type":"number","description":"Seconds to wait (default 30 with wait_for, else 0; max 120)"},` +
			`"kill":{"type":"boolean","description":"Stop the shell"}}}`),
	}
}

func (t *bashOutputTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		ID      string  `json:"id"`
		WaitFor string  `json:"wait_for"`
		Wait    float64 `json:"wait"`
		Kill    bool    `json:"kill"`
	}
	if err := decode(input, &in); err != nil {
		return "", err
	}
	if in.ID == "" {
		return t.env.listShells(), nil
	}
	s, err := t.env.shell(in.ID)
	if err != nil {
		return "", err
	}
	if in.Kill {
		was := !s.finished()
		s.stop()
		out, dropped := s.unread()
		res := fmt.Sprintf("%s stopped; %s.", s.id, s.status())
		if !was {
			res = fmt.Sprintf("%s had already %s.", s.id, s.status())
		}
		return res + formatNew(out, dropped), nil
	}
	var re *regexp.Regexp
	if in.WaitFor != "" {
		if re, err = regexp.Compile(in.WaitFor); err != nil {
			return "", fmt.Errorf("wait_for: %v", err)
		}
		if in.Wait <= 0 {
			in.Wait = 30
		}
	}
	wait := time.Duration(min(max(in.Wait, 0), bgMaxWait) * float64(time.Second))
	matched := s.waitOutput(ctx, re, wait)
	out, dropped := s.unread()
	res := fmt.Sprintf("%s %s.", s.id, s.status())
	if re != nil && !matched {
		res += fmt.Sprintf(" No new line matched %q within %s.", in.WaitFor, wait.Round(time.Second))
	}
	return res + formatNew(out, dropped), nil
}

// waitOutput waits up to d for new output matching re (any new output if
// re is nil) or for the shell to end; it reports whether re matched.
func (s *bgShell) waitOutput(ctx context.Context, re *regexp.Regexp, d time.Duration) bool {
	if d <= 0 {
		return false
	}
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		from := s.total - int64(len(s.buf))
		fresh := s.buf[max(s.readTo-from, 0):]
		hit := len(fresh) > 0 && (re == nil || re.Match(fresh))
		ended, grew := s.exit != "", s.grew
		s.mu.Unlock()
		if hit {
			return re != nil
		}
		if ended {
			return false
		}
		select {
		case <-grew:
		case <-deadline.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func formatNew(out string, dropped int64) string {
	out = strings.TrimRight(out, "\n")
	switch {
	case out == "" && dropped == 0:
		return " No new output."
	case len(out) > bgOutputLimit:
		out = capText(out)
	}
	s := " New output:\n"
	if dropped > 0 {
		s = fmt.Sprintf(" New output (%d earlier bytes were dropped before you read them):\n", dropped)
	}
	return s + out
}

func (e *Env) listShells() string {
	e.bg.mu.Lock()
	list := append([]*bgShell(nil), e.bg.list...)
	e.bg.mu.Unlock()
	if len(list) == 0 {
		return "No background shells. Start one with bash and background: true."
	}
	sort.Slice(list, func(i, j int) bool { return list[i].started.Before(list[j].started) })
	var b strings.Builder
	for _, s := range list {
		s.mu.Lock()
		unread := s.total - s.readTo
		s.mu.Unlock()
		fmt.Fprintf(&b, "%s  %s  %s", s.id, s.status(), oneLine(s.command, 80))
		if unread > 0 {
			fmt.Fprintf(&b, "  (%d bytes unread)", unread)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
