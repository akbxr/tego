package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeAccount answers with its own name, or fails with err.
type fakeAccount struct {
	name  string
	err   error
	emit  bool // emit an event before failing
	calls int
}

func (f *fakeAccount) Name() string { return "fake" }

func (f *fakeAccount) Stream(ctx context.Context, req *Request, onEvent func(Event)) (*Message, error) {
	f.calls++
	if f.emit {
		onEvent(Event{Type: EvText, Text: "partial"})
	}
	if f.err != nil {
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
