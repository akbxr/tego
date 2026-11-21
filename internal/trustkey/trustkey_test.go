package trustkey

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestKey(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(root, "repo")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, ".tego"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(repo, ".tego", "mcp.json")
	if err := os.WriteFile(cfg, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repo, "init", "-q")
	git(repo, "add", ".")
	git(repo, "commit", "-qm", "x")
	wt := filepath.Join(root, "wt")
	git(repo, "worktree", "add", "-q", wt)

	if got := Key(cfg); got != cfg {
		t.Errorf("main checkout: %s", got)
	}
	if got := Key(filepath.Join(wt, ".tego", "mcp.json")); got != cfg {
		t.Errorf("worktree: %s, want %s", got, cfg)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	if got := Key(filepath.Join(link, ".tego", "..", ".tego", "mcp.json")); got != cfg {
		t.Errorf("through a symlink: %s", got)
	}
	// A submodule's .git file points at a gitdir without commondir.
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(filepath.Join(root, "modules", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: ../modules/sub\n"), 0o644)
	if got := Key(filepath.Join(sub, ".mcp.json")); got != filepath.Join(sub, ".mcp.json") {
		t.Errorf("submodule: %s", got)
	}
	if got := Key(filepath.Join(root, "none", "x")); got != filepath.Join(root, "none", "x") {
		t.Errorf("no repository: %s", got)
	}
}
