package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// initTimeout bounds the handshake and each tools/list refresh; generous
// because bunx/npx may download the server first.
const initTimeout = 30 * time.Second

// Server states reported by Status.
const (
	StateConnecting = "connecting"
	StateConnected  = "connected"
	StateFailed     = "failed"
	StateDisabled   = "disabled"
	StateUntrusted  = "untrusted" // a project's, its config not trusted (yet, or since it changed)
	StateClosed     = "closed"
)

// Status describes one configured server.
type Status struct {
	Name, Transport, State, Err string
	Tools                       int
	Project                     bool   // from the project's config: runs once trusted
	Source                      string // the config file it came from
	Protocol                    string // the MCP version it speaks ("" before it ran)
	Tasks                       bool   // it may run calls as tasks
	Instructions                int    // the length of its instructions to the model
}

// Manager owns the connections to all configured MCP servers.
type Manager struct {
	cwd, logDir string
	servers     []*server
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup // connect goroutines
	snap        atomic.Pointer[[]tools.Tool]
	rebuildMu   sync.Mutex
	closeOnce   sync.Once
	idleAfter   time.Duration // Options.IdleAfter
	cacheDir    string
	home        string     // Options.Home: where the trust is kept
	trustMu     sync.Mutex // one Trust at a time
	elicit      Elicitor   // Options.Elicit

	launchMu   sync.Mutex
	connecting int           // connect attempts not finished
	done       chan struct{} // closed when connecting drops to 0, new when it rises again; nil: none launched
	stopped    bool          // Close began: launch no more
}

type toolInfo struct {
	remote   string
	desc     string
	schema   json.RawMessage
	readOnly bool          // the server's readOnlyHint annotation
	headers  []paramHeader // arguments its HTTP requests mirror in headers (modern)
}

type server struct {
	m          *Manager
	cfg        ServerConfig
	refreshing atomic.Bool
	dirty      atomic.Bool // tools/list_changed seen since the last fetch

	wakeMu sync.Mutex // one start of an idle server at a time

	mu        sync.Mutex
	state     string
	err       string
	transport string
	closed    bool
	conn      *conn
	list      []toolInfo
	inflight  int       // calls running
	lastUsed  time.Time // when the latest call ended, or it started

	// What its handshake said, kept (and cached) while it doesn't run.
	version      string
	instructions string
	tasks        bool
	// noProbe: it ignored server/discover or died at it; initialize it
	// right away.
	noProbe bool
}

// maxInstructions caps a server's instructions to the model.
const maxInstructions = 4096

// learned keeps what h said of s; s.mu is held.
func (s *server) learned(h *handshake) {
	if h == nil {
		return
	}
	s.version, s.tasks = h.version, h.tasks
	s.instructions = truncate(strings.TrimSpace(h.instructions), maxInstructions)
}

// Start connects to all enabled servers concurrently in the background and
// returns immediately. stdio servers run in cwd (or their own cwd) and log
// stderr to <logDir>/mcp-<name>.log (1 MB at most, then .log.1).
func Start(cfgs []ServerConfig, cwd, logDir string) *Manager {
	return StartWith(cfgs, cwd, logDir, Options{})
}

