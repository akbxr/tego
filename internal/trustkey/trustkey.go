// Package trustkey names the project files tego runs code from (MCP
// configs, extensions) for the records of which ones the user trusted.
package trustkey

import (
	"os"
	"path/filepath"
	"strings"
)

// Key is the name path is trusted under: its real path, and in a linked git
// worktree (tego -w, git worktree add) the same file in the main checkout.
// Trusting a project's file then covers its worktrees and /tmp's other
// name, /private/tmp. The content is checked besides, so a worktree whose
// copy differs is still not trusted.
func Key(path string) string {
	path = filepath.Clean(path)
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p
	}
	for dir := filepath.Dir(path); ; {
		gitPath := filepath.Join(dir, ".git")
		if fi, err := os.Lstat(gitPath); err == nil {
			if fi.Mode().IsRegular() {
				if main := mainCheckout(gitPath); main != "" {
					if rel, err := filepath.Rel(dir, path); err == nil {
						return filepath.Join(main, rel)
					}
				}
			}
			return path
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return path
		}
		dir = parent
	}
}

// mainCheckout is the main checkout's folder of the linked worktree whose
// .git file is gitFile ("gitdir: <main>/.git/worktrees/<name>"), "" when it
// is not one: a submodule's gitdir has no commondir, a bare repository's
// common dir is not a .git folder.
func mainCheckout(gitFile string) string {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(gitFile), gitdir)
	}
	common, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return ""
	}
	c := strings.TrimSpace(string(common))
	if !filepath.IsAbs(c) {
		c = filepath.Join(gitdir, c)
	}
	c = filepath.Clean(c)
	if filepath.Base(c) != ".git" {
		return ""
	}
	main := filepath.Dir(c)
	if p, err := filepath.EvalSymlinks(main); err == nil {
		main = p
	}
	return main
}
