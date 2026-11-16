package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/akbxr/tego/internal/peer"
)

// startedTegos stands for the tegos session_start opens: each takes the
// session id on its command line and is listed open at once.
type startedTegos struct {
	mu    sync.Mutex
	lines []string
	ids   []string
	got   []peer.Message
	hub   *peer.Hub
}

func (s *startedTegos) Open() []peer.Open {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []peer.Open
	for _, id := range s.ids {
		out = append(out, peer.Open{ID: id})
	}
	return out
}

func (s *startedTegos) Deliver(m peer.Message) peer.Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, m)
	return peer.Outcome{Status: "started"}
}

func (s *startedTegos) open(base string) func(ctx context.Context, dir, label, id, cmdline string) (*pane, error) {
	return func(ctx context.Context, dir, label, id, cmdline string) (*pane, error) {
		s.mu.Lock()
		s.lines = append(s.lines, dir+" | "+label+" | "+cmdline)
		s.ids = append(s.ids, id)
		start := s.hub == nil
		s.mu.Unlock()
		if start {
			hub, err := peer.Listen(base, s)
			if err != nil {
				return nil, err
			}
			s.hub = hub
		}
		return &pane{where: "a test pane", read: func() string { return "" }, close: func() {}}, nil
	}
}

// session_start opens a tego on the session's model, or the one asked
// for, sends it the prompt asking for a notice, and stops at the cap.
func TestSessionStart(t *testing.T) {
	a := peerApp(t, &scripted{})
	a.ag.ProviderName, a.ag.Model, a.ag.Thinking, a.perms = "anthropic", "claude-opus-5-5", "high", "edits"
	tegos := &startedTegos{}
	tool := &startTool{a: a, open: tegos.open(a.cfg.Dir)}
	t.Cleanup(func() { tegos.hub.Close() })
	run := func(in map[string]any) (string, error) {
		data, _ := json.Marshal(in)
		return tool.Run(context.Background(), data)
	}

	out, err := run(map[string]any{"prompt": "Run the test suite and fix what fails", "worktree": true})
	if err != nil {
		t.Fatal(err)
	}
	id := tegos.ids[0]
	if !strings.Contains(out, "Started session "+id+" in a test pane") || !strings.Contains(out, "anthropic/claude-opus-5-5 · high") ||
		!strings.Contains(out, "you get a notice") {
		t.Fatalf("result: %s", out)
	}
	line := tegos.lines[0]
	if !strings.HasPrefix(line, a.cwd+" | Run the test suite and… | ") ||
		!strings.Contains(line, " --worktree --session-id "+id+" -m anthropic/claude-opus-5-5 -t high --permissions edits") {
		t.Fatalf("command: %s", line)
	}
	m := tegos.got[0]
	if m.To != id || m.From != a.ag.Session.ID || m.Text != "Run the test suite and fix what fails" || !m.Notify || !m.CanReply || m.Hops != 1 {
		t.Fatalf("message %+v", m)
	}

	if _, err := run(map[string]any{"prompt": "review", "name": "review", "model": "opus:low", "folder": t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if line := tegos.lines[1]; !strings.Contains(line, "| review | ") || !strings.Contains(line, "-m anthropic/claude-opus") || !strings.Contains(line, "-t low") {
		t.Fatalf("command: %s", line)
	}

	if _, err := run(map[string]any{"prompt": "x", "folder": "/no/such/folder"}); err == nil || !strings.Contains(err.Error(), "no folder") {
		t.Fatalf("missing folder: %v", err)
	}
	if _, err := run(map[string]any{"prompt": "x", "thinking": "loud"}); err == nil || !strings.Contains(err.Error(), "thinking level") {
		t.Fatalf("bad thinking: %v", err)
	}
	for len(tegos.ids) < maxStarted {
		if _, err := run(map[string]any{"prompt": "more"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := run(map[string]any{"prompt": "one too many"}); err == nil || !strings.Contains(err.Error(), "are open") {
		t.Fatalf("over the cap: %v", err)
	}
}

func TestShellLine(t *testing.T) {
	got := shellLine([]string{"/usr/local/bin/tego", "-m", "anthropic/claude-opus-5-5", "--x", "it's here", ""})
	if want := `/usr/local/bin/tego -m anthropic/claude-opus-5-5 --x 'it'\''s here' ''`; got != want {
		t.Fatalf("%s\nwant %s", got, want)
	}
}
