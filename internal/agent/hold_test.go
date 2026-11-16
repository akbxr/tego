package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// orderTool records when it runs; it may change files (roOrderTool reads).
type orderTool struct {
	name string
	log  *orderLog
}

type orderLog struct {
	mu  sync.Mutex
	got []string
}

func (l *orderLog) add(s string) {
	l.mu.Lock()
	l.got = append(l.got, s)
	l.mu.Unlock()
}

func (t orderTool) Def() llm.Tool { return llm.Tool{Name: t.name} }
func (t orderTool) Run(context.Context, json.RawMessage) (string, error) {
	t.log.add(t.name)
	return "ok", nil
}

type roOrderTool struct{ orderTool }

func (roOrderTool) ReadOnly() bool { return true }

// The /undo snapshot runs beside the first request: a read goes ahead, a
// call that may change files waits for it, and the user hears why once
// the wait is long.
func TestHoldTools(t *testing.T) {
	old := holdNote
	holdNote = 20 * time.Millisecond
	defer func() { holdNote = old }()
	log := &orderLog{}
	done := &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn", Blocks: []llm.Block{text("ok")}}
	prov := &scripted{replies: []*llm.Message{
		callMsg(use("1", "look", `{}`)),
		callMsg(use("2", "change", `{}`)),
		done,
	}}
	ui := &infoUI{}
	a := &Agent{Provider: prov, Model: "m", ContextWindow: 100_000, UI: ui,
		Tools: []tools.Tool{roOrderTool{orderTool{name: "look", log: log}}, orderTool{name: "change", log: log}}}
	hold := make(chan struct{})
	a.HoldTools(hold)
	go func() {
		time.Sleep(150 * time.Millisecond)
		log.add("snapshot")
		close(hold)
	}()
	if err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log.got, " "); got != "look snapshot change" {
		t.Fatalf("order: %s", got)
	}
	if len(ui.infos) != 1 || !strings.Contains(ui.infos[0], "waiting for the /undo snapshot of the files before change runs") {
		t.Fatalf("infos %q", ui.infos)
	}

	// Done: nothing waits, and nothing is said.
	log.got, ui.infos = nil, nil
	prov.replies = []*llm.Message{callMsg(use("3", "change", `{}`)), done}
	start := time.Now()
	if err := a.Run(context.Background(), "again"); err != nil || len(ui.infos) != 0 || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("%v %q %v", err, ui.infos, time.Since(start))
	}

	// A stopped turn doesn't wait for it.
	a.HoldTools(make(chan struct{}))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ch := make(chan struct{})
	go func() { a.waitHold(ctx, use("4", "change", `{}`)); close(ch) }()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("waited past the turn's end")
	}
}
