package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// runsTool counts its runs.
type runsTool struct {
	name string
	runs *int
}

func (t runsTool) Def() llm.Tool { return llm.Tool{Name: t.name} }
func (t runsTool) Run(context.Context, json.RawMessage) (string, error) {
	*t.runs++
	return "ok", nil
}

func resultFor(msgs []llm.Message, id string) (llm.Block, bool) {
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolResult && b.ToolUseID == id {
				return b, true
			}
		}
	}
	return llm.Block{}, false
}

func TestCutOffCallIsNotRun(t *testing.T) {
	var edits, writes int
	a := &Agent{Tools: []tools.Tool{runsTool{"edit", &edits}, runsTool{"write", &writes}, namedTool{name: "bash"}}}
	cut := assistant(
		call("e1", "edit", `{"path":"a.go"}`),
		// Valid JSON, but the limit cut the content short.
		call("w1", "write", `{"path":"b.go","content":"package b\nfunc"}`),
	)
	cut.StopReason = "max_tokens"
	runWith(t, a, cut, assistant(call("b1", "bash", `{"command":"go build"}`)), assistant(text("done")))

	if edits != 1 || writes != 0 {
		t.Fatalf("edit ran %d times, write %d times; want 1 and 0", edits, writes)
	}
	res, ok := resultFor(a.Messages, "w1")
	if !ok || !res.IsError || !strings.Contains(res.Content, "output token limit") || !strings.Contains(res.Content, "smaller pieces") {
		t.Fatalf("result for the cut call: %+v", res)
	}
	if res, _ := resultFor(a.Messages, "e1"); res.IsError {
		t.Fatalf("the complete call before it failed: %+v", res)
	}
	if a.cutCall != "" {
		t.Fatal("cutCall left set after the batch")
	}
}

func TestLimitAfterTheCallsRunsThemAll(t *testing.T) {
	var writes int
	a := &Agent{Tools: []tools.Tool{runsTool{"write", &writes}, namedTool{name: "bash"}}}
	// The limit hit while the model wrote text after its call: the call is whole.
	m := assistant(call("w1", "write", `{"path":"b.go","content":"x"}`), text("Now I will"))
	m.StopReason = "max_tokens"
	runWith(t, a, m, assistant(call("b1", "bash", `{"command":"true"}`)), assistant(text("done")))
	if writes != 1 {
		t.Fatalf("write ran %d times", writes)
	}
}

func TestCutOffCall(t *testing.T) {
	m := assistant(text("hi"), call("x", "write", `{}`))
	if cutOffCall(&m) != "" {
		t.Fatal("cut without max_tokens")
	}
	m.StopReason = "max_tokens"
	if cutOffCall(&m) != "x" {
		t.Fatal("last call not cut")
	}
	m.StopReason = llm.StopContextFull
	if cutOffCall(&m) != "x" {
		t.Fatal("last call not cut by a full window")
	}
	m.Blocks = append(m.Blocks, llm.Block{Type: llm.BlockThinking, Text: "…"})
	if cutOffCall(&m) != "" {
		t.Fatal("call cut although a block followed it")
	}
}

// A response the output limit cut off goes on by itself, at most cutResumes
// times a prompt; one that only thought is told to act.
func TestResumeAfterTheOutputLimit(t *testing.T) {
	thought := assistant(llm.Block{Type: llm.BlockThinking, Text: "planning the whole app…"})
	thought.StopReason = "max_tokens"
	cutText := assistant(text("Here is the first half"))
	cutText.StopReason = "max_tokens"

	a := &Agent{}
	prov := runWith(t, a, thought, cutText, assistant(text("done")))
	if got := lastUserText(prov.seen[1]); !strings.Contains(got, "spent the whole output token limit thinking") || !strings.Contains(got, "act now") {
		t.Fatalf("after a cut think: %q", got)
	}
	if got := lastUserText(prov.seen[2]); !strings.Contains(got, "Go on directly from where it stopped") {
		t.Fatalf("after cut text: %q", got)
	}

	// The third cut in a prompt is left to the user.
	a = &Agent{}
	runWith(t, a, thought, thought, thought)
	if last := a.Messages[len(a.Messages)-1]; last.Role != llm.RoleAssistant || last.StopReason != "max_tokens" {
		t.Fatalf("did not stop after %d resumes: %+v", cutResumes, last)
	}
	// A new prompt may resume again.
	runWithMore(t, a, thought, assistant(text("done")))
}

// runWithMore sends a further prompt to an agent runWith set up.
func runWithMore(t *testing.T, a *Agent, replies ...llm.Message) {
	t.Helper()
	prov := &recorder{replies: replies}
	a.Provider = prov
	if err := a.Run(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if len(prov.replies) != 0 {
		t.Fatalf("%d replies left unused", len(prov.replies))
	}
}

// The window filling up mid-response compacts and goes on, once.
func TestContextFullMidResponse(t *testing.T) {
	full := assistant(text("Half an ans"))
	full.StopReason = llm.StopContextFull
	a := &Agent{ContextWindow: 1_000_000}
	prov := runWith(t, a, full, assistant(text("summary of the work")), assistant(text("done")))
	if got := lastUserText(prov.seen[2]); !strings.Contains(got, "context window filled up") {
		t.Fatalf("after compaction: %q", got)
	}
	// Full again in the same prompt: the user decides.
	a = &Agent{ContextWindow: 1_000_000}
	runWith(t, a, full, assistant(text("summary")), full)
	if last := a.Messages[len(a.Messages)-1]; last.StopReason != llm.StopContextFull {
		t.Fatalf("went on: %+v", last)
	}
}

// max_output_tokens reaches the request, the subagents' too.
func TestMaxOutputTokensInTheRequest(t *testing.T) {
	var got []int
	prov := providerFunc(func(req *llm.Request) *llm.Message {
		got = append(got, req.MaxTokens)
		return &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn", Blocks: []llm.Block{text("ok")}}
	})
	a := &Agent{Provider: prov, UI: nopUI{}, MaxOutputTokens: 48000}
	if err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	task := &TaskTool{Parent: a, Env: &tools.Env{Cwd: t.TempDir()}}
	if _, err := task.Run(context.Background(), json.RawMessage(`{"tasks":[{"name":"A","prompt":"p"}]}`)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 48000 || got[1] != 48000 {
		t.Fatalf("max tokens %v", got)
	}
}

type progressUI struct {
	nopUI
	out *[]string
}

func (u progressUI) OnToolOutput(name string, b []byte) { *u.out = append(*u.out, name+":"+string(b)) }

func TestBashOutputReachesTheUIOnlyForTheMainAgent(t *testing.T) {
	for _, sub := range []bool{false, true} {
		var got []string
		a := &Agent{Tools: tools.Builtins(&tools.Env{Cwd: t.TempDir()}), Subagent: sub}
		prov := &recorder{replies: []llm.Message{assistant(call("b1", "bash", `{"command":"echo live"}`)), assistant(text("done"))}}
		a.Provider, a.UI = prov, progressUI{out: &got}
		if err := a.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		if want := !sub; (strings.Join(got, "") == "bash:live\n") != want {
			t.Fatalf("subagent=%v: progress %q", sub, got)
		}
	}
}
