package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

func taskUse(id string, task map[string]any) llm.Message {
	return assistant(call(id, "task", jsonOf(map[string]any{"tasks": []any{task}})))
}

// A plain no to a subagent's call stops that turn only: resumed in the next
// one, the subagent's calls run (they were all refused as "declined").
func TestResumedSubagentAfterDecline(t *testing.T) {
	asked := 0
	ap := &approver{answer: func(Approval) Decision {
		asked++
		return Decision{Allow: asked > 1} // a plain no the first time
	}}
	a, dir, _ := permAgent(t, PermEdits, ap)
	a.Model, a.ContextWindow = "m", 100_000
	a.Tools = append(a.Tools, &TaskTool{Parent: a, Env: &tools.Env{Cwd: dir}})
	touch := func(id, f string) llm.Message {
		return assistant(call(id, "bash", jsonOf(map[string]any{"command": "touch " + f})))
	}
	runWith(t, a,
		taskUse("t1", map[string]any{"name": "Sub", "prompt": "touch s1.go"}),
		touch("b1", "s1.go"), // declined: the subagent and the turn stop
	)
	if _, err := os.Stat(filepath.Join(dir, "s1.go")); err == nil {
		t.Fatal("the declined call ran")
	}
	runWithMore(t, a,
		taskUse("t2", map[string]any{"resume": "Sub", "prompt": "now touch s2.go"}),
		touch("b2", "s2.go"),
		assistant(text("touched s2.go")),
		assistant(text("done")),
	)
	if _, err := os.Stat(filepath.Join(dir, "s2.go")); err != nil {
		t.Fatalf("the resumed subagent's call did not run (asked %d times): %v", asked, err)
	}
	if res, _ := resultFor(a.Messages, "t2"); !strings.Contains(res.Content, "touched s2.go") {
		t.Fatalf("report: %q", res.Content)
	}
}

// A subagent's change to a file the parent read is a change for the parent:
// its write from the stale copy is refused instead of undoing the edit.
func TestSubagentEditCatchesStaleWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.go")
	_ = os.WriteFile(p, []byte("one\ntwo\n"), 0o644)
	env := &tools.Env{Cwd: dir}
	a := &Agent{Model: "m", ContextWindow: 100_000, Tools: tools.Builtins(env)}
	a.Tools = append(a.Tools, &TaskTool{Parent: a, Env: env})
	runWith(t, a,
		assistant(call("r1", "read", `{"path":"f.go"}`)),
		taskUse("t1", map[string]any{"name": "Sub", "prompt": "change two"}),
		assistant(call("e1", "edit", `{"path":"f.go","old_string":"two","new_string":"TWO"}`)), // the subagent
		assistant(text("changed it")),
		assistant(call("w1", "write", jsonOf(map[string]any{"path": "f.go", "content": "one\ntwo\nthree\n"}))), // stale
		assistant(text("done")),
	)
	if res, _ := resultFor(a.Messages, "e1"); res.IsError {
		t.Fatalf("subagent's edit: %q", res.Content)
	}
	res, _ := resultFor(a.Messages, "w1")
	if !res.IsError || !strings.Contains(res.Content, "changed since it was last read") || !strings.Contains(res.Content, "TWO") {
		t.Fatalf("stale write: %+v", res)
	}
	if b, _ := os.ReadFile(p); string(b) != "one\nTWO\n" {
		t.Fatalf("the subagent's edit was overwritten: %q", b)
	}
}

// rewriter is a BeforeTool hook that replaces the input of calls to a tool.
type rewriter map[string]string

func (r rewriter) BeforeTool(_ context.Context, _, tool string, _ json.RawMessage) (json.RawMessage, string) {
	if in, ok := r[tool]; ok {
		return json.RawMessage(in), ""
	}
	return nil, ""
}

func (rewriter) AfterTool(context.Context, string, string, json.RawMessage, string, bool) (string, bool) {
	return "", false
}

