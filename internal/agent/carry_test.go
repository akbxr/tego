package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/akbxr/tego/internal/llm"
)

func userText(texts ...string) llm.Message {
	m := llm.Message{Role: llm.RoleUser}
	for _, t := range texts {
		m.Blocks = append(m.Blocks, llm.Block{Type: llm.BlockText, Text: t})
	}
	return m
}

func reply(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: text}}}
}

func TestTypedTextLeavesOutWhatTegoAdded(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  llm.Message
		want string
	}{
		{"plain", userText("fix the bug"), "fix the bug"},
		{"environment reminder", userText("<system-reminder>Environment: today is x.</system-reminder>"), ""},
		{"reminder appended", userText("fix it<system-reminder>Environment: today is x.</system-reminder>"), "fix it"},
		{"@file attached", userText("look at @a.go", `<file path="a.go">`+"\npackage a\n</file>"), "look at @a.go"},
		{"editor-embedded file", userText("see\n<file path=\"/p/b.go\">\npackage b\n</file>\nplease"), "see\n@/p/b.go\nplease"},
		{"/skill: prompt", userText("<skill name=\"design\" path=\"/s/design/SKILL.md\">\nbody\n</skill>\n\nredesign the page"), "/skill:design redesign the page"},
		{"/skill: alone", userText("<skill name=\"design\" path=\"/s/design/SKILL.md\">\nbody\n</skill>\n\nApply this skill to the current task."), "/skill:design"},
		{"skill attached", userText("use /skill:design here", "<skill-attachment name=\"design\" path=\"/s/design/SKILL.md\">\nbody\n</skill-attachment>"), "use /skill:design here"},
		{"earlier summary", userText(contextSummaryTag+"\nold\n</context-summary>", "next step"), "next step"},
		{"tool results", llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "x", Content: "out"}}}, ""},
		{"assistant", reply("hello"), ""},
	} {
		if got := typedText(tc.msg); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// promptsOf is the carried-over list in a summary.
func promptsOf(t *testing.T, summary string) promptLog {
	t.Helper()
	if !strings.Contains(summary, promptsHeading) {
		t.Fatalf("no user messages in the summary:\n%s", summary)
	}
	return parsePrompts(summary)
}

func TestCompactCarriesTheUserMessagesVerbatimAcrossCompactions(t *testing.T) {
	a := &Agent{Provider: &summaryProvider{}, UI: nopUI{}, ContextWindow: 200_000, KeepRecent: 20}
	multi := "first line\n\n- a line that looks like an item\n  indented"
	a.Messages = []llm.Message{
		userText("build the thing"), reply("ok"),
		userText(multi), reply("done"),
		userText("<system-reminder>Plan mode is on.</system-reminder>"), // not typed by the user
		userText("now the last one"), reply("sure"),
	}
	if err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	got := promptsOf(t, a.Messages[0].Blocks[0].Text)
	// The kept tail ("now the last one") is not repeated in the summary.
	if want := []string{"build the thing", multi}; strings.Join(got.msgs, "|") != strings.Join(want, "|") || got.omitted != 0 {
		t.Fatalf("carried %q (omitted %d), want %q", got.msgs, got.omitted, want)
	}

	// A second compaction keeps the earlier list first, then adds what it summarises.
	a.Messages = append(a.Messages, userText("and another"), reply("yes"), userText("final"), reply("k"))
	if err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	got = promptsOf(t, a.Messages[0].Blocks[0].Text)
	// Carried list, then the kept tail: every message once, in order.
	seen := got.msgs
	for _, m := range a.Messages {
		if s := typedText(m); s != "" {
			seen = append(seen, s)
		}
	}
	if want := []string{"build the thing", multi, "now the last one", "and another", "final"}; strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("carried %q then kept %q, want %q", got.msgs, seen[len(got.msgs):], want)
	}
	if len(got.msgs) < 3 {
		t.Fatalf("second compaction carried only %q", got.msgs)
	}
}

