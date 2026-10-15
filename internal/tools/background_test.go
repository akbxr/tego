package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func bgEnv(t *testing.T) (*Env, *bashTool, *bashOutputTool) {
	t.Helper()
	env := &Env{Cwd: t.TempDir()}
	t.Cleanup(func() { env.KillShells() })
	return env, &bashTool{env}, &bashOutputTool{env}
}

func TestBackgroundServerFlow(t *testing.T) {
	env, bash, out := bgEnv(t)
	// A "server": starts, says it is ready after a moment, then keeps running.
	start := time.Now()
	res, err := run(t, bash, map[string]any{"command": "echo booting; sleep 2.5; echo 'listening on :8080'; sleep 60", "background": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "Started background shell bg1") || !strings.Contains(res, "booting") || time.Since(start) > 5*time.Second {
		t.Fatalf("start: %q (%s)", res, time.Since(start))
	}
	if env.RunningShells() != 1 {
		t.Fatalf("running %d", env.RunningShells())
	}
	// Wait for the ready line; "booting" was already read, so it isn't repeated.
	res, err = run(t, out, map[string]any{"id": "bg1", "wait_for": "listening on", "wait": 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "running for") || !strings.Contains(res, "listening on :8080") || strings.Contains(res, "booting") {
		t.Fatalf("wait_for: %q", res)
	}
	// Nothing new since.
	if res, _ = run(t, out, map[string]any{"id": "bg1"}); !strings.Contains(res, "No new output") {
		t.Fatalf("second read: %q", res)
	}
	// A wait that never matches returns after its time.
	t0 := time.Now()
	if res, _ = run(t, out, map[string]any{"id": "bg1", "wait_for": "never", "wait": 1}); !strings.Contains(res, `No new line matched "never"`) || time.Since(t0) > 3*time.Second {
		t.Fatalf("timeout: %q", res)
	}
	// The listing shows it.
	if res, _ = run(t, out, map[string]any{}); !strings.Contains(res, "bg1  running for") {
		t.Fatalf("list: %q", res)
	}
	// kill stops the shell and the sleep it started.
	if res, _ = run(t, out, map[string]any{"id": "bg1", "kill": true}); !strings.Contains(res, "bg1 stopped") {
		t.Fatalf("kill: %q", res)
	}
	if env.RunningShells() != 0 {
		t.Fatal("still running after kill")
	}
	// A killed shell the model stopped itself needs no notice.
	if n := env.ShellNotices(); n != "" {
		t.Fatalf("notice after own kill: %q", n)
	}
}

func TestBackgroundKillReachesChildren(t *testing.T) {
	env, bash, out := bgEnv(t)
	pidFile := filepath.Join(env.Cwd, "child.pid")
	if _, err := run(t, bash, map[string]any{"command": "sleep 300 & echo $! > child.pid; wait", "background": true}); err != nil {
		t.Fatal(err)
	}
	var pid int
	for range 50 {
		if b, err := os.ReadFile(pidFile); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			_, _ = fmtSscan(strings.TrimSpace(string(b)), &pid)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("child never started")
	}
	_, _ = run(t, out, map[string]any{"id": "bg1", "kill": true})
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("child %d survived the kill", pid)
	}
}

func TestBackgroundImmediateFailureAndNotices(t *testing.T) {
	env, bash, out := bgEnv(t)
	res, err := run(t, bash, map[string]any{"command": "echo 'no such script'; exit 3", "background": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "ended (exit code 3) after") || !strings.Contains(res, "no such script") {
		t.Fatalf("immediate failure: %q", res)
	}
	if n := env.ShellNotices(); n != "" {
		t.Fatalf("notice for a failure already reported: %q", n)
	}
	// One that ends later, unread: told once, with the unread output counted.
	if _, err := run(t, bash, map[string]any{"command": "sleep 2.2; echo done building", "background": true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	n := env.ShellNotices()
	if !strings.Contains(n, "bg2") || !strings.Contains(n, "exit code 0") || !strings.Contains(n, "bytes of output not read yet") {
		t.Fatalf("notice: %q", n)
	}
	if again := env.ShellNotices(); again != "" {
		t.Fatalf("told twice: %q", again)
	}
	if res, _ = run(t, out, map[string]any{"id": "bg2"}); !strings.Contains(res, "done building") {
		t.Fatalf("output after the end: %q", res)
	}
	if _, err := run(t, out, map[string]any{"id": "bg9"}); err == nil || !strings.Contains(err.Error(), "bg1, bg2") {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestBackgroundLimitsAndCleanup(t *testing.T) {
	defer func(d time.Duration) { bgStartWait = d }(bgStartWait)
	bgStartWait = 50 * time.Millisecond
	env, bash, _ := bgEnv(t)
	for i := range bgMaxShells {
		if _, err := run(t, bash, map[string]any{"command": "sleep 300", "background": true}); err != nil {
			t.Fatalf("shell %d: %v", i+1, err)
		}
	}
	if _, err := run(t, bash, map[string]any{"command": "sleep 300", "background": true}); err == nil {
		t.Fatal("more than the limit")
	}
	// Blocked commands stay blocked in the background.
	if _, err := run(t, bash, map[string]any{"command": "rm -rf ~", "background": true}); err == nil {
		t.Fatal("blocked command ran in the background")
	}
	if n := env.KillShells(); n != bgMaxShells || env.RunningShells() != 0 {
		t.Fatalf("killed %d, running %d", n, env.RunningShells())
	}
}

func TestBackgroundBufferDropsOldOutput(t *testing.T) {
	s := &bgShell{grew: make(chan struct{})}
	chunk := []byte(strings.Repeat("x", 1024) + "\n")
	for range bgBufBytes/len(chunk) + 50 {
		_, _ = s.Write(chunk)
	}
	text, dropped := s.unread()
	if dropped == 0 || len(text) > bgBufBytes || !strings.Contains(formatNew(text, dropped), "earlier bytes were dropped") {
		t.Fatalf("dropped %d, kept %d", dropped, len(text))
	}
}

func fmtSscan(s string, p *int) (int, error) { return fmt.Sscan(s, p) }
