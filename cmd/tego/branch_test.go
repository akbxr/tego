package main

import (
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/session"
)

func userText(texts ...string) llm.Message {
	m := llm.Message{Role: llm.RoleUser}
	for _, t := range texts {
		m.Blocks = append(m.Blocks, llm.Block{Type: llm.BlockText, Text: t})
	}
	return m
}

func answer(text string, calls ...string) llm.Message {
	m := llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: text}}}
	for _, id := range calls {
		m.Blocks = append(m.Blocks, llm.Block{Type: llm.BlockToolUse, ID: id, Name: "read"})
	}
	return m
}

func results(ids ...string) llm.Message {
	m := llm.Message{Role: llm.RoleUser}
	for _, id := range ids {
		m.Blocks = append(m.Blocks, llm.Block{Type: llm.BlockToolResult, ToolUseID: id, Text: "ok"})
	}
	return m
}

const summary = "<context-summary>\nEarlier conversation was compacted.\n</context-summary>"

// texts renders a history compactly: S for the summary, P:/A: for prompts
// and answers, R for tool results.
func texts(msgs []llm.Message) string {
	var out []string
	for i := range msgs {
		m := &msgs[i]
		var parts []string
		if _, ok := summaryOnly(*m); ok {
			parts = append(parts, "S")
		}
		switch {
		case promptText(m) != "":
			parts = append(parts, "P:"+promptText(m))
		case assistantText(m) != "":
			parts = append(parts, "A:"+assistantText(m))
		case m.Role == llm.RoleUser && len(m.Blocks) > 0 && m.Blocks[0].Type == llm.BlockToolResult:
			parts = append(parts, "R")
		}
		out = append(out, strings.Join(parts, "+"))
	}
	return strings.Join(out, " ")
}

func TestReplacementPoints(t *testing.T) {
	// The compaction that broke a rewind: the summary went in front of the
	// first kept prompt, and the prompt being answered came last.
	repl := []llm.Message{
		userText(summary, "commit it"),
		answer("committing", "c1"),
		results("c1"),
		answer("committed"),
		userText("find e2e tests"),
	}
	var got []string
	for _, p := range replacementPoints(repl) {
		got = append(got, p.prompt+"|"+p.answer+" => "+texts(p.keep))
	}
	want := []string{
		"commit it| => S",
		"|committing => S+P:commit it A:committing R",
		"|committed => S+P:commit it A:committing R A:committed",
		"find e2e tests| => S+P:commit it A:committing R A:committed",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("points:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if texts(repl) != "S+P:commit it A:committing R A:committed P:find e2e tests" {
		t.Fatalf("the replacement itself changed: %s", texts(repl))
	}

	// A summary of its own comes first, as a point to continue from.
	pts := replacementPoints([]llm.Message{userText(summary), answer("on it")})
	if len(pts) != 2 || texts(pts[0].keep) != "S" || pts[0].whole || !pts[1].whole {
		t.Fatalf("summary of its own: %+v", pts)
	}
}

// Rewinding to a point inside a compaction continues from there, and the
// prompt the compaction ended with never stays in the history unanswered.
func TestCheckoutInsideCompaction(t *testing.T) {
	s, err := session.New(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Append(userText("old prompt"))
	repl := []llm.Message{userText(summary, "commit it"), answer("committed"), userText("find e2e tests")}
	s.Compacted(repl)
	comp := s.Leaf()
	s.Append(answer("searching"))
	nodes := s.ActivePath()
	if nodes[0].ID != comp {
		t.Fatal("the active path does not start at the compaction")
	}

	pts := replacementPoints(nodes[0].Replace)
	last := pts[len(pts)-1]
	if last.prompt != "find e2e tests" {
		t.Fatalf("last point %+v", last)
	}
	hist, err := checkoutInside(s, nodes[0], last)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(hist); got != "S+P:commit it A:committed" {
		t.Fatalf("history after rewinding to the last prompt: %s", got)
	}
	if got := texts(s.History(s.Leaf())); got != "S+P:commit it A:committed" {
		t.Fatalf("the session disagrees: %s", got)
	}
	// The next prompt follows the answer, not the unanswered prompt.
	s.Append(userText("compare edit tools"))
	if got := texts(s.History(s.Leaf())); got != "S+P:commit it A:committed P:compare edit tools" {
		t.Fatalf("after the next prompt: %s", got)
	}
	// The old path is still there.
	if got := texts(s.History(comp)); got != texts(repl) {
		t.Fatalf("the compaction changed: %s", got)
	}

	// Rewinding to an answer that ends the replacement adds no node.
	whole := replPoint{keep: repl, whole: true}
	if _, err := checkoutInside(s, nodes[0], whole); err != nil || s.Leaf() != comp {
		t.Fatalf("whole: leaf %s, want %s (%v)", s.Leaf(), comp, err)
	}
}

func TestHistoryBeforeKeepsAMergedSummary(t *testing.T) {
	msgs := []llm.Message{userText(summary, "commit it"), answer("done")}
	if got := texts(historyBefore(msgs, 0)); got != "S" {
		t.Fatalf("before the first prompt: %q", got)
	}
	if got := texts(historyBefore(msgs, 1)); got != "S+P:commit it" {
		t.Fatalf("before the answer: %q", got)
	}
}
