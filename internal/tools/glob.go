package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

const globMaxResults = 500

// skipDirs are never descended into by the fallback walkers.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "target": true, "dist": true,
}

type globTool struct{ env *Env }

func (t *globTool) ReadOnly() bool { return true }

func (t *globTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "glob",
		Description: "Find files by glob pattern (`**`, `*`, `?`, `[...]`, `{a,b}`); a pattern without `/` matches file names at any depth. Respects .gitignore. Newest first.",
		Schema: schema(`{"type":"object","properties":{` +
			`"pattern":{"type":"string","description":"e.g. **/*.go or src/{a,b}/*.ts"},` +
			`"path":{"type":"string","description":"Directory to search (default cwd)"}},` +
			`"required":["pattern"]}`),
	}
}

func (t *globTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := decode(input, &in); err != nil {
		return "", err
	}
	m, err := compileGlob(in.Pattern)
	if err != nil {
		return "", err
	}
	root := t.env.abs(in.Path)
	if st, err := os.Stat(root); err != nil {
		return "", err
	} else if !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", in.Path)
	}

	type hit struct {
		path  string
		mtime time.Time
	}
	var hits []hit
	err = listFiles(ctx, root, func(rel string) {
		if !m.Match(rel) {
			return
		}
		abs := filepath.Join(root, filepath.FromSlash(rel))
		var mt time.Time
		if st, err := os.Stat(abs); err == nil {
			mt = st.ModTime()
		}
		hits = append(hits, hit{t.env.rel(abs), mt})
	})
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "No files found", nil
	}
	sort.Slice(hits, func(i, j int) bool {
		if !hits[i].mtime.Equal(hits[j].mtime) {
			return hits[i].mtime.After(hits[j].mtime)
		}
		return hits[i].path < hits[j].path
	})
	var b strings.Builder
	for i, h := range hits {
		if i == globMaxResults {
			fmt.Fprintf(&b, "... %d more files omitted (narrow the pattern)\n", len(hits)-globMaxResults)
			break
		}
		b.WriteString(h.path)
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// searchHidden reports whether a search under root takes in hidden files
// and folders (.github, .env.example). It does in a git repository, whose
// .gitignore keeps out the hidden folders that are not the project's (.venv,
// .next). It doesn't outside one, or in a repository at the home folder: there
// hidden folders hold caches and tools' data (.cache, .npm, .rustup, ~/.tego)
// by the hundred thousand files, which nothing ignores.
func searchHidden(root string) bool {
	home, _ := os.UserHomeDir()
	homeInfo, _ := os.Stat(home)
	for dir := filepath.Clean(root); ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			info, err := os.Stat(dir)
			return err == nil && (homeInfo == nil || !os.SameFile(info, homeInfo))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// hiddenArgs are rg's flags for hidden files under root: hidden files but
// not .git (a later --glob wins over these), or none.
func hiddenArgs(root string) []string {
	if searchHidden(root) {
		return []string{"--hidden", "--glob", "!.git"}
	}
	return nil
}

// skipEntry reports whether the Go walker leaves out d, below the root:
// .git (a folder, or a worktree's or submodule's link file), the folders
// of skipDirs and, unless hidden, every hidden file and folder.
func skipEntry(d fs.DirEntry, hidden bool) bool {
	name := d.Name()
	return name == ".git" || (d.IsDir() && skipDirs[name]) || (!hidden && strings.HasPrefix(name, "."))
}

// listFiles calls fn with every file under root as a slash-separated path
// relative to root, hidden files too in a project's repository (but not
// .git). Uses `rg --files` (gitignore-aware) when available.
func listFiles(ctx context.Context, root string, fn func(rel string)) error {
	hidden := searchHidden(root)
	if rg := rgPath(); rg != "" {
		args := []string{"--files", "--color", "never"}
		if hidden {
			args = append(args, "--hidden", "--glob", "!.git")
		}
		cmd := exec.CommandContext(ctx, rg, args...)
		cmd.Dir = root
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.StdoutPipe()
		if err == nil {
			if err = cmd.Start(); err == nil {
				sc := bufio.NewScanner(out)
				sc.Buffer(make([]byte, 64<<10), 1<<20)
				n := 0
				for sc.Scan() {
					fn(filepath.ToSlash(strings.TrimPrefix(sc.Text(), "./")))
					n++
				}
				werr := cmd.Wait()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				var ee *exec.ExitError
				// Exit 1 = nothing found; exit 2 with output = partial errors (e.g. permissions).
				if werr == nil || (errors.As(werr, &ee) && (ee.ExitCode() == 1 || n > 0)) {
					return nil
				}
				if n == 0 && stderr.Len() > 0 {
					return errors.New(strings.TrimSpace(stderr.String()))
				}
			}
		}
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		if p != root && skipEntry(d, hidden) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		fn(filepath.ToSlash(rel))
		return nil
	})
}
