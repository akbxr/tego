package llm

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// usageOf builds an account's limits: weekly used and reset, 5-hour used.
func usageOf(now time.Time, week float64, resetsIn time.Duration, fiveHour float64) *UsageReport {
	return &UsageReport{Limits: []UsageLimit{
		{Label: "Claude 5 Hour", Window: "5h", Used: fiveHour, ResetsAt: now.Add(3 * time.Hour)},
		{Label: "Claude 7 Day", Window: "7d", Used: week, ResetsAt: now.Add(resetsIn)},
	}}
}

// expiringPool is a pool on a fake clock whose accounts report the limits
// in reports (changed by the test between refreshes); calls counts reads.
type expiringPool struct {
	*Pool
	mu      sync.Mutex
	now     time.Time
	reports map[string]*UsageReport
	calls   int
}

func newExpiringPool(t *testing.T, accts ...*fakeAccount) *expiringPool {
	t.Helper()
	ep := &expiringPool{now: time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC), reports: map[string]*UsageReport{}}
	var members []PoolMember
	for _, a := range accts {
		members = append(members, PoolMember{Label: a.name, Provider: a, Usage: func(context.Context) (*UsageReport, error) {
			ep.mu.Lock()
			defer ep.mu.Unlock()
			ep.calls++
			return ep.reports[a.name], nil
		}})
	}
	ep.Pool = NewPool("fake", StrategyExpiring, members)
	ep.Pool.now = func() time.Time { ep.mu.Lock(); defer ep.mu.Unlock(); return ep.now }
	return ep
}

func (ep *expiringPool) set(name string, r *UsageReport) {
	ep.mu.Lock()
	ep.reports[name] = r
	ep.mu.Unlock()
}

func (ep *expiringPool) advance(d time.Duration) {
	ep.mu.Lock()
	ep.now = ep.now.Add(d)
	ep.mu.Unlock()
}

// The account that loses the most of its week per hour gets the prompt:
// not the one used most recently, and not one whose 5 hours are used up.
func TestExpiringPicksQuotaLostSoonest(t *testing.T) {
	info, budi, kipli, full := &fakeAccount{name: "info"}, &fakeAccount{name: "budi"}, &fakeAccount{name: "kipli"}, &fakeAccount{name: "full"}
	ep := newExpiringPool(t, info, budi, kipli, full)
	now := ep.now
	ep.set("info", usageOf(now, 0.21, 86*time.Hour, 0.04))
	ep.set("budi", usageOf(now, 0, 11*time.Hour, 0))
	ep.set("kipli", usageOf(now, 0, 30*time.Hour, 0))
	ep.set("full", usageOf(now, 0, 2*time.Hour, 0.95)) // would lose the most, but its 5 hours are used up
	var notes []string
	ep.OnSwitch = func(s string) { notes = append(notes, s) }
	if got := answer(t, ep.Pool, prompt("s")); got != "budi" {
		t.Fatalf("answered by %q, want budi", got)
	}
	if len(notes) != 1 || notes[0] != "fake: using budi, 100% of the week left, resets in 11h0m" {
		t.Fatalf("notes %q", notes)
	}
	st := ep.Status()
	if !st[1].Active || st[0].Quota != "79% of the week left, resets in 3d14h" {
		t.Fatalf("status %+v", st)
	}
}

// A prompt whose session's cache is warm stays on its account; once the
// cache is cold, it moves to the account now better. Tool rounds never move.
func TestExpiringKeepsAWarmCache(t *testing.T) {
	a, b := &fakeAccount{name: "a"}, &fakeAccount{name: "b"}
	ep := newExpiringPool(t, a, b)
	ep.set("a", usageOf(ep.now, 0, 10*time.Hour, 0))
	ep.set("b", usageOf(ep.now, 0, 50*time.Hour, 0))
	if got := answer(t, ep.Pool, prompt("s")); got != "a" {
		t.Fatalf("first prompt on %q", got)
	}
	ep.set("a", usageOf(ep.now, 0.97, 10*time.Hour, 0)) // a nearly done, read at the next refresh
	ep.advance(6 * time.Minute)                         // limits stale, cache cold
	if got := answer(t, ep.Pool, toolRound("s")); got != "a" {
		t.Fatalf("tool round moved to %q", got)
	}
	ep.advance(time.Minute)
	if got := answer(t, ep.Pool, prompt("s")); got != "a" {
		t.Fatalf("prompt with a warm cache moved to %q", got)
	}
	ep.advance(6 * time.Minute)
	long := prompt("s")
	long.LongCache = true // an hour's cache: still warm
	if got := answer(t, ep.Pool, long); got != "a" {
		t.Fatalf("prompt with a warm 1-hour cache moved to %q", got)
	}
	if got := answer(t, ep.Pool, prompt("s")); got != "a" {
		t.Fatal("5-minute cache taken as cold right after a request")
	}
	ep.advance(6 * time.Minute)
	if got := answer(t, ep.Pool, prompt("s")); got != "b" {
		t.Fatalf("prompt with a cold cache stayed on %q, want b", got)
	}
}

