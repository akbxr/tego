// Package ext runs tego's extensions: programs, one process each, that
// talk JSON-RPC over stdio with tego. They can block or change tool calls,
// change tool results and prompts, add tools and slash commands, show
// messages and ask the user. JavaScript and TypeScript files run through
// a small host (host.mjs) under bun or node, with an API like pi's.
package ext

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ProtocolVersion is what tego speaks; initialize carries it.
const ProtocolVersion = 1

// initTimeout bounds an extension's start and initialize: bun or node
// loading it, no more.
const initTimeout = 15 * time.Second

// hookTimeout bounds one event handler; long, as one may ask the user.
const hookTimeout = 10 * time.Minute

// Events an extension can subscribe to. The request events wait for the
// handlers and take what they return; the others are notifications.
const (
	EventSessionStart = "session_start" // {cwd, session_id}
	EventInput        = "input"         // {text} -> {text?, handled?}
	EventToolCall     = "tool_call"     // {tool, input, agent} -> {block?, reason?, input?}
	EventToolResult   = "tool_result"   // {tool, input, output, is_error, agent} -> {output?}
	EventTurnEnd      = "turn_end"      // {text}
	EventSessionEnd   = "session_end"   // {}
)

// Candidate is an extension found on disk.
type Candidate struct {
	Path    string // file or folder
	Entry   string // the file that runs (Path, or the index file of a folder)
	Project bool   // from the project's .tego/extensions: needs trust
}

// Name is the extension's name from its path.
func (c Candidate) Name() string {
	base := filepath.Base(c.Path)
	if c.Path == c.Entry {
		base = strings.TrimSuffix(base, filepath.Ext(base))
	}
	return base
}

var scriptExts = []string{".ts", ".mts", ".js", ".mjs", ".py"}

// entry is the file that runs for path: itself if it is a script or an
// executable, a folder's index.ts/.js/.mjs or main.py; "" if none.
func entry(path string, fi os.FileInfo) string {
	if fi.IsDir() {
		for _, n := range []string{"index.ts", "index.mts", "index.js", "index.mjs", "main.py"} {
			if st, err := os.Stat(filepath.Join(path, n)); err == nil && !st.IsDir() {
				return filepath.Join(path, n)
			}
		}
		return ""
	}
	name := fi.Name()
	if strings.HasSuffix(name, ".d.ts") {
		return ""
	}
	for _, e := range scriptExts {
		if strings.HasSuffix(name, e) {
			return path
		}
	}
	if fi.Mode()&0o111 != 0 {
		return path
	}
	return ""
}

// Discover lists the extensions in home's extensions folder, then in the
// .tego/extensions folders from the outermost folder above cwd (below the
// user's home) in to cwd. A later one of the same name replaces an earlier.
func Discover(home, cwd string) []Candidate {
	byName := map[string]Candidate{}
	var order []string
	add := func(dir string, project bool) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
				continue
			}
			p := filepath.Join(dir, e.Name())
			fi, err := os.Stat(p)
			if err != nil {
				continue
			}
			if en := entry(p, fi); en != "" {
				c := Candidate{Path: p, Entry: en, Project: project}
				if _, ok := byName[c.Name()]; !ok {
					order = append(order, c.Name())
				}
				byName[c.Name()] = c
			}
		}
	}
	add(filepath.Join(home, "extensions"), false)
	userHome, _ := os.UserHomeDir()
	var chain []string
	for d := cwd; d != userHome; d = filepath.Dir(d) {
		chain = append(chain, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		add(filepath.Join(chain[i], ".tego", "extensions"), true)
	}
	out := make([]Candidate, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return out
}

//go:embed host.mjs
var hostJS []byte

var hostMu sync.Mutex

// hostPath writes the JS host to home's cache once per version of it.
func hostPath(home string) (string, error) {
	hostMu.Lock()
	defer hostMu.Unlock()
	dir := filepath.Join(home, "cache")
	path := filepath.Join(dir, fmt.Sprintf("ext-host-%08x.mjs", fnv32(hostJS)))
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "ext-host-*.tmp") // another tego may write it too
	if err != nil {
		return "", err
	}
	_, err = f.Write(hostJS)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return path, os.Rename(f.Name(), path)
}

func fnv32(b []byte) uint32 {
	h := uint32(2166136261)
	for _, c := range b {
		h = (h ^ uint32(c)) * 16777619
	}
	return h
}

// command is how to run c's entry.
func command(home string, c Candidate) (*exec.Cmd, error) {
	ext := filepath.Ext(c.Entry)
	switch ext {
	case ".ts", ".mts", ".js", ".mjs":
		host, err := hostPath(home)
		if err != nil {
			return nil, err
		}
		if bun, err := exec.LookPath("bun"); err == nil {
			return exec.Command(bun, host, c.Entry), nil
		}
		node, err := exec.LookPath("node")
		if err != nil {
			return nil, errors.New("needs bun or node to run " + ext + " extensions")
		}
		return exec.Command(node, host, c.Entry), nil
	case ".py":
		py, err := exec.LookPath("python3")
		if err != nil {
			return nil, errors.New("needs python3")
		}
		return exec.Command(py, c.Entry), nil
	}
	return exec.Command(c.Entry), nil
}

