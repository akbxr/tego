package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Spec says how to run a language server and which files it checks.
type Spec struct {
	Name    string
	Command []string
	Env     []string // added to tego's environment, "KEY=value"
	// Languages maps a file extension (".go") to its LSP language id.
	Languages map[string]string
	// Roots are files or directories that mark a project's root, e.g.
	// go.mod; a server runs per root.
	Roots []string
	// NeedRoot: without a root marker the server's errors are mostly
	// noise (clangd without compile flags), so it is not used.
	NeedRoot bool
	// LocalBins are directories under the root searched for the program
	// before PATH: a project's own node_modules/.bin or virtualenv.
	LocalBins []string
	// InitOptions go in initialize; Settings answer workspace/configuration.
	InitOptions any
	Settings    any
}

// Builtin are the servers tego uses when they are installed. It installs
// none of them.
var Builtin = []Spec{
	{
		Name:      "gopls",
		Command:   []string{"gopls"},
		Languages: map[string]string{".go": "go"},
		Roots:     []string{"go.work", "go.mod"},
	},
	// TypeScript 7 serves LSP itself. tsc 5 does not know --lsp, fails
	// to start and the next one is used.
	{
		Name:      "tsc",
		Command:   []string{"tsc", "--lsp", "--stdio"},
		Languages: tsLanguages,
		Roots:     tsRoots,
		LocalBins: nodeBins,
	},
	{
		Name:      "tsgo", // @typescript/native-preview
		Command:   []string{"tsgo", "--lsp", "--stdio"},
		Languages: tsLanguages,
		Roots:     tsRoots,
		LocalBins: nodeBins,
	},
	{
		Name:      "typescript-language-server",
		Command:   []string{"typescript-language-server", "--stdio"},
		Languages: tsLanguages,
		Roots:     tsRoots,
		LocalBins: nodeBins,
	},
	{
		Name:      "basedpyright",
		Command:   []string{"basedpyright-langserver", "--stdio"},
		Languages: pyLanguages,
		Roots:     pyRoots,
		LocalBins: pyBins,
	},
	{
		Name:      "pyright",
		Command:   []string{"pyright-langserver", "--stdio"},
		Languages: pyLanguages,
		Roots:     pyRoots,
		LocalBins: append(slices.Clone(pyBins), nodeBins...),
	},
	{
		Name:      "rust-analyzer",
		Command:   []string{"rust-analyzer"},
		Languages: map[string]string{".rs": "rust"},
		Roots:     []string{"Cargo.toml"},
		NeedRoot:  true,
	},
	{
		Name:    "clangd",
		Command: []string{"clangd"},
		Languages: map[string]string{
			".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hh": "cpp",
			".m": "objective-c", ".mm": "objective-cpp",
		},
		Roots:    []string{"compile_commands.json", "build/compile_commands.json", "compile_flags.txt", ".clangd"},
		NeedRoot: true,
	},
	{
		Name:      "sourcekit-lsp",
		Command:   []string{"sourcekit-lsp"},
		Languages: map[string]string{".swift": "swift"},
		Roots:     []string{"Package.swift"},
		NeedRoot:  true,
	},
	{
		Name:      "zls",
		Command:   []string{"zls"},
		Languages: map[string]string{".zig": "zig"},
		Roots:     []string{"build.zig"},
	},
}

// serverConfig is a server in lsp_servers. Fields left out keep the
// built-in server's of the same name.
type serverConfig struct {
	Command     []string          `json:"command"`
	Env         map[string]string `json:"env"`
	Languages   map[string]string `json:"languages"` // ".luau": "luau"
	Roots       []string          `json:"roots"`
	NeedRoot    *bool             `json:"need_root"`
	InitOptions any               `json:"init_options"`
	Settings    any               `json:"settings"`
}