// StartWith is Start with options: with IdleAfter, local servers whose
// tools are cached start at their first call and stop when left idle. A
// project's servers (ServerConfig.Project) neither start nor list cached
// tools until their config file is trusted in Home (see Trust).
func StartWith(cfgs []ServerConfig, cwd, logDir string, o Options) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{cwd: cwd, logDir: logDir, ctx: ctx, cancel: cancel, idleAfter: o.IdleAfter, cacheDir: o.CacheDir, home: o.Home, elicit: o.Elicit}
	m.snap.Store(&[]tools.Tool{})
	m.servers = make([]*server, len(cfgs))
	now := time.Now()
	anyIdle := false
	trusted := loadTrust(o.Home)
	for i, cfg := range cfgs {
		if cfg.Project && cfg.digest == "" { // not read by LoadConfig
			cfg.digest = fileDigest(cfg.Source)
		}
		s := &server{m: m, cfg: cfg, transport: cfg.Type}
		m.servers[i] = s
		switch {
		case cfg.Disabled:
			s.state = StateDisabled
		case cfg.Project && !trusted.has(cfg.Source, cfg.digest):
			// Neither run nor its cached tools listed until trusted.
			s.state, s.err = StateUntrusted, trusted.why(cfg.Source)
		default:
			anyIdle = s.ready(now) || anyIdle
		}
	}
	if anyIdle {
		m.rebuild()
	}
	if m.idleAfter > 0 {
		go m.reapIdle()
	}
	for _, s := range m.servers {
		if s.state == StateConnecting {
			m.launch(s)
		}
	}
	return m
}

// ready readies s to run: idle, its tools listed, when it starts on demand
// and its tool list is cached (it says so); else connecting, for launch.
// s.mu is held, or s not shared yet.
func (s *server) ready(now time.Time) bool {
	s.state, s.err = StateConnecting, ""
	if s.onDemand() {
		if list, ok := s.loadCache(now); ok {
			s.state, s.list = StateIdle, list
			return true
		}
	}
	return false
}

// launch connects s in the background; Wait waits for it.
func (m *Manager) launch(s *server) {
	m.launchMu.Lock()
	defer m.launchMu.Unlock()
	if m.stopped {
		return
	}
	if m.connecting == 0 {
		m.done = make(chan struct{})
	}
	m.connecting++
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		s.connect(m.ctx)
		m.launchMu.Lock()
		if m.connecting--; m.connecting == 0 {
			close(m.done)
		}
		m.launchMu.Unlock()
	}()
}

// Tools returns a snapshot of the tools of all connected servers. The slice
// is shared and must not be modified (appending is safe).
func (m *Manager) Tools() []tools.Tool {
	if m == nil {
		return nil
	}
	p := *m.snap.Load()
	return p[:len(p):len(p)]
}

// Wait blocks until every server has finished connecting (or failed), those
// Trust started too, or until timeout elapses. A non-positive timeout does
// not wait.
func (m *Manager) Wait(timeout time.Duration) {
	if m == nil || timeout <= 0 {
		return
	}
	m.launchMu.Lock()
	done := m.done
	m.launchMu.Unlock()
	if done == nil {
		return
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

// Status reports every configured server in config order.
func (m *Manager) Status() []Status {
	if m == nil {
		return nil
	}
	out := make([]Status, len(m.servers))
	for i, s := range m.servers {
		s.mu.Lock()
		out[i] = Status{Name: s.cfg.Name, Transport: s.transport, State: s.state, Err: s.err, Tools: len(s.list),
			Project: s.cfg.Project, Source: s.cfg.Source, Protocol: s.version, Tasks: s.tasks, Instructions: len(s.instructions)}
		s.mu.Unlock()
	}
	return out
}

// Close shuts down all servers and waits for them to exit.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		m.cancel()
		m.launchMu.Lock()
		m.stopped = true // no wg.Add after the wg.Wait below
		m.launchMu.Unlock()
		var wg sync.WaitGroup
		for _, s := range m.servers {
			s.mu.Lock()
			c := s.conn
			s.closed, s.conn, s.list = true, nil, nil
			if s.state == StateConnected || s.state == StateConnecting || s.state == StateIdle {
				s.state = StateClosed
			}
			s.mu.Unlock()
			if c != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					c.close()
				}()
			}
		}
		wg.Wait()
		m.wg.Wait()
		m.snap.Store(&[]tools.Tool{})
	})
}

