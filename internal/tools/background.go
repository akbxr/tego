package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// Background shells: bash with background:true starts a command that keeps
// running (a dev server, a watcher, a long build) while the agent goes on,
// and a plain bash call that leaves a process running (`npm run dev &`)
// has it adopted as one. bash_output reads what it printed since the last
// read, can wait for a line (a server's "listening on"), and stops it.
//
// A shell is its process group: it runs until no process of the group is
// left, so children that outlive the shell itself still count. It stops
// when it ends, when it is stopped, when the subagent that started it
// finishes (StopShells), or when tego exits (KillShells). The groups of
// running shells are written to a file, so a tego started after a crash
// can offer to stop what the crashed one left (StaleShells).

const (
	bgMaxShells   = 8
	bgBufBytes    = 256 << 10       // output kept per shell; older bytes drop
	bgMaxWait     = 120             // seconds bash_output may wait
	bgKillGrace   = 3 * time.Second // SIGTERM, then SIGKILL
	bgOutputLimit = 20 << 10        // bytes of new output returned per read
)

var (
	// bgStartWait is how long starting a shell waits, to catch commands
	// that fail at once (a var for tests).
	bgStartWait = 2 * time.Second
	// bgPoll is how often a group whose shell exited is checked for the
	// processes still in it.
	bgPoll = 200 * time.Millisecond
	// bgSettle is how long what a plain bash call left running gets to end
	// by itself before it is adopted (`(sleep 1; touch x) &` isn't worth a
	// shell).
	bgSettle = 500 * time.Millisecond
)

// portRe finds the address a server says it listens on.
var portRe = regexp.MustCompile(`(?i)(?:localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1?\]):(\d{2,5})\b`)

type bgShell struct {
	id, command string
	owner       string // the subagent that started it; "" the session
	cwd         string
	started     time.Time
	pgid        int
	adopted     bool          // left running by a plain bash call
	done        chan struct{} // closed when no process of the group is left
	once        sync.Once
	reg         *shells

	mu       sync.Mutex
	buf      []byte // the last bgBufBytes of output
	total    int64  // bytes written in all
	readTo   int64  // stream offset the model has seen up to
	grew     chan struct{}
	exit     string // the shell's "exit code 0", "killed: …"; "" for an adopted one
	ended    time.Time
	reported bool   // the model knows it ended (read after the end, told, or the user stopped it)
	port     string // ":3000", from the first address it printed
}

func (s *bgShell) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	if over := len(s.buf) - bgBufBytes; over > 0 {
		s.buf = append(s.buf[:0], s.buf[over:]...)
	}
	s.total += int64(len(p))
	if s.port == "" {
		if m := portRe.FindSubmatch(p); m != nil {
			s.port = ":" + string(m[1])
		}
	}
	close(s.grew)
	s.grew = make(chan struct{})
	return len(p), nil
}

// unread returns the output after readTo (and how many bytes of it were
// dropped from the buffer before they were read), and marks it read.
func (s *bgShell) unread() (text string, dropped int64) {
	ended := s.finished()
	s.mu.Lock()
	defer s.mu.Unlock()
	from := s.total - int64(len(s.buf)) // stream offset of buf[0]
	if s.readTo < from {
		dropped = from - s.readTo
		s.readTo = from
	}
	text = string(s.buf[s.readTo-from:])
	s.readTo = s.total
	if ended {
		s.reported = true
	}
	return text, dropped
}

// status is "running for 34s" or "ended (exit code 1) after 12s".
func (s *bgShell) status() string {
	if !s.finished() {
		return "running for " + since(s.started)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.ended.Sub(s.started).Round(time.Second)
	if s.exit == "" {
		return "ended after " + d.String()
	}
	return fmt.Sprintf("ended (%s) after %s", s.exit, d)
}

func since(t time.Time) string { return time.Since(t).Round(time.Second).String() }

func (s *bgShell) finished() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// finish marks the shell ended, once.
func (s *bgShell) finish() {
	s.once.Do(func() {
		s.mu.Lock()
		s.ended = time.Now()
		close(s.grew)
		s.grew = make(chan struct{})
		s.mu.Unlock()
		close(s.done)
		s.reg.save()
	})
}

// waitGroupGone returns once no process of the shell's group is left.
func (s *bgShell) waitGroupGone() {
	for groupAlive(s.pgid) {
		time.Sleep(bgPoll)
	}
}

// groupAlive reports whether a process of group pgid is left.
func groupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// stop ends the shell's whole process group: SIGTERM, then SIGKILL.
func (s *bgShell) stop() {
	if s.finished() {
		return
	}
	_ = syscall.Kill(-s.pgid, syscall.SIGTERM)
	select {
	case <-s.done:
		return
	case <-time.After(bgKillGrace):
	}
	_ = syscall.Kill(-s.pgid, syscall.SIGKILL)
	select {
	case <-s.done:
	case <-time.After(bgKillGrace):
	}
}

func (s *bgShell) visibleTo(owner string) bool { return owner == "" || s.owner == owner }

// describe is "bg2 (npm run dev)".
func (s *bgShell) describe() string {
	return fmt.Sprintf("%s (%s)", s.id, oneLine(s.command, 60))
}

// shells is the background shells of a session and its subagents.
type shells struct {
	mu     sync.Mutex
	list   []*bgShell
	next   int
	record string     // file naming the running shells' groups ("": none)
	saveMu sync.Mutex // one save at a time, so the file ends up current
}

// reg is e's shells, made at first use.
func (e *Env) reg() *shells {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.bg == nil {
		e.bg = &shells{}
	}
	return e.bg
}

// At is an Env for dir (a subagent's worktree) that shares e's background
// shells, so they show, stop and get cleaned up with the session's.
func (e *Env) At(dir string) *Env {
	if e == nil {
		return &Env{Cwd: dir}
	}
	return &Env{Cwd: dir, bg: e.reg()}
}

type shellOwnerKey struct{}

// WithShellOwner marks the tool calls of ctx as a subagent's: the shells
// they start are its own, and it sees only those.
func WithShellOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, shellOwnerKey{}, owner)
}

