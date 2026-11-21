package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Account strategies for a Pool.
const (
	StrategyFallback   = "fallback"    // stay on one account; switch only when it hits a limit
	StrategyRoundRobin = "round-robin" // next account for every new user prompt; also switches on limits
	// A new prompt whose cache has gone cold goes to the account whose
	// weekly quota is lost fastest if unused (most left per hour until its
	// reset); also switches on limits.
	StrategyExpiring = "expiring"
)

// PoolMember is one account of a provider.
type PoolMember struct {
	Label    string // e.g. the account email
	Provider Provider
	// Usage reads the account's subscription limits (expiring strategy);
	// nil when it has none to read (an API key).
	Usage func(context.Context) (*UsageReport, error)
}

// Pool spreads requests for one provider over several accounts. When the
// account in use reports a rate/usage limit (or rejects its credentials)
// before streaming anything, the pool puts it on cooldown and retries the
// same request on the next account. A whole turn (prompt plus its tool
// rounds) stays on one account so the provider's prompt cache stays warm.
type Pool struct {
	name     string
	strategy string
	members  []*poolMember
	OnSwitch func(msg string) // optional: reports account switches

	mu       sync.Mutex
	current  int                  // fallback: account in use; also the last account used
	rr       int                  // round-robin cursor
	sessions map[string]int       // session -> account for the turn in progress
	cut      map[string]int       // session -> account whose response was cut off mid-stream: its retry goes there
	lastUse  map[string]time.Time // session -> its last answered request
	told     string               // expiring: the account last picked or said, so concurrent requests say it once
	fetching chan struct{}        // closed when the usage refresh in flight ends
	now      func() time.Time
}

type poolMember struct {
	PoolMember
	coolUntil time.Time // on the wall clock (Pool.now)
	reason    string
	// rateLimited: the rest is for a rate limit (a 429 that is not a used
	// up quota), which passes within its Retry-After.
	rateLimited bool
	usage       *UsageReport // last read (expiring strategy)
	usageAt     time.Time    // when it was read, or last failed to be
}

func NewPool(name, strategy string, members []PoolMember) *Pool {
	if strategy != StrategyRoundRobin && strategy != StrategyExpiring {
		strategy = StrategyFallback
	}
	p := &Pool{name: name, strategy: strategy, sessions: map[string]int{}, cut: map[string]int{}, lastUse: map[string]time.Time{}, now: wallNow}
	for _, m := range members {
		p.members = append(p.members, &poolMember{PoolMember: m})
	}
	return p
}

func (p *Pool) Name() string { return p.name }

// ClearsToolResults and DefersTools: the accounts of a pool are one kind
// of provider, so the first member answers for all.
func (p *Pool) ClearsToolResults(model string) bool {
	return len(p.members) > 0 && ClearsToolResults(p.members[0].Provider, model)
}

func (p *Pool) KnownCacheTTL(model string) bool {
	return len(p.members) > 0 && KnownCacheTTL(p.members[0].Provider, model)
}

func (p *Pool) ImplicitPrefixCache(model string) bool {
	return len(p.members) > 0 && HasImplicitCache(p.members[0].Provider, model)
}

func (p *Pool) DefersTools(model string) bool {
	return len(p.members) > 0 && DefersTools(p.members[0].Provider, model)
}

func (p *Pool) TakesVideo(model string) bool {
	return len(p.members) > 0 && TakesVideo(p.members[0].Provider, model)
}

func (p *Pool) MaxMedia(model string) (images, bytes int) {
	if len(p.members) == 0 {
		return 0, 0
	}
	return MaxMedia(p.members[0].Provider, model)
}

func (p *Pool) TakesAudio(model string) bool {
	return len(p.members) > 0 && TakesAudio(p.members[0].Provider, model)
}

type failFastKey struct{}

// failFastLimits reports whether rate-limit errors should be returned
// immediately (the pool fails over) instead of retried with backoff.
func failFastLimits(ctx context.Context) bool { return ctx.Value(failFastKey{}) != nil }

