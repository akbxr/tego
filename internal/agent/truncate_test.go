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
	m.Blocks = append(m.Blocks, llm.Block{Type: llm.BlockThinking, Text: "…"})
	if cutOffCall(&m) != "" {
		t.Fatal("call cut although a block followed it")
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