// Plan mode and the permission mode judge a call by the input a BeforeTool
// hook gave it, not the one the model sent: an approval (or no need for
// one) doesn't carry over to what the hook made of it.
func TestBeforeToolInputIsWhatIsChecked(t *testing.T) {
	ls := assistant(call("b1", "bash", `{"command":"ls"}`))
	t.Run("permission", func(t *testing.T) {
		ap := &approver{answer: func(Approval) Decision { return Decision{Feedback: "not that"} }}
		a, dir, _ := permAgent(t, PermEdits, ap) // ls runs unasked there; touch asks
		a.Hooks = rewriter{"bash": `{"command":"touch hooked.go"}`}
		runWith(t, a, ls, assistant(text("done")))
		if len(ap.asked) != 1 || !strings.Contains(ap.asked[0].Summary, "touch hooked.go") ||
			!strings.Contains(string(ap.asked[0].Input), "touch hooked.go") {
			t.Fatalf("asked %+v", ap.asked)
		}
		if _, err := os.Stat(filepath.Join(dir, "hooked.go")); err == nil {
			t.Fatal("the hook's command ran unapproved")
		}
	})
	t.Run("path", func(t *testing.T) {
		ap := &approver{answer: func(Approval) Decision { return Decision{Allow: true} }}
		a, dir, _ := permAgent(t, PermAsk, ap)
		a.Hooks = rewriter{"write": `{"path":"other.go","content":"package a\n"}`}
		runWith(t, a, assistant(call("w1", "write", `{"path":"asked.go","content":"x"}`)),
			assistant(text("done")), assistant(text("verified")))
		if len(ap.asked) != 1 || !strings.Contains(ap.asked[0].Summary, "other.go") || strings.Contains(ap.asked[0].Summary, "asked.go") {
			t.Fatalf("asked %v", ap.summaries())
		}
		if _, err := os.Stat(filepath.Join(dir, "other.go")); err != nil {
			t.Fatalf("approved write: %v", err)
		}
	})
	t.Run("plan", func(t *testing.T) {
		dir := t.TempDir()
		plan := &atomic.Bool{}
		plan.Store(true)
		a := &Agent{Plan: plan, Tools: tools.Builtins(&tools.Env{Cwd: dir}), Hooks: rewriter{"bash": `{"command":"touch planned.go"}`}}
		runWith(t, a, ls, assistant(text("Plan: nothing.")))
		if res, _ := resultFor(a.Messages, "b1"); !res.IsError || !strings.Contains(res.Content, "Plan mode is on") {
			t.Fatalf("result %+v", res)
		}
		if _, err := os.Stat(filepath.Join(dir, "planned.go")); err == nil {
			t.Fatal("the hook's command ran in plan mode")
		}
	})
	t.Run("invalid", func(t *testing.T) {
		n := 0
		a := &Agent{Tools: []tools.Tool{runsTool{"mcp__x__y", &n}}, Hooks: rewriter{"mcp__x__y": `{"broken`}}
		runWith(t, a, assistant(call("m1", "mcp__x__y", `{}`)), assistant(text("done")))
		if res, _ := resultFor(a.Messages, "m1"); !res.IsError || !strings.Contains(res.Content, "invalid JSON") || n != 0 {
			t.Fatalf("result %+v, runs %d", res, n)
		}
	})
}

// A refused response ends the turn with ErrRefused: no reminder or
// continuation pushes the model on, its calls don't run, and what it wrote
// is left out of the history, as Anthropic asks.
func TestRefusalEndsTheTurn(t *testing.T) {
	refusal := assistant(text("I can't help with th"), call("b2", "bash", `{"command":"touch x"}`))
	refusal.StopReason = llm.StopRefusal
	bash := &countingTool{}
	ui := &infoUI{}
	a := &Agent{Tools: []tools.Tool{namedTool{name: "edit"}, bashNamed{bash}}}
	// An unverified edit, which would get a reminder to verify it.
	prov := &strict{recorder{replies: []llm.Message{assistant(call("e1", "edit", `{"path":"main.go"}`)), refusal}}}
	a.Provider, a.UI = prov, ui
	if err := a.Run(context.Background(), "do it"); !errors.Is(err, ErrRefused) {
		t.Fatalf("run: %v", err)
	}
	if len(prov.replies) != 0 || len(prov.seen) != 2 {
		t.Fatalf("%d requests, %d replies left", len(prov.seen), len(prov.replies))
	}
	if bash.runs != 0 {
		t.Fatal("the refused response's call ran")
	}
	last := a.Messages[len(a.Messages)-1]
	if last.Role != llm.RoleUser || len(last.Blocks) != 1 || last.Blocks[0].Type != llm.BlockToolResult {
		t.Fatalf("last message %+v", last)
	}
	if r := reminders(a.Messages); len(r) != 0 {
		t.Fatalf("reminders %q", r)
	}

	// Refused before writing anything: nothing is recorded (an empty
	// message would break the next request). A subagent's run fails.
	empty := llm.Message{Role: llm.RoleAssistant, StopReason: llm.StopRefusal}
	sub := &Agent{Subagent: true, Provider: &recorder{replies: []llm.Message{empty}}, UI: nopUI{}}
	if err := sub.Run(context.Background(), "go"); !errors.Is(err, ErrRefused) {
		t.Fatalf("subagent: %v", err)
	}
	if n := len(sub.Messages); n != 1 || sub.Messages[0].Role != llm.RoleUser {
		t.Fatalf("subagent's messages %+v", sub.Messages)
	}
}

// strict is a recorder that fails a request beyond its replies.
type strict struct{ recorder }

func (p *strict) Stream(ctx context.Context, req *llm.Request, onEvent func(llm.Event)) (*llm.Message, error) {
	if len(p.replies) == 0 {
		return nil, errors.New("a request beyond the script")
	}
	return p.recorder.Stream(ctx, req, onEvent)
}

