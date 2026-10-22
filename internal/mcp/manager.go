package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
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
	StateClosed     = "closed"
)

// Status describes one configured server.
type Status struct {
	Name, Transport, State, Err string
	Tools                       int
}

// Manager owns the connections to all configured MCP servers.
type Manager struct {
	cwd, logDir string
	servers     []*server
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup // connect goroutines
	done        chan struct{}  // closed when every connect attempt finished
	snap        atomic.Pointer[[]tools.Tool]
	rebuildMu   sync.Mutex
	closeOnce   sync.Once
	idleAfter   time.Duration // Options.IdleAfter
	cacheDir    string
}

type toolInfo struct {
	remote   string
	desc     string
	schema   json.RawMessage
	readOnly bool // the server's readOnlyHint annotation
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
}

// Start connects to all enabled servers concurrently in the background and
// returns immediately. stdio servers run in cwd (or their own cwd) and log
// stderr to <logDir>/mcp-<name>.log (1 MB at most, then .log.1).
func Start(cfgs []ServerConfig, cwd, logDir string) *Manager {
	return StartWith(cfgs, cwd, logDir, Options{})
}

// StartWith is Start with options: with IdleAfter, local servers whose
// tools are cached start at their first call and stop when left idle.
func StartWith(cfgs []ServerConfig, cwd, logDir string, o Options) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{cwd: cwd, logDir: logDir, ctx: ctx, cancel: cancel, done: make(chan struct{}), idleAfter: o.IdleAfter, cacheDir: o.CacheDir}
	m.snap.Store(&[]tools.Tool{})
	m.servers = make([]*server, len(cfgs))
	now := time.Now()
	anyIdle := false
	for i, cfg := range cfgs {
		s := &server{m: m, cfg: cfg, transport: cfg.Type, state: StateConnecting}
		m.servers[i] = s
		if cfg.Disabled {
			s.state = StateDisabled
		} else if s.onDemand() {
			if list, ok := s.loadCache(now); ok {
				s.state, s.list = StateIdle, list
				anyIdle = true
			}
		}
	}
	if anyIdle {
		m.rebuild()
	}
	if m.idleAfter > 0 {
		go m.reapIdle()
	}
	for _, s := range m.servers {
		if s.state != StateConnecting {
			continue
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			s.connect(ctx)
		}()
	}
	go func() {
		m.wg.Wait()
		close(m.done)
	}()
	return m
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

// Wait blocks until every server has finished connecting (or failed), or
// until timeout elapses. A non-positive timeout does not wait.
func (m *Manager) Wait(timeout time.Duration) {
	if m == nil || timeout <= 0 {
		return
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-m.done:
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
		out[i] = Status{Name: s.cfg.Name, Transport: s.transport, State: s.state, Err: s.err, Tools: len(s.list)}
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
		out[i] = &mcpTool{s: owner[i], remote: ti.remote, readOnly: ti.readOnly,
			def: llm.Tool{Name: names[i], Description: ti.desc, Schema: ti.schema}}
	}
	m.snap.Store(&out)
}

func (s *server) connect(ctx context.Context) {
	c, kind, err := s.open(ctx)
	var list []toolInfo
	if err == nil {
		list, err = s.fetch(ctx, c)
	}
	s.mu.Lock()
	if kind != "" {
		s.transport = kind
	}
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
func (s *server) open(ctx context.Context) (*conn, string, error) {
	switch s.cfg.Type {
	case "stdio", "sse":
		c, err := s.dial(ctx, s.cfg.Type)
		return c, s.cfg.Type, err
	case "http":
		c, err := s.dial(ctx, "http")
		if err != nil && fallbackToSSE(err) {
			c2, err2 := s.dial(ctx, "sse")
			if err2 == nil {
				return c2, "sse", nil
			}
			return nil, "http", fmt.Errorf("%w (legacy SSE fallback: %v)", err, err2)
		}
		return c, "http", err
	}
	return nil, "", fmt.Errorf("unknown transport type %q", s.cfg.Type)
}

func (s *server) dial(ctx context.Context, kind string) (*conn, error) {
	c := newConn()
	c.serve = s.serve
	c.notify = func(method string, _ json.RawMessage) {
		if method == "notifications/tools/list_changed" {
			s.refresh()
		}
	}
	c.onFail = func(err error) { s.lost(c, err) }

	ictx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
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
		} else if _, err := dialSSE(ictx, c, s.cfg); err != nil {
			return nil, timeoutErr(ctx, err)
		}
	}
	s.dirty.Store(false)
	if _, err := c.initialize(ictx); err != nil {
		c.close()
		return nil, timeoutErr(ctx, err)
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
	out := make([]toolInfo, 0, len(rts))
	for i := range rts {
		if rts[i].Name == "" {
			continue
		}
		out = append(out, toolInfo{remote: rts[i].Name, desc: description(&rts[i]), schema: normalizeSchema(rts[i].InputSchema),
			readOnly: rts[i].Annotations.ReadOnlyHint})
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

// serve answers server->client requests.
func (s *server) serve(method string, _ json.RawMessage) (any, *rpcError) {
	switch method {
	case "ping":
		return struct{}{}, nil
	case "roots/list":
		u := url.URL{Scheme: "file", Path: s.m.cwd}
		return map[string]any{"roots": []map[string]string{{"uri": u.String(), "name": filepath.Base(s.m.cwd)}}}, nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found: " + method}
}
