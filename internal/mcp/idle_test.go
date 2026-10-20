package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func starts(t *testing.T, path string) []string {
	t.Helper()
	data, _ := os.ReadFile(path)
	return strings.Fields(string(data))
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func state(m *Manager, i int) Status { return m.Status()[i] }

// A local server stops when left idle and starts again at the next call;
// once its tools are cached, the next session starts it only when called.
func TestServerOnDemand(t *testing.T) {
	dir := t.TempDir()
	startsFile := filepath.Join(dir, "starts")
	cfgs := []ServerConfig{
		{Name: "fake", Type: "stdio", Command: os.Args[0], Env: map[string]string{"TEGO_MCP_FAKE": "1", "TEGO_MCP_STARTS": startsFile}},
		{Name: "kept", Type: "stdio", Command: os.Args[0], Env: map[string]string{"TEGO_MCP_FAKE": "1"}, KeepAlive: true},
	}
	o := Options{IdleAfter: 150 * time.Millisecond, CacheDir: filepath.Join(dir, "cache")}

	// No cache yet: it connects at the start, as before, and lists its tools.
	m := StartWith(cfgs, dir, "", o)
	m.Wait(20 * time.Second)
	if st := state(m, 0); st.State != StateConnected || st.Tools != 6 {
		t.Fatalf("first start: %+v", st)
	}
	before := m.Tools()
	waitFor(t, "the idle server to stop", func() bool { return state(m, 0).State == StateIdle })
	pid := starts(t, startsFile)[0]
	waitFor(t, "its process to exit", func() bool { return !alive(pid) })
	if st := state(m, 0); st.Tools != 6 || len(m.Tools()) != len(before) {
		t.Fatalf("idle server's tools: %+v, %d tools", st, len(m.Tools()))
	}
	if st := state(m, 1); st.State != StateConnected {
		t.Fatalf("keepAlive server: %+v", st)
	}
	// A call starts it again, through the tool listed before it stopped.
	if got := call(t, toolMap(m)["mcp__fake__echo"], `{"text":"again"}`); got != "again\n\nsecond" {
		t.Fatalf("echo after waking: %q", got)
	}
	if n := len(starts(t, startsFile)); n != 2 {
		t.Fatalf("%d starts, want 2", n)
	}
	m.Close()

	// The next session: the cached list, no process until a call.
	m = StartWith(cfgs, dir, "", o)
	defer m.Close()
	if st := state(m, 0); st.State != StateIdle || st.Tools != 6 {
		t.Fatalf("cached start: %+v", st)
	}
	m.Wait(20 * time.Second) // the keepAlive one connects
	if len(toolMap(m)) != 12 {
		t.Fatalf("tools before any start: %d", len(toolMap(m)))
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(starts(t, startsFile)); n != 2 {
		t.Fatalf("started without a call: %d starts", n)
	}
	if got := call(t, toolMap(m)["mcp__fake__echo"], `{"text":"lazy"}`); got != "lazy\n\nsecond" {
		t.Fatalf("echo: %q", got)
	}
	if st := state(m, 0); st.State != StateConnected || len(starts(t, startsFile)) != 3 {
		t.Fatalf("after the call: %+v, %d starts", st, len(starts(t, startsFile)))
	}

	// Another command line is another cache entry: connects at the start.
	other := append([]ServerConfig(nil), cfgs[0])
	other[0].Args = []string{"-v"}
	m2 := StartWith(other, dir, "", o)
	defer m2.Close()
	if st := state(m2, 0); st.State == StateIdle {
		t.Fatalf("changed command started from the cache: %+v", st)
	}
	// Without IdleAfter, servers run for the whole session.
	m3 := StartWith(cfgs[:1], dir, "", Options{CacheDir: o.CacheDir})
	defer m3.Close()
	if st := state(m3, 0); st.State == StateIdle {
		t.Fatalf("on demand with IdleAfter 0: %+v", st)
	}
}

// A server that dies is started again by the next call.
func TestServerRestartsAfterExit(t *testing.T) {
	dir := t.TempDir()
	startsFile := filepath.Join(dir, "starts")
	m := StartWith([]ServerConfig{{Name: "fake", Type: "stdio", Command: os.Args[0],
		Env: map[string]string{"TEGO_MCP_FAKE": "1", "TEGO_MCP_STARTS": startsFile}}}, dir, "", Options{IdleAfter: time.Hour})
	defer m.Close()
	m.Wait(20 * time.Second)
	pid := starts(t, startsFile)[0]
	kill(t, pid)
	waitFor(t, "the exit to be seen", func() bool { return state(m, 0).State == StateIdle })
	if got := call(t, toolMap(m)["mcp__fake__echo"], `{"text":"back"}`); got != "back\n\nsecond" {
		t.Fatalf("echo after the exit: %q", got)
	}
}

func alive(pid string) bool {
	p, err := os.FindProcess(atoi(pid))
	return err == nil && p.Signal(syscall.Signal(0)) == nil
}

func kill(t *testing.T, pid string) {
	t.Helper()
	p, err := os.FindProcess(atoi(pid))
	if err != nil {
		t.Fatal(err)
	}
	p.Kill()
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}
