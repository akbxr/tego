package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// coldWait is how long the first check on a new server waits: it may
	// still be loading the project. A check that runs out says nothing and
	// the server goes on loading for the next one.
	coldWait = 10 * time.Second
	// warmWait is how long later checks wait.
	warmWait = 5 * time.Second
	// settle is how long to wait for more diagnostics (other files, a
	// second pass) after the edited file's arrived.
	settle = 150 * time.Millisecond
	// DefaultIdle is how long a server may sit unused before it stops.
	DefaultIdle = 10 * time.Minute

	maxFileErrors  = 15
	maxOtherErrors = 10
)

// Manager runs language servers on demand: a server starts at the first
// edit of a file it checks and stops after it sat unused for Idle. Safe
// for concurrent use.
type Manager struct {
	Cwd   string
	Specs []Spec
	Idle  time.Duration // 0: run until Close
	// OnFail, when set, is told about a server that could not start when
	// no other server for the file could either; once per server and root.
	OnFail func(server, reason string)

	mu       sync.Mutex
	servers  map[serverKey]*server
	starting map[serverKey]chan struct{}
	failed   map[serverKey]bool
	onPath   map[string]string // program -> its path on PATH, "" when not there
	closed   bool
	reaper   bool
}

type serverKey struct{ name, root string }

// NewManager returns a Manager for cwd using specs.
func NewManager(cwd string, specs []Spec) *Manager {
	return &Manager{Cwd: cwd, Specs: specs, Idle: DefaultIdle}
}

// candidate is a server that can check a file: installed, with its root.
type candidate struct {
	spec Spec // Command[0] resolved to the program's path
	lang string
	root string
}

// candidates are the installed servers that check path, in Specs order.
func (m *Manager) candidates(path string) []candidate {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return nil
	}
	var out []candidate
	for _, s := range m.Specs {
		lang, ok := s.Languages[ext]
		if !ok || len(s.Command) == 0 {
			continue
		}
		root := findRoot(s, path, m.Cwd)
		if root == "" {
			continue
		}
		prog := m.program(s, root)
		if prog == "" {
			continue
		}
		s.Command = append([]string{prog}, s.Command[1:]...)
		out = append(out, candidate{s, lang, root})
	}
	return out
}