// IsAccountLimit reports errors tied to the account rather than the
// service: rate/usage/quota limits and rejected credentials.
func IsAccountLimit(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		switch ae.Status {
		case http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired:
			return true
		}
	}
	s := strings.ToLower(err.Error())
	for _, k := range []string{"rate_limit", "rate limit", "usage_limit", "usage limit", "quota", "resource_exhausted", "extra usage", "not logged in", "token expired", "auth: refresh"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// poolMaxWait: when every account rests from a rate limit and one is back
// within this, a request waits for it instead of failing.
const poolMaxWait = time.Minute

// isRateLimit reports a 429 that passes with time, not a used up quota.
func isRateLimit(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusTooManyRequests && ae.retryable()
}

// cooldown is how long an account sits out after err.
func cooldown(err error) time.Duration {
	var ae *APIError
	if errors.As(err, &ae) {
		if ae.RetryAfter > 0 {
			return min(ae.RetryAfter, 6*time.Hour)
		}
		if ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden {
			return 10 * time.Minute
		}
	}
	return time.Minute
}

// pick chooses the starting account for req: a new prompt in round-robin
// mode takes the next account, in expiring mode the account chosen by
// expiringPick; otherwise the turn's account is reused. note says why an
// expiring pick moved to another account.
func (p *Pool) pick(req *Request) (i int, note string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i, ok := p.cut[req.SessionID]; ok {
		// Asked again after a response was cut off: the same account, whose
		// cache holds the prompt, even when it is a turn's first request.
		delete(p.cut, req.SessionID)
		return i, ""
	}
	newTurn := isNewTurn(req)
	if p.strategy == StrategyExpiring {
		return p.expiringPick(req, newTurn)
	}
	if p.strategy == StrategyRoundRobin {
		if i, ok := p.sessions[req.SessionID]; ok && !newTurn {
			return i, ""
		}
		i := p.rr % len(p.members)
		p.rr++
		return i, ""
	}
	if i, ok := p.sessions[req.SessionID]; ok && !newTurn {
		return i, ""
	}
	return p.current, ""
}

func isNewTurn(req *Request) bool {
	return len(req.Messages) == 0 || isPromptMessage(req.Messages[len(req.Messages)-1])
}

func (p *Pool) CanPrewarm(model string) bool {
	return len(p.members) > 0 && CanPrewarm(p.members[0].Provider, model)
}

// Prewarm goes to the account the session's last turn used, as the cache
// it keeps is that account's; there is no switching on a limit.
func (p *Pool) Prewarm(ctx context.Context, req *Request) (Usage, error) {
	p.mu.Lock()
	i, ok := p.sessions[req.SessionID]
	if !ok {
		i = p.current
	}
	p.mu.Unlock()
	return Prewarm(ctx, p.members[i].Provider, req)
}

func isPromptMessage(m Message) bool {
	if m.Role != RoleUser {
		return false
	}
	for _, b := range m.Blocks {
		if b.Type == BlockToolResult {
			return false
		}
	}
	return true
}

func (p *Pool) Stream(ctx context.Context, req *Request, onEvent func(Event)) (*Message, error) {
	if p.strategy == StrategyExpiring && isNewTurn(req) {
		p.refreshUsage(ctx)
	}
	start, note := p.pick(req)
	if note != "" && p.OnSwitch != nil {
		p.OnSwitch(note)
	}
	n := len(p.members)
	if n > 1 {
		// Limits switch accounts; a single account waits them out itself.
		ctx = context.WithValue(ctx, failFastKey{}, true)
	}
	var lastErr error
	var lastMsg *Message
	tried := 0
	announce := true // say when another account than start takes over
	// attempt streams req on account i; done is false when it hit a limit
	// before streaming anything and now rests, so another may take over.
	attempt := func(i int) (msg *Message, err error, done bool) {
		tried++
		m := p.members[i]
		if i != start && announce && p.OnSwitch != nil {
			p.OnSwitch(fmt.Sprintf("%s: switching to %s", p.name, m.Label))
		}
		emitted := false
		msg, err = m.Provider.Stream(ctx, req, func(e Event) {
			emitted = true
			if onEvent != nil {
				onEvent(e)
			}
		})
		if err == nil {
			p.mu.Lock()
			p.current, p.sessions[req.SessionID] = i, i
			p.lastUse[req.SessionID] = p.now()
			m.coolUntil, m.reason, m.rateLimited = time.Time{}, "", false
			if p.strategy == StrategyRoundRobin && i != start {
				p.rr = i + 1 // continue the rotation after the account that answered
			}
			p.mu.Unlock()
			return msg, nil, true
		}
		if emitted && ctx.Err() == nil {
			p.mu.Lock()
			p.cut[req.SessionID] = i
			p.mu.Unlock()
		}
		if ctx.Err() != nil || emitted || !IsAccountLimit(err) || n == 1 {
			return msg, err, true
		}
		d, reason := cooldown(err), truncate(firstLine(err.Error()), 100)
		p.mu.Lock()
		m.coolUntil, m.reason, m.rateLimited = p.now().Add(d), reason, isRateLimit(err)
		p.mu.Unlock()
		if p.OnSwitch != nil {
			p.OnSwitch(fmt.Sprintf("%s: %s hit a limit (%s); resting it for %s", p.name, m.Label, reason, d.Round(time.Second)))
		}
		lastErr, lastMsg = err, msg
		return msg, err, false
	}
	// Accounts not resting first, in rotation order.
	for k := range n {
		i := (start + k) % n
		if p.resting(i) {
			continue
		}
		if msg, err, done := attempt(i); done {
			return msg, err
		}
	}
	if tried == 0 {
		// All rest: a rest is a guess, so the account back soonest is
		// tried anyway.
		if msg, err, done := attempt(p.soonest(start)); done {
			return msg, err
		}
	}
	// Every account rests now. When all of them hit a rate limit and one is
	// back within poolMaxWait, wait for it and try it once more.
	if i, wait, ok := p.shortWait(start); ok {
		if p.OnSwitch != nil {
			p.OnSwitch(fmt.Sprintf("%s: every account is rate limited; waiting %s for %s", p.name,
				max(wait, time.Second).Round(time.Second), p.members[i].Label))
		}
		if err := sleepCtx(ctx, wait); err != nil {
			msg := &Message{Role: RoleAssistant}
			if lastMsg != nil {
				c := *lastMsg
				msg = &c
			}
			msg.StopReason = "aborted"
			return msg, err
		}
		announce = false // the wait named it
		if msg, err, done := attempt(i); done {
			return msg, err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no usable account")
	}
	return lastMsg, fmt.Errorf("%s: all %d accounts are limited (tried %d): %w", p.name, n, tried, lastErr)
}

// resting reports whether account i is on cooldown. Rests run on the wall
// clock (p.now), as the limits they wait out reset by it: a laptop's sleep
// must not stretch them.
func (p *Pool) resting(i int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.now().Before(p.members[i].coolUntil)
}

// soonest is the account whose rest ends first; ties go to the one first
// in rotation order from start.
func (p *Pool) soonest(start int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.members)
	best := start % n
	for k := 1; k < n; k++ {
		if i := (start + k) % n; p.members[i].coolUntil.Before(p.members[best].coolUntil) {
			best = i
		}
	}
	return best
}

// shortWait reports, when every account rests from a rate limit, the one
// back soonest and how long until then, if that is within poolMaxWait.
func (p *Pool) shortWait(start int) (i int, wait time.Duration, ok bool) {
	i = p.soonest(start)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.members {
		if !m.rateLimited {
			return 0, 0, false // a used up quota or a login: waiting does not help
		}
	}
	wait = max(p.members[i].coolUntil.Sub(p.now()), 0)
	return i, wait, wait <= poolMaxWait
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// AccountStatus is one line of /accounts output.
type AccountStatus struct {
	Label     string
	Active    bool
	CoolUntil time.Time
	Reason    string
	Quota     string // expiring strategy: its weekly quota, e.g. "80% of the week left, resets in 11h"
}

func (p *Pool) Status() []AccountStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]AccountStatus, len(p.members))
	now := p.now()
	for i, m := range p.members {
		out[i] = AccountStatus{Label: m.Label, Active: i == p.current, CoolUntil: m.coolUntil, Reason: m.reason}
		if p.strategy == StrategyExpiring && m.usage != nil {
			if q, ok := weeklyQuota(m.usage, "", now); ok {
				out[i].Quota = q.String(now)
			}
		}
	}
	return out
}

func (p *Pool) Strategy() string { return p.strategy }

// ListModels and WebSearch use the account currently in use.
func (p *Pool) ListModels(ctx context.Context) ([]string, error) {
	p.mu.Lock()
	m := p.members[p.current]
	p.mu.Unlock()
	if l, ok := m.Provider.(ModelLister); ok {
		return l.ListModels(ctx)
	}
	return nil, errors.New("provider cannot list models")
}

// WebSearch starts on the account in use and moves on when it is limited.
func (p *Pool) WebSearch(ctx context.Context, model, query string) (*WebResult, error) {
	p.mu.Lock()
	start := p.current
	p.mu.Unlock()
	var err error
	for k := range p.members {
		m := p.members[(start+k)%len(p.members)]
		w, ok := m.Provider.(WebSearcher)
		if !ok {
			return nil, errors.New("provider has no native web search")
		}
		var res *WebResult
		if res, err = w.WebSearch(ctx, model, query); err == nil || ctx.Err() != nil || !IsAccountLimit(err) {
			return res, err
		}
	}
	return nil, err
}

// wallNow is time.Now without its monotonic reading, which stops while a
// Mac sleeps: cache lifetimes run on the provider's clock.
func wallNow() time.Time { return time.Now().Round(0) }
