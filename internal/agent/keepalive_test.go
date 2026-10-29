package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// warmProvider is a cacheProvider with a known TTL that keeps caches alive.
type warmProvider struct {
	*cacheProvider
	warms []*llm.Request
}

func (p *warmProvider) KnownCacheTTL(string) bool { return true }
func (p *warmProvider) CanPrewarm(string) bool    { return true }
func (p *warmProvider) Prewarm(_ context.Context, req *llm.Request) (llm.Usage, error) {
	p.warms = append(p.warms, req)
	return llm.Usage{Input: 9, CacheRead: 40_000}, nil
}

// keptSession is cachedSession on a warmProvider with a clock the test
// moves, kept alive for up to keep.
func keptSession(t *testing.T, keep time.Duration, auto bool) (*Agent, *warmProvider, *time.Time) {
	t.Helper()
	prov := &warmProvider{cacheProvider: &cacheProvider{}}
	for range 3 {
		prov.replies = append(prov.replies, assistant(text(strings.Repeat("reasoning about the code ", 300)), call("c", "read", `{"path":"a.go"}`)))
	}
	prov.replies = append(prov.replies, assistant(text("All done.")))
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	a := &Agent{Provider: prov, UI: &infoUI{}, Model: "m", System: "sys", ContextWindow: 200_000,
		Tools: []tools.Tool{namedTool{name: "read"}}, Thinking: "low", LongCache: true,
		KeepAlive: keep, KeepAliveAuto: auto, clock: func() time.Time { return now }}
	if err := a.Run(context.Background(), "fix the bug"); err != nil {
		t.Fatal(err)
	}
	return a, prov, &now
}

// warmFor runs the keep-alives due over d, a minute at a time, as the REPL
// does, and returns how many went out.
func warmFor(a *Agent, now *time.Time, d time.Duration) int {
	n := 0
	for end := now.Add(d); now.Before(end); *now = now.Add(time.Minute) {
		if req := a.KeepAliveDue(); req != nil {
			u, err := llm.Prewarm(context.Background(), a.Provider, req)
			a.KeepAliveDone(req, *now, u, err)
			n++
		}
	}
	return n
}

// Before the cache would expire, the last request goes out again with the
// whole history and a placeholder, its breakpoint on the history's end;
// the cache then counts as warm from it.
func TestKeepAliveRepeatsTheLastRequest(t *testing.T) {
	a, prov, now := keptSession(t, 12*time.Hour, false)
	last := prov.reqs[len(prov.reqs)-1]
	start := *now

	*now = start.Add(49 * time.Minute)
	if a.KeepAliveDue() != nil {
		t.Fatal("due 49 minutes into a 1-hour cache")
	}
	*now = start.Add(51 * time.Minute)
	req := a.KeepAliveDue()
	if req == nil {
		t.Fatal("not due 51 minutes in")
	}
	n := len(a.Messages)
	if req.Class != llm.ClassKeepAlive || req.CachePrefix != n || len(req.Messages) != n+1 {
		t.Fatalf("class %q, prefix %d, %d messages (history %d)", req.Class, req.CachePrefix, len(req.Messages), n)
	}
	if !reflect.DeepEqual(req.Messages[:n], a.Messages) || req.Messages[n].Blocks[0].Text != keepAlivePrompt {
		t.Fatal("not the history and the placeholder")
	}
	if req.System != last.System || !reflect.DeepEqual(req.Tools, last.Tools) || req.Model != last.Model ||
		req.Thinking != last.Thinking || !req.LongCache || req.SessionID != last.SessionID {
		t.Fatal("the request's fields differ from the last one's")
	}
	// Its own copy of the blocks: a turn may change them meanwhile.
	req.Messages[0].Blocks[0].Text = "changed"
	if a.Messages[0].Blocks[0].Text == "changed" {
		t.Fatal("the request shares the history's blocks")
	}

	a.KeepAliveDone(req, *now, llm.Usage{CacheRead: 40_000}, nil)
	*now = start.Add(51*time.Minute + 59*time.Minute)
	if a.cacheIdle() != 0 {
		t.Fatal("the cache counts as cold an hour after the last reply, though kept warm")
	}
	if a.KeepAliveDue() == nil {
		t.Fatal("no second refresh 50 minutes after the first")
	}
}

