package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/session"
)

// idleProvider has an exact cache TTL (Anthropic) and answers compaction
// requests with a summary, others with "done"; it records the classes.
type idleProvider struct {
	classes []string
	last    *llm.Request
}

func (*idleProvider) Name() string              { return "fake" }
func (*idleProvider) KnownCacheTTL(string) bool { return true }
func (p *idleProvider) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	p.classes = append(p.classes, req.Class)
	text := "done"
	if req.Class == llm.ClassCompaction {
		text = "SUMMARY"
	} else {
		p.last = req
	}
	return &llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: text}}, StopReason: "end_turn"}, nil
}

// talkHistory is n rounds of prompts and long answers (~2.5k tokens each):
// text, which cold elision cannot shrink.
func talkHistory(n int) []llm.Message {
	var msgs []llm.Message
	for i := range n {
		msgs = append(msgs,
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: fmt.Sprintf("question %d", i)}}},
			llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: strings.Repeat("answer ", 1430)}}})
	}
	return msgs
}

// idleAgent has ~75k tokens of conversation, compacts after breaks above
// 50k, and last heard from the provider idle ago.
func idleAgent(t *testing.T, idle time.Duration) (*Agent, *idleProvider, *infoUI) {
	t.Helper()
	prov, ui := &idleProvider{}, &infoUI{}
	a := &Agent{Provider: prov, UI: ui, ContextWindow: 1_000_000, AutoCompact: true, KeepRecent: 5000,
		LongCache: true, IdleCompactAt: 50_000}
	a.SetHistory(talkHistory(30))
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	a.lastReply = t0
	a.clock = func() time.Time { return t0.Add(idle) }
	return a, prov, ui
}

func TestIdleCompaction(t *testing.T) {
	cases := []struct {
		name  string
		idle  time.Duration
		setup func(*Agent, *idleProvider)
		want  bool
	}{
		{"back after 2h", 2 * time.Hour, nil, true},
		{"back after 40m: the cache is warm", 40 * time.Minute, nil, false},
		{"back after 62m: within the margin", 62 * time.Minute, nil, false},
		// A 5-minute cache (tego -p) has expired, but the session may have
		// been cached for an hour by an interactive run: not before 65m.
		{"5m cache, back after 20m", 20 * time.Minute, func(a *Agent, _ *idleProvider) { a.LongCache = false }, false},
		{"below the threshold", 2 * time.Hour, func(a *Agent, _ *idleProvider) { a.IdleCompactAt = 500_000 }, false},
		{"turned off", 2 * time.Hour, func(a *Agent, _ *idleProvider) { a.IdleCompactAt = 0 }, false},
		{"auto-compaction off", 2 * time.Hour, func(a *Agent, _ *idleProvider) { a.AutoCompact = false }, false},
		{"a cache of unknown lifetime", 2 * time.Hour, func(a *Agent, p *idleProvider) { a.Provider = &countingProvider{} }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, prov, ui := idleAgent(t, c.idle)
			if c.setup != nil {
				c.setup(a, prov)
			}
			if err := a.Run(context.Background(), "next"); err != nil {
				t.Fatal(err)
			}
			got := strings.Contains(strings.Join(prov.classes, ","), llm.ClassCompaction)
			if got != c.want {
				t.Fatalf("compacted=%v, want %v (classes %v, infos %q)", got, c.want, prov.classes, ui.infos)
			}
			if !c.want {
				return
			}
			// Compacted before the request: it carries the summary and the
			// prompt, not the 75k tokens of conversation.
			if first := prov.last.Messages[0].Blocks[0].Text; !strings.Contains(first, "SUMMARY") {
				t.Fatalf("request starts with %.80q", first)
			}
			if n := estimateTokens(prov.last.Messages); n > 20_000 {
				t.Fatalf("request still ~%d tokens", n)
			}
			if len(ui.infos) == 0 || !strings.Contains(ui.infos[0], "idle 2h0m0s, so the prompt cache expired: compacting ~") {
				t.Fatalf("infos %q", ui.infos)
			}
		})
	}
}

// After an idle compaction the full history is still in the session, so
// /tree can go back to it.
func TestIdleCompactionKeepsTheFullHistoryInTheSession(t *testing.T) {
	s, err := session.New(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, m := range talkHistory(30) {
		if err := s.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	before := s.Leaf()
	a, prov, _ := idleAgent(t, 2*time.Hour)
	a.Session = s
	if err := a.Run(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(prov.classes, ","), llm.ClassCompaction) {
		t.Fatal("did not compact")
	}
	if full := s.History(before); len(full) != 60 {
		t.Fatalf("history before the compaction has %d messages, want 60", len(full))
	}
	if cur := s.History(s.Leaf()); len(cur) >= 60 {
		t.Fatalf("active history has %d messages after compacting", len(cur))
	}
}
