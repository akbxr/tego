package agent

import (
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/session"
)

func results(m llm.Message) map[string]bool {
	ids := map[string]bool{}
	for _, b := range m.Blocks {
		if b.Type == llm.BlockToolResult {
			ids[b.ToolUseID] = b.IsError && b.Content == interruptedResult
		}
	}
	return ids
}

// validCalls fails unless every tool call in msgs is answered by the next
// message, as providers require.
func validCalls(t *testing.T, msgs []llm.Message) {
	t.Helper()
	for i, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type != llm.BlockToolUse {
				continue
			}
			if i+1 == len(msgs) || msgs[i+1].Role != llm.RoleUser {
				t.Fatalf("call %s at %d is not followed by results", b.ID, i)
			}
			if _, ok := results(msgs[i+1])[b.ID]; !ok {
				t.Fatalf("call %s at %d has no result in the next message", b.ID, i)
			}
		}
	}
}

func TestAnswerInterrupted(t *testing.T) {
	prompt := llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{text("go")}}
	two := assistant(call("a", "bash", `{}`), call("b", "read", `{}`))

	// Killed while the calls ran: the history ends with them.
	out, tail, n := answerInterrupted([]llm.Message{prompt, two})
	if n != 2 || tail == nil || len(out) != 2 {
		t.Fatalf("n %d, tail %v, %d messages", n, tail, len(out))
	}
	if r := results(*tail); !r["a"] || !r["b"] {
		t.Fatalf("tail results %v", r)
	}

	// Continued by an older version: a prompt follows the calls.
	broken := []llm.Message{prompt, two, prompt, assistant(text("ok"))}
	out, tail, n = answerInterrupted(broken)
	if n != 2 || tail != nil || len(out) != 5 {
		t.Fatalf("n %d, tail %v, %d messages", n, tail, len(out))
	}
	validCalls(t, out)
	if len(broken[2].Blocks) != 1 {
		t.Fatal("the stored message was changed in place")
	}

	// One call answered, the other not: its result joins the answered one.
	partial := []llm.Message{prompt, two, {Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "a", Content: "ok"}}}}
	out, _, n = answerInterrupted(partial)
	if n != 1 || len(out) != 3 || len(out[2].Blocks) != 2 || len(partial[2].Blocks) != 1 {
		t.Fatalf("n %d, %d messages, %d results", n, len(out), len(out[2].Blocks))
	}
	validCalls(t, out)

	// A complete history is left alone.
	done := []llm.Message{prompt, two, {Role: llm.RoleUser, Blocks: []llm.Block{
		{Type: llm.BlockToolResult, ToolUseID: "a"}, {Type: llm.BlockToolResult, ToolUseID: "b"}}}}
	if out, tail, n := answerInterrupted(done); n != 0 || tail != nil || &out[0] != &done[0] {
		t.Fatalf("complete history changed: n %d", n)
	}
}

// tego was killed while a call ran; the session is resumed and continued.
func TestResumeAfterInterruptedCall(t *testing.T) {
	base, cwd := t.TempDir(), t.TempDir()
	s, err := session.New(base, cwd)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{text("run the tests")}},
		assistant(call("c1", "bash", `{"command":"go test ./..."}`))} {
		if err := s.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	s, msgs, err := session.Open(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{Session: s}
	a.SetHistory(msgs)
	prov := runWith(t, a, assistant(text("I'll check whether the tests ran.")))
	validCalls(t, prov.seen[0])
	s.Close()

	// The answer was recorded: opening the session again adds nothing.
	_, msgs, err = session.Open(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, tail, n := answerInterrupted(msgs); n != 0 || tail != nil {
		t.Fatalf("reopened session still has %d unanswered call(s)", n)
	}
	validCalls(t, msgs)
}