// ToolDef is a tool an extension adds.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Command is a slash command an extension adds.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Ext         string `json:"-"` // the extension's name
}

// Status describes one extension.
type Status struct {
	Name, Path, State, Err string
	Project                bool
	Tools, Commands        []string
	Events                 []string
}

// Extension states.
const (
	StateRunning   = "running"
	StateFailed    = "failed"
	StateUntrusted = "untrusted" // a project's, not trusted yet
	StateClosed    = "closed"
)

// Host is what extensions ask of tego: show a message, ask the user,
// send a prompt.
type Host interface {
	Notify(ext, message, level string)
	Confirm(ctx context.Context, ext, title, message string) (bool, error)
	Select(ctx context.Context, ext, title string, options []string) (int, error) // -1: none chosen
	Input(ctx context.Context, ext, title, placeholder string) (string, bool, error)
	SendMessage(ext, text string)
}

type extension struct {
	cand Candidate
	mu   sync.Mutex
	st   string
	err  string
	c    *conn
	cmd  *exec.Cmd
	init initResult
}

type initResult struct {
	Name     string    `json:"name"`
	Tools    []ToolDef `json:"tools"`
	Commands []Command `json:"commands"`
	Events   []string  `json:"events"`
}

func (e *extension) name() string { return e.cand.Name() }

func (e *extension) subscribed(event string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.st != StateRunning {
		return false
	}
	for _, ev := range e.init.Events {
		if ev == event {
			return true
		}
	}
	return false
}

func (e *extension) conn() (*conn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.st != StateRunning {
		if e.err != "" {
			return nil, fmt.Errorf("extension %s is %s: %s", e.name(), e.st, e.err)
		}
		return nil, fmt.Errorf("extension %s is %s", e.name(), e.st)
	}
	return e.c, nil
}

// Options are what a Manager needs.
type Options struct {
	Home      string // tego's home: the host script's cache, logs, trust
	Cwd       string
	SessionID string
	Version   string // tego's
	Host      Host
}

// Manager runs the extensions of one session.
type Manager struct {
	o    Options
	mu   sync.Mutex
	exts []*extension
	wg   sync.WaitGroup
}

// Start runs the trusted candidates, each in the background, and returns
// at once: Wait waits for them to be ready. Untrusted ones are listed but
// don't run.
func Start(cands []Candidate, o Options) *Manager {
	m := &Manager{o: o}
	trusted := loadTrust(o.Home)
	for _, c := range cands {
		e := &extension{cand: c}
		m.exts = append(m.exts, e)
		if c.Project && !trusted.has(c) {
			e.st = StateUntrusted
			continue
		}
		m.launch(e)
	}
	return m
}

func (m *Manager) launch(e *extension) {
	e.mu.Lock()
	e.st = "starting"
	e.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		err := m.run(e)
		e.mu.Lock()
		if err != nil {
			e.st, e.err = StateFailed, err.Error()
			e.mu.Unlock()
			return
		}
		if e.st == "starting" { // not exited meanwhile
			e.st = StateRunning
		}
		e.mu.Unlock()
		if e.subscribed(EventSessionStart) {
			_ = e.c.notifyExt("event", map[string]any{"event": EventSessionStart, "data": map[string]any{"cwd": m.o.Cwd, "session_id": m.o.SessionID}})
		}
	}()
}

