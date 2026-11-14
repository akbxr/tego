package undo

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// keepFor is how long snapshots are kept: /undo goes back a month at most.
const keepFor = 30 * 24 * time.Hour

// maintainEvery is how often one tego process tidies all snapshots.
const maintainEvery = 24 * time.Hour

// pruneGrace keeps unreachable objects this young: a snapshot another
// process is taking has written its files' contents but not yet its tree.
var pruneGrace = time.Hour

// link lets the snapshots of work use the objects of the project's own git
// repository (git alternates), so a file committed there is not stored a
// second time; snapshots of a gen3 rendering project were half copies of
// its committed images. A repository that is gone is unlinked.
func link(ctx context.Context, gitDir, work string) {
	path := filepath.Join(gitDir, "objects", "info", "alternates")
	data, _ := os.ReadFile(path)
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" && isDir(l) {
			lines = append(lines, l) // a project's earlier repository keeps older snapshots whole
		}
	}
	if objs := projectObjects(ctx, work); objs != "" && !slices.Contains(lines, objs) {
		lines = append(lines, objs)
	}
	want := strings.Join(lines, "\n")
	if want != "" {
		want += "\n"
	}
	switch {
	case want == string(data):
	case want == "":
		_ = os.Remove(path)
	default:
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		_ = os.WriteFile(path, []byte(want), 0o600)
	}
}

// projectObjects is the object folder of the git repository work is in,
// "" when it is in none.
func projectObjects(ctx context.Context, work string) string {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.Dir = work
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	dir := filepath.Join(strings.TrimSpace(string(out)), "objects")
	if !isDir(dir) {
		return ""
	}
	return dir
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// lockLogs locks a folder's snapshot logs against other processes.
func lockLogs(gitDir string) (unlock func()) {
	f, err := os.OpenFile(filepath.Join(gitDir, "logs.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX) != nil {
		f.Close()
		return func() {}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

// MaintainSoon tidies the snapshots under base in the background, when no
// tego process has in the last day (Maintain).
func MaintainSoon(base string) {
	dir := filepath.Join(base, "snapshots")
	if !isDir(dir) {
		return
	}
	stamp := filepath.Join(dir, ".maintained")
	fi, err := os.Stat(stamp)
	if err != nil {
		// The first tego to see the folder starts the clock: tidying
		// begins a day later, not under a tego that just started.
		_ = os.WriteFile(stamp, nil, 0o600)
		return
	}
	if time.Since(fi.ModTime()) < maintainEvery {
		return
	}
	go func() {
		f, err := os.OpenFile(filepath.Join(dir, ".maintain.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			return // another tego is at it
		}
		defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < maintainEvery {
			return
		}
		_ = os.WriteFile(stamp, nil, 0o600)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		Maintain(ctx, dir, time.Now())
	}()
}

// Maintain tidies every folder's snapshots in dir: those of folders that
// no longer exist go, snapshots older than keepFor are dropped with the
// contents only they held, so is what a snapshot that took too long left,
// and contents the project's git has stored since are kept there only.
func Maintain(ctx context.Context, dir string, now time.Time) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if ctx.Err() != nil {
			return
		}
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			maintain(ctx, filepath.Join(dir, e.Name()), now)
		}
	}
}

func maintain(ctx context.Context, gitDir string, now time.Time) {
	out, err := exec.CommandContext(ctx, "git", "config", "--file", filepath.Join(gitDir, "config"), "core.worktree").Output()
	work := strings.TrimSpace(string(out))
	if err != nil || work == "" {
		return // not a snapshot store
	}
	if !isDir(work) {
		if isDir(filepath.Dir(work)) { // not a drive that is out
			_ = os.RemoveAll(gitDir)
		}
		return
	}
	if fi, err := os.Stat(filepath.Join(gitDir, "index")); err == nil && now.Sub(fi.ModTime()) < pruneGrace {
		// Snapshotted within the hour: one may be being taken and not yet
		// logged, and rechaining would drop it. Tidied another day.
		return
	}
	unlock := lockLogs(gitDir)
	trees, dropped := trimLogs(gitDir, now.Add(-keepFor))
	unlock()
	if len(trees) == 0 {
		_ = os.RemoveAll(gitDir)
		return
	}
	s := &Store{gitDir: gitDir, work: work}
	link(ctx, gitDir, work)
	marker := filepath.Join(gitDir, tooBigFile)
	_, statErr := os.Stat(marker)
	tooBig := statErr == nil
	if tooBig && s.tooBig() == "" {
		_ = os.Remove(marker)
	}
	if n, err := s.git(ctx, nil, "rev-list", "--count", "refs/tego/latest"); dropped || tooBig || err != nil || strings.TrimSpace(n) != strconv.Itoa(len(trees)) {
		if s.rechain(ctx, trees) == nil {
			_, _ = s.git(ctx, nil, "prune", "--expire="+now.Add(-pruneGrace).Format(time.RFC3339))
		}
	}
	s.dropShared(ctx)
}

// trimLogs drops log entries from before cutoff and returns the trees of
// the rest, oldest first and once each, and whether any were dropped.
func trimLogs(gitDir string, cutoff time.Time) (trees []string, dropped bool) {
	files, _ := filepath.Glob(filepath.Join(gitDir, "sessions", "*.json"))
	var kept []Entry
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var list []Entry
		if json.Unmarshal(data, &list) != nil {
			continue
		}
		n := len(list)
		list = slices.DeleteFunc(list, func(e Entry) bool { return e.Time.Before(cutoff) })
		switch {
		case len(list) == 0:
			_ = os.Remove(f)
		case len(list) < n:
			if data, err := json.Marshal(list); err == nil {
				_ = os.WriteFile(f, data, 0o600)
			}
		}
		dropped = dropped || len(list) < n
		kept = append(kept, list...)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Time.Before(kept[j].Time) })
	seen := map[string]bool{}
	for _, e := range kept {
		if !seen[e.Tree] {
			seen[e.Tree] = true
			trees = append(trees, e.Tree)
		}
	}
	return trees, dropped
}

