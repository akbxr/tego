package main

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDescribe(t *testing.T) {
	for _, c := range []struct {
		tool, input string
		details     bool
		want        string
	}{
		{"read", `{"path":"/a/b/main.go"}`, false, "Reading code"},
		{"read", `{"path":"/a/b/main.go"}`, true, "Reading main.go"},
		{"edit", `{"path":"x/y.go"}`, true, "Editing y.go"},
		{"write", `{}`, true, "Editing code"},
		{"grep", `{"pattern":"TODO"}`, false, "Searching the code"},
		{"grep", `{"pattern":"TODO"}`, true, "Searching for TODO"},
		{"bash", `{"command":"go test ./...\necho done"}`, true, "Running go test ./..."},
		{"bash", `{"command":"cat ~/.ssh/id_rsa"}`, false, "Running a command"},
		{"web_search", `{"query":"go 1.26"}`, false, "Searching the web"},
		{"task", `{}`, true, "Running subagents"},
		{"todo", `{}`, false, "Planning"},
		{"mcp__github__create_pr", `{}`, false, "Using github"},
		{"deploy_status", `{}`, false, "Using deploy_status"},
	} {
		if got := describe(c.tool, json.RawMessage(c.input), c.details); got != c.want {
			t.Errorf("%s %s details=%v: %q, want %q", c.tool, c.input, c.details, got, c.want)
		}
	}
	if f := field(strings.Repeat("é", 200)); len([]rune(f)) != 128 {
		t.Errorf("long field: %d runes", len([]rune(f)))
	}
	if f := field("x"); len(f) < 2 {
		t.Errorf("short field %q", f)
	}
}

// fakeDiscord listens where the Discord app does and records what it gets.
type fakeDiscord struct {
	ln       net.Listener
	mu       sync.Mutex
	clientID string
	acts     []map[string]any // each SET_ACTIVITY's args
	closed   bool
	refuse   bool
}

func socketDir(t *testing.T) string {
	// A short path: a unix socket's must fit in about 100 bytes.
	dir, err := os.MkdirTemp("/tmp", "dc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	for _, env := range []string{"XDG_RUNTIME_DIR", "TMP", "TEMP"} {
		t.Setenv(env, "")
	}
	t.Setenv("TMPDIR", dir)
	return dir
}

