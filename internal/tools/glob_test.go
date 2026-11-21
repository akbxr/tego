package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestGlobHiddenFiles(t *testing.T) {
	for _, engine := range grepEngines() {
		t.Run(engine, func(t *testing.T) {
			want := []string{".env.example", ".github/workflows/ci.yml", ".gitignore", "src/main.go"}
			if engine == "go" {
				t.Setenv("PATH", t.TempDir()) // no rg: the Go walker
				// The walker does not read .gitignore.
				want = []string{".env.example", ".github/workflows/ci.yml", ".gitignore", "ignored.txt", "src/main.go"}
			}
			g := &globTool{hiddenFixture(t)}
			run := func(pattern string) []string {
				t.Helper()
				in, _ := json.Marshal(map[string]string{"pattern": pattern})
				out, err := g.Run(context.Background(), in)
				if err != nil {
					t.Fatal(err)
				}
				got := strings.Split(out, "\n")
				sort.Strings(got)
				return got
			}
			// Hidden files and folders are listed; .git, the folder and the
			// submodule link file, is not.
			if got := run("*"); !slices.Equal(got, want) {
				t.Fatalf("* gives %q, want %q", got, want)
			}
			if got := run("**/*.yml"); !slices.Equal(got, []string{".github/workflows/ci.yml"}) {
				t.Fatalf("**/*.yml gives %q", got)
			}
			if got := run(".env*"); !slices.Equal(got, []string{".env.example"}) {
				t.Fatalf(".env* gives %q", got)
			}
		})
	}
}

// Outside a git repository, or in one at the home folder, hidden folders
// are caches and tools' data: glob and grep leave them out there.
func TestHiddenOnlyInAProject(t *testing.T) {
	files := map[string]string{".cache/x/a.txt": "needle\n", ".env": "needle\n", "src/a.txt": "needle\n"}
	plain := t.TempDir()
	writeFiles(t, plain, files)
	home := t.TempDir()
	writeFiles(t, home, files)
	writeFiles(t, home, map[string]string{".git/HEAD": "ref: refs/heads/main\n"})
	t.Setenv("HOME", home)
	project := filepath.Join(home, "code", "p")
	writeFiles(t, project, files)
	writeFiles(t, project, map[string]string{".git/HEAD": "ref: refs/heads/main\n"})
	if searchHidden(plain) || searchHidden(home) || searchHidden(filepath.Join(home, "code")) || !searchHidden(project) || !searchHidden(filepath.Join(project, "src")) {
		t.Fatal("searchHidden")
	}
	for _, engine := range grepEngines() {
		t.Run(engine, func(t *testing.T) {
			if engine == "go" {
				t.Setenv("PATH", t.TempDir())
			}
			for dir, want := range map[string]string{plain: "src/a.txt", home: "code/p/src/a.txt\nsrc/a.txt", project: ".cache/x/a.txt\n.env\nsrc/a.txt"} {
				env := &Env{Cwd: dir}
				in, _ := json.Marshal(map[string]string{"pattern": "**/*"})
				out, err := (&globTool{env}).Run(context.Background(), in)
				if err != nil {
					t.Fatal(err)
				}
				got := strings.Split(out, "\n")
				sort.Strings(got)
				if strings.Join(got, "\n") != want {
					t.Errorf("glob in %s: %q, want %q", dir, got, want)
				}
				if got := grepRun(t, env, engine, map[string]any{"pattern": "needle", "output": "files"}); got != want {
					t.Errorf("grep in %s: %q, want %q", dir, got, want)
				}
			}
		})
	}
}
