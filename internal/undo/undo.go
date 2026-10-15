// Package undo snapshots the working tree before every prompt so /undo can
// put the files back. Snapshots are git trees in a shadow repository under
// tego's own folder (the project's .git is never touched), so they also
// cover what shell commands changed, follow the project's .gitignore, and
// cost little: git only stores what changed.
package undo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// snapshotTimeout bounds one snapshot; a project that takes longer is too
// big to snapshot before every prompt, so snapshots stop for the session.
const snapshotTimeout = 15 * time.Second

// defaultExcludes are left out of snapshots even without a .gitignore:
// dependencies and caches that are big, generated and never edited by hand.
var defaultExcludes = []string{"node_modules/", ".venv/", "venv/", "__pycache__/", ".pytest_cache/", ".mypy_cache/",
	".next/", ".nuxt/", ".gradle/", ".cache/", "target/", ".DS_Store"}

// Entry is one snapshot: the files as they were before a prompt, or just
// before an /undo (so it can be undone in turn).
type Entry struct {
	Tree   string    `json:"tree"`
	Prompt string    `json:"prompt,omitempty"` // the prompt it precedes; "" before an /undo
	Time   time.Time `json:"time"`
}

// Store keeps the snapshots of one working folder.
type Store struct {
	gitDir, work string
	mu           sync.Mutex
	off          string // why snapshots are off ("" = on)
}

// Open returns the store for work under base (tego's folder). Snapshots are
// off for the home folder and its parents, which are too big and not
// projects, and when git is missing.
func Open(base, work string) *Store {
	abs, err := filepath.Abs(work)
	if err != nil {
		abs = work
	}
	sum := sha256.Sum256([]byte(abs))
	s := &Store{gitDir: filepath.Join(base, "snapshots", filepath.Base(abs)+"-"+hex.EncodeToString(sum[:6])), work: abs}
	home, _ := os.UserHomeDir()
	switch {
	case abs == "/" || (home != "" && (abs == home || strings.HasPrefix(home, abs+string(filepath.Separator)))):
		s.off = "the home folder and its parents are not snapshotted"
	default:
		if _, err := exec.LookPath("git"); err != nil {
			s.off = "git is not installed"
		}
	}
	return s
}

// Off reports why snapshots are off, "" when they are on.
func (s *Store) Off() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.off
}

func (s *Store) git(ctx context.Context, stdin []byte, args ...string) (string, error) {
	full := append([]string{"--git-dir=" + s.gitDir, "--work-tree=" + s.work,
		"-c", "core.autocrlf=false", "-c", "core.quotepath=false", "-c", "advice.addEmbeddedRepo=false",
		"-c", "core.fsmonitor=false", "-c", "gc.auto=0"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = s.work
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (s *Store) init(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(s.gitDir, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(s.gitDir, 0o700); err != nil {
		return err
	}
	if _, err := s.git(ctx, nil, "init", "-q"); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.gitDir, "info", "exclude"), []byte(strings.Join(defaultExcludes, "\n")+"\n"), 0o600)
}

// Snapshot records the working tree and returns its tree id.
func (s *Store) Snapshot(ctx context.Context) (string, error) {
	if why := s.Off(); why != "" {
		return "", errors.New(why)
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	if err := s.init(ctx); err != nil {
		return "", err
	}
	if _, err := s.git(ctx, nil, "add", "-A", "--ignore-errors", "."); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			s.mu.Lock()
			s.off = fmt.Sprintf("the folder took over %s to snapshot", snapshotTimeout)
			s.mu.Unlock()
		}
		return "", err
	}
	tree, err := s.git(ctx, nil, "write-tree")
	if err != nil {
		return "", err
	}
	tree = strings.TrimSpace(tree)
	// Trees are kept alive by a ref, so git's housekeeping never drops one.
	_, _ = s.git(ctx, nil, "update-ref", "refs/tego/latest", s.commit(ctx, tree))
	return tree, nil
}

// commit wraps tree in a commit whose parent is the previous one, so every
// snapshot stays reachable from refs/tego/latest.
func (s *Store) commit(ctx context.Context, tree string) string {
	args := []string{"commit-tree", tree, "-m", "tego snapshot"}
	if prev, err := s.git(ctx, nil, "rev-parse", "-q", "--verify", "refs/tego/latest"); err == nil {
		args = append(args, "-p", strings.TrimSpace(prev))
	}
	out, err := s.gitEnv(ctx, []string{"GIT_AUTHOR_NAME=tego", "GIT_AUTHOR_EMAIL=tego@localhost",
		"GIT_COMMITTER_NAME=tego", "GIT_COMMITTER_EMAIL=tego@localhost"}, args...)
	if err != nil {
		return tree
	}
	return strings.TrimSpace(out)
}

func (s *Store) gitEnv(ctx context.Context, env []string, args ...string) (string, error) {
	full := append([]string{"--git-dir=" + s.gitDir, "--work-tree=" + s.work}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = s.work
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	return string(out), err
}

// Changes lists what restoring from would do to the files as they are in
// to: the paths to write back and the paths to delete (created since).
func (s *Store) Changes(ctx context.Context, from, to string) (restore, remove []string, err error) {
	if from == to {
		return nil, nil, nil
	}
	out, err := s.git(ctx, nil, "diff-tree", "-r", "-z", "--name-status", "--no-renames", from, to)
	if err != nil {
		return nil, nil, err
	}
	f := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		switch status, p := f[i], f[i+1]; status {
		case "A":
			remove = append(remove, p)
		default: // M, D, T
			restore = append(restore, p)
		}
	}
	return restore, remove, nil
}

// Restore puts the working tree back to tree: files changed or deleted
// since are written back, files created since are deleted (current must be
// a snapshot of the working tree as it is now). Ignored files are left alone.
func (s *Store) Restore(ctx context.Context, tree, current string) (restored, removed []string, err error) {
	restore, remove, err := s.Changes(ctx, tree, current)
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.git(ctx, nil, "read-tree", tree); err != nil {
		return nil, nil, err
	}
	if len(restore) > 0 {
		if _, err := s.git(ctx, []byte(strings.Join(restore, "\x00")+"\x00"), "checkout-index", "-f", "-z", "--stdin"); err != nil {
			return nil, nil, err
		}
	}
	for _, p := range remove {
		full := filepath.Join(s.work, filepath.FromSlash(p))
		if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
			return restore, removed, err
		}
		removed = append(removed, p)
		// Drop the folders that only held it.
		for d := filepath.Dir(full); d != s.work && strings.HasPrefix(d, s.work); d = filepath.Dir(d) {
			if os.Remove(d) != nil {
				break
			}
		}
	}
	return restore, removed, nil
}

// Log is the list of snapshots of one session, kept next to the shadow
// repository so it survives a restart and a resume.
func (s *Store) logPath(session string) string {
	return filepath.Join(s.gitDir, "sessions", session+".json")
}

// Entries returns a session's snapshots, oldest first.
func (s *Store) Entries(session string) []Entry {
	data, err := os.ReadFile(s.logPath(session))
	if err != nil {
		return nil
	}
	var list []Entry
	_ = json.Unmarshal(data, &list)
	return list
}

// Add appends e to a session's snapshots.
func (s *Store) Add(session string, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.Entries(session)
	if n := len(list); n > 0 && list[n-1].Tree == e.Tree && list[n-1].Prompt == "" && e.Prompt == "" {
		return nil // an /undo of nothing new
	}
	list = append(list, e)
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.logPath(session)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.logPath(session), data, 0o600)
}
