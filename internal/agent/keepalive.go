package agent

import (
	"fmt"
	"slices"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// Cache keep-alive: while the user is away, the conversation's prompt is
// read into the cache again shortly before it expires (Anthropic's
// max_tokens 0, at the cache read price: a twentieth or a tenth of input),
// so the next prompt finds it warm instead of writing it all again or
// compacting it (idle.go). It runs while the session stays open, at most
// KeepAlive after the last reply; with KeepAliveAuto it also stops once the
// refreshes have cost what letting the cache go cold would (as renting
// skis until the rent reaches their price: never more than twice the best
// choice made knowing when the user comes back).
//
// The request repeats the last one that got a reply (sentRequest), with
// the whole history and a placeholder prompt after it; the breakpoint sits
// on the history's last block, which the next real request's cache lookup
// finds one block before its own.

const (
	// keepAliveMargin: a refresh goes out this long before the cache would
	// expire. Its lifetime counts from the start of the request that last
	// read it, and lastReply is the end of one that may have streamed for
	// minutes.
	keepAliveMargin = 10 * time.Minute
	// keepAliveMinCtx: below this a cold cache is cheap to write again.
	keepAliveMinCtx = 30_000
	// keepAlivePrompt is the placeholder after the history: read, never
	// answered.
	keepAlivePrompt = "(keeping the prompt cache warm; no reply)"
)

type keepAliveState struct {
	since    time.Time // the reply the idle period follows (sent.at)
	n        int       // refreshes since
	read     int       // tokens they read from the cache
	off      bool      // no more in this idle period
	verified int       // len(Messages) when the history was checked against sent
	note     string    // why it stopped, said once at the next prompt
}

// KeepAliveDue returns the keep-alive request to send now, or nil. Call it
// only while no turn runs, from the goroutine that starts turns.
func (a *Agent) KeepAliveDue() *llm.Request {
	if a.KeepAlive <= 0 || !a.LongCache || a.Subagent || a.Provider == nil {
		return nil
	}
	s := &a.sent
	if s.n == 0 || s.at.IsZero() {
		return nil
	}
	if !a.ka.since.Equal(s.at) {
		a.ka = keepAliveState{since: s.at}
	}
	if a.ka.off {
		return nil
	}
	now := a.now()
	ttl := a.cacheTTL()
	if now.Before(a.lastReply.Add(ttl - keepAliveMargin)) {
		return nil
	}
	stop := func(note string) *llm.Request {
		a.ka.off, a.ka.note = true, note
		return nil
	}
	switch {
	case !llm.CanPrewarm(a.Provider, a.Model) || !llm.KnownCacheTTL(a.Provider, a.Model):
		return stop("")
	case s.req.Model != a.Model || !sameProvider(s.provider, a.Provider):
		return stop("")
	case s.usage.CacheRead+s.usage.CacheWrite == 0:
		return stop("") // the provider reported no cache to keep
	case a.LastCtx < keepAliveMinCtx:
		return stop("")
	case now.Sub(a.lastReply) > ttl-time.Minute:
		// Asleep or offline past the TTL: a refresh would write it all.
		return stop("")
	case now.Sub(s.at) >= a.KeepAlive:
		return stop(fmt.Sprintf("stopped after %s", a.KeepAlive.Round(time.Minute)))
	case a.KeepAliveAuto && a.ka.n >= a.keepAliveBudget():
		return stop("stopped when it would have cost more than letting the cache go cold")
	}
	if a.ka.verified != len(a.Messages) {
		// The cache holds what the last request sent: the history must
		// still start with it (a rewind or /tree may have changed it).
		if len(a.Messages) <= s.n || hashMessages(a.Messages[:s.n]) != s.sum {
			return stop("")
		}
		a.ka.verified = len(a.Messages)
	}
	req := s.req
	req.Class = llm.ClassKeepAlive
	req.CachePrefix = len(a.Messages)
	req.Messages = make([]llm.Message, 0, len(a.Messages)+1)
	for _, m := range a.Messages {
		// Its own copy: a turn may start (and elide old blocks) while
		// the request is being encoded.
		m.Blocks = slices.Clone(m.Blocks)
		req.Messages = append(req.Messages, m)
	}
	req.Messages = append(req.Messages, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: keepAlivePrompt}}})
	return &req
}

// KeepAliveDone records the outcome of a keep-alive sent at start. Call it
// as KeepAliveDue, before the next turn starts.
func (a *Agent) KeepAliveDone(req *llm.Request, start time.Time, u llm.Usage, err error) {
	if req == nil || !a.ka.since.Equal(a.sent.at) || a.ka.off {
		return // a reply came since, or it was stopped
	}
	switch {
	case err != nil:
		a.ka.off, a.ka.note = true, "failed: "+err.Error()
		return
	case u.CacheRead == 0:
		// The cache had gone: this wrote it again. Warm now, but the
		// next refresh would only keep paying for a cache that a break
		// in the schedule (sleep) already lost once.
		a.ka.off, a.ka.note = true, "the cache had expired"
	}
	a.ka.n++
	a.ka.read += u.CacheRead
	a.lastReply = start // the cache's lifetime counts from the request's start
}

// keepAliveBudget is how many refreshes cost what a cold cache would: the
// whole context written to the 1-hour cache again (twice the input price),
// or, above IdleCompactAt, a compaction (the transcript in, the summary
// out at five times the input price, the compacted context written).
func (a *Agent) keepAliveBudget() int {
	ctx := float64(a.LastCtx)
	if ctx <= 0 {
		return 0
	}
	r := 0.1
	if a.CacheReadRatio != nil {
		if v := a.CacheReadRatio(a.ProviderName, a.Model); v > 0 {
			r = v
		}
	}
	cold := 2 * ctx
	if a.IdleCompactAt > 0 && a.AutoCompact && a.LastCtx >= a.IdleCompactAt {
		cold = min(ctx, 200_000) + 5*6_000 + 2*50_000
	}
	return int(cold / (r * ctx))
}

// keepAliveReport tells, at the first request after a break, what kept
// the cache warm.
func (a *Agent) keepAliveReport() {
	k := a.ka
	if k.n == 0 && k.note == "" {
		return
	}
	a.ka.n, a.ka.read, a.ka.note = 0, 0, ""
	if k.n == 0 {
		a.UI.OnInfo("prompt cache keep-alive " + k.note)
		return
	}
	times := "1 refresh"
	if k.n != 1 {
		times = fmt.Sprintf("%d refreshes", k.n)
	}
	read := fmt.Sprintf("%.0fk", float64(k.read)/1e3)
	if k.read >= 1e6 {
		read = fmt.Sprintf("%.1fM", float64(k.read)/1e6)
	}
	msg := fmt.Sprintf("kept the prompt cache warm while idle: %s, ~%s tokens read from it", times, read)
	if k.note != "" {
		msg += "; then " + k.note
	}
	a.UI.OnInfo(msg)
}
