package mcp

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfig(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("MCP_TOKEN", "tok")
	t.Setenv("MCP_EMPTY", "")
	home := filepath.Join(userHome, ".tego")
	cwd := t.TempDir()

	// Other agents' config folders are not read.
	writeFile(t, filepath.Join(userHome, ".omp", "agent", "mcp.json"), `{"mcpServers": {"foreign": {"command": "x"}}}`)
	writeFile(t, filepath.Join(cwd, ".omp", "mcp.json"), `{"mcpServers": {"foreign2": {"command": "x"}}}`)
	writeFile(t, filepath.Join(home, "mcp.json"), `{"mcpServers": {
		"zeta": {"command": "home-zeta"},
		"shared": {"command": "from-tego-home", "args": ["--token=${MCP_TOKEN}", "${MISSING:-def}", "${MCP_EMPTY:-fallback}", "$MCP_TOKEN"]},
		"gone": {"command": "x", "enabled": false}
	}}`)
	writeFile(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers": {
		"remote": {"url": "https://example.com/${MISSING}mcp", "headers": {"Authorization": "Bearer ${MCP_TOKEN}"}},
		"legacy": {"type": "sse", "url": "https://example.com/sse", "disabled": true}
	}}`)
	writeFile(t, filepath.Join(cwd, ".tego", "mcp.json"), `{"mcpServers": {
		"zeta": {"command": "project-zeta"},
		"local": {"command": "srv", "env": {"K": "${MCP_TOKEN}"}, "cwd": "sub", "type": "streamable-http"}
	}}`)

	cfgs, err := LoadConfig(home, cwd)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ServerConfig{}
	var order []string
	for _, c := range cfgs {
		by[c.Name] = c
		order = append(order, c.Name)
	}
	if got := strings.Join(order, ","); got != "zeta,shared,gone,remote,legacy,local" {
		t.Fatalf("order: %s", got)
	}
	if c := by["zeta"]; c.Command != "project-zeta" || c.Source != filepath.Join(cwd, ".tego", "mcp.json") || c.Type != "stdio" {
		t.Fatalf("zeta: %+v", c)
	}
	if c := by["shared"]; c.Command != "from-tego-home" ||
		strings.Join(c.Args, " ") != "--token=tok def fallback $MCP_TOKEN" {
		t.Fatalf("shared: %+v", c)
	}
	if !by["gone"].Disabled || !by["legacy"].Disabled || by["legacy"].Type != "sse" {
		t.Fatalf("disabled: %+v %+v", by["gone"], by["legacy"])
	}
	if c := by["remote"]; c.Type != "http" || c.URL != "https://example.com/mcp" || c.Headers["Authorization"] != "Bearer tok" {
		t.Fatalf("remote: %+v", c)
	}
	if c := by["local"]; c.Env["K"] != "tok" || c.Cwd != "sub" || c.Type != "http" {
		t.Fatalf("local: %+v", c)
	}

	// A malformed file names itself; the other files still load.
	bad := filepath.Join(cwd, ".tego", "mcp.json")
	writeFile(t, bad, `{"mcpServers": {`)
	cfgs, err = LoadConfig(home, cwd)
	if err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("want error naming %s, got %v", bad, err)
	}
	if len(cfgs) != 5 {
		t.Fatalf("partial load: %d servers", len(cfgs))
	}
}

func TestToolNames(t *testing.T) {
	valid := regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	long := strings.Repeat("very_long_tool_name_", 5)
	keys := []toolKey{
		{"my server", "do.thing"},
		{"my_server", "do_thing"}, // sanitizes to the same name as above
		{"srv", long + "a"},
		{"srv", long + "b"}, // shares the truncated prefix
		{"ünï", "tööl"},
	}
	names := uniqueNames(keys)
	seen := map[string]bool{}
	for i, n := range names {
		if !valid.MatchString(n) {
			t.Errorf("invalid name %q for %v", n, keys[i])
		}
		if seen[n] {
			t.Errorf("duplicate name %q", n)
		}
		seen[n] = true
	}
	if names[0] != "mcp__my_server__do_thing" || !strings.HasPrefix(names[1], "mcp__my_server__do_thing_") {
		t.Errorf("collision names: %q %q", names[0], names[1])
	}
	if names[4] != "mcp___n___t__l" {
		t.Errorf("unicode: %q", names[4])
	}
	// Names are deterministic across rebuilds.
	if again := uniqueNames(keys); strings.Join(again, ",") != strings.Join(names, ",") {
		t.Errorf("unstable names: %v vs %v", again, names)
	}
}
