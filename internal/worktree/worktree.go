// Package worktree gives a session a git worktree of its own: a separate
// checkout on a new branch, so several sessions can work on one repository
// without touching each other's files.
//
// Worktrees live outside the repository, in ~/.tego/worktrees/<repo>/<name>,
// where the repository's own tools (tsc, eslint, test runners, file search)
// don't walk into them and no .gitignore needs changing. Their branches are
// tego/<name>.
package worktree

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// BranchPrefix starts the name of every branch tego makes for a worktree.
const BranchPrefix = "tego/"

// IncludeFile lists, gitignore style, the ignored files (such as .env) to
// copy into a new worktree; the same file Claude Code reads.
const IncludeFile = ".worktreeinclude"

// Tree is a worktree tego made.
type Tree struct {
	Name   string
	Path   string
	Branch string // the branch checked out in it ("" when detached)
	Repo   string // the repository's main working tree
}

// Status is what removing a worktree would lose.
type Status struct {
	Changed  int // files changed or added and not committed
	Unmerged int // commits on no other branch, local or remote
}

// Clean reports whether a worktree can go without losing anything.
func (s Status) Clean() bool { return s.Changed == 0 && s.Unmerged == 0 }

// Describe says what is in a worktree, for the user.
func (s Status) Describe() string {
	var parts []string
	if s.Changed > 0 {
		parts = append(parts, plural(s.Changed, "uncommitted change"))
	}
	if s.Unmerged > 0 {
		parts = append(parts, plural(s.Unmerged, "commit")+" on no other branch")
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, ", ")
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Root is the folder that holds every worktree tego makes.
func Root(home string) string { return filepath.Join(home, "worktrees") }

// Open makes the worktree name for the repository dir is in, or returns it
// if it already exists (created is then false). An empty name picks one from
// the time. A new worktree starts at dir's HEAD, on branch tego/<name> (an
// existing branch of that name is checked out instead), and gets copies of
// the ignored files IncludeFile lists.
func Open(home, dir, name string) (t *Tree, created bool, err error) {
	repo, err := mainTree(dir)
	if err != nil {
		return nil, false, err
	}
	base := repoDir(home, repo)
	if name == "" {
		name = freeName(base, time.Now())
	}
	if !validName.MatchString(name) || strings.HasSuffix(name, ".lock") || strings.Contains(name, "..") {
		return nil, false, fmt.Errorf("worktree name %q: use letters, digits, '.', '_' and '-'", name)
	}
	path := filepath.Join(base, name)
	branch := BranchPrefix + name

	trees, err := list(repo)
	if err != nil {
		return nil, false, err
	}
	for _, w := range trees {
		if sameFile(w.path, path) {
			return &Tree{Name: name, Path: path, Branch: w.branch, Repo: repo}, false, nil
		}
	}
	if _, err := os.Stat(path); err == nil {
		return nil, false, fmt.Errorf("%s exists but is not a worktree of %s", path, repo)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, false, err
	}
	args := []string{"worktree", "add", "--quiet", "-b", branch, path, "HEAD"}
	if git(repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch) == nil {
		args = []string{"worktree", "add", "--quiet", path, branch}
	}
	if _, err := output(dir, args...); err != nil {
		return nil, false, err
	}
	t = &Tree{Name: name, Path: path, Branch: branch, Repo: repo}
	if top, err := output(dir, "rev-parse", "--show-toplevel"); err == nil {
		if err := copyIncluded(top, path); err != nil {
			return t, true, fmt.Errorf("copying the files in %s: %w", IncludeFile, err)
		}
	}
	return t, true, nil
}

// List is the worktrees tego made for the repository dir is in.
func List(home, dir string) ([]*Tree, error) {
	repo, err := mainTree(dir)
	if err != nil {
		return nil, err
	}
	_ = git(repo, "worktree", "prune") // forget the ones whose folder was deleted
	trees, err := list(repo)
	if err != nil {
		return nil, err
	}
	base := repoDir(home, repo)
	var out []*Tree
	for _, w := range trees {
		if filepath.Dir(w.path) != base && !sameFile(filepath.Dir(w.path), base) {
			continue
		}
		out = append(out, &Tree{Name: filepath.Base(w.path), Path: w.path, Branch: w.branch, Repo: repo})
	}
	return out, nil
}

// Find is the worktree tego made with that name, for the repository dir is in.
func Find(home, dir, name string) (*Tree, error) {
	trees, err := List(home, dir)
	if err != nil {
		return nil, err
	}
	for _, t := range trees {
		if t.Name == name {
			return t, nil
		}
	}
	return nil, fmt.Errorf("no worktree %q for this repository (tego worktree lists them)", name)
}

// Status counts what removing t would lose.
func (t *Tree) Status() (Status, error) {
	var s Status
	out, err := output(t.Path, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return s, err
	}
	if out != "" {
		s.Changed = strings.Count(out, "\n") + 1
	}
	// Commits HEAD has that no other branch, local or remote, has.
	args := []string{"rev-list", "--count", "HEAD", "--not"}
	if b, err := output(t.Path, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil && b != "" {
		args = append(args, "--exclude="+b)
	}
	args = append(args, "--branches", "--remotes")
	out, err = output(t.Path, args...)
	if err != nil {
		return s, err
	}
	s.Unmerged, _ = strconv.Atoi(out)
	return s, nil
}

// Remove deletes the worktree, discarding what is in it, and with
// deleteBranch also its tego/ branch.
func (t *Tree) Remove(deleteBranch bool) error {
	if _, err := output(t.Repo, "worktree", "remove", "--force", t.Path); err != nil {
		if _, statErr := os.Stat(t.Path); !errors.Is(statErr, os.ErrNotExist) {
			return err
		}
		_ = git(t.Repo, "worktree", "prune") // its folder was already gone
	}
	if deleteBranch && strings.HasPrefix(t.Branch, BranchPrefix) {
		if _, err := output(t.Repo, "branch", "-D", t.Branch); err != nil {
			return err
		}
	}
	_ = os.Remove(filepath.Dir(t.Path)) // the repository's folder, once empty
	return nil
}

// mainTree is the main working tree of the repository dir is in, the same
// from any of its worktrees.
func mainTree(dir string) (string, error) {
	common, err := output(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		if _, lookErr := exec.LookPath("git"); lookErr != nil {
			return "", errors.New("git is not installed")
		}
		return "", fmt.Errorf("%s is not in a git repository", dir)
	}
	if filepath.Base(common) != ".git" {
		return "", fmt.Errorf("%s: a bare repository has no working tree to copy", common)
	}
	return filepath.Dir(common), nil
}

// repoDir is where the worktrees of repo go: its folder name, plus a hash of
// its path so two repositories of the same name don't share it.
func repoDir(home, repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return filepath.Join(Root(home), filepath.Base(repo)+"-"+hex.EncodeToString(sum[:3]))
}

// freeName is a name from the time that no folder in base has yet.
func freeName(base string, now time.Time) string {
	name := now.Format("0102-1504")
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(base, name)); errors.Is(err, os.ErrNotExist) {
			return name
		}
		name = fmt.Sprintf("%s-%d", now.Format("0102-1504"), i)
	}
}