// rebuild recomputes the tool snapshot from every server's tool list.
func (m *Manager) rebuild() {
	m.rebuildMu.Lock()
	defer m.rebuildMu.Unlock()
	var (
		keys  []toolKey
		infos []toolInfo
		owner []*server
	)
	for _, s := range m.servers {
		s.mu.Lock()
		for _, ti := range s.list {
			keys = append(keys, toolKey{s.cfg.Name, ti.remote})
			infos = append(infos, ti)
			owner = append(owner, s)
		}
		s.mu.Unlock()
	}
	names := uniqueNames(keys)
	out := make([]tools.Tool, len(keys))
	for i, ti := range infos {
		out[i] = &mcpTool{s: owner[i], remote: ti.remote, readOnly: ti.readOnly, headers: ti.headers,
			def: llm.Tool{Name: names[i], Description: ti.desc, Schema: ti.schema}}
	}
	m.snap.Store(&out)
}

func (s *server) connect(ctx context.Context) {
	c, h, kind, err := s.open(ctx)
	var list []toolInfo
	if err == nil {
		list, err = s.fetch(ctx, c)
	}
	s.mu.Lock()
	if kind != "" {
		s.transport = kind
	}
	s.learned(h)
	if s.closed {
		s.mu.Unlock()
		if c != nil {
			c.close()
		}
		return
	}
	if err != nil {
		s.state, s.err = StateFailed, err.Error()
		s.mu.Unlock()
		if c != nil {
			c.close()
		}
		return
	}
	s.state, s.conn, s.list, s.lastUsed = StateConnected, c, list, time.Now()
	s.mu.Unlock()
	s.saveCache(list)
	s.m.rebuild()
	if s.dirty.Load() {
		s.refresh()
	}
}

// open establishes the transport and performs the handshake, falling back
// from streamable HTTP to legacy SSE when the server rejects the POST.
func (s *server) open(ctx context.Context) (*conn, *handshake, string, error) {
	switch s.cfg.Type {
	case "stdio", "sse":
		c, h, err := s.dial(ctx, s.cfg.Type)
		return c, h, s.cfg.Type, err
	case "http":
		c, h, err := s.dial(ctx, "http")
		if err != nil && fallbackToSSE(err) {
			c2, h2, err2 := s.dial(ctx, "sse")
			if err2 == nil {
				return c2, h2, "sse", nil
			}
			return nil, nil, "http", fmt.Errorf("%w (legacy SSE fallback: %v)", err, err2)
		}
		return c, h, "http", err
	}
	return nil, nil, "", fmt.Errorf("unknown transport type %q", s.cfg.Type)
}

// dial connects and performs the handshake: server/discover first (MCP
// 2026-07-28), initialize for a server that doesn't take it. A local server
// that died at discover is started again and initialized.
func (s *server) dial(ctx context.Context, kind string) (*conn, *handshake, error) {
	ictx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	s.mu.Lock()
	probe := kind != "sse" && !s.noProbe
	s.mu.Unlock()
	for {
		c, err := s.start(ictx, kind)
		if err != nil {
			return nil, nil, timeoutErr(ctx, err)
		}
		s.dirty.Store(false)
		began := time.Now()
		h, ignored, err := c.handshake(ictx, probe)
		if errors.Is(err, errNoProbe) && kind == "stdio" {
			c.close()
			s.mu.Lock()
			s.noProbe = true
			s.mu.Unlock()
			probe = false
			continue
		}
		if err != nil {
			c.close()
			return nil, nil, timeoutErr(ctx, err)
		}
		if ignored && time.Since(began) < discoverWait+2*time.Second {
			// It answered initialize at once: it was up, and let discover
			// go unanswered. Not asked again.
			s.mu.Lock()
			s.noProbe = true
			s.mu.Unlock()
		}
		c.tasks.Store(h.tasks)
		if c.modern.Load() && h.listChanged {
			go c.listen()
		}
		return c, h, nil
	}
}

