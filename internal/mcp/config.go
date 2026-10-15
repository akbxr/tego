// Package mcp is a Model Context Protocol client: it connects to configured
// MCP servers (stdio, streamable HTTP or legacy HTTP+SSE) and exposes their
// tools as agent tools.
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ServerConfig describes one MCP server.
type ServerConfig struct {
	Name     string
	Type     string // "stdio" (default when Command set) | "http" (streamable HTTP) | "sse" (legacy HTTP+SSE)
	Command  string
	Args     []string
	Env      map[string]string
	Cwd      string
	URL      string
	Headers  map[string]string
	Disabled bool
	Source   string // config file it came from
}

type rawServer struct {
	Type     string            `json:"type"`
	Command  string            `json:"command"`
	Args     []string          `json:"args"`
	Env      map[string]string `json:"env"`
	Cwd      string            `json:"cwd"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	Disabled *bool             `json:"disabled"`
	Enabled  *bool             `json:"enabled"`
}

type rawFile struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
}

// configPaths lists the files LoadConfig reads, lowest precedence first.
func configPaths(home, cwd string) []string {
	var paths []string
	if home != "" {
		paths = append(paths, filepath.Join(home, "mcp.json"))
	}
	if cwd != "" {
		paths = append(paths,
			filepath.Join(cwd, ".mcp.json"),
			filepath.Join(cwd, ".tego", "mcp.json"))
	}
	return paths
}

// LoadConfig merges `mcpServers` from <home>/mcp.json, <cwd>/.mcp.json and
// <cwd>/.tego/mcp.json; a later file replaces an earlier entry of the same
// name. ${VAR} and ${VAR:-default} are expanded from the environment in
// command, args, env, url and headers. Missing files are skipped. Malformed
// files produce an error naming the file; servers from the remaining files
// are still returned alongside it.
func LoadConfig(home, cwd string) ([]ServerConfig, error) {
	var (
		out   []ServerConfig
		index = map[string]int{}
		seen  = map[string]bool{}
		errs  []error
	)
	for _, p := range configPaths(home, cwd) {
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		cfgs, err := loadFile(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, c := range cfgs {
			if i, ok := index[c.Name]; ok {
				out[i] = c
				continue
			}
			index[c.Name] = len(out)
			out = append(out, c)
		}
	}
	return out, errors.Join(errs...)
}

func loadFile(path string) ([]ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("mcp config %s: %w", path, err)
	}
	var f rawFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("mcp config %s: %w", path, err)
	}
	names := make([]string, 0, len(f.MCPServers))
	for name := range f.MCPServers {
		names = append(names, name)
	}
	// Keep file order where possible: map iteration is random, so sort by
	// position of the key in the source text.
	sortByPosition(names, data)
	out := make([]ServerConfig, 0, len(names))
	for _, name := range names {
		var r rawServer
		if err := json.Unmarshal(f.MCPServers[name], &r); err != nil {
			return nil, fmt.Errorf("mcp config %s: server %q: %w", path, name, err)
		}
		c := ServerConfig{
			Name:    name,
			Type:    normalizeType(r.Type, r.Command, r.URL),
			Command: expandEnv(r.Command),
			Cwd:     r.Cwd,
			URL:     expandEnv(r.URL),
			Source:  path,
		}
		if r.Disabled != nil && *r.Disabled || r.Enabled != nil && !*r.Enabled {
			c.Disabled = true
		}
		if len(r.Args) > 0 {
			c.Args = make([]string, len(r.Args))
			for i, a := range r.Args {
				c.Args[i] = expandEnv(a)
			}
		}
		c.Env = expandMap(r.Env)
		c.Headers = expandMap(r.Headers)
		out = append(out, c)
	}
	return out, nil
}

func sortByPosition(names []string, data []byte) {
	pos := make(map[string]int, len(names))
	s := string(data)
	for _, n := range names {
		q, _ := json.Marshal(n)
		i := strings.Index(s, string(q))
		if i < 0 {
			i = len(s)
		}
		pos[n] = i
	}
	// insertion sort: server lists are tiny
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && (pos[names[j]] < pos[names[j-1]] ||
			pos[names[j]] == pos[names[j-1]] && names[j] < names[j-1]); j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
}

func normalizeType(t, command, url string) string {
	switch strings.ToLower(t) {
	case "":
		if command == "" && url != "" {
			return "http"
		}
		return "stdio"
	case "http", "streamable-http", "streamablehttp", "streamable_http":
		return "http"
	default:
		return strings.ToLower(t)
	}
}

func expandMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = expandEnv(v)
	}
	return out
}

// expandEnv replaces ${VAR} and ${VAR:-default} with values from the
// environment. An unset VAR without a default expands to "". Anything else,
// including bare $VAR, is left untouched.
func expandEnv(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			break
		}
		j := strings.IndexByte(s[i+2:], '}')
		if j < 0 {
			break
		}
		b.WriteString(s[:i])
		expr := s[i+2 : i+2+j]
		name, def, hasDef := strings.Cut(expr, ":-")
		v, ok := os.LookupEnv(name)
		if (!ok || v == "") && hasDef {
			v = def
		}
		b.WriteString(v)
		s = s[i+2+j+1:]
	}
	b.WriteString(s)
	return b.String()
}