// program finds a server's program: in the project's own bin directories
// first (node_modules/.bin, .venv/bin), then on PATH.
func (m *Manager) program(s Spec, root string) string {
	name := s.Command[0]
	if filepath.IsAbs(name) {
		return name
	}
	for _, d := range s.LocalBins {
		p := filepath.Join(root, d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.onPath == nil {
		m.onPath = map[string]string{}
	}
	p, seen := m.onPath[name]
	if !seen {
		p, _ = lookPath(name)
		m.onPath[name] = p
	}
	return p
}

// Check sends after as path's content to its language server and returns
// a note on the errors it reports, for the end of an edit's result: "" when
// there are none. before is the content the edit replaced ("" for a new
// file): a file the server has not seen gets it first, so errors other
// files had already are not blamed on the edit. ok is false when no server
// checked the file (none installed, it failed, or it did not answer in
// time).
func (m *Manager) Check(ctx context.Context, path, before, after string, rel func(string) string) (note string, ok bool) {
	if m == nil {
		return "", false
	}
	// The first server that runs checks it; one that fails to start is
	// not tried again, and only reported when none of them runs.
	var failed []string
	for _, c := range m.candidates(path) {
		s, cold, err := m.server(ctx, c.spec, c.root)
		if err != nil {
			failed = append(failed, c.spec.Name, err.Error())
		}
		if s == nil {
			continue
		}
		defer m.release(s)
		return m.check(ctx, s, cold, c, path, before, after, rel)
	}
	if m.OnFail != nil {
		for i := 0; i < len(failed); i += 2 {
			m.OnFail(failed[i], failed[i+1])
		}
	}
	return "", false
}

func (m *Manager) check(ctx context.Context, s *server, cold bool, c candidate, path, before, after string, rel func(string) string) (string, bool) {
	lang := c.lang
	wait := warmWait
	if cold {
		wait = coldWait
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	uri := fileURI(path)
	if before != "" && !s.pull && !s.isOpen(uri) {
		_, seq0, _ := s.snapshot()
		v, err := s.open(uri, lang, before)
		if err != nil {
			return "", false
		}
		if _, _, ok := waitPublished(ctx, s, uri, v, seq0); !ok {
			return "", false
		}
	}
	prev, seq0, _ := s.snapshot()
	v, err := s.open(uri, lang, after)
	if err != nil {
		return "", false
	}
	var file []Diagnostic
	others := map[string][]Diagnostic{}
	if s.pull {
		if file, err = s.pullDiagnostics(ctx, uri); err != nil {
			return "", false
		}
	} else {
		got, published, ok := waitPublished(ctx, s, uri, v, seq0)
		if !ok {
			return "", false
		}
		file = got
		// Other files: only errors that were not there before.
		for u, p := range published {
			if u == uri || p.seq <= seq0 {
				continue
			}
			if added := newErrors(prev[u].diags, p.diags); len(added) > 0 {
				others[u] = added
			}
		}
	}
	return format(c.spec.Name, path, errorsOnly(file), others, rel), true
}

// waitPublished waits until the server published diagnostics for uri's
// version v (or, if it does not say versions, anything for uri after seq0),
// then a little longer for the rest of the burst.
func waitPublished(ctx context.Context, s *server, uri string, v int, seq0 uint64) ([]Diagnostic, map[string]published, bool) {
	var got bool
	var quiet <-chan time.Time
	for {
		m, _, changed := s.snapshot()
		if !got {
			if p, ok := m[uri]; ok && p.seq > seq0 && (p.version == 0 || p.version >= v) {
				got = true
				quiet = time.After(settle)
			}
		}
		select {
		case <-changed:
			if got {
				quiet = time.After(settle)
			}
		case <-quiet:
			m, _, _ = s.snapshot()
			return m[uri].diags, m, true
		case <-ctx.Done():
			if got {
				m, _, _ = s.snapshot()
				return m[uri].diags, m, true
			}
			return nil, nil, false
		case <-s.done:
			return nil, nil, false
		}
	}
}

// server returns the running server for spec and root, starting it if
// needed; nil when it cannot run, with the error when this call tried to
// start it. cold is true for a server that has not answered a check yet.
// The caller must release it.
func (m *Manager) server(ctx context.Context, spec Spec, root string) (s *server, cold bool, err error) {
	k := serverKey{spec.Name, root}
	for {
		m.mu.Lock()
		if m.closed || m.failed[k] {
			m.mu.Unlock()
			return nil, false, nil
		}
		if s := m.servers[k]; s != nil && !s.exited() {
			s.busy++
			s.lastUsed = time.Now()
			cold := !s.checked
			s.checked = true
			m.mu.Unlock()
			return s, cold, nil
		}
		if ch := m.starting[k]; ch != nil {
			m.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, false, nil
			}
		}
		if m.starting == nil {
			m.starting, m.servers, m.failed = map[serverKey]chan struct{}{}, map[serverKey]*server{}, map[serverKey]bool{}
		}
		ch := make(chan struct{})
		m.starting[k] = ch
		m.mu.Unlock()

		sctx, cancel := context.WithTimeout(context.Background(), coldWait)
		tail := &tailBuffer{max: 2048}
		s, err := startServer(sctx, spec, root, tail)
		cancel()

		m.mu.Lock()
		delete(m.starting, k)
		close(ch)
		if err != nil {
			m.failed[k] = true
			m.mu.Unlock()
			if t := tail.last(); t != "" && !strings.Contains(err.Error(), t) {
				err = fmt.Errorf("%w: %s", err, t)
			}
			return nil, false, err
		}
		if m.closed {
			m.mu.Unlock()
			s.stop()
			return nil, false, nil
		}
		m.servers[k] = s
		if !m.reaper && m.Idle > 0 {
			m.reaper = true
			go m.reap()
		}
		m.mu.Unlock()
	}
}

func (m *Manager) release(s *server) {
	m.mu.Lock()
	s.busy--
	s.lastUsed = time.Now()
	m.mu.Unlock()
}

// reap stops servers unused for Idle.
func (m *Manager) reap() {
	tick := time.NewTicker(min(m.Idle/4, time.Minute))
	defer tick.Stop()
	for range tick.C {
		var stale []*server
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return
		}
		for k, s := range m.servers {
			if s.exited() || (s.busy == 0 && time.Since(s.lastUsed) >= m.Idle) {
				delete(m.servers, k)
				stale = append(stale, s)
			}
		}
		m.mu.Unlock()
		for _, s := range stale {
			s.stop()
		}
	}
}

// Running lists the servers running now, as "name (root)".
func (m *Manager) Running() []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k, s := range m.servers {
		if !s.exited() {
			out = append(out, fmt.Sprintf("%s (%s)", k.name, k.root))
		}
	}
	slices.Sort(out)
	return out
}

