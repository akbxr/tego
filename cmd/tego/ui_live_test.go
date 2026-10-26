package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
)

// Thinking is word-wrapped like the answer once its line is complete, and
// stays dim and italic.
func TestThinkingWrapsAtWords(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var out bytes.Buffer
	u := newTUIUI(&out, func() int { return 40 }, nil)
	thought := "I should save this somewhere private since it contains PII, not in the repo. I'll make it easy to copy."
	for _, w := range strings.SplitAfter(thought, " ") {
		u.OnThinking(w)
	}
	u.Finish()
	s := out.String()
	erase := strings.LastIndex(s, "\x1b[J")
	final := s[erase+len("\x1b[J"):] // after the raw echo is erased
	if before := s[:erase]; strings.LastIndex(before, "\x1b[2m\x1b[3m") < strings.LastIndex(before, "\x1b[0m") ||
		!strings.HasSuffix(final, "\x1b[0m") {
		t.Fatalf("not dim italic: %q", s)
	}
	rows := strings.Split(strings.TrimSuffix(regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(final, ""), "\n"), "\n")
	for _, r := range rows {
		if len(r) > 40 {
			t.Fatalf("row of %d columns: %q", len(r), r)
		}
	}
	if got := strings.Join(rows, " "); got != thought {
		t.Fatalf("rows %q", rows)
	}
}

// The response being streamed is recorded, so a redraw mid-turn can show
// it again; history replayed is not, and a finished response is dropped.
func TestLiveResponseIsRecorded(t *testing.T) {
	var out bytes.Buffer
	u := &termUI{out: &out, diag: &out, atBOL: true, width: func() int { return 80 }}
	var before []int
	u.beforeStream = func() { before = append(before, len(u.takeLive(true))) }

	u.OnThinking("let me ")
	u.OnThinking("think")
	u.OnText("Hello ")
	u.OnText("world")
	want := []liveSeg{{kind: liveThinking, text: "let me think"}, {kind: liveText, text: "Hello world"}}
	if got := u.takeLive(true); !reflect.DeepEqual(got, want) {
		t.Fatalf("live %+v", got)
	}
	// The hook runs before each delta is recorded: a redraw there shows
	// what came before, and the delta follows.
	if !reflect.DeepEqual(before, []int{0, 1, 1, 2}) {
		t.Fatalf("hook saw %v", before)
	}

	// A redraw shows it again without recording it twice.
	out.Reset()
	u.Finish()
	u.showLive(u.takeLive(true))
	u.Finish()
	if s := out.String(); !strings.Contains(s, "let me think") || !strings.Contains(s, "Hello world") ||
		strings.Index(s, "let me think") > strings.Index(s, "Hello world") {
		t.Fatalf("shown again: %q", s)
	}
	if got := u.takeLive(true); !reflect.DeepEqual(got, want) {
		t.Fatalf("live after showing it %+v", got)
	}

	u.OnTurnEnd(&llm.Message{})
	if got := u.takeLive(false); got != nil {
		t.Fatalf("live after the response ended %+v", got)
	}
	u.ReplayText("from history")
	u.ReplayThinking("old thought")
	u.ReplayToolCall("grep", json.RawMessage(`{"pattern":"x"}`))
	u.ReplayToolResult("grep", json.RawMessage(`{"pattern":"x"}`), "a.go:1:x", false)
	if got := u.takeLive(false); got != nil {
		t.Fatalf("replay recorded %+v", got)
	}
	u.OnText("next")
	u.resetLive()
	if got := u.takeLive(false); got != nil {
		t.Fatalf("live after reset %+v", got)
	}
}

// While a batch of calls runs, the results of those done and the notices
// since are live: a redraw then shows them after the history's calls. Once
// the next response streams they are in the history and not live. toolRun
// brackets each run, and a replay leaves the running command's footer box.
func TestToolResultsAreLiveUntilTheNextResponse(t *testing.T) {
	var out bytes.Buffer
	u := &termUI{out: &out, diag: &out, atBOL: true, width: func() int { return 80 }}
	var runs []bool
	u.toolRun = func(on bool) { runs = append(runs, on) }
	grep := json.RawMessage(`{"pattern":"needle"}`)
	bash := json.RawMessage(`{"command":"make build"}`)

	u.OnText("Looking.")
	u.OnTurnEnd(&llm.Message{})
	u.OnToolCall("grep", grep)
	u.OnToolResult("grep", grep, "a.go:3:needle", false)
	u.OnInfo("  · [scout] read a.go")
	u.OnToolCall("bash", bash) // runs now
	if !reflect.DeepEqual(runs, []bool{true, false, true}) {
		t.Fatalf("toolRun %v", runs)
	}
	want := []liveSeg{
		{kind: liveResult, name: "grep", input: grep, text: "a.go:3:needle"},
		{kind: liveInfo, text: "  · [scout] read a.go"},
	}
	live := u.takeLive(false)
	if !reflect.DeepEqual(live, want) {
		t.Fatalf("live %+v", live)
	}
	out.Reset()
	u.ReplayToolResult("bash", json.RawMessage(`{"command":"ls"}`), "a.go", false) // an earlier command, from the history
	u.showLive(live)
	if s := out.String(); !strings.Contains(s, "a.go:3:needle") || !strings.Contains(s, "[scout] read a.go") {
		t.Fatalf("shown again: %q", s)
	}
	if u.running == "" {
		t.Fatal("a replayed result ended the running command's box")
	}
	if got := u.takeLive(false); !reflect.DeepEqual(got, want) {
		t.Fatalf("showing it recorded it again: %+v", got)
	}

	u.OnToolResult("bash", bash, "ok", false)
	if u.running != "" {
		t.Fatal("the command's box outlived its result")
	}
	if got := u.takeLive(true); len(got) != 0 {
		t.Fatalf("results kept for the next response: %+v", got)
	}
}