func listen(t *testing.T, dir string, refuse bool) *fakeDiscord {
	ln, err := net.Listen("unix", filepath.Join(dir, "discord-ipc-0"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDiscord{ln: ln, refuse: refuse}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func frame(op uint32, v any) []byte {
	p, _ := json.Marshal(v)
	b := make([]byte, 8+len(p))
	binary.LittleEndian.PutUint32(b, op)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(p)))
	copy(b[8:], p)
	return b
}

func (f *fakeDiscord) serve(c net.Conn) {
	defer c.Close()
	for {
		var head [8]byte
		if _, err := io.ReadFull(c, head[:]); err != nil {
			return
		}
		body := make([]byte, binary.LittleEndian.Uint32(head[4:]))
		if _, err := io.ReadFull(c, body); err != nil {
			return
		}
		var m map[string]any
		json.Unmarshal(body, &m)
		switch binary.LittleEndian.Uint32(head[:4]) {
		case opHandshake:
			f.mu.Lock()
			f.clientID, _ = m["client_id"].(string)
			f.mu.Unlock()
			if f.refuse {
				c.Write(frame(opClose, map[string]any{"code": 4000, "message": "Invalid Client ID"}))
				return
			}
			c.Write(frame(opFrame, map[string]any{"cmd": "DISPATCH", "evt": "READY", "data": map[string]any{"v": 1}}))
			c.Write(frame(opPing, map[string]any{"n": 1}))
		case opFrame:
			args, _ := m["args"].(map[string]any)
			f.mu.Lock()
			f.acts = append(f.acts, args)
			f.mu.Unlock()
			c.Write(frame(opFrame, map[string]any{"cmd": "SET_ACTIVITY", "nonce": m["nonce"], "evt": nil}))
		case opClose:
			f.mu.Lock()
			f.closed = true
			f.mu.Unlock()
			return
		}
	}
}

func (f *fakeDiscord) snapshot() (string, []map[string]any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clientID, append([]map[string]any(nil), f.acts...), f.closed
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func fastTimers(t *testing.T) {
	gap, retry := minGap, retryEvery
	minGap, retryEvery = 30*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { minGap, retryEvery = gap, retry })
}

func TestPresence(t *testing.T) {
	fastTimers(t)
	f := listen(t, socketDir(t), false)
	off := false
	p := newPresence(config{ClientID: "123", ShowProject: &off, LargeImage: "https://example.com/tego.png"})
	p.set(func(p *presence) { p.project, p.state = "secret-project", "Waiting for a prompt" })
	p.set(func(p *presence) { p.state = "Thinking" })
	p.set(func(p *presence) { p.state = "Editing code" })

	last := func() map[string]any {
		_, acts, _ := f.snapshot()
		if len(acts) == 0 {
			return nil
		}
		a, _ := acts[len(acts)-1]["activity"].(map[string]any)
		return a
	}
	waitFor(t, "the latest state", func() bool { a := last(); return a != nil && a["state"] == "Editing code" })
	id, acts, _ := f.snapshot()
	a := last()
	if id != "123" || a["details"] != "Coding with tego" || strings.Contains(js(acts), "secret-project") {
		t.Fatalf("client %q, activity %s", id, js(a))
	}
	if ts := a["timestamps"].(map[string]any)["start"].(float64); time.Since(time.Unix(int64(ts), 0)) > time.Minute {
		t.Errorf("start %v", ts)
	}
	if as := a["assets"].(map[string]any); as["large_image"] != "https://example.com/tego.png" || as["large_text"] != "tego" {
		t.Errorf("assets %v", as)
	}
	if len(acts) > 3 {
		t.Errorf("%d updates for 3 quick changes: not spaced", len(acts))
	}
	// The same status again is not sent again.
	n := len(acts)
	p.set(func(p *presence) { p.state = "Editing code" })
	time.Sleep(3 * minGap)
	if _, acts, _ := f.snapshot(); len(acts) != n {
		t.Errorf("an unchanged status was sent again: %s", js(acts[n:]))
	}
	if !p.connected() {
		t.Error("not connected")
	}

	// Stopping clears the status and says goodbye.
	p.stop()
	waitFor(t, "the status cleared and the goodbye", func() bool {
		_, acts, closed := f.snapshot()
		_, has := acts[len(acts)-1]["activity"]
		return !has && closed
	})
}

func TestDetailsLine(t *testing.T) {
	on, off := true, false
	for _, c := range []struct {
		project, model *bool
		want           string
	}{
		{nil, nil, "Working on tego(Opus 5.5)"},
		{&off, nil, "Using Opus 5.5"},
		{&on, &off, "Working on tego"},
		{&off, &off, "Coding with tego"},
	} {
		p := &presence{cfg: config{ShowProject: c.project, ShowModel: c.model}, project: "tego", model: "Opus 5.5", state: "Thinking"}
		if got := p.activity().Details; got != c.want {
			t.Errorf("project %v model %v: %q, want %q", c.project, c.model, got, c.want)
		}
	}
	// No model known yet: the project alone.
	p := &presence{project: "tego", state: "Thinking"}
	if got := p.activity().Details; got != "Working on tego" {
		t.Errorf("no model: %q", got)
	}
}

// Discord started after tego: found by the retry. A wrong client id:
// the error says so.
func TestPresenceWaitsForDiscord(t *testing.T) {
	fastTimers(t)
	dir := socketDir(t)
	p := newPresence(config{ClientID: "123"})
	defer p.stop()
	p.set(func(p *presence) { p.project, p.state = "tego", "Thinking" })
	waitFor(t, "the first attempt", func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.lastErr != "" })
	if p.connected() {
		t.Fatal("connected without Discord")
	}
	f := listen(t, dir, false)
	waitFor(t, "the retry to connect", func() bool { _, acts, _ := f.snapshot(); return len(acts) > 0 })
	if _, acts, _ := f.snapshot(); acts[0]["activity"].(map[string]any)["details"] != "Working on tego" {
		t.Fatalf("activity %s", js(acts[0]))
	}
}

func TestWrongClientID(t *testing.T) {
	listen(t, socketDir(t), true)
	if _, err := connect("nope"); err == nil || !strings.Contains(err.Error(), "Invalid Client ID") {
		t.Fatalf("err %v", err)
	}
}

func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
