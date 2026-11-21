package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/trustkey"
)

func projectTools(m *Manager) []string {
	var out []string
	for name := range toolMap(m) {
		if strings.HasPrefix(name, "mcp__proj__") {
			out = append(out, name)
		}
	}
	return out
}

// A project's server neither runs nor lists its cached tools until its
// config file is trusted; Trust starts it and keeps the trust for the next
// sessions, and a change to the file lapses it.
func TestProjectServerTrust(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cwd := filepath.Join(dir, "project")
	proj := filepath.Join(cwd, ".mcp.json")
	marker := filepath.Join(dir, "started") // the server writes its pid here when it runs
	command, _ := json.Marshal(os.Args[0])
	writeProject := func(more string) {
		env, _ := json.Marshal(map[string]string{"TEGO_MCP_FAKE": "1", "TEGO_MCP_STARTS": marker})
		writeFile(t, proj, fmt.Sprintf(`{"mcpServers": {"proj": {"command": %s, "env": %s}%s}}`, command, env, more))
	}
	writeProject("")
	// The user's own server needs no trust.
	writeFile(t, filepath.Join(home, "mcp.json"), fmt.Sprintf(`{"mcpServers": {"user": {"command": %s, "env": {"TEGO_MCP_FAKE": "1"}}}}`, command))
	load := func() []ServerConfig {
		t.Helper()
		cfgs, err := LoadConfig(home, cwd)
		if err != nil {
			t.Fatal(err)
		}
		return cfgs
	}
	o := Options{Home: home, CacheDir: filepath.Join(dir, "cache")}

	// Untrusted: listed, not run.
	m := StartWith(load(), cwd, "", o)
	m.Wait(20 * time.Second) // would wait for the project's server too, had it started
	st := m.Status()
	if s := st[0]; s.Name != "user" || s.State != StateConnected || s.Project || s.Source != filepath.Join(home, "mcp.json") {
		t.Fatalf("user server: %+v", s)
	}
	if s := st[1]; s.Name != "proj" || s.State != StateUntrusted || !s.Project || s.Source != proj || s.Tools != 0 ||
		s.Err != "project config not trusted yet" {
		t.Fatalf("untrusted server: %+v", s)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the untrusted server ran: %v", err)
	}
	if len(toolMap(m)) != 6 || len(projectTools(m)) != 0 {
		t.Fatalf("tools while untrusted: %d, project's %v", len(toolMap(m)), projectTools(m))
	}

	// Trust starts it, and Wait waits for it to connect.
	if names := m.Trust(); strings.Join(names, ",") != "proj" {
		t.Fatalf("trusted %v", names)
	}
	m.Wait(20 * time.Second)
	if s := state(m, 1); s.State != StateConnected || s.Tools != 6 || s.Err != "" {
		t.Fatalf("after trust: %+v", s)
	}
	if n := len(starts(t, marker)); n != 1 || len(projectTools(m)) != 6 {
		t.Fatalf("after trust: %d starts, project's tools %v", n, projectTools(m))
	}
	if names := m.Trust(); names != nil {
		t.Fatalf("trusted again: %v", names)
	}
	fi, err := os.Stat(trustFile(home))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("trust file: %v %v", fi, err)
	}
	var kept map[string]string
	data, _ := os.ReadFile(trustFile(home))
	if json.Unmarshal(data, &kept) != nil || len(kept) != 1 || kept[trustkey.Key(proj)] != fileDigest(proj) {
		t.Fatalf("trust file: %s", data)
	}
	if _, err := os.Stat(trustFile(home) + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file left: %v", err)
	}
	m.Close()

	// The next session trusts and starts it by itself.
	m = StartWith(load(), cwd, "", o)
	m.Wait(20 * time.Second)
	if s := state(m, 1); s.State != StateConnected || s.Tools != 6 || len(starts(t, marker)) != 2 {
		t.Fatalf("trusted session: %+v, %d starts", s, len(starts(t, marker)))
	}
	m.Close()

	// One starting servers on demand lists its tools, cached above, and
	// starts it at its first call.
	idle := o
	idle.IdleAfter = time.Hour
	m = StartWith(load(), cwd, "", idle)
	if s := state(m, 1); s.State != StateIdle || s.Tools != 6 || len(projectTools(m)) != 6 {
		t.Fatalf("trusted on-demand session: %+v", s)
	}
	m.Close()

	// A change to the file lapses the trust: untrusted again, its cached
	// tools (the same command: the same cache) not listed.
	writeProject(`, "other": {"command": "x", "disabled": true}`)
	m = StartWith(load(), cwd, "", idle)
	m.Wait(20 * time.Second)
	if s := state(m, 1); s.State != StateUntrusted || s.Tools != 0 || s.Err != "project config changed since it was trusted" {
		t.Fatalf("after the change: %+v", s)
	}
	if len(projectTools(m)) != 0 || len(starts(t, marker)) != 2 {
		t.Fatalf("after the change: project's tools %v, %d starts", projectTools(m), len(starts(t, marker)))
	}
	// Trusted again, it lists the cached tools and starts at its first call.
	if names := m.Trust(); strings.Join(names, ",") != "proj" {
		t.Fatalf("trusted %v", names)
	}
	m.Wait(20 * time.Second)
	if s := state(m, 1); s.State != StateIdle || s.Tools != 6 || len(starts(t, marker)) != 2 {
		t.Fatalf("trusted on demand: %+v, %d starts", s, len(starts(t, marker)))
	}
	if got := call(t, toolMap(m)["mcp__proj__echo"], `{"text":"hi"}`); got != "hi\n\nsecond" || len(starts(t, marker)) != 3 {
		t.Fatalf("echo: %q, %d starts", got, len(starts(t, marker)))
	}
	m.Close()
	data, _ = os.ReadFile(trustFile(home))
	if json.Unmarshal(data, &kept) != nil || kept[trustkey.Key(proj)] != fileDigest(proj) {
		t.Fatalf("trust file after the change: %s", data)
	}

	// Without a home nothing is trusted; Trust starts it for the session
	// alone, and nothing is trusted after Close.
	m = StartWith(load(), cwd, "", Options{})
	if s := state(m, 1); s.State != StateUntrusted {
		t.Fatalf("no home: %+v", s)
	}
	if names := m.Trust(); strings.Join(names, ",") != "proj" {
		t.Fatalf("trusted %v", names)
	}
	m.Wait(20 * time.Second)
	if s := state(m, 1); s.State != StateConnected || len(starts(t, marker)) != 4 {
		t.Fatalf("trusted without a home: %+v", s)
	}
	m.Close()
	if _, err := os.Stat(trustFile("")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trust kept without a home: %v", err)
	}
	m = StartWith(load(), cwd, "", Options{})
	m.Close()
	if names := m.Trust(); names != nil || state(m, 1).State != StateUntrusted {
		t.Fatalf("trusted after close: %v %+v", names, state(m, 1))
	}
}