// Wait waits for the extensions to start, up to timeout.
func (m *Manager) Wait(timeout time.Duration) {
	if m == nil {
		return
	}
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (m *Manager) run(e *extension) error {
	cmd, err := command(m.o.Home, e.cand)
	if err != nil {
		return err
	}
	cmd.Dir = m.o.Cwd
	cmd.Env = append(os.Environ(), "TEGO_EXTENSION="+e.name(), fmt.Sprintf("TEGO_PROTOCOL=%d", ProtocolVersion))
	logDir := filepath.Join(m.o.Home, "logs")
	_ = os.MkdirAll(logDir, 0o700)
	if f, err := os.OpenFile(filepath.Join(logDir, "ext-"+e.name()+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
		cmd.Stderr = f
		defer f.Close() // the child keeps its own descriptor
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	c := newConn(stdin)
	c.handle = func(method string, params json.RawMessage) (any, error) { return m.serve(e, method, params) }
	c.notify = func(method string, params json.RawMessage) { _, _ = m.serve(e, method, params) }
	go c.readLoop(stdout)
	go func() {
		<-c.done
		werr := cmd.Wait()
		e.mu.Lock()
		if e.st == StateRunning || e.st == "starting" {
			e.st, e.err = StateFailed, exitMsg(werr, m.o.Home, e.name())
		}
		e.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()
	var res initResult
	err = c.call(ctx, "initialize", map[string]any{
		"protocol_version": ProtocolVersion, "tego_version": m.o.Version, "cwd": m.o.Cwd, "session_id": m.o.SessionID,
	}, &res)
	if err != nil {
		_ = cmd.Process.Kill()
		if errors.Is(err, errClosed) {
			return errors.New(exitMsg(nil, m.o.Home, e.name()))
		}
		return fmt.Errorf("initialize: %w", err)
	}
	for i := range res.Commands {
		res.Commands[i].Ext = e.name()
	}
	e.mu.Lock()
	e.c, e.cmd, e.init = c, cmd, res
	e.mu.Unlock()
	return nil
}

// exitMsg says how an extension ended, with the end of its log.
func exitMsg(err error, home, name string) string {
	msg := "exited"
	if err != nil {
		msg += ": " + err.Error()
	}
	data, _ := os.ReadFile(filepath.Join(home, "logs", "ext-"+name+".log"))
	if s := strings.TrimSpace(string(data)); s != "" {
		lines := strings.Split(s, "\n")
		// The error's own line (bun and node follow it with a stack), else the end.
		for _, l := range lines {
			if l = strings.TrimSpace(l); strings.Contains(strings.ToLower(l), "error") && !strings.HasPrefix(l, "at ") {
				return msg + ": " + l
			}
		}
		msg += ": " + strings.Join(lines[max(len(lines)-3, 0):], " | ")
	}
	return msg
}

// serve answers an extension's request.
func (m *Manager) serve(e *extension, method string, params json.RawMessage) (any, error) {
	h := m.o.Host
	var p struct {
		Message     string   `json:"message"`
		Level       string   `json:"level"`
		Title       string   `json:"title"`
		Options     []string `json:"options"`
		Placeholder string   `json:"placeholder"`
		Text        string   `json:"text"`
	}
	_ = json.Unmarshal(params, &p)
	if h == nil {
		return nil, errors.New("no host")
	}
	ctx := context.Background()
	switch method {
	case "ui/notify":
		h.Notify(e.name(), p.Message, p.Level)
		return nil, nil
	case "ui/confirm":
		ok, err := h.Confirm(ctx, e.name(), p.Title, p.Message)
		return map[string]bool{"ok": ok}, err
	case "ui/select":
		i, err := h.Select(ctx, e.name(), p.Title, p.Options)
		return map[string]int{"index": i}, err
	case "ui/input":
		s, ok, err := h.Input(ctx, e.name(), p.Title, p.Placeholder)
		return map[string]any{"text": s, "cancelled": !ok}, err
	case "session/sendMessage":
		h.SendMessage(e.name(), p.Text)
		return nil, nil
	}
	return nil, fmt.Errorf("method not found: %s", method)
}

// running is the extensions running, in order.
func (m *Manager) running() []*extension {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*extension
	for _, e := range m.exts {
		e.mu.Lock()
		ok := e.st == StateRunning
		e.mu.Unlock()
		if ok {
			out = append(out, e)
		}
	}
	return out
}

// Status reports every extension found.
func (m *Manager) Status() []Status {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	exts := append([]*extension(nil), m.exts...)
	m.mu.Unlock()
	out := make([]Status, len(exts))
	for i, e := range exts {
		e.mu.Lock()
		s := Status{Name: e.name(), Path: e.cand.Path, State: e.st, Err: e.err, Project: e.cand.Project, Events: e.init.Events}
		for _, t := range e.init.Tools {
			s.Tools = append(s.Tools, t.Name)
		}
		for _, c := range e.init.Commands {
			s.Commands = append(s.Commands, c.Name)
		}
		e.mu.Unlock()
		out[i] = s
	}
	return out
}

// Trust trusts the untrusted project extensions and starts them; it
// returns their names.
func (m *Manager) Trust() []string {
	if m == nil {
		return nil
	}
	t := loadTrust(m.o.Home)
	var names []string
	m.mu.Lock()
	exts := append([]*extension(nil), m.exts...)
	m.mu.Unlock()
	for _, e := range exts {
		e.mu.Lock()
		untrusted := e.st == StateUntrusted
		e.mu.Unlock()
		if untrusted {
			t.add(e.cand)
			names = append(names, e.name())
			m.launch(e)
		}
	}
	if len(names) > 0 {
		_ = t.save(m.o.Home)
	}
	return names
}

// Close tells the extensions the session ends and stops them.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.Wait(initTimeout)
	m.Emit(EventSessionEnd, map[string]any{})
	var wg sync.WaitGroup
	for _, e := range m.running() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.mu.Lock()
			c, cmd := e.c, e.cmd
			e.st = StateClosed
			e.mu.Unlock()
			_ = c.notifyExt("shutdown", struct{}{})
			select {
			case <-c.done:
			case <-time.After(time.Second):
				_ = cmd.Process.Kill()
				<-c.done
			}
		}()
	}
	wg.Wait()
}
