package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Fast mode (platform.claude.com/docs/en/build-with-claude/fast-mode): the
// same Claude Opus model with a faster inference configuration, up to 2.5x
// the output tokens per second at twice the price per token. On a Claude
// subscription it draws from usage credits only, never from the plan's
// included usage.

const (
	fastBeta = "fast-mode-2026-02-01"
	// fastCooldownDefault is how long fast mode rests after a rate limit or
	// overload that names no Retry-After.
	fastCooldownDefault = time.Minute
)

// fastModelRe matches the models with fast mode, Opus 5.5, 5 and 4.8, with
// an optional date and context suffix. Opus 4.7 lost it on 2026-07-24;
// Opus 4.6 accepts the field but runs at standard speed.
var fastModelRe = regexp.MustCompile(`^claude-opus-(5-5|5|4-8)(-\d{8})?(\[1m\])?$`)

// fastModel reports whether model has fast mode.
func fastModel(model string) bool { return fastModelRe.MatchString(strings.ToLower(model)) }

// SupportsFast reports whether requests to model through p can run in fast
// mode: Anthropic's own API or a Claude subscription, an Opus with it.
func SupportsFast(p Provider, model string) bool {
	if pool, ok := p.(*Pool); ok && len(pool.members) > 0 {
		p = pool.members[0].Provider
	}
	_, ok := p.(*anthropicProvider)
	return ok && fastModel(model)
}

// FastBillsCredits reports whether fast mode through p is billed from usage
// credits: on a Claude subscription it never draws from the plan's
// included usage, even with some left.
func FastBillsCredits(p Provider) bool {
	if pool, ok := p.(*Pool); ok && len(pool.members) > 0 {
		p = pool.members[0].Provider
	}
	a, ok := p.(*anthropicProvider)
	return ok && a.oauth != nil
}

// fastState is a provider's view of fast mode for its account: resting
// after a rate limit until a time, or off for the session because the
// account can't use it (no usage credits, not enabled for the organization).
type fastState struct {
	until time.Time
	off   string
}

// fastFor reports whether this request should ask for fast mode now.
func (p *anthropicProvider) fastFor(req *Request) bool {
	if !req.Fast || !fastModel(req.Model) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fast.off == "" && !time.Now().Before(p.fast.until)
}

// fastRejected handles an error to a fast request. A refusal that names
// fast mode, speed or usage credits turns it off for the session; a rate
// limit or overload rests it for a while. It returns the note for the user
// and whether the request should be sent again at standard speed.
//
// Without usage credits the API answers 429 rate_limit_error "Usage credits
// are required for fast mode." (measured): that is a refusal, not a rate
// limit, so it is checked first, and waiting would not help.
func (p *anthropicProvider) fastRejected(err error) (string, bool) {
	var ae *APIError
	if !errors.As(err, &ae) {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch body := strings.ToLower(ae.Body); {
	case ae.Status == 429 && (strings.Contains(body, "credit") || strings.Contains(body, "overage")):
		p.fast.off = shortReason(ae.Body)
		return "fast mode unavailable (" + p.fast.off + "): standard speed for this session", true
	case ae.Status == 429 || ae.Status == 529:
		wait := ae.RetryAfter
		if wait <= 0 {
			wait = fastCooldownDefault
		}
		p.fast.until = time.Now().Add(wait)
		reason := "rate limited"
		if ae.Status == 529 {
			reason = "overloaded"
		}
		return fmt.Sprintf("fast mode %s: standard speed until %s", reason, p.fast.until.Format("15:04:05")), true
	case ae.Status >= 400 && ae.Status < 500 && (strings.Contains(body, "fast") || strings.Contains(body, "speed") ||
		strings.Contains(body, "credit") || strings.Contains(body, "overage")):
		p.fast.off = shortReason(ae.Body)
		return "fast mode unavailable (" + p.fast.off + "): standard speed for this session", true
	}
	return "", false
}

// shortReason is the message of an API error body (the whole body if it
// has none), on one line and at most 240 bytes.
func shortReason(body string) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	s := body
	if json.Unmarshal([]byte(body), &e) == nil && e.Error.Message != "" {
		s = e.Error.Message
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 240 {
		s = s[:237] + "..."
	}
	return s
}
