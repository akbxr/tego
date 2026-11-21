package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/akbxr/tego/internal/trustkey"
)

// A project's MCP servers run commands from whoever wrote the repository,
// so they start only once trusted: the trust is for the config file and
// its content, and lapses when that changes. A file is known by its
// trustkey.Key, so trusting a project covers its worktrees.

type trust map[string]string // config file's key -> sha256 of its content

func trustFile(home string) string { return filepath.Join(home, "trusted-mcp.json") }

// loadTrust reads home's trusted configs; none without a home.
func loadTrust(home string) trust {
	t := trust{}
	if home == "" {
		return t
	}
	if data, err := os.ReadFile(trustFile(home)); err == nil {
		_ = json.Unmarshal(data, &t)
	}
	return t
}

func hexSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// fileDigest is the sha256 of path's content, "" if it can't be read.
func fileDigest(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return hexSum(data)
}

// has says whether the config file path, with this content, is trusted.
func (t trust) has(path, digest string) bool {
	return path != "" && digest != "" && t[trustkey.Key(path)] == digest
}

func (t trust) add(path, digest string) {
	if path != "" && digest != "" {
		t[trustkey.Key(path)] = digest
	}
}

// why says why the config file path is not trusted.
func (t trust) why(path string) string {
	if _, ok := t[trustkey.Key(path)]; ok {
		return "project config changed since it was trusted"
	}
	return "project config not trusted yet"
}

// TrustProject trusts the MCP servers of cwd's project config files as
// they are now, without starting them (tego trust); it returns their
// names.
func TrustProject(home, cwd string) ([]string, error) {
	cfgs, err := LoadConfig(home, cwd) // with an error, the files that could be read
	t := loadTrust(home)
	var names []string
	for _, c := range cfgs {
		if c.Project && !c.Disabled {
			t.add(c.Source, c.digest)
			names = append(names, c.Name)
		}
	}
	if len(names) == 0 {
		return nil, err
	}
	return names, errors.Join(err, t.save(home))
}

// Trust trusts the untrusted project servers, their config files as they
// were read, and starts them; it returns their names. Wait waits for them
// to connect. The trust is kept in <home>/trusted-mcp.json (for this
// session only when that can't be written) and lapses when a file changes.
func (m *Manager) Trust() []string {
	if m == nil {
		return nil
	}
	m.trustMu.Lock()
	defer m.trustMu.Unlock()
	t := loadTrust(m.home)
	var (
		names   []string
		start   []*server
		anyIdle bool
		now     = time.Now()
	)
	for _, s := range m.servers {
		s.mu.Lock()
		if s.state != StateUntrusted || s.closed {
			s.mu.Unlock()
			continue
		}
		t.add(s.cfg.Source, s.cfg.digest)
		names = append(names, s.cfg.Name)
		if s.ready(now) {
			anyIdle = true
		} else {
			start = append(start, s)
		}
		s.mu.Unlock()
	}
	if len(names) == 0 {
		return nil
	}
	if m.home != "" {
		_ = t.save(m.home)
	}
	if anyIdle {
		m.rebuild()
	}
	for _, s := range start {
		m.launch(s)
	}
	return names
}

func (t trust) save(home string) error {
	if home == "" {
		return errors.New("no home to keep the trust in")
	}
	// Another session may have trusted something since t was read: keep it.
	merged := loadTrust(home)
	for k, v := range t {
		merged[k] = v
	}
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	tmp := trustFile(home) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, trustFile(home))
}
