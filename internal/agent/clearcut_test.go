package agent

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// clearingServer clears like Anthropic's clear_tool_uses with a tool_uses
// trigger, as measured against the API: reading the history from the start,
// each time more than the trigger of tool uses are uncleared, it clears all
// but the last Keep of them, if that frees ClearAtLeast tokens. Like a long
// real session, most of the prompt is not tool output. It counts the
// requests whose cleared set differs from the one before: each rewrites the
// prompt cache.
type clearingServer struct {
	featured
	perResult, other int // tokens per tool result; per turn of other content
	calls            int // tool calls to make before answering
	turns, rewrites  int
	last             []bool // which results the previous request cleared
	cleared          []int  // tool results cleared, per request
}

func (s *clearingServer) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	p := req.ClearToolResults
	var cut []bool // per clearable result, in order
	turns := 0
	for _, m := range req.Messages {
		if m.Role == llm.RoleAssistant {
			turns++
		}
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolUse && (p == nil || !slices.Contains(p.Exclude, b.Name)) {
				cut = append(cut, false)
			}
		}
	}
	if p != nil {
		uncleared := []int{}
		for i := range cut {
			uncleared = append(uncleared, i)
			if len(uncleared) > p.TriggerToolUses {
				n := len(uncleared) - p.Keep
				if n*s.perResult >= p.ClearAtLeast {
					for _, j := range uncleared[:n] {
						cut[j] = true
					}
					uncleared = slices.Clone(uncleared[n:])
				}
			}
		}
	}
	n := 0
	for _, c := range cut {
		if c {
			n++
		}
	}
	if s.last != nil && !slices.Equal(s.last, cut[:len(s.last)]) {
		s.rewrites++
	}
	s.last, s.cleared = cut, append(s.cleared, n)
	s.turns++
	prompt := 20_000 + len(cut)*s.perResult + turns*s.other - n*s.perResult
	msg := &llm.Message{Role: llm.RoleAssistant, ClearedToolUses: n, ClearedTokens: n * s.perResult,
		Usage: &llm.Usage{CacheRead: prompt}}
	if s.turns <= s.calls {
		id := fmt.Sprintf("t%d", s.turns)
		name := "bash"
		if s.turns%7 == 0 {
			name = "todo" // excluded
		}
		msg.Blocks = []llm.Block{{Type: llm.BlockToolUse, ID: id, Name: name, Input: []byte(`{"todos":[]}`)}}
		if name == "bash" {
			msg.Blocks[0].Input = []byte(`{"command":"true"}`)
		}
		msg.StopReason = "tool_use"
	} else {
		msg.Blocks = []llm.Block{text("done")}
		msg.StopReason = "end_turn"
	}
	return msg, nil
}

func (s *clearingServer) Name() string { return "claude" }

func clearingAgent(s *clearingServer, dir string) *Agent {
	return &Agent{Provider: s, UI: nopUI{}, AutoCompact: true, ContextWindow: 1_000_000,
		Tools: tools.Builtins(&tools.Env{Cwd: dir})}
}

// A long session whose prompt stays far above 200k after clearing (the
// case that rewrote the cache on most tool calls): the cache is rewritten
// only by the passes, and every pass frees a lot.
func TestClearingRewritesTheCacheOnlyOnPasses(t *testing.T) {
	s := &clearingServer{featured: featured{clears: true}, perResult: 1_500, other: 2_500, calls: 300}
	a := clearingAgent(s, t.TempDir())
	if err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	passes := 0
	for i := 1; i < len(s.cleared); i++ {
		if s.cleared[i] < s.cleared[i-1] {
			t.Fatalf("request %d cleared less than the one before: %v", i, s.cleared)
		}
		if s.cleared[i] != s.cleared[i-1] {
			passes++
		}
	}
	// ~300 tool calls of 1.5k each and 2.5k of other content per turn:
	// past 200k after ~50 turns, then a pass each time the uncleared tool
	// uses double.
	if passes == 0 || passes > 4 || s.rewrites != passes {
		t.Fatalf("%d passes, %d cache rewrites in %d requests: %v", passes, s.rewrites, len(s.cleared), s.cleared)
	}
	if last := a.Messages[len(a.Messages)-1]; last.ClearTrigger == 0 {
		t.Fatal("the reply does not record the trigger")
	}
}

// A session resumed while the cache is warm sends the same trigger, so its
// first request clears what the last one did.
func TestResumedSessionKeepsTheTrigger(t *testing.T) {
	s := &clearingServer{featured: featured{clears: true}, perResult: 1_500, other: 2_500, calls: 120}
	a := clearingAgent(s, t.TempDir())
	if err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if s.cleared[len(s.cleared)-1] == 0 {
		t.Fatalf("nothing cleared before the resume: %v", s.cleared)
	}
	trigger := a.clearUses
	b := clearingAgent(s, t.TempDir())
	b.SetHistory(a.Messages)
	if b.clearUses != trigger {
		t.Fatalf("resumed with trigger %d, was %d", b.clearUses, trigger)
	}
	before := s.rewrites
	s.calls += 10
	if err := b.Run(context.Background(), "more"); err != nil {
		t.Fatal(err)
	}
	if s.rewrites != before {
		t.Fatalf("the resumed session rewrote the cache: %v", s.cleared)
	}
}
