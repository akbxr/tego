package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeAccount answers with its own name, or fails with err: always, or
// its first fails calls when fails > 0.
type fakeAccount struct {
	name  string
	err   error
	fails int
	emit  bool // emit an event before failing
	calls int
	ctx   context.Context // of the last call
}

func (f *fakeAccount) Name() string { return "fake" }

func (f *fakeAccount) Stream(ctx context.Context, req *Request, onEvent func(Event)) (*Message, error) {
	f.calls++
	f.ctx = ctx
	if f.emit {
		onEvent(Event{Type: EvText, Text: "partial"})
	}
	if f.err != nil && (f.fails == 0 || f.calls <= f.fails) {
		return &Message{Role: RoleAssistant}, f.err
	}
	return &Message{Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: f.name}}}, nil
}

func prompt(session string) *Request {
	return &Request{SessionID: session, Messages: []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "hi"}}}}}
}

func toolRound(session string) *Request {
	return &Request{SessionID: session, Messages: []Message{
		{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "hi"}}},
		{Role: RoleAssistant, Blocks: []Block{{Type: BlockToolUse, ID: "t1", Name: "bash"}}},
		{Role: RoleUser, Blocks: []Block{{Type: BlockToolResult, ToolUseID: "t1", Text: "ok"}}},
	}}
}

func answer(t *testing.T, p *Pool, req *Request) string {
	t.Helper()
	msg, err := p.Stream(context.Background(), req, func(Event) {})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return msg.Blocks[0].Text
}

func newPool(strategy string, accts ...*fakeAccount) *Pool {
	var members []PoolMember
	for _, a := range accts {
		members = append(members, PoolMember{Label: a.name, Provider: a})
	}
	return NewPool("fake", strategy, members)
}

func TestPoolFallbackSwitchesOnLimitAndRestsAccount(t *testing.T) {
	a := &fakeAccount{name: "a", err: &APIError{Status: 429, RetryAfter: 50 * time.Millisecond}}
	b := &fakeAccount{name: "b"}
	p := newPool(StrategyFallback, a, b)
	var notes []string
	p.OnSwitch = func(s string) { notes = append(notes, s) }
	if got := answer(t, p, prompt("s")); got != "b" {
		t.Fatalf("first prompt answered by %q, want b", got)
	}
	if len(notes) == 0 {
		t.Fatal("switch was not reported")
	}
	// Fallback stays on b, and a is resting anyway.
	a.err = nil
	if got := answer(t, p, prompt("s")); got != "b" || a.calls != 1 {
		t.Fatalf("second prompt answered by %q (a called %d times)", got, a.calls)
	}
	if st := p.Status(); len(st) != 2 || st[0].Active || !st[1].Active {
		t.Fatalf("status %+v, want b active", st)
	}
	// Once b is limited, a is back from its rest and takes over.
	time.Sleep(60 * time.Millisecond)
	b.err = &APIError{Status: 429}
	if got := answer(t, p, prompt("s")); got != "a" {
		t.Fatalf("after b limited: %q", got)
	}
}

func TestPoolDoesNotSwitchAfterOutputOrOnOtherErrors(t *testing.T) {
	a := &fakeAccount{name: "a", err: &APIError{Status: 429}, emit: true}
	b := &fakeAccount{name: "b"}
	p := newPool(StrategyFallback, a, b)
	if _, err := p.Stream(context.Background(), prompt("s"), func(Event) {}); err == nil || b.calls != 0 {
		t.Fatalf("switched after streaming started: err=%v b.calls=%d", err, b.calls)
	}
	a.emit, a.err = false, &APIError{Status: 400, Body: "invalid request"}
	if _, err := p.Stream(context.Background(), prompt("s"), func(Event) {}); err == nil || b.calls != 0 {
		t.Fatalf("switched on a request error: err=%v b.calls=%d", err, b.calls)
	}
}

