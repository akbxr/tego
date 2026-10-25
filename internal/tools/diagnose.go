package tools

import (
	"bytes"
	"context"
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

// Post-edit diagnostics (Fan et al. 2026): after edit/write, the file's
// language server (Env.LSP) reports its errors, types and imports
// included, and they are appended to the tool result, so a broken edit is
// fixed before a turn is spent on builds or tests. Without a server that
// answers, a fast, read-only syntax check runs instead (no type checking,
// no builds); checkers that are not installed are skipped silently.

const diagnoseTimeout = 5 * time.Second

type checker struct {
	name string
	argv []string // nil: checked in-process (JSON)
}

// checkerFor picks the syntax checker for p by extension.
func checkerFor(p string) (checker, bool) {
	run := func(argv ...string) (checker, bool) { return checker{argv[0], argv}, true }
	switch strings.ToLower(filepath.Ext(p)) {
	case ".go":
		return run("gofmt", "-e", "-l", p)
	case ".py":
		// ast.parse instead of py_compile, which would write __pycache__.
		return run("python3", "-c", "import ast,sys; ast.parse(open(sys.argv[1],'rb').read(), sys.argv[1])", p)
	case ".js", ".mjs", ".cjs":
		return run("node", "--check", p)
	case ".ts", ".mts", ".cts", ".tsx", ".jsx":
		// Bun's transpiler parses TS/TSX/JSX without type checking or
		// resolving imports (node --check cannot parse type annotations).
		return run("bun", "build", "--no-bundle", p)
	case ".luau":
		return run("luau-compile", "--null", p)
	case ".lua":
		if robloxProject(filepath.Dir(p)) {
			return run("luau-compile", "--null", p) // Rojo/Roblox .lua files are Luau
		}
		return run("luac", "-p", p)
	case ".sh", ".bash":
		return run("bash", "-n", p)
	case ".json":
		return checker{name: "json"}, true
	}
	return checker{}, false
}

// robloxMarkers mark a Roblox (Luau) project besides Rojo's *.project.json:
// Luau config and the usual toolchain/package manifests.
var robloxMarkers = []string{".luaurc", "wally.toml", "aftman.toml", "rokit.toml", "foreman.toml"}

var (
	robloxMu    sync.Mutex
	robloxCache = map[string]bool{}
)

// robloxProject reports whether dir or a parent holds a Roblox marker (or
// any *.project.json).
func robloxProject(dir string) bool {
	robloxMu.Lock()
	defer robloxMu.Unlock()
	if v, ok := robloxCache[dir]; ok {
		return v
	}
	found := false
	for d := dir; !found; d = filepath.Dir(d) {
		for _, m := range robloxMarkers {
			if _, err := os.Stat(filepath.Join(d, m)); err == nil {
				found = true
			}
		}
		if !found {
			if hits, _ := filepath.Glob(filepath.Join(d, "*.project.json")); len(hits) > 0 {
				found = true
			}
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	robloxCache[dir] = found
	return found
}

var (
	lookMu    sync.Mutex
	lookCache = map[string]bool{}
)

func haveTool(name string) bool {
	lookMu.Lock()
	defer lookMu.Unlock()
	ok, seen := lookCache[name]
	if !seen {
		_, err := exec.LookPath(name)
		ok = err == nil
		lookCache[name] = ok
	}
	return ok
}

// Checker checks a file's new content with a language server: a note on
// its errors ("" when clean), and ok false when no server checked it.
// before is the content the edit replaced ("" for a new file): errors
// already there in other files are not the edit's.
type Checker interface {
	Check(ctx context.Context, path, before, after string, rel func(string) string) (note string, ok bool)
}

// diagnose returns a note to append to an edit/write result, or "" when the
// file checks clean (or has no checker). before and after are the content
// replaced and written.
func (e *Env) diagnose(ctx context.Context, p, before, after string) string {
	if e.LSP != nil {
		if note, ok := e.LSP.Check(ctx, p, before, after, e.rel); ok {
			return note
		}
	}
	c, ok := checkerFor(p)
	if !ok {
		return ""
	}
	var msg string
	if c.argv == nil {
		msg = jsonSyntax(p)
	} else {
		argv := c.argv
		if !haveTool(argv[0]) {
			return ""
		}
		ctx, cancel := context.WithTimeout(ctx, diagnoseTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = e.Cwd
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		var ee *exec.ExitError
		if err == nil || !errors.As(err, &ee) || ctx.Err() != nil {
			return "" // clean, checker missing/broken, or too slow: say nothing
		}
		msg = stderr.String()
	}
	msg = strings.TrimSpace(strings.ReplaceAll(msg, p, e.rel(p)))
	if msg == "" {
		return ""
	}
	if lines := strings.Split(msg, "\n"); len(lines) > 20 {
		msg = strings.Join(lines[:20], "\n") + fmt.Sprintf("\n… %d more lines", len(lines)-20)
	}
	return fmt.Sprintf("\n\nSyntax check (%s) found problems; fix them before moving on:\n%s", c.name, msg)
}

func jsonSyntax(p string) string {
	data, err := os.ReadFile(p)
	if err != nil || json.Valid(data) {
		return ""
	}
	var v any
	err = json.Unmarshal(data, &v)
	var se *json.SyntaxError
	if errors.As(err, &se) {
		line := bytes.Count(data[:min(int(se.Offset), len(data))], []byte("\n")) + 1
		return fmt.Sprintf("%s:%d: %v", p, line, se)
	}
	if err != nil {
		return fmt.Sprintf("%s: %v", p, err)
	}
	return ""
}
