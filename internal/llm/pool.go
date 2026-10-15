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
)

// PoolMember is one account of a provider.
type PoolMember struct {
	Label    string // e.g. the account email
	Provider Provider
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
	current  int            // fallback: account in use; also the last account used
	rr       int            // round-robin cursor
	sessions map[string]int // session -> account for the turn in progress
}

type poolMember struct {
	PoolMember
	coolUntil time.Time
	reason    string
}

func NewPool(name, strategy string, members []PoolMember) *Pool {
	if strategy != StrategyRoundRobin {
		strategy = StrategyFallback
	}
	p := &Pool{name: name, strategy: strategy, sessions: map[string]int{}}
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
// mode takes the next account; otherwise the turn's account is reused.
func (p *Pool) pick(req *Request) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	newTurn := len(req.Messages) == 0 || isPromptMessage(req.Messages[len(req.Messages)-1])
	if p.strategy == StrategyRoundRobin {
		if i, ok := p.sessions[req.SessionID]; ok && !newTurn {
			return i
		}
		i := p.rr % len(p.members)
		p.rr++
		return i
	}
	if i, ok := p.sessions[req.SessionID]; ok && !newTurn {
		return i
	}
	return p.current
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
	start := p.pick(req)
	ctx = context.WithValue(ctx, failFastKey{}, true)
	n := len(p.members)
	var lastErr error
	var lastMsg *Message
	tried := 0
	for k := range n {
		i := (start + k) % n
		m := p.members[i]
		p.mu.Lock()
		cooling := time.Now().Before(m.coolUntil)
		p.mu.Unlock()
		if cooling && k < n-1 {
			continue // try a healthy account first; the last one is attempted anyway
		}
		tried++
		if i != start && p.OnSwitch != nil {
			p.OnSwitch(fmt.Sprintf("%s: switching to %s", p.name, m.Label))
		}
		emitted := false
		msg, err := m.Provider.Stream(ctx, req, func(e Event) {
			emitted = true
			if onEvent != nil {
				onEvent(e)
			}
		})
		if err == nil {
			p.mu.Lock()
			p.current, p.sessions[req.SessionID] = i, i
			if p.strategy == StrategyRoundRobin && i != start {
				p.rr = i + 1 // continue the rotation after the account that answered
			}
			p.mu.Unlock()
			return msg, nil
		}
		if ctx.Err() != nil || emitted || !IsAccountLimit(err) || n == 1 {
			return msg, err
		}
		d, reason := cooldown(err), truncate(firstLine(err.Error()), 100)
		p.mu.Lock()
		m.coolUntil, m.reason = time.Now().Add(d), reason
		p.mu.Unlock()
		if p.OnSwitch != nil {
			p.OnSwitch(fmt.Sprintf("%s: %s hit a limit (%s); resting it for %s", p.name, m.Label, reason, d.Round(time.Second)))
		}
		lastErr, lastMsg = err, msg
	}
	if lastErr == nil {
		lastErr = errors.New("no usable account")
	}
	return lastMsg, fmt.Errorf("%s: all %d accounts are limited (tried %d): %w", p.name, n, tried, lastErr)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// Active describes the account in use, e.g. "me@x.com (2/3)".
func (p *Pool) Active() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("%s (%d/%d)", p.members[p.current].Label, p.current+1, len(p.members))
}

// AccountStatus is one line of /accounts output.
type AccountStatus struct {
	Label     string
	Active    bool
	CoolUntil time.Time
	Reason    string
}

func (p *Pool) Status() []AccountStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]AccountStatus, len(p.members))
	for i, m := range p.members {
		out[i] = AccountStatus{Label: m.Label, Active: i == p.current, CoolUntil: m.coolUntil, Reason: m.reason}
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