func TestPoolAllAccountsLimited(t *testing.T) {
	a := &fakeAccount{name: "a", err: &APIError{Status: 429}}
	b := &fakeAccount{name: "b", err: errors.New("anthropic: usage limit reached")}
	p := newPool(StrategyFallback, a, b)
	_, err := p.Stream(context.Background(), prompt("s"), func(Event) {})
	if err == nil || a.calls != 1 || b.calls != 1 {
		t.Fatalf("err=%v calls a=%d b=%d", err, a.calls, b.calls)
	}
}

func TestPoolRoundRobinRotatesPerPromptNotPerToolRound(t *testing.T) {
	a, b, c := &fakeAccount{name: "a"}, &fakeAccount{name: "b"}, &fakeAccount{name: "c"}
	p := newPool(StrategyRoundRobin, a, b, c)
	var got []string
	for range 2 {
		got = append(got, answer(t, p, prompt("s")), answer(t, p, toolRound("s")))
	}
	got = append(got, answer(t, p, prompt("s")))
	want := []string{"a", "a", "b", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("answers = %v, want %v", got, want)
		}
	}
}

// A response cut off mid-stream is asked for again on the same account,
// whose cache holds the prompt, though it is the turn's first request.
func TestPoolRetryAfterCutStaysOnTheAccount(t *testing.T) {
	a := &fakeAccount{name: "a", err: &APIError{Status: 529}, fails: 1, emit: true}
	b := &fakeAccount{name: "b"}
	p := newPool(StrategyRoundRobin, a, b)
	if _, err := p.Stream(context.Background(), prompt("s"), func(Event) {}); err == nil {
		t.Fatal("the cut response did not fail")
	}
	a.emit = false
	if got := answer(t, p, prompt("s")); got != "a" {
		t.Fatalf("the retry went to %s", got)
	}
	if got := answer(t, p, prompt("s")); got != "b" {
		t.Fatalf("the next prompt went to %s, not on in the rotation", got)
	}
}

func TestPoolMembersDoNotWaitOutRateLimits(t *testing.T) {
	tries := 0
	attempt := func(ctx context.Context, emit func(Event)) (*Message, error) {
		tries++
		return nil, &APIError{Status: 429, RetryAfter: 10 * time.Millisecond}
	}
	ctx := context.WithValue(context.Background(), failFastKey{}, true)
	if _, err := streamWithRetry(ctx, "fake", &Request{Model: "m"}, nil, attempt); err == nil || tries != 1 {
		t.Fatalf("pool member retried a 429: tries=%d err=%v", tries, err)
	}
	tries = 0
	if _, err := streamWithRetry(context.Background(), "fake", &Request{Model: "m"}, nil, attempt); err == nil || tries != maxRetries+1 {
		t.Fatalf("single account should retry: tries=%d", tries)
	}
}

// fakeClock is a wall clock the test moves.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// Rests run on the wall clock (Pool.now), which goes on while a laptop
// sleeps, as the limits they wait out do.
func TestPoolRestsEndOnTheWallClock(t *testing.T) {
	a := &fakeAccount{name: "a", err: &APIError{Status: 401}}
	b := &fakeAccount{name: "b"}
	p := newPool(StrategyFallback, a, b)
	// A year ahead of time.Now: a rest timed by it would not end below.
	clock := &fakeClock{t: time.Now().AddDate(1, 0, 0).Round(0)}
	p.now = clock.now
	if got := answer(t, p, prompt("s")); got != "b" {
		t.Fatalf("answered by %q", got)
	}
	if st := p.Status(); !st[0].CoolUntil.Equal(clock.t.Add(10 * time.Minute)) {
		t.Fatalf("a rests until %v, want 10 minutes from %v", st[0].CoolUntil, clock.t)
	}
	clock.t = clock.t.Add(11 * time.Minute) // passed on the wall clock only
	a.err, b.err = nil, &APIError{Status: 401}
	if got := answer(t, p, prompt("s")); got != "a" || a.calls != 2 {
		t.Fatalf("after its rest: answered by %q, a called %d times", got, a.calls)
	}
}

