package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// missProvider reports a cache miss on the requests in miss (1-based).
type missProvider struct {
	*cacheProvider
	miss map[int]*llm.CacheMiss
}

func (p *missProvider) Stream(ctx context.Context, req *llm.Request, on func(llm.Event)) (*llm.Message, error) {
	m, err := p.cacheProvider.Stream(ctx, req, on)
	if m != nil {
		m.CacheMiss = p.miss[len(p.reqs)]
	}
	return m, err
}

func missSession(miss map[int]*llm.CacheMiss) (*Agent, *missProvider, *time.Time) {
	prov := &missProvider{cacheProvider: &cacheProvider{}, miss: miss}
	for range 3 {
		prov.replies = append(prov.replies, assistant(text("looking"), call("c", "read", `{"path":"a.go"}`)))
	}
	prov.replies = append(prov.replies, assistant(text("All done.")))
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	a := &Agent{Provider: prov, UI: &infoUI{}, Model: "m", System: "sys",
		Tools: []tools.Tool{namedTool{name: "read"}}, clock: func() time.Time { return now }}
	return a, prov, &now
}

func missInfos(a *Agent) []string {
	var out []string
	for _, s := range a.UI.(*infoUI).infos {
		if strings.HasPrefix(s, "prompt cache missed") {
			out = append(out, s)
		}
	}
	return out
}

// A miss on a request that extends the last one, while its cache should
// still be there, is told once per prompt.
func TestCacheMissTold(t *testing.T) {
	tools := &llm.CacheMiss{Reason: "tools_changed", Tokens: 41_850}
	a, _, _ := missSession(map[int]*llm.CacheMiss{2: tools, 3: tools})
	if err := a.Run(context.Background(), "fix the bug"); err != nil {
		t.Fatal(err)
	}
	got := missInfos(a)
	want := "prompt cache missed: the tool definitions changed since the last request (42k tokens not read from the cache)"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("infos %q", got)
	}
}

// Not told: the first request (nothing to compare), a provider that could
// not compare, a small miss, a history tego changed, an expired cache, or
// another thinking level.
func TestCacheMissNotTold(t *testing.T) {
	big := &llm.CacheMiss{Reason: "messages_changed", Tokens: 30_000}
	for name, c := range map[string]struct {
		miss  map[int]*llm.CacheMiss
		setup func(a *Agent, now *time.Time)
	}{
		"first request": {miss: map[int]*llm.CacheMiss{1: big}},
		"unavailable":   {miss: map[int]*llm.CacheMiss{2: {Reason: "unavailable", Tokens: 30_000}}},
		"small":         {miss: map[int]*llm.CacheMiss{2: {Reason: "system_changed", Tokens: 900}}},
		"history changed": {miss: map[int]*llm.CacheMiss{5: big}, setup: func(a *Agent, _ *time.Time) {
			a.Messages[0].Blocks[0].Text = "fix the other bug"
		}},
		"expired":  {miss: map[int]*llm.CacheMiss{5: big}, setup: func(_ *Agent, now *time.Time) { *now = now.Add(10 * time.Minute) }},
		"thinking": {miss: map[int]*llm.CacheMiss{5: big}, setup: func(a *Agent, _ *time.Time) { a.Thinking = "high" }},
	} {
		t.Run(name, func(t *testing.T) {
			a, prov, now := missSession(c.miss)
			if err := a.Run(context.Background(), "fix the bug"); err != nil {
				t.Fatal(err)
			}
			if c.setup != nil {
				prov.replies = append(prov.replies, assistant(text("ok")))
				c.setup(a, now)
				if err := a.Run(context.Background(), "and now?"); err != nil {
					t.Fatal(err)
				}
			}
			if got := missInfos(a); len(got) != 0 {
				t.Fatalf("infos %q", got)
			}
		})
	}
	// The control: the same second prompt, nothing changed, is told.
	a, prov, _ := missSession(map[int]*llm.CacheMiss{5: big})
	a.Run(context.Background(), "fix the bug")
	prov.replies = append(prov.replies, assistant(text("ok")))
	a.Run(context.Background(), "and now?")
	if got := missInfos(a); len(got) != 1 || !strings.Contains(got[0], "an earlier message changed") {
		t.Fatalf("control: %q", got)
	}
}

// hashMessagesAt hashes the head in the same pass.
func TestHashMessagesAt(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{text("a")}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{text("b")}},
		{Role: llm.RoleUser, Blocks: []llm.Block{text("c")}},
	}
	for n := 0; n <= 4; n++ {
		all, head := hashMessagesAt(msgs, n)
		if all != hashMessages(msgs) || head != hashMessages(msgs[:min(n, 3)]) {
			t.Fatalf("n=%d", n)
		}
	}
}