// rechain points refs/tego/latest at a new chain of commits, one per tree
// still in a log, so the trees of dropped snapshots become unreachable.
// A snapshot taken meanwhile may move the ref back onto the old chain:
// then nothing is dropped this time, and nothing is lost.
func (s *Store) rechain(ctx context.Context, trees []string) error {
	if len(trees) > 0 {
		out, err := s.git(ctx, []byte(strings.Join(trees, "\n")+"\n"), "cat-file", "--batch-check")
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[1] == "tree" {
				have[f[0]] = true
			}
		}
		trees = slices.DeleteFunc(slices.Clone(trees), func(t string) bool { return !have[t] })
	}
	if len(trees) == 0 {
		_, err := s.git(ctx, nil, "update-ref", "-d", "refs/tego/latest")
		return err
	}
	prev := ""
	for _, t := range trees {
		args := []string{"commit-tree", t, "-m", "tego snapshot"}
		if prev != "" {
			args = append(args, "-p", prev)
		}
		out, err := s.gitEnv(ctx, []string{"GIT_AUTHOR_NAME=tego", "GIT_AUTHOR_EMAIL=tego@localhost",
			"GIT_COMMITTER_NAME=tego", "GIT_COMMITTER_EMAIL=tego@localhost"}, args...)
		if err != nil {
			return err
		}
		prev = strings.TrimSpace(out)
	}
	_, err := s.git(ctx, nil, "update-ref", "refs/tego/latest", prev)
	return err
}

// dropShared deletes the loose objects the linked project repositories
// hold too: contents snapshotted before the project committed them.
func (s *Store) dropShared(ctx context.Context) {
	data, err := os.ReadFile(filepath.Join(s.gitDir, "objects", "info", "alternates"))
	if err != nil {
		return
	}
	objects := filepath.Join(s.gitDir, "objects")
	var ids []string
	dirs, _ := os.ReadDir(objects)
	for _, d := range dirs {
		if len(d.Name()) != 2 || !d.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(objects, d.Name()))
		for _, f := range files {
			if len(f.Name()) == 38 {
				ids = append(ids, d.Name()+f.Name())
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	for _, alt := range strings.Fields(string(data)) {
		cmd := exec.CommandContext(ctx, "git", "--git-dir="+filepath.Dir(alt), "cat-file", "--batch-check")
		cmd.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(strings.NewReader(string(out)))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) == 3 && len(f[0]) == 40 { // "<id> <type> <size>": the project has it
				_ = os.Remove(filepath.Join(objects, f[0][:2], f[0][2:]))
			}
		}
	}
}
