package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// Idle compaction (as Claude Code's): after a break the prompt cache did not
// survive, the next request writes the whole context to the cache again,
// which for a long session costs more than summarising it. So a context
// still above IdleCompactAt after cold elision (free, and first) is
// compacted before that request.
//
// Claude Code compacts just before the cache expires, because its summary
// request reads the cached context. tego's reads a transcript instead,
// which costs the same warm or cold, so it waits until the user is back:
// a session never resumed costs nothing, and nothing runs while they are
// away. The full history stays in the session tree (/tree goes back to it).

// idleBreak is how long a break must be before it compacts: past the 1-hour
// cache of interactive sessions whatever the agent's own TTL, as a summary
// loses detail and a request with a 5-minute TTL (tego -p) may resume a
// session whose context an interactive one cached for an hour.
const idleBreak = time.Hour + 5*time.Minute

// idleCompact compacts before a request after a long break, when the
// context is above IdleCompactAt.
func (a *Agent) idleCompact(ctx context.Context) {
	if a.IdleCompactAt <= 0 || !a.AutoCompact || a.ContextWindow <= 0 || len(a.Messages) < 2 ||
		a.LastCtx < a.IdleCompactAt || a.Provider == nil || !llm.KnownCacheTTL(a.Provider, a.Model) {
		return
	}
	idle := a.cacheIdle()
	if idle < idleBreak {
		return
	}
	a.UI.OnInfo(fmt.Sprintf("idle %s, so the prompt cache expired: compacting ~%.0fk tokens rather than caching them all again (\"idle_compact\": false turns this off; /tree goes back to the full history)",
		idle.Round(time.Minute), float64(a.LastCtx)/1000))
	if err := a.Compact(ctx, ""); err != nil && ctx.Err() == nil {
		a.UI.OnInfo("idle compaction failed: " + err.Error())
	}
}