// It stops after KeepAlive; the next prompt says what it did, and the
// reply after it starts a new idle period.
func TestKeepAliveStopsAfterItsTime(t *testing.T) {
	a, prov, now := keptSession(t, 3*time.Hour, false)
	if n := warmFor(a, now, 5*time.Hour); n != 3 {
		t.Fatalf("%d refreshes in 3 hours", n)
	}
	prov.replies = append(prov.replies, assistant(text("hi")))
	if err := a.Run(context.Background(), "back"); err != nil {
		t.Fatal(err)
	}
	info := strings.Join(a.UI.(*infoUI).infos, "\n")
	if !strings.Contains(info, "kept the prompt cache warm while idle: 3 refreshes, ~120k tokens read from it; then stopped after 3h0m") {
		t.Fatalf("info %q", info)
	}
	if strings.Contains(info, "idle") && strings.Contains(info, "compacting") {
		t.Fatal("compacted after the break")
	}
	if n := warmFor(a, now, 2*time.Hour); n != 2 {
		t.Fatalf("%d refreshes after the next reply", n)
	}
}

// Auto stops once the refreshes cost what a cold cache would: here a read
// costs half an input token, so two refreshes match one write at twice.
func TestKeepAliveAutoBudget(t *testing.T) {
	a, _, now := keptSession(t, 12*time.Hour, true)
	a.CacheReadRatio = func(string, string) float64 { return 0.5 }
	if b := a.keepAliveBudget(); b != 4 {
		t.Fatalf("budget %d", b)
	}
	a.CacheReadRatio = func(string, string) float64 { return 1 }
	if n := warmFor(a, now, 12*time.Hour); n != 2 {
		t.Fatalf("%d refreshes", n)
	}
	if !strings.Contains(a.ka.note, "cost more than letting the cache go cold") {
		t.Fatalf("note %q", a.ka.note)
	}
	// Default reads at a tenth, Opus 5.5 at a twentieth: 20 and 40 hours'
	// worth, or a compaction's worth above IdleCompactAt.
	a.CacheReadRatio = nil
	if b := a.keepAliveBudget(); b != 20 {
		t.Fatalf("budget at 0.1: %d", b)
	}
	a.LastCtx, a.IdleCompactAt, a.AutoCompact = 1_000_000, 200_000, true
	a.CacheReadRatio = func(string, string) float64 { return 0.05 }
	if b := a.keepAliveBudget(); b != 6 {
		t.Fatalf("budget for a compacting 1M context: %d", b)
	}
}

// Nothing goes out when it can't keep the cache: off, a short cache, a
// history changed since, a cache already gone, or after a failure.
func TestKeepAliveWhenNot(t *testing.T) {
	for _, c := range []struct {
		name string
		prep func(a *Agent, now *time.Time)
	}{
		{"off", func(a *Agent, _ *time.Time) { a.KeepAlive = 0 }},
		{"5-minute cache", func(a *Agent, _ *time.Time) { a.LongCache = false }},
		{"rewound", func(a *Agent, _ *time.Time) { a.Messages = a.Messages[:2] }},
		{"small context", func(a *Agent, _ *time.Time) { a.LastCtx = 10_000 }},
		{"model changed", func(a *Agent, _ *time.Time) { a.Model = "other" }},
		{"asleep past the TTL", func(a *Agent, now *time.Time) { *now = now.Add(2 * time.Hour) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, prov, now := keptSession(t, 12*time.Hour, false)
			c.prep(a, now)
			warmFor(a, now, 3*time.Hour)
			if len(prov.warms) != 0 {
				t.Fatalf("%d keep-alives", len(prov.warms))
			}
		})
	}

	t.Run("failed", func(t *testing.T) {
		a, prov, now := keptSession(t, 12*time.Hour, false)
		*now = now.Add(51 * time.Minute)
		req := a.KeepAliveDue()
		a.KeepAliveDone(req, *now, llm.Usage{}, errors.New("boom"))
		warmFor(a, now, 3*time.Hour)
		if a.KeepAliveDue() != nil {
			t.Fatal("another after a failure")
		}
		prov.replies = append(prov.replies, assistant(text("hi")))
		a.Run(context.Background(), "back")
		if !strings.Contains(strings.Join(a.UI.(*infoUI).infos, "\n"), "prompt cache keep-alive failed: boom") {
			t.Fatalf("infos %q", a.UI.(*infoUI).infos)
		}
	})
}