func shellOwner(ctx context.Context) string {
	s, _ := ctx.Value(shellOwnerKey{}).(string)
	return s
}

// add registers a new shell (not started yet) and names it.
func (r *shells) add(owner, command, cwd string, started time.Time) *bgShell {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	s := &bgShell{id: fmt.Sprintf("bg%d", r.next), command: command, owner: owner, cwd: cwd, started: started,
		done: make(chan struct{}), grew: make(chan struct{}), reg: r}
	r.list = append(r.list, s)
	return s
}

func (r *shells) running() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.list {
		if !s.finished() {
			n++
		}
	}
	return n
}

func (r *shells) snapshot() []*bgShell {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*bgShell(nil), r.list...)
}

func (r *shells) drop(s *bgShell) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, x := range r.list {
		if x == s {
			r.list = append(r.list[:i], r.list[i+1:]...)
			return
		}
	}
}

// startBackground starts command as a background shell and waits briefly,
// so a command that fails at once reports its error right away.
func (e *Env) startBackground(ctx context.Context, command string) (string, error) {
	r := e.reg()
	if n := r.running(); n >= bgMaxShells {
		return "", fmt.Errorf("%d background shells are already running; stop one with bash_output (kill: true) first", n)
	}
	s := r.add(shellOwner(ctx), command, e.Cwd, time.Now())

	shell := "bash"
	if _, err := exec.LookPath(shell); err != nil {
		shell = "sh"
	}
	cmd := exec.Command(shell, "-c", command)
	cmd.Dir = e.Cwd
	cmd.Env = append(os.Environ(), "TERM=dumb", "PAGER=cat", "GIT_PAGER=cat", "NO_COLOR=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = s, s
	if err := cmd.Start(); err != nil {
		r.drop(s)
		return "", fmt.Errorf("failed to start %s: %v", shell, err)
	}
	s.pgid = cmd.Process.Pid
	r.save()
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		s.exit = exitText(err)
		s.mu.Unlock()
		s.waitGroupGone() // what it started may outlive it
		s.finish()
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
		"(wait_for a line such as a server's ready message), and stop it with kill: true. You are told when it ends.", s.id, s.pgid, s.id)
	if out != "" {
		res += " Output so far:\n" + capText(out)
	}
	return res, nil
}

// adopt keeps what a plain bash call left running in its process group as
// a background shell; its output goes on into the shell. It returns the
// note for the model.
func (e *Env) adopt(ctx context.Context, command string, started time.Time, l *leftover) string {
	r := e.reg()
	s := r.add(shellOwner(ctx), command, e.Cwd, started)
	s.pgid, s.adopted = l.pgid, true
	l.sink.set(s)
	r.save()
	go func() {
		s.waitGroupGone()
		s.finish()
		_ = l.r.Close() // a daemon that left the group may hold the pipe
	}()
	return fmt.Sprintf("\n[Something this command started is still running: tego keeps it as background shell %s. "+
		"Read its output with bash_output {\"id\":%q} and stop it with kill: true once it isn't needed. "+
		"Start such commands with background: true rather than &.]", s.id, s.id)
}

