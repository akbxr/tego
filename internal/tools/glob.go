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

// listFiles calls fn with every file under root as a slash-separated path
// relative to root. Uses `rg --files` (gitignore-aware) when available.
func listFiles(ctx context.Context, root string, fn func(rel string)) error {
	if rg := rgPath(); rg != "" {
		cmd := exec.CommandContext(ctx, rg, "--files", "--color", "never")
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
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		fn(filepath.ToSlash(rel))
		return nil
	})
}
