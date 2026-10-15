package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/session"
)

// exactCache is a provider whose prompt cache has an exact TTL (Anthropic).
type exactCache struct{ countingProvider }

func (*exactCache) KnownCacheTTL(string) bool { return true }

// coldAgent has ~60k tokens of old read output on a 1M window: below the
// warm elision threshold (100k), above the cold one.
func coldAgent(t *testing.T, idle time.Duration, longCache bool) (*Agent, *countingProvider, *infoUI) {
	t.Helper()
	ec, ui := &exactCache{}, &infoUI{}
	prov := &ec.countingProvider
	a := &Agent{Provider: ec, UI: ui, ContextWindow: 1_000_000, AutoCompact: true, KeepRecent: 5000, LongCache: longCache}
	h := toolHistory(30, 2000)
	stripUsage(h)
	a.SetHistory(h)
	a.Messages = append(a.Messages, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "next"}}})
	t0 := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	a.lastReply = t0
	a.clock = func() time.Time { return t0.Add(idle) }
	return a, prov, ui
}

func elidedIn(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolResult && strings.HasPrefix(b.Content, elidedMarker) {
				n++
			}
		}
	}
	return n
}

func TestColdCacheElides(t *testing.T) {
	cases := []struct {
		name      string
		idle      time.Duration
		longCache bool
		want      bool
	}{
		{"5m cache, idle 10m", 10 * time.Minute, false, true},
		{"5m cache, idle 3m", 3 * time.Minute, false, false},
		{"1h cache, idle 30m", 30 * time.Minute, true, false},
		{"1h cache, idle 70m", 70 * time.Minute, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, prov, ui := coldAgent(t, c.idle, c.longCache)
			if err := a.loop(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := elidedIn(prov.last.Messages) > 0
			if got != c.want {
				t.Fatalf("elided=%v, want %v (infos %q)", got, c.want, ui.infos)
			}
			if c.want && (len(ui.infos) == 0 || !strings.Contains(ui.infos[0], "prompt cache expired")) {
				t.Fatalf("infos %q", ui.infos)
			}
			if !a.lastReply.Equal(a.now()) {
				t.Fatal("lastReply not updated after the reply")
			}
		})
	}
}

func TestColdCacheOnlyWhereTheTTLIsExact(t *testing.T) {
	a, prov, ui := coldAgent(t, 3*time.Hour, false)
	a.Provider = prov // an implicit cache (Gemini, OpenAI): may still be warm
	if err := a.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elidedIn(prov.last.Messages) > 0 || len(ui.infos) > 0 {
		t.Fatalf("elided on a best-effort cache: %q", ui.infos)
	}
}

func TestColdCacheLeavesSmallContexts(t *testing.T) {
	a, prov, _ := coldAgent(t, time.Hour, false)
	a.Messages = append(a.Messages[:1], a.Messages[len(a.Messages)-12:]...) // ~12k tokens
	if err := a.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := elidedIn(prov.last.Messages); n != 0 {
		t.Fatalf("elided %d blocks of a small context", n)
	}
}

func TestResumeKnowsWhenTheSessionWasLastUsed(t *testing.T) {
	s, err := session.New(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.LeafTime().IsZero() {
		t.Fatal("empty session has a leaf time")
	}
	msg := llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "hi"}}}
	if err := s.Append(msg); err != nil {
		t.Fatal(err)
	}
	a := &Agent{UI: nopUI{}, Session: s}
	a.SetHistory([]llm.Message{msg})
	if d := time.Since(a.lastReply); d < 0 || d > 5*time.Second {
		t.Fatalf("lastReply %v (%v ago)", a.lastReply, d)
	}
}