// Close stops every server.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.closed = true
	all := make([]*server, 0, len(m.servers))
	for _, s := range m.servers {
		all = append(all, s)
	}
	m.servers = nil
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.stop()
		}()
	}
	wg.Wait()
}

// errorsOnly keeps the errors: warnings and hints (unused variables, style)
// would bury them, and a build or linter shows those anyway.
func errorsOnly(ds []Diagnostic) []Diagnostic {
	var out []Diagnostic
	for _, d := range ds {
		if d.Severity == 1 || d.Severity == 0 { // unset means error
			out = append(out, d)
		}
	}
	return out
}

// newErrors are the errors in after that were not in before (other files
// do not move, so line and message identify one).
func newErrors(before, after []Diagnostic) []Diagnostic {
	type id struct {
		line int
		msg  string
	}
	had := map[id]bool{}
	for _, d := range before {
		had[id{d.Range.Start.Line, d.Message}] = true
	}
	var out []Diagnostic
	for _, d := range errorsOnly(after) {
		if !had[id{d.Range.Start.Line, d.Message}] {
			out = append(out, d)
		}
	}
	return out
}

// format writes the note: the edited file's errors, then new errors in
// other files.
func format(name, path string, file []Diagnostic, others map[string][]Diagnostic, rel func(string) string) string {
	if len(file) == 0 && len(others) == 0 {
		return ""
	}
	if rel == nil {
		rel = func(p string) string { return p }
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n%s reports errors after this edit:\n", name)
	write := func(p string, ds []Diagnostic, limit int) {
		slices.SortStableFunc(ds, func(a, b Diagnostic) int { return a.Range.Start.Line - b.Range.Start.Line })
		for i, d := range ds {
			if i == limit {
				fmt.Fprintf(&b, "… %d more in %s\n", len(ds)-limit, rel(p))
				break
			}
			msg := strings.ReplaceAll(strings.TrimSpace(d.Message), "\n", " ")
			fmt.Fprintf(&b, "%s:%d:%d: %s\n", rel(p), d.Range.Start.Line+1, d.Range.Start.Character+1, msg)
		}
	}
	write(path, file, maxFileErrors)
	if len(others) > 0 {
		uris := make([]string, 0, len(others))
		for u := range others {
			uris = append(uris, u)
		}
		slices.Sort(uris)
		n := 0
		for _, u := range uris {
			if n >= maxOtherErrors {
				fmt.Fprintf(&b, "… and errors in %d more files\n", len(uris)-n)
				break
			}
			write(uriPath(u), others[u], maxOtherErrors-n)
			n += min(len(others[u]), maxOtherErrors-n)
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// tailBuffer keeps the end of a server's stderr, to say why it failed.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

// last is the last non-empty line written.
func (t *tailBuffer) last() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
