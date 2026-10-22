package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func setpgid() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

func runCtx(t *testing.T, ctx context.Context, tool Tool, args map[string]any) string {
	t.Helper()
	in, _ := json.Marshal(args)
	out, err := tool.Run(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// waitGone waits for every process running marker to end.
func waitGone(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("pgrep", "-f", marker).Run() != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s still running", marker)
}

func alive(marker string) bool { return exec.Command("pgrep", "-f", marker).Run() == nil }

// A plain bash call that leaves a server running (`… &`) has it adopted:
// its output goes on into the shell, and it stops with it.
func TestPlainBashLeftoversAreAdopted(t *testing.T) {
	env, bash, out := bgEnv(t)
	marker := fmt.Sprintf("sleep 31%04d", os.Getpid()%10000)
	res := runCtx(t, context.Background(), bash, map[string]any{
		"command": fmt.Sprintf("(echo before; sleep 1.5; echo 'ready on http://localhost:5173/'; %s) & echo started", marker)})
	if !strings.Contains(res, "started") || !strings.Contains(res, "before") || strings.Contains(res, "ready on") || !strings.Contains(res, "background shell bg1") || !strings.Contains(res, "background: true rather than &") {
		t.Fatalf("result: %q", res)
	}
	if env.RunningShells() != 1 {
		t.Fatalf("running %d", env.RunningShells())
	}
	// What it printed after the shell exited is the shell's.
	r := runCtx(t, context.Background(), out, map[string]any{"id": "bg1", "wait_for": "ready", "wait": 5})
	if !strings.Contains(r, "ready on") || !strings.Contains(r, "running for") {
		t.Fatalf("read: %q", r)
	}
	if p := env.RunningPort(); p != ":5173" {
		t.Fatalf("port %q", p)
	}
	if s := env.Shells(); len(s) != 1 || !s[0].Adopted || s[0].Port != ":5173" {
		t.Fatalf("shells %+v", s)
	}
	runCtx(t, context.Background(), out, map[string]any{"id": "bg1", "kill": true})
	waitGone(t, marker)
	if env.RunningShells() != 0 {
		t.Fatal("still counted")
	}

	// Short-lived leftovers end by themselves: nothing is adopted.
	res = runCtx(t, context.Background(), bash, map[string]any{"command": "(sleep 0.2) & echo quick"})
	if strings.Contains(res, "background shell") || env.RunningShells() != 0 {
		t.Fatalf("adopted a short one: %q", res)
	}
	// Output redirected away: still adopted, without output.
	marker2 := fmt.Sprintf("sleep 32%04d", os.Getpid()%10000)
	res = runCtx(t, context.Background(), bash, map[string]any{"command": fmt.Sprintf("nohup %s >/dev/null 2>&1 &", marker2)})
	if !strings.Contains(res, "background shell bg2") {
		t.Fatalf("nohup: %q", res)
	}
	if n := env.KillShells(); n != 1 {
		t.Fatalf("KillShells %d", n)
	}
	waitGone(t, marker2)
}

// A command that times out or is aborted has its leftovers stopped, not
// adopted; runShell (no adopting caller) stops them too.
func TestLeftoversOfStoppedCommandsDie(t *testing.T) {
	env, _, _ := bgEnv(t)
	marker := fmt.Sprintf("sleep 33%04d", os.Getpid()%10000)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	out, left, _ := runShellLeft(ctx, env.Cwd, marker+" & sleep 5", 10*time.Second)
	if left != nil || !strings.Contains(out, "[aborted]") {
		t.Fatalf("aborted: %q %v", out, left)
	}
	waitGone(t, marker)

	marker = fmt.Sprintf("sleep 34%04d", os.Getpid()%10000)
	if _, err := runShell(context.Background(), env.Cwd, marker+" & echo hi", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	waitGone(t, marker)
}

// A background shell runs while anything of its group does, even after
// the shell itself exits; stopping it stops them.
func TestBackgroundShellLastsAsLongAsItsGroup(t *testing.T) {
	env, bash, out := bgEnv(t)
	marker := fmt.Sprintf("sleep 35%04d", os.Getpid()%10000)
	defer bgStartWaitFor(50 * time.Millisecond)()
	runCtx(t, context.Background(), bash, map[string]any{"command": marker + " & exit 0", "background": true})
	time.Sleep(4 * bgPoll)
	if env.RunningShells() != 1 || !alive(marker) {
		t.Fatalf("ended with its child running: %d", env.RunningShells())
	}
	if r := runCtx(t, context.Background(), out, map[string]any{"id": "bg1", "kill": true}); !strings.Contains(r, "bg1 stopped") {
		t.Fatalf("kill: %q", r)
	}
	waitGone(t, marker)
}

func bgStartWaitFor(d time.Duration) func() {
	old := bgStartWait
	bgStartWait = d
	return func() { bgStartWait = old }
}

// A subagent's shells are its own: it sees only those, its worktree's Env
// shares the registry, and they stop when it finishes.
func TestSubagentShells(t *testing.T) {
	env, bash, out := bgEnv(t)
	defer bgStartWaitFor(50 * time.Millisecond)()
	runCtx(t, context.Background(), bash, map[string]any{"command": "sleep 60", "background": true}) // the session's bg1

	sub := WithShellOwner(context.Background(), "Builder#1")
	wt := env.At(t.TempDir())
	wtBash := &bashTool{wt}
	r := runCtx(t, sub, wtBash, map[string]any{"command": "pwd; sleep 60", "background": true})
	if !strings.Contains(r, "bg2") || !strings.Contains(r, wt.Cwd[len(wt.Cwd)-8:]) {
		t.Fatalf("subagent start: %q", r)
	}
	if env.RunningShells() != 2 {
		t.Fatalf("registry not shared: %d", env.RunningShells())
	}
	// The subagent sees its own; the session sees all.
	if r := runCtx(t, sub, &bashOutputTool{wt}, map[string]any{}); strings.Contains(r, "bg1") || !strings.Contains(r, "bg2") {
		t.Fatalf("subagent list: %q", r)
	}
	if _, err := (&bashOutputTool{wt}).Run(sub, json.RawMessage(`{"id":"bg1","kill":true}`)); err == nil {
		t.Fatal("a subagent stopped the session's shell")
	}
	if r := runCtx(t, context.Background(), out, map[string]any{}); !strings.Contains(r, "bg1") || !strings.Contains(r, "bg2") {
		t.Fatalf("session list: %q", r)
	}
	// It finishes: its shells stop, the session's go on, and the session
	// isn't told about the subagent's.
	if got := env.StopShells("Builder#1"); len(got) != 1 || !strings.HasPrefix(got[0], "bg2 (pwd; sleep 60)") {
		t.Fatalf("StopShells %q", got)
	}
	if env.RunningShells() != 1 {
		t.Fatalf("running %d", env.RunningShells())
	}
	if n := env.ShellNotices(); n != "" {
		t.Fatalf("notices %q", n)
	}
	if env.StopShells("") != nil || env.StopShells("nobody") != nil {
		t.Fatal("StopShells of no one")
	}
	var nilEnv *Env
	if e := nilEnv.At("/x"); e.Cwd != "/x" || nilEnv.StopShells("a") != nil {
		t.Fatal("nil Env")
	}
}

// For the turn-end reminder and the user's /tasks.
func TestShellsForTheUser(t *testing.T) {
	env, bash, _ := bgEnv(t)
	defer bgStartWaitFor(50 * time.Millisecond)()
	runCtx(t, context.Background(), bash, map[string]any{"command": "echo one; echo two; echo 'Local: http://127.0.0.1:3000'; sleep 60", "background": true})
	mid := time.Now()
	time.Sleep(10 * time.Millisecond)
	runCtx(t, context.Background(), bash, map[string]any{"command": "sleep 61", "background": true})
	if got := env.ShellsSince(mid, ""); len(got) != 1 || !strings.HasPrefix(got[0].Describe(), "bg2 `sleep 61` (running") {
		t.Fatalf("since: %+v", got)
	}
	if got := env.ShellsSince(time.Time{}, ""); len(got) != 2 || !strings.HasSuffix(got[0].Describe(), ", :3000)") {
		t.Fatalf("all: %+v", got)
	}
	if got := env.ShellsSince(time.Time{}, "Sub#1"); len(got) != 0 {
		t.Fatalf("another owner's: %+v", got)
	}
	if env.RunningPort() != "" { // two run: no single port
		t.Fatal("port with two shells")
	}
	if tail := env.ShellTail("bg1", 2); tail != "two\nLocal: http://127.0.0.1:3000" {
		t.Fatalf("tail %q", tail)
	}
	if !env.StopShell("bg2") || env.StopShell("bg2") || env.StopShell("bg9") {
		t.Fatal("StopShell")
	}
	if n := env.ShellNotices(); n != "" { // the user stopped it: nothing to tell
		t.Fatalf("notice after the user's stop: %q", n)
	}
	if env.RunningPort() != ":3000" {
		t.Fatalf("port %q", env.RunningPort())
	}
}

// The record names running groups, goes once none runs, and a later tego
// finds what a gone one left.
func TestShellRecordAndStale(t *testing.T) {
	dir := t.TempDir()
	env, bash, _ := bgEnv(t)
	defer bgStartWaitFor(50 * time.Millisecond)()
	path := filepath.Join(dir, fmt.Sprintf("%d.json", os.Getpid()))
	env.RecordShells(path)
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a record with nothing running")
	}
	runCtx(t, context.Background(), bash, map[string]any{"command": "sleep 60", "background": true})
	var rec shellRecord
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &rec) != nil || len(rec.Shells) != 1 || rec.Shells[0].Command != "sleep 60" {
		t.Fatalf("record %s %v", data, err)
	}
	// Our own record isn't stale.
	if s := StaleShells(dir); len(s) != 0 {
		t.Fatalf("own record stale: %+v", s)
	}
	env.KillShells()
	if _, err := os.Stat(path); err == nil {
		t.Fatal("record left after KillShells")
	}

	// A crashed tego's: a group still running, and one gone.
	marker := fmt.Sprintf("sleep 36%04d", os.Getpid()%10000)
	cmd := exec.Command("bash", "-c", marker)
	cmd.SysProcAttr = setpgid()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()
	dead := deadPID(t)
	stale := fmt.Sprintf(`{"pid":%d,"shells":[{"pgid":%d,"command":"npm run dev","cwd":"/w"},{"pgid":999999,"command":"gone"}]}`, dead, cmd.Process.Pid)
	_ = os.WriteFile(filepath.Join(dir, "crashed.json"), []byte(stale), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "empty.json"), []byte(fmt.Sprintf(`{"pid":%d,"shells":[{"pgid":999998}]}`, dead)), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "junk.json"), []byte(`{`), 0o600)
	got := StaleShells(dir)
	if len(got) != 1 || got[0].Command != "npm run dev" || len(got[0].Members) == 0 || !strings.Contains(got[0].Members[0], marker) {
		t.Fatalf("stale %+v", got)
	}
	for _, f := range []string{"empty.json", "junk.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Fatalf("%s kept", f)
		}
	}
	StopStale(got)
	waitGone(t, marker)
	if _, err := os.Stat(filepath.Join(dir, "crashed.json")); err == nil {
		t.Fatal("record kept after StopStale")
	}
}

// deadPID is a pid no process has.
func deadPID(t *testing.T) int {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}