// With every account resting, the one back soonest is tried, not the last
// in rotation; an answer ends its rest.
func TestPoolAllRestingTriesTheFirstBack(t *testing.T) {
	a, b, c := &fakeAccount{name: "a"}, &fakeAccount{name: "b"}, &fakeAccount{name: "c"}
	p := newPool(StrategyFallback, a, b, c)
	now := p.now()
	for i, d := range []time.Duration{30 * time.Minute, 5 * time.Minute, 20 * time.Minute} {
		p.members[i].coolUntil, p.members[i].reason = now.Add(d), "limit"
	}
	if got := answer(t, p, prompt("s")); got != "b" || a.calls+c.calls != 0 {
		t.Fatalf("answered by %q; a called %d, c %d times", got, a.calls, c.calls)
	}
	if st := p.Status(); !st[1].CoolUntil.IsZero() || st[1].Reason != "" {
		t.Fatalf("b still rests after answering: %+v", st[1])
	}
}

// When every account hits a short rate limit, the request waits for the
// first one back and tries it again, instead of failing.
func TestPoolWaitsOutShortRateLimits(t *testing.T) {
	waits := noRetryWait(t)
	a := &fakeAccount{name: "a", err: &APIError{Status: 429, RetryAfter: 40 * time.Second}, fails: 1}
	b := &fakeAccount{name: "b", err: &APIError{Status: 429, RetryAfter: 15 * time.Second}, fails: 1}
	p := newPool(StrategyFallback, a, b)
	p.now = (&fakeClock{t: time.Now().Round(0)}).now
	var notes []string
	p.OnSwitch = func(s string) { notes = append(notes, s) }
	if got := answer(t, p, prompt("s")); got != "b" || a.calls != 1 || b.calls != 2 {
		t.Fatalf("answered by %q; calls a=%d b=%d", got, a.calls, b.calls)
	}
	if fmt.Sprint(*waits) != "[15s]" || !strings.Contains(strings.Join(notes, "\n"), "waiting 15s for b") {
		t.Fatalf("waits %v, notes %q", *waits, notes)
	}

	// No wait when the first back is more than a minute away, or when an
	// account's limit is a used up quota.
	for _, errs := range [][2]error{
		{&APIError{Status: 429, RetryAfter: 2 * time.Minute}, &APIError{Status: 429, RetryAfter: 90 * time.Second}},
		{&APIError{Status: 429, RetryAfter: 5 * time.Second}, &APIError{Status: 429, Body: "usage_limit_reached", RetryAfter: 10 * time.Second}},
	} {
		*waits = nil
		a := &fakeAccount{name: "a", err: errs[0]}
		b := &fakeAccount{name: "b", err: errs[1]}
		p := newPool(StrategyFallback, a, b)
		if _, err := p.Stream(context.Background(), prompt("s"), nil); err == nil || len(*waits) != 0 || a.calls != 1 || b.calls != 1 {
			t.Fatalf("%v: err %v, waits %v, calls a=%d b=%d", errs, err, *waits, a.calls, b.calls)
		}
	}
}

func TestPoolWaitEndsWithTheRequest(t *testing.T) {
	a := &fakeAccount{name: "a", err: &APIError{Status: 429, RetryAfter: 30 * time.Second}}
	b := &fakeAccount{name: "b", err: &APIError{Status: 429, RetryAfter: 30 * time.Second}}
	p := newPool(StrategyFallback, a, b)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	msg, err := p.Stream(ctx, prompt("s"), nil)
	if !errors.Is(err, context.DeadlineExceeded) || msg.StopReason != "aborted" || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v, stop %q, after %v", err, msg.StopReason, time.Since(start))
	}
}

// A pool of one account has nothing to switch to: its provider waits out
// rate limits itself.
func TestSingleAccountPoolRetriesLimits(t *testing.T) {
	a, b := &fakeAccount{name: "a"}, &fakeAccount{name: "b"}
	answer(t, newPool(StrategyFallback, a), prompt("s"))
	answer(t, newPool(StrategyFallback, b, &fakeAccount{name: "c"}), prompt("s"))
	if failFastLimits(a.ctx) || !failFastLimits(b.ctx) {
		t.Fatalf("fail fast: one account %v, two %v", failFastLimits(a.ctx), failFastLimits(b.ctx))
	}
}