// tego trust trusts the project's servers without a session, and keeps
// what another session trusted meanwhile.
func TestTrustProject(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cwd := filepath.Join(dir, "project")
	proj := filepath.Join(cwd, ".tego", "mcp.json")
	command, _ := json.Marshal(os.Args[0])
	writeFile(t, proj, fmt.Sprintf(`{"mcpServers": {"proj": {"command": %s, "env": {"TEGO_MCP_FAKE": "1"}}, "off": {"command": "x", "disabled": true}}}`, command))
	writeFile(t, filepath.Join(home, "mcp.json"), `{"mcpServers": {"user": {"command": "x"}}}`)
	if err := (trust{"/elsewhere/.mcp.json": "abc"}).save(home); err != nil {
		t.Fatal(err)
	}
	stale := loadTrust(home) // read before another session trusts more
	if names, err := TrustProject(home, cwd); err != nil || strings.Join(names, ",") != "proj" {
		t.Fatalf("trusted %v, %v", names, err)
	}
	stale.add("/third/.mcp.json", "def")
	if err := stale.save(home); err != nil {
		t.Fatal(err)
	}
	kept := loadTrust(home)
	if len(kept) != 3 || kept[trustkey.Key(proj)] != fileDigest(proj) {
		t.Fatalf("trust file: %v", kept)
	}
	cfgs, _ := LoadConfig(home, cwd)
	var project []ServerConfig
	for _, c := range cfgs {
		if c.Project {
			project = append(project, c)
		}
	}
	m := StartWith(project, cwd, "", Options{Home: home})
	defer m.Close()
	m.Wait(20 * time.Second)
	if s := state(m, 0); s.State != StateConnected {
		t.Fatalf("after tego trust: %+v", s)
	}
	if names, err := TrustProject(home, t.TempDir()); names != nil || err != nil {
		t.Fatalf("a folder without project servers: %v %v", names, err)
	}
}

// Trust and Close at once: a server is started before Close waits, or not
// at all.
func TestTrustWhileClosing(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, ".mcp.json")
	writeFile(t, src, `{}`)
	var cfgs []ServerConfig
	for i := range 4 {
		cfgs = append(cfgs, ServerConfig{Name: fmt.Sprint("p", i), Type: "stdio", Command: filepath.Join(dir, "missing"),
			Project: true, Source: src})
	}
	for range 20 {
		m := StartWith(cfgs, dir, "", Options{Home: dir})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); m.Trust() }()
		go func() { defer wg.Done(); m.Close() }()
		wg.Wait()
		m.Wait(time.Second)
		for _, s := range m.Status() {
			if s.State == StateConnecting || s.State == StateConnected {
				t.Fatalf("after close: %+v", s)
			}
		}
	}
}
