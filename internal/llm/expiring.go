package llm

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The expiring strategy. Subscription quotas reset on fixed weekly
// windows, so quota an account has left at its reset is lost. A new prompt
// goes to the account that loses the most per hour if unused: the share of
// its week left divided by the hours until it resets. A prompt whose
// session's cache is still warm stays on its account, since moving writes
// the whole prompt cache again on the other one.

const (
	// Limits read longer ago are read again: from the store all processes
	// share (LimitStore), which asks a usage endpoint only now and then.
	usageFresh   = time.Minute
	usageTimeout = 4 * time.Second // the longest a prompt waits for them
	burstFull    = 0.9             // a short window (5 hours) this used is skipped
	weekEmpty    = 0.02            // a week with this little left is skipped
)

// quota is an account's weekly limit that binds: the one with the least
// left of those that apply to the model.
type quota struct {
	left   float64 // 0..1
	resets time.Time
}

// rate is how much of the week is lost per hour if the account sits idle.
func (q quota) rate(now time.Time) float64 {
	h := 7 * 24.0 // a reset not reported: as if a whole week were left
	if !q.resets.IsZero() {
		h = q.resets.Sub(now).Hours()
	}
	return q.left / max(h, 1)
}

func (q quota) String(now time.Time) string {
	s := fmt.Sprintf("%.0f%% of the week left", q.left*100)
	if !q.resets.IsZero() {
		s += ", resets in " + shortDuration(q.resets.Sub(now))
	}
	return s
}

// weeklyQuota reads r's weekly limits that apply to model: shared ones and
// those of model's tier ("Fable", "Opus"). model "" takes shared ones only.
func weeklyQuota(r *UsageReport, model string, now time.Time) (quota, bool) {
	q, found := quota{left: 1}, false
	for _, l := range r.Limits {
		if l.Window != "7d" || l.Used < 0 || (!l.ResetsAt.IsZero() && l.ResetsAt.Before(now)) {
			continue
		}
		if l.Tier != "" && (model == "" || !strings.Contains(strings.ToLower(model), strings.ToLower(l.Tier))) {
			continue
		}
		if left := max(0, 1-l.Used); !found || left < q.left {
			q, found = quota{left: left, resets: l.ResetsAt}, true
		}
	}
	return q, found
}

// burstFullUp reports a short window (5 hours) nearly used up.
func burstFullUp(r *UsageReport, now time.Time) bool {
	for _, l := range r.Limits {
		if strings.HasSuffix(l.Window, "h") && l.Used >= burstFull && (l.ResetsAt.IsZero() || l.ResetsAt.After(now)) {
			return true
		}
	}
	return false
}

// expiringPick is pick for the expiring strategy; p.mu is held.
func (p *Pool) expiringPick(req *Request, newTurn bool) (int, string) {
	prev, had := p.sessions[req.SessionID]
	if had && (!newTurn || p.cacheWarm(req)) {
		return prev, ""
	}
	now := p.now()
	best, bestRate := -1, -1.0
	var bestQuota quota
	for i, m := range p.members {
		if m.usage == nil || now.Before(m.coolUntil) || burstFullUp(m.usage, now) {
			continue
		}
		q, ok := weeklyQuota(m.usage, req.Model, now)
		if !ok {
			q = quota{left: 1} // no weekly limit reported: nothing to lose soon
		}
		if q.left <= weekEmpty {
			continue
		}
		if r := q.rate(now); r > bestRate {
			best, bestRate, bestQuota = i, r, q
		}
	}
	from := p.current
	if had {
		from = prev
	}
	if best < 0 { // no limits read: as fallback does
		return from, ""
	}
	if best == from {
		return best, ""
	}
	return best, fmt.Sprintf("%s: using %s, %s", p.name, p.members[best].Label, bestQuota.String(now))
}

// cacheWarm reports whether req's session made a request within the
// cache's lifetime (5 minutes, or an hour with the long cache).
func (p *Pool) cacheWarm(req *Request) bool {
	last, ok := p.lastUse[req.SessionID]
	if !ok {
		return false
	}
	ttl := 5 * time.Minute
	if req.LongCache {
		ttl = time.Hour
	}
	return p.now().Sub(last) < ttl
}

// refreshUsage reads the limits of the accounts last read more than
// usageFresh ago, at most usageTimeout long. One refresh runs at a time;
// others wait for it.
func (p *Pool) refreshUsage(ctx context.Context) {
	p.mu.Lock()
	if wait := p.fetching; wait != nil {
		p.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
		}
		return
	}
	now := p.now()
	var stale []*poolMember
	for _, m := range p.members {
		if m.Usage != nil && now.Sub(m.usageAt) >= usageFresh {
			stale = append(stale, m)
		}
	}
	if len(stale) == 0 {
		p.mu.Unlock()
		return
	}
	done := make(chan struct{})
	p.fetching = done
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()
	type result struct {
		m *poolMember
		r *UsageReport
	}
	results := make(chan result, len(stale))
	for _, m := range stale {
		go func() {
			r, err := m.Usage(ctx)
			if err != nil {
				r = nil
			}
			results <- result{m, r}
		}()
	}
	got := map[*poolMember]*UsageReport{}
	for range stale {
		select {
		case res := <-results:
			got[res.m] = res.r
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	p.mu.Lock()
	now = p.now()
	for _, m := range stale {
		m.usageAt = now // a failure waits as long before the next try
		if r := got[m]; r != nil {
			m.usage = r
		}
	}
	p.fetching = nil
	p.mu.Unlock()
	close(done)
}

// shortDuration formats d as "45m", "11h39m" or "3d14h".
func shortDuration(d time.Duration) string {
	d = max(d, 0).Round(time.Minute)
	switch h := int(d.Hours()); {
	case h >= 24:
		return fmt.Sprintf("%dd%dh", h/24, h%24)
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}
