package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// probeTool records how many of its calls run at the same time.
type probeTool struct {
	name     string
	readOnly bool
	running  atomic.Int32
	peak     atomic.Int32
	out      string
	err      error
}

func (t *probeTool) Def() llm.Tool  { return llm.Tool{Name: t.name} }
func (t *probeTool) ReadOnly() bool { return t.readOnly }
func (t *probeTool) Run(_ context.Context, in json.RawMessage) (string, error) {
	n := t.running.Add(1)
	defer t.running.Add(-1)
	for p := t.peak.Load(); n > p && !t.peak.CompareAndSwap(p, n); p = t.peak.Load() {
	}
	time.Sleep(30 * time.Millisecond)
	if t.out != "" {
		return t.out, t.err
	}
	return "out " + string(in), t.err
}

func calls(name string, n int) []llm.Block {
	var out []llm.Block
	for i := range n {
		out = append(out, llm.Block{Type: llm.BlockToolUse, ID: string(rune('a' + i)), Name: name, Input: json.RawMessage(`{"i":` + string(rune('0'+i)) + `}`)})
	}
	return out
}

func TestReadOnlyCallsRunConcurrentlyInOrder(t *testing.T) {
	ro := &probeTool{name: "read", readOnly: true}
	rw := &probeTool{name: "edit"}
	a := &Agent{UI: nopUI{}}
	a.Tools = append(a.Tools, ro, rw)

	res := a.runCalls(context.Background(), calls("read", 4))
	if ro.peak.Load() < 2 {
		t.Fatalf("read-only calls ran one at a time (peak %d)", ro.peak.Load())
	}
	for i, r := range res {
		if r.ToolUseID != string(rune('a'+i)) || !strings.Contains(r.Content, string(rune('0'+i))) {
			t.Fatalf("result %d out of order: %+v", i, r)
		}
	}
	a.runCalls(context.Background(), calls("edit", 3))
	if rw.peak.Load() != 1 {
		t.Fatalf("state-changing calls overlapped (peak %d)", rw.peak.Load())
	}
}

func TestHugeToolResultIsCapped(t *testing.T) {
	big := "START" + strings.Repeat("x", 500<<10) + "END"
	a := &Agent{UI: nopUI{}}
	a.Tools = append(a.Tools, &probeTool{name: "mcp__db__dump", out: big})
	res := a.runCalls(context.Background(), calls("mcp__db__dump", 1))[0]
	if len(res.Content) > maxToolResult+200 || !strings.HasPrefix(res.Content, "START") || !strings.HasSuffix(res.Content, "END") || !strings.Contains(res.Content, "KB omitted") {
		t.Fatalf("capped result: %d bytes, head %q, tail %q", len(res.Content), res.Content[:10], res.Content[len(res.Content)-10:])
	}
}

func TestStuckRemindsThenStops(t *testing.T) {
	failing := &probeTool{name: "bash", err: errors.New("exit status 1")}
	var replies []*llm.Message
	for range 10 {
		replies = append(replies, &llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "t", Name: "bash", Input: json.RawMessage(`{"command":"make"}`)}}})
	}
	prov := &scripted{replies: replies}
	a := &Agent{Provider: prov, UI: nopUI{}}
	a.Tools = append(a.Tools, failing)
	err := a.Run(context.Background(), "build it")
	if err == nil || !strings.Contains(err.Error(), "repeated 8 times") {
		t.Fatalf("err = %v", err)
	}
	var reminders int
	for i, m := range a.Messages {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolResult && strings.Contains(b.Content, "<system-reminder>") {
				reminders++
				if i != 10 { // results of call 5 (prompt, then call/result pairs)
					t.Fatalf("reminder on message %d", i)
				}
			}
		}
	}
	if reminders != 1 {
		t.Fatalf("reminders = %d, want 1", reminders)
	}
	// A different call resets the streak.
	a.stuck = stuckState{}
	ok := []llm.Block{{Type: llm.BlockToolResult}}
	for i := range 7 {
		c := []llm.Block{{Name: "bash", Input: json.RawMessage(`{"command":"make ` + string(rune('a'+i)) + `"}`)}}
		if err := a.checkStuck(c, ok); err != nil || a.stuck.same != 1 {
			t.Fatalf("distinct calls counted as a streak: %v %d", err, a.stuck.same)
		}
	}
}