// bashNamed is a countingTool named bash.
type bashNamed struct{ *countingTool }

func (bashNamed) Def() llm.Tool { return llm.Tool{Name: "bash"} }

// dropping streams part of a response, then fails as drops does (nil: the
// whole response).
type dropping struct {
	drops []error
	reply llm.Message
	calls int
}

func (p *dropping) Name() string { return "fake" }

func (p *dropping) Stream(_ context.Context, _ *llm.Request, onEvent func(llm.Event)) (*llm.Message, error) {
	p.calls++
	onEvent(llm.Event{Type: llm.EvText, Text: "Half an ans"})
	if len(p.drops) > 0 {
		err := p.drops[0]
		p.drops = p.drops[1:]
		if err != nil {
			return &llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{text("Half an ans")}}, err
		}
	}
	m := p.reply
	return &m, nil
}

// A response cut off after it began to stream (which the provider doesn't
// retry) is asked for again, once.
func TestStreamDropRetriedOnce(t *testing.T) {
	old := streamRetryWait
	streamRetryWait = 0
	t.Cleanup(func() { streamRetryWait = old })
	dropped := fmt.Errorf("stream interrupted: %w", io.ErrUnexpectedEOF)
	idle := fmt.Errorf("stream interrupted: %w", llm.ErrStreamIdle)
	answer := assistant(text("The whole answer."))
	answer.StopReason = "end_turn"
	for _, c := range []struct {
		name  string
		drops []error
		calls int
		fails bool
	}{
		{"dropped", []error{dropped}, 2, false},
		{"idle", []error{idle}, 2, false},
		{"overloaded", []error{&llm.APIError{Status: 529, Body: "overloaded"}}, 2, false},
		{"twice", []error{dropped, dropped}, 2, true}, // once per request
		{"refused", []error{&llm.APIError{Status: 400, Body: "bad request"}}, 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			prov := &dropping{drops: c.drops, reply: answer}
			ui := &infoUI{}
			a := &Agent{Provider: prov, UI: ui}
			err := a.Run(context.Background(), "go")
			if prov.calls != c.calls || (err != nil) != c.fails {
				t.Fatalf("%d requests, err %v", prov.calls, err)
			}
			retried := strings.Contains(strings.Join(ui.infos, "\n"), "asking again")
			if retried != (c.calls == 2) {
				t.Fatalf("infos %q", ui.infos)
			}
			if c.fails {
				return
			}
			// Only the whole response is recorded.
			if n := len(a.Messages); n != 2 || a.LastText() != "The whole answer." {
				t.Fatalf("messages %+v", a.Messages)
			}
		})
	}
	// A UI that marks a restart gets the notice that way.
	rui := &restartUI{}
	if err := (&Agent{Provider: &dropping{drops: []error{dropped}, reply: answer}, UI: rui}).Run(context.Background(), "go"); err != nil ||
		len(rui.restarts) != 1 || len(rui.infos) != 0 {
		t.Fatalf("restart UI: %v, restarts %q, infos %q", err, rui.restarts, rui.infos)
	}
	// Nothing streamed yet: the provider did its retries; none here.
	quiet := &quietFail{}
	a := &Agent{Provider: quiet, UI: nopUI{}}
	if err := a.Run(context.Background(), "go"); err == nil || quiet.calls != 1 {
		t.Fatalf("%d requests, err %v", quiet.calls, err)
	}
	// Interrupted while it waits to retry: the interruption ends the turn.
	streamRetryWait = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	prov := &dropping{drops: []error{dropped}, reply: answer}
	a = &Agent{Provider: prov, UI: cancelOnInfo{cancel}}
	if err := a.Run(ctx, "go"); !errors.Is(err, context.Canceled) || prov.calls != 1 {
		t.Fatalf("%d requests, err %v", prov.calls, err)
	}
}

type restartUI struct {
	infoUI
	restarts []string
}

func (u *restartUI) OnStreamRestart(note string) { u.restarts = append(u.restarts, note) }

type quietFail struct{ calls int }

func (p *quietFail) Name() string { return "fake" }
func (p *quietFail) Stream(context.Context, *llm.Request, func(llm.Event)) (*llm.Message, error) {
	p.calls++
	return nil, fmt.Errorf("stream interrupted: %w", io.ErrUnexpectedEOF)
}

// cancelOnInfo interrupts the turn at the first notice.
type cancelOnInfo struct{ cancel context.CancelFunc }

func (cancelOnInfo) OnText(string)                                      {}
func (cancelOnInfo) OnThinking(string)                                  {}
func (cancelOnInfo) OnToolStart(string)                                 {}
func (cancelOnInfo) OnToolCall(string, json.RawMessage)                 {}
func (cancelOnInfo) OnToolResult(string, json.RawMessage, string, bool) {}
func (cancelOnInfo) OnTurnEnd(*llm.Message)                             {}
func (u cancelOnInfo) OnInfo(string)                                    { u.cancel() }
