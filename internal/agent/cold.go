package agent

import (
	"fmt"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// Cache-cold elision (as Claude Code's time-based microcompact): once the
// provider's prompt cache has expired, the next request writes the whole
// prompt to the cache again anyway, so stubbing old tool output right then
// costs nothing extra, and the write and every later turn get smaller. The
// edit lands in the history itself, so later requests keep one stable
// prefix (a server-side policy change would not: the server recomputes what
// it clears from the policy on every request).
const coldElideAt = 40_000 // smaller contexts gain too little to drop anything

// cacheTTL is how long the provider keeps the prompt cache after a request.
func (a *Agent) cacheTTL() time.Duration {
	if a.LongCache {
		return time.Hour
	}
	return 5 * time.Minute
}

func (a *Agent) now() time.Time {
	if a.clock != nil {
		return a.clock()
	}
	return time.Now()
}

// cacheIdle is how long ago the last reply arrived, if the prompt cache has
// expired since (with a margin for clock skew); 0 while it is warm or unknown.
func (a *Agent) cacheIdle() time.Duration {
	if a.lastReply.IsZero() {
		return 0
	}
	idle := a.now().Sub(a.lastReply)
	if ttl := a.cacheTTL(); idle <= ttl+ttl/12 {
		return 0
	}
	return idle
}

// coldElide stubs old tool output before a request that will miss the cache.
// Only where the cache's lifetime is exact (Anthropic): an implicit cache may
// still be warm after any fixed wait (a Gemini benchmark found it so after
// 6 minutes), and stubbing then forces a rebuild that cost more than it saved.
func (a *Agent) coldElide() {
	if !a.AutoCompact || a.ContextWindow <= 0 || len(a.Messages) < 2 || a.LastCtx <= coldElideAt {
		return
	}
	if a.Provider == nil || !llm.KnownCacheTTL(a.Provider, a.Model) {
		return
	}
	idle := a.cacheIdle()
	if idle == 0 {
		return
	}
	before := a.LastCtx
	if n, _ := a.elide(coldMinSave); n > 0 {
		a.LastCtx = a.EstimateContext()
		a.UI.OnInfo(fmt.Sprintf("prompt cache expired (idle %s): elided %d old tool block(s) (~%.1fk → ~%.1fk tokens)",
			idle.Round(time.Minute), n, float64(before)/1000, float64(a.LastCtx)/1000))
	}
}
