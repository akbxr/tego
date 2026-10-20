package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// A local (stdio) server runs only while it is used. Its tool list is kept
// on disk, so a session whose list is cached starts it at the first call
// instead of at startup, and one left idle is stopped, its tools still
// listed (the same tools: the prompt cache keeps), to start again at the
// next call. A server that dies does the same. KeepAlive opts out.

// StateIdle is a server not running, its tools listed from before.
const StateIdle = "idle"

// Options tune a Manager.
type Options struct {
	// IdleAfter stops a local server unused this long and lets a server
	// with a cached tool list start at its first call; 0: servers run for
	// the whole session.
	IdleAfter time.Duration
	// CacheDir keeps the servers' tool lists ("": none kept).
	CacheDir string
}

// toolCacheMaxAge is how long a cached tool list lets a server start on
// demand; an older one means connecting at startup to list it again.
const toolCacheMaxAge = 7 * 24 * time.Hour

// onDemand says whether s runs only while used.
func (s *server) onDemand() bool {
	return s.m.idleAfter > 0 && s.cfg.Type == "stdio" && !s.cfg.KeepAlive
}

type cachedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
	ReadOnly    bool            `json:"read_only,omitempty"`
}

type toolCache struct {
	Saved int64        `json:"saved"` // unix seconds
	Tools []cachedTool `json:"tools"`
}

var unsafeFile = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// cachePath is where s's tool list is kept: one file per server and
// command line, environment and folder, any of which may change the tools.
func (s *server) cachePath() string {
	if s.m.cacheDir == "" {
		return ""
	}
	h := sha256.New()
	cwd := s.cfg.Cwd
	if cwd == "" {
		cwd = s.m.cwd
	}
	fmt.Fprintf(h, "%q %q %q\n", s.cfg.Command, s.cfg.Args, cwd)
	keys := make([]string, 0, len(s.cfg.Env))
	for k := range s.cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "%q=%q\n", k, s.cfg.Env[k])
	}
	return filepath.Join(s.m.cacheDir, unsafeFile.ReplaceAllString(s.cfg.Name, "_")+"-"+hex.EncodeToString(h.Sum(nil)[:8])+".json")
}

func (s *server) loadCache(now time.Time) ([]toolInfo, bool) {
	path := s.cachePath()
	if path == "" {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var c toolCache
	if json.Unmarshal(data, &c) != nil || now.Sub(time.Unix(c.Saved, 0)) > toolCacheMaxAge {
		return nil, false
	}
	list := make([]toolInfo, len(c.Tools))
	for i, t := range c.Tools {
		list[i] = toolInfo{remote: t.Name, desc: t.Description, schema: t.Schema, readOnly: t.ReadOnly}
	}
	return list, true
}

func (s *server) saveCache(list []toolInfo) {
	path := s.cachePath()
	if path == "" || s.cfg.Type != "stdio" {
		return
	}
	c := toolCache{Saved: time.Now().Unix(), Tools: make([]cachedTool, len(list))}
	for i, t := range list {
		c.Tools[i] = cachedTool{Name: t.remote, Description: t.desc, Schema: t.schema, ReadOnly: t.readOnly}
	}
	data, err := json.Marshal(c)
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// acquire is the connection for a call, starting an idle server first;
// release must follow once the call is done.
func (s *server) acquire() (*conn, error) {
	s.mu.Lock()
	if s.conn == nil && s.state == StateIdle && !s.closed {
		s.mu.Unlock()
		if err := s.wake(); err != nil {
			return nil, err
		}
		s.mu.Lock()
	}
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil, s.stateErr()
	}
	s.inflight++
	s.lastUsed = time.Now()
	return s.conn, nil
}

func (s *server) release() {
	s.mu.Lock()
	s.inflight--
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

// stateErr says why s has no connection; s.mu is held.
func (s *server) stateErr() error {
	if s.err != "" {
		return fmt.Errorf("mcp server %q is %s: %s", s.cfg.Name, s.state, s.err)
	}
	return fmt.Errorf("mcp server %q is %s", s.cfg.Name, s.state)
}

// wake starts an idle server; its tools stay listed meanwhile, and are
// listed again once it runs, as they may have changed.
func (s *server) wake() error {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	s.mu.Lock()
	if s.conn != nil || s.state != StateIdle || s.closed {
		s.mu.Unlock()
		return nil // started by another call meanwhile, or closed
	}
	s.mu.Unlock()
	c, err := s.dial(s.m.ctx, "stdio") // the session's context: the server outlives this call
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.err = "starting it: " + err.Error() // still idle: the next call tries again
		return fmt.Errorf("mcp server %q: starting it: %w", s.cfg.Name, err)
	}
	if s.closed {
		go c.close()
		return nil
	}
	s.state, s.err, s.conn, s.lastUsed = StateConnected, "", c, time.Now()
	go s.refetch()
	return nil
}

// sleep stops s if it has run idle for d; it stays listed.
func (s *server) sleep(d time.Duration, now time.Time) bool {
	s.mu.Lock()
	c := s.conn
	if c == nil || s.closed || s.state != StateConnected || s.inflight > 0 || now.Sub(s.lastUsed) < d {
		s.mu.Unlock()
		return false
	}
	s.conn, s.state = nil, StateIdle // lost ignores the close: s.conn is no longer c
	s.mu.Unlock()
	c.close()
	return true
}

// reapIdle stops the servers left idle, until the manager closes.
func (m *Manager) reapIdle() {
	every := min(max(m.idleAfter/4, 10*time.Millisecond), time.Minute)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-t.C:
			for _, s := range m.servers {
				if s.onDemand() {
					s.sleep(m.idleAfter, now)
				}
			}
		}
	}
}