type entry struct{ path, branch string }

// list reads `git worktree list --porcelain`.
func list(repo string) ([]entry, error) {
	out, err := output(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var trees []entry
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "worktree "):
			trees = append(trees, entry{path: strings.TrimPrefix(line, "worktree ")})
		case strings.HasPrefix(line, "branch ") && len(trees) > 0:
			trees[len(trees)-1].branch = strings.TrimPrefix(line, "branch refs/heads/")
		}
	}
	return trees, nil
}

// copyIncluded copies the files under src that IncludeFile matches and git
// ignores (a tracked file is in the new checkout already) into dst. Only
// regular files are copied, so a symlink can't reach outside the tree.
func copyIncluded(src, dst string) error {
	include := filepath.Join(src, IncludeFile)
	if _, err := os.Stat(include); err != nil {
		return nil
	}
	matched, err := output(src, "ls-files", "--others", "--ignored", "--exclude-from="+include)
	if err != nil || matched == "" {
		return err
	}
	cmd := exec.Command("git", "-C", src, "check-ignore", "--stdin")
	cmd.Stdin = strings.NewReader(matched + "\n")
	ignored, _ := cmd.Output() // exits 1 when nothing is ignored
	for _, rel := range strings.Split(strings.TrimSpace(string(ignored)), "\n") {
		if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(filepath.Clean(rel), "..") {
			continue
		}
		if err := copyFile(filepath.Join(src, rel), filepath.Join(dst, rel)); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(from, to string) error {
	fi, err := os.Lstat(from)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if errors.Is(err, os.ErrExist) {
		return nil // the checkout has it: leave that one
	}
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func git(dir string, args ...string) error {
	_, err := output(dir, args...)
	return err
}

// output runs git in dir and returns its trimmed stdout, or an error with
// what git printed.
func output(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func sameFile(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func plural(n int, s string) string {
	if n == 1 {
		return "1 " + s
	}
	return strconv.Itoa(n) + " " + s + "s"
}