// Limits are read once per usageFresh, and only for new prompts.
func TestExpiringReadsLimitsSparingly(t *testing.T) {
	a, b := &fakeAccount{name: "a"}, &fakeAccount{name: "b"}
	ep := newExpiringPool(t, a, b)
	ep.set("a", usageOf(ep.now, 0, 10*time.Hour, 0))
	ep.set("b", usageOf(ep.now, 0, 50*time.Hour, 0))
	for range 3 {
		answer(t, ep.Pool, prompt("s"))
		answer(t, ep.Pool, toolRound("s"))
	}
	if ep.calls != 2 {
		t.Fatalf("%d reads, want one per account", ep.calls)
	}
	ep.advance(usageFresh)
	answer(t, ep.Pool, toolRound("s"))
	if ep.calls != 2 {
		t.Fatalf("a tool round read the limits (%d reads)", ep.calls)
	}
	answer(t, ep.Pool, prompt("s"))
	if ep.calls != 4 {
		t.Fatalf("%d reads after they went stale, want 4", ep.calls)
	}
}

// Prompts at once (subagents) share one refresh.
func TestExpiringRefreshesOnceForConcurrentPrompts(t *testing.T) {
	a, b := &fakeAccount{name: "a"}, &fakeAccount{name: "b"}
	ep := newExpiringPool(t, a, b)
	ep.set("a", usageOf(ep.now, 0, 10*time.Hour, 0))
	ep.set("b", usageOf(ep.now, 0, 50*time.Hour, 0))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); ep.refreshUsage(context.Background()) }()
	}
	wg.Wait()
	if ep.calls != 2 {
		t.Fatalf("%d reads, want 2", ep.calls)
	}
	if i, _ := ep.pick(prompt("x")); i != 0 {
		t.Fatalf("picked %d after the refresh, want a", i)
	}
}

// A tier's weekly limit counts for that tier's models only.
func TestExpiringHonoursTierLimits(t *testing.T) {
	a, b := &fakeAccount{name: "a"}, &fakeAccount{name: "b"}
	ep := newExpiringPool(t, a, b)
	r := usageOf(ep.now, 0, 10*time.Hour, 0)
	r.Limits = append(r.Limits, UsageLimit{Window: "7d", Tier: "Fable", Used: 0.99, ResetsAt: ep.now.Add(10 * time.Hour)})
	ep.set("a", r)
	ep.set("b", usageOf(ep.now, 0.5, 50*time.Hour, 0))
	fable := prompt("f")
	fable.Model = "claude-fable-5"
	if got := answer(t, ep.Pool, fable); got != "b" {
		t.Fatalf("Fable prompt on %q, want b", got)
	}
	opus := prompt("o")
	opus.Model = "claude-opus-5-5"
	if got := answer(t, ep.Pool, opus); got != "a" {
		t.Fatalf("Opus prompt on %q, want a", got)
	}
}

// Without limits to read (API keys, the endpoint failing) it acts as
// fallback, and a limited account still hands over to the next.
func TestExpiringWithoutLimits(t *testing.T) {
	a := &fakeAccount{name: "a", err: &APIError{Status: 429, RetryAfter: time.Hour}}
	b := &fakeAccount{name: "b"}
	p := newPool(StrategyExpiring, a, b)
	if p.Strategy() != StrategyExpiring {
		t.Fatalf("strategy %q", p.Strategy())
	}
	if got := answer(t, p, prompt("s")); got != "b" {
		t.Fatalf("answered by %q", got)
	}
	if got := answer(t, p, prompt("s")); got != "b" || a.calls != 1 {
		t.Fatalf("answered by %q, a called %d times", got, a.calls)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Minute:              "45m",
		11*time.Hour + 39*time.Minute: "11h39m",
		86*time.Hour + 10*time.Minute: "3d14h",
		-time.Minute:                  "0m",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
	if !strings.HasPrefix(quota{left: 0.5}.String(time.Now()), "50% of the week left") {
		t.Fatal("quota string")
	}
}