func (e *Env) shell(ctx context.Context, id string) (*bgShell, error) {
	owner := shellOwner(ctx)
	var ids []string
	for _, s := range e.reg().snapshot() {
		if !s.visibleTo(owner) {
			continue
		}
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

// stopAll stops shells in parallel, marks them reported, and returns what
// was running of them.
func stopAll(list []*bgShell) []*bgShell {
	var stopped []*bgShell
	var wg sync.WaitGroup
	for _, s := range list {
		if s.finished() {
			continue
		}
		stopped = append(stopped, s)
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.stop()
			s.mu.Lock()
			s.reported = true
			s.mu.Unlock()
		}()
	}
	wg.Wait()
	return stopped
}

// KillShells stops every running background shell (tego is exiting) and
// returns how many there were.
func (e *Env) KillShells() int {
	r := e.reg()
	n := len(stopAll(r.snapshot()))
	r.save()
	return n
}

// StopShells stops the shells of owner (a subagent that finished) and
// describes them: "bg2 (npm run dev)".
func (e *Env) StopShells(owner string) []string {
	if e == nil || owner == "" {
		return nil
	}
	var mine []*bgShell
	for _, s := range e.reg().snapshot() {
		if s.owner == owner {
			mine = append(mine, s)
		}
	}
	var out []string
	for _, s := range stopAll(mine) {
		out = append(out, s.describe())
	}
	return out
}

// RunningShells is the number of background shells still running.
func (e *Env) RunningShells() int { return e.reg().running() }

// ShellNotices tells the session's model about its background shells that
// ended since it last looked, once each; "" when there is nothing new.
func (e *Env) ShellNotices() string { return e.ShellNoticesFor("") }

// ShellNoticesFor is ShellNotices for the shells of owner ("": the
// session's own).
func (e *Env) ShellNoticesFor(owner string) string {
	var lines []string
	for _, s := range e.reg().snapshot() {
		if s.owner != owner || !s.finished() {
			continue
		}
		status := s.status()
		s.mu.Lock()
		if !s.reported {
			s.reported = true
			line := fmt.Sprintf("Background shell %s (%s) %s", s.id, oneLine(s.command, 60), status)
			if unread := s.total - s.readTo; unread > 0 {
				line += fmt.Sprintf("; %d bytes of output not read yet (bash_output)", unread)
			}
			lines = append(lines, line+".")
		}
		s.mu.Unlock()
	}
	return strings.Join(lines, "\n")
}

// ShellsSince is owner's shells started at or after t that still run.
func (e *Env) ShellsSince(t time.Time, owner string) []ShellInfo {
	var out []ShellInfo
	for _, s := range e.Shells() {
		if s.Owner == owner && s.Running && !s.Started.Before(t) {
			out = append(out, s)
		}
	}
	return out
}

// Describe is "bg1 `npm run dev` (running 2m0s, :3000)".
func (s ShellInfo) Describe() string {
	d := fmt.Sprintf("%s `%s` (running %s", s.ID, oneLine(s.Command, 60), since(s.Started))
	if s.Port != "" {
		d += ", " + s.Port
	}
	return d + ")"
}

// ShellInfo is a background shell, for the user.
type ShellInfo struct {
	ID, Command string
	Owner       string // the subagent's; "" the session's
	Status      string // "running for 3m0s", "ended (exit code 0) after 2s"
	Port        string // ":3000" when it printed an address
	Running     bool
	Adopted     bool
	Started     time.Time
}

// Shells lists the background shells, oldest first.
func (e *Env) Shells() []ShellInfo {
	list := e.reg().snapshot()
	sort.SliceStable(list, func(i, j int) bool { return list[i].started.Before(list[j].started) })
	out := make([]ShellInfo, 0, len(list))
	for _, s := range list {
		info := ShellInfo{ID: s.id, Command: s.command, Owner: s.owner, Status: s.status(), Running: !s.finished(),
			Adopted: s.adopted, Started: s.started}
		s.mu.Lock()
		info.Port = s.port
		s.mu.Unlock()
		out = append(out, info)
	}
	return out
}

// RunningPort is the address port of the one running shell, when exactly
// one runs and it printed one: for the footer.
func (e *Env) RunningPort() string {
	port, n := "", 0
	for _, s := range e.reg().snapshot() {
		if s.finished() {
			continue
		}
		n++
		s.mu.Lock()
		port = s.port
		s.mu.Unlock()
	}
	if n != 1 {
		return ""
	}
	return port
}

// ShellTail is the last lines of a shell's output, for the user; it doesn't
// count as read.
func (e *Env) ShellTail(id string, lines int) string {
	for _, s := range e.reg().snapshot() {
		if s.id != id {
			continue
		}
		s.mu.Lock()
		text := strings.TrimRight(string(s.buf), "\n")
		s.mu.Unlock()
		all := strings.Split(text, "\n")
		if len(all) > lines {
			all = all[len(all)-lines:]
		}
		return strings.Join(all, "\n")
	}
	return ""
}

// StopShell stops shell id for the user; false if it wasn't running.
func (e *Env) StopShell(id string) bool {
	for _, s := range e.reg().snapshot() {
		if s.id == id {
			return len(stopAll([]*bgShell{s})) == 1
		}
	}
	return false
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
		return t.env.listShells(ctx), nil
	}
	s, err := t.env.shell(ctx, in.ID)
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
		ended := s.finished()
		s.mu.Lock()
		from := s.total - int64(len(s.buf))
		fresh := s.buf[max(s.readTo-from, 0):]
		hit := len(fresh) > 0 && (re == nil || re.Match(fresh))
		grew := s.grew
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

func (e *Env) listShells(ctx context.Context) string {
	owner := shellOwner(ctx)
	var list []*bgShell
	for _, s := range e.reg().snapshot() {
		if s.visibleTo(owner) {
			list = append(list, s)
		}
	}
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

// ---- after a crash ----

// shellRecord is the file a tego keeps of its running shells' groups.
type shellRecord struct {
	PID    int `json:"pid"` // tego's
	Shells []struct {
		PGID    int       `json:"pgid"`
		Command string    `json:"command"`
		Cwd     string    `json:"cwd"`
		Started time.Time `json:"started"`
	} `json:"shells"`
}

// RecordShells keeps the running shells' groups in path, which is removed
// once none runs.
func (e *Env) RecordShells(path string) {
	r := e.reg()
	r.mu.Lock()
	r.record = path
	r.mu.Unlock()
	r.save()
}

func (r *shells) save() {
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	r.mu.Lock()
	path := r.record
	rec := shellRecord{PID: os.Getpid()}
	for _, s := range r.list {
		if s.finished() || s.pgid <= 0 {
			continue
		}
		rec.Shells = append(rec.Shells, struct {
			PGID    int       `json:"pgid"`
			Command string    `json:"command"`
			Cwd     string    `json:"cwd"`
			Started time.Time `json:"started"`
		}{s.pgid, s.command, s.cwd, s.started})
	}
	r.mu.Unlock()
	if path == "" {
		return
	}
	if len(rec.Shells) == 0 {
		_ = os.Remove(path)
		return
	}
	data, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// StaleShell is a process group a tego that is gone left running.
type StaleShell struct {
	File    string // the record it came from
	PGID    int
	Command string // as tego ran it
	Cwd     string
	Started time.Time
	Members []string // the commands of its processes now
}

// StaleShells reads the records in dir of tegos no longer running and
// returns the groups of theirs that still run. Records with nothing left
// are removed.
func StaleShells(dir string) []StaleShell {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var out []StaleShell
	var members map[int][]string
	for _, f := range files {
		data, err := os.ReadFile(f)
		var rec shellRecord
		if err != nil || json.Unmarshal(data, &rec) != nil {
			_ = os.Remove(f)
			continue
		}
		if rec.PID == os.Getpid() || tegoRunning(rec.PID) {
			continue
		}
		if members == nil {
			members = groupMembers()
		}
		found := false
		for _, s := range rec.Shells {
			if m := members[s.PGID]; len(m) > 0 {
				out = append(out, StaleShell{File: f, PGID: s.PGID, Command: s.Command, Cwd: s.Cwd, Started: s.Started, Members: m})
				found = true
			}
		}
		if !found {
			_ = os.Remove(f)
		}
	}
	return out
}

// StopStale stops the groups (SIGTERM, then SIGKILL) and forgets them.
func StopStale(list []StaleShell) {
	var wg sync.WaitGroup
	for _, s := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = syscall.Kill(-s.PGID, syscall.SIGTERM)
			deadline := time.Now().Add(bgKillGrace)
			for groupAlive(s.PGID) && time.Now().Before(deadline) {
				time.Sleep(bgPoll)
			}
			if groupAlive(s.PGID) {
				_ = syscall.Kill(-s.PGID, syscall.SIGKILL)
			}
		}()
	}
	wg.Wait()
	ForgetStale(list)
}

// ForgetStale removes the records the groups came from.
func ForgetStale(list []StaleShell) {
	for _, s := range list {
		_ = os.Remove(s.File)
	}
}

// tegoRunning reports whether pid is a running tego (not a process that
// got its pid since).
func tegoRunning(pid int) bool {
	if pid <= 0 || !groupAliveProc(pid) {
		return false
	}
	out, err := exec.Command("ps", "-p", fmt.Sprint(pid), "-o", "comm=").Output()
	return err == nil && strings.Contains(strings.ToLower(string(out)), "tego")
}

func groupAliveProc(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// groupMembers maps process groups to their processes' commands.
func groupMembers() map[int][]string {
	out, err := exec.Command("ps", "-A", "-o", "pgid=,command=").Output()
	m := map[int][]string{}
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		var pgid int
		if _, err := fmt.Sscan(f[0], &pgid); err != nil {
			continue
		}
		m[pgid] = append(m[pgid], oneLine(strings.Join(f[1:], " "), 100))
	}
	return m
}