// start opens a connection of kind to s, without a handshake.
func (s *server) start(ctx context.Context, kind string) (*conn, error) {
	c := newConn()
	c.serve = s.serve
	c.notify = func(method string, _ json.RawMessage) {
		if method == "notifications/tools/list_changed" {
			s.refresh()
		}
	}
	c.onFail = func(err error) { s.lost(c, err) }
	c.elicit = s.m.elicit != nil
	switch kind {
	case "stdio":
		if _, err := startStdio(c, s.cfg, s.m.cwd, s.m.logDir); err != nil {
			return nil, err
		}
	case "http", "sse":
		if s.cfg.URL == "" {
			return nil, errors.New("no url configured")
		}
		if kind == "http" {
			newHTTPTransport(c, s.cfg)
		} else if _, err := dialSSE(ctx, c, s.cfg); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown transport type %q", kind)
	}
	return c, nil
}

func timeoutErr(parent context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil {
		return fmt.Errorf("no response to initialize within %s", initTimeout)
	}
	return err
}

func (s *server) fetch(ctx context.Context, c *conn) ([]toolInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	rts, err := c.listTools(ctx)
	if err != nil {
		var re *rpcError
		if errors.As(err, &re) && re.Code == -32601 {
			return nil, nil // server has no tools
		}
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	mirror := c.modern.Load() && c.t.kind() == "http"
	out := make([]toolInfo, 0, len(rts))
	for i := range rts {
		if rts[i].Name == "" {
			continue
		}
		ti := toolInfo{remote: rts[i].Name, desc: description(&rts[i]), schema: normalizeSchema(rts[i].InputSchema),
			readOnly: rts[i].Annotations.ReadOnlyHint}
		if mirror {
			hs, ok := paramHeaders(ti.schema)
			if !ok {
				continue // a header it asks for can't be sent: the spec leaves the tool out
			}
			ti.headers = hs
		}
		out = append(out, ti)
	}
	return out, nil
}

// refresh refetches the tool list after notifications/tools/list_changed.
// Concurrent notifications coalesce into at most one extra fetch.
func (s *server) refresh() {
	s.dirty.Store(true)
	s.mu.Lock()
	connected := s.state == StateConnected
	s.mu.Unlock()
	if !connected || !s.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		for {
			for s.dirty.Swap(false) {
				s.refetch()
			}
			s.refreshing.Store(false)
			if !s.dirty.Load() || !s.refreshing.CompareAndSwap(false, true) {
				return
			}
		}
	}()
}

func (s *server) refetch() {
	c, err := s.currentConn()
	if err != nil {
		return
	}
	list, err := s.fetch(s.m.ctx, c)
	if err != nil {
		return // keep the previous list
	}
	s.mu.Lock()
	if s.conn != c {
		s.mu.Unlock()
		return
	}
	s.list = list
	s.mu.Unlock()
	s.saveCache(list)
	s.m.rebuild()
}

// lost handles a connection dying underneath a connected server.
func (s *server) lost(c *conn, err error) {
	s.mu.Lock()
	if s.conn != c {
		s.mu.Unlock()
		return
	}
	if s.onDemand() && !s.closed {
		// Started again at the next call, its tools listed meanwhile.
		s.state, s.err, s.conn = StateIdle, "exited: "+err.Error(), nil
		s.mu.Unlock()
		return
	}
	s.state, s.err, s.conn, s.list = StateFailed, err.Error(), nil, nil
	s.mu.Unlock()
	s.m.rebuild()
}

func (s *server) currentConn() (*conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		return s.conn, nil
	}
	return nil, s.stateErr()
}

// serve answers server->client requests (up to MCP 2025-11-25; a modern
// server asks through input_required instead).
func (s *server) serve(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "ping":
		return struct{}{}, nil
	case "roots/list":
		return s.roots(), nil
	case "elicitation/create":
		if s.m.elicit == nil {
			break
		}
		r, err := s.elicit(s.m.ctx, params)
		if err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		return r, nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found: " + method}
}

// roots is roots/list's answer: the session's folder.
func (s *server) roots() map[string]any {
	u := url.URL{Scheme: "file", Path: s.m.cwd}
	return map[string]any{"roots": []map[string]string{{"uri": u.String(), "name": filepath.Base(s.m.cwd)}}}
}
