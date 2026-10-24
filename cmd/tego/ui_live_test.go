package main

import (
	"bytes"
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
	u.beforeStream = func() { before = append(before, len(u.takeLive())) }

	u.OnThinking("let me ")
	u.OnThinking("think")
	u.OnText("Hello ")
	u.OnText("world")
	want := []liveSeg{{true, "let me think"}, {false, "Hello world"}}
	if got := u.takeLive(); !reflect.DeepEqual(got, want) {
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
	u.showLive(u.takeLive())
	u.Finish()
	if s := out.String(); !strings.Contains(s, "let me think") || !strings.Contains(s, "Hello world") ||
		strings.Index(s, "let me think") > strings.Index(s, "Hello world") {
		t.Fatalf("shown again: %q", s)
	}
	if got := u.takeLive(); !reflect.DeepEqual(got, want) {
		t.Fatalf("live after showing it %+v", got)
	}

	u.OnTurnEnd(&llm.Message{})
	if got := u.takeLive(); got != nil {
		t.Fatalf("live after the response ended %+v", got)
	}
	u.ReplayText("from history")
	u.ReplayThinking("old thought")
	if got := u.takeLive(); got != nil {
		t.Fatalf("replay recorded %+v", got)
	}
	u.OnText("next")
	u.resetLive()
	if got := u.takeLive(); got != nil {
		t.Fatalf("live after reset %+v", got)
	}
}