// Specs is Builtin changed by lsp_servers: a name set to false (or null)
// turns that server off, an object adds a server or changes the built-in
// one's fields. Added servers come first, so they win over built-in ones
// for the same files.
func Specs(config map[string]json.RawMessage) ([]Spec, error) {
	out := slices.Clone(Builtin)
	var added []Spec
	names := slices.Sorted(maps.Keys(config))
	for _, name := range names {
		raw := bytes.TrimSpace(config[name])
		i := slices.IndexFunc(out, func(s Spec) bool { return s.Name == name })
		if string(raw) == "false" || string(raw) == "null" {
			if i >= 0 {
				out = slices.Delete(out, i, i+1)
			}
			continue
		}
		var c serverConfig
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("lsp_servers.%s: %v", name, err)
		}
		s := Spec{Name: name}
		if i >= 0 {
			s = out[i]
			out = slices.Delete(out, i, i+1)
		}
		if len(c.Command) > 0 {
			s.Command = c.Command
		}
		for _, k := range slices.Sorted(maps.Keys(c.Env)) {
			s.Env = append(s.Env, k+"="+c.Env[k])
		}
		if len(c.Languages) > 0 {
			s.Languages = map[string]string{}
			for ext, id := range c.Languages {
				if !strings.HasPrefix(ext, ".") {
					ext = "." + ext
				}
				s.Languages[strings.ToLower(ext)] = id
			}
		}
		if c.Roots != nil {
			s.Roots = c.Roots
		}
		if c.NeedRoot != nil {
			s.NeedRoot = *c.NeedRoot
		}
		if c.InitOptions != nil {
			s.InitOptions = c.InitOptions
		}
		if c.Settings != nil {
			s.Settings = c.Settings
		}
		if len(s.Command) == 0 || len(s.Languages) == 0 {
			return nil, fmt.Errorf("lsp_servers.%s: needs \"command\" and \"languages\"", name)
		}
		added = append(added, s)
	}
	return append(added, out...), nil
}

var (
	tsLanguages = map[string]string{
		".ts": "typescript", ".mts": "typescript", ".cts": "typescript", ".tsx": "typescriptreact",
		".js": "javascript", ".mjs": "javascript", ".cjs": "javascript", ".jsx": "javascriptreact",
	}
	tsRoots     = []string{"tsconfig.json", "jsconfig.json", "package.json"}
	nodeBins    = []string{"node_modules/.bin"}
	pyLanguages = map[string]string{".py": "python", ".pyi": "python"}
	pyRoots     = []string{"pyproject.toml", "pyrightconfig.json", "setup.py", "setup.cfg", "requirements.txt"}
	pyBins      = []string{".venv/bin", "venv/bin"}
)

// lookPath finds a server's program; a variable so tests can fake it.
var lookPath = exec.LookPath

// findRoot picks the root a server for file runs in. Inside cwd it is the
// outermost marked directory between the file and cwd, so a monorepo gets
// one server; else the nearest marked directory above the file. With no
// marker it is cwd, or "" when the spec needs a marker.
func findRoot(spec Spec, file, cwd string) string {
	marked := func(d string) bool {
		for _, m := range spec.Roots {
			if _, err := os.Stat(filepath.Join(d, m)); err == nil {
				return true
			}
		}
		return false
	}
	inside := within(file, cwd)
	root := ""
	for d := filepath.Dir(file); ; d = filepath.Dir(d) {
		if marked(d) {
			root = d
			if !inside {
				return root // nearest
			}
		}
		if inside && d == cwd {
			break
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	if root != "" {
		return root
	}
	if inside {
		// Nothing between the file and cwd: the project may start above
		// cwd (cwd is a subdirectory of a module).
		for d := filepath.Dir(cwd); filepath.Dir(d) != d; d = filepath.Dir(d) {
			if marked(d) {
				return d
			}
		}
		if !spec.NeedRoot {
			return cwd
		}
	} else if !spec.NeedRoot {
		return filepath.Dir(file) // a lone file outside the project
	}
	return ""
}

// within reports whether p is dir or below it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