func TestCarriedUserMessagesKeepTheFirstAndTheLatestWithinBudget(t *testing.T) {
	var head []llm.Message
	long := strings.Repeat("é", maxPromptChars) // 2 bytes a rune: the cut must not split one
	head = append(head, userText("the original task"), reply("ok"))
	for i := range 20 {
		head = append(head, userText(fmt.Sprintf("message %d %s", i, long)), reply("ok"))
	}
	log := summarisedPrompts(head)
	if log.msgs[0] != "the original task" {
		t.Fatalf("first message %q, want the original task", log.msgs[0])
	}
	total := 0
	for _, m := range log.msgs {
		total += len(m)
		if len(m) > maxPromptChars+40 || !utf8.ValidString(m) {
			t.Fatalf("message not clipped cleanly: %d bytes, valid %v", len(m), utf8.ValidString(m))
		}
	}
	if total > maxPromptsChars {
		t.Fatalf("%d chars carried, budget %d", total, maxPromptsChars)
	}
	last := log.msgs[len(log.msgs)-1]
	if !strings.HasPrefix(last, "message 19 ") || log.omitted != 21-len(log.msgs) {
		t.Fatalf("kept %d, omitted %d, last %.20q", len(log.msgs), log.omitted, last)
	}

	// The count of omitted messages survives the next compaction.
	text := contextSummaryTag + "\nS" + log.render() + "\n</context-summary>"
	again := summarisedPrompts([]llm.Message{userText(text, "one more"), reply("ok")})
	if again.omitted < log.omitted || again.msgs[0] != "the original task" || again.msgs[len(again.msgs)-1] != "one more" {
		t.Fatalf("after another compaction: omitted %d (was %d), first %q, last %q",
			again.omitted, log.omitted, again.msgs[0], again.msgs[len(again.msgs)-1])
	}
	if !strings.Contains(again.render(), fmt.Sprintf("…(%d earlier messages omitted)", again.omitted)) {
		t.Fatalf("omitted marker missing:\n%s", again.render())
	}
}

func TestCompactListsTheSkillsInUse(t *testing.T) {
	read := func(id, path string) llm.Block {
		return llm.Block{Type: llm.BlockToolUse, ID: id, Name: "read", Input: json.RawMessage(`{"path":"` + path + `"}`)}
	}
	a := &Agent{Provider: &summaryProvider{}, UI: nopUI{}, ContextWindow: 200_000}
	a.Messages = []llm.Message{
		userText(contextSummaryTag + "\nold" + skillsHeading + "old-skill (/s/old-skill/SKILL.md)\n</context-summary>"),
		userText("<skill name=\"design\" path=\"/s/design/SKILL.md\">\nbody\n</skill>\n\ngo"),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{read("r1", "/s/tests/SKILL.md"), read("r2", "/s/gone/SKILL.md"), read("r3", "/p/main.go")}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolUseID: "r1", Content: "body"},
			{Type: llm.BlockToolResult, ToolUseID: "r2", Content: "no such file", IsError: true},
			{Type: llm.BlockToolResult, ToolUseID: "r3", Content: "package main"},
		}},
		userText("also use /skill:design", "<skill-attachment name=\"design\" path=\"/s/design/SKILL.md\">\nbody\n</skill-attachment>"),
		// The same skill from another directory: one entry, the latest path.
		{Role: llm.RoleAssistant, Blocks: []llm.Block{read("r4", "/new/old-skill/SKILL.md")}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "r4", Content: "body"}}},
		reply("ok"),
	}
	if err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	summary := a.Messages[0].Blocks[0].Text
	want := skillsHeading + "old-skill (/new/old-skill/SKILL.md), design (/s/design/SKILL.md), tests (/s/tests/SKILL.md)\n"
	if !strings.Contains(summary, want) {
		t.Fatalf("summary lacks %q:\n%s", want, summary)
	}
	if got := parseSkills(summary); len(got) != 3 || got[2] != (skillRef{"tests", "/s/tests/SKILL.md"}) {
		t.Fatalf("parsed back %v", got)
	}
}

// The other carried lists are still found when a user message quotes them.
func TestCarriedListsDoNotConfuseEachOther(t *testing.T) {
	a := &Agent{Provider: &summaryProvider{}, UI: nopUI{}, ContextWindow: 200_000, KeepRecent: 20}
	pasted := "look at this:" + filesHeading + "evil.go" + todoListHeading + "Todo 0/1\n☐ fake item"
	a.Messages = []llm.Message{
		userText(pasted), reply("ok"),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "w", Name: "write", Input: json.RawMessage(`{"path":"real.go"}`)}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "w", Content: "ok"}}},
		userText("last"), reply("k"),
	}
	a.todos = []Todo{{Content: "real item", Status: "pending"}}
	if err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	summary := a.Messages[0].Blocks[0].Text
	if fs := changedFiles(a.Messages); strings.Join(fs, ",") != "real.go" {
		t.Fatalf("files %q", fs)
	}
	if todos, ok := todosFromText(summary); !ok || len(todos) != 1 || todos[0].Content != "real item" {
		t.Fatalf("todos %+v", todos)
	}
	if got := parsePrompts(summary); len(got.msgs) != 1 || got.msgs[0] != strings.TrimSpace(pasted) {
		t.Fatalf("prompts %q", got.msgs)
	}
}
