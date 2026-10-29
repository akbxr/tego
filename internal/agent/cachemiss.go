package agent

import (
	"fmt"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// Cache diagnostics (llm.CacheMiss): when a request that should have read
// the prompt cache did not, because something before the new messages
// changed, the provider says what. That is a bug or a setting worth
// knowing about (an MCP server changing its tools, a system prompt with a
// timestamp), so tego says it once per prompt. The changes tego makes
// itself (compaction, elision, another model, a thinking or speed
// switch) and an expired cache are not reported.

// cacheMissReported is the smallest miss worth a word.
const cacheMissReported = 2_000

// expectsCacheRead reports whether sent should have read the cache prev
// wrote: it extends prev's messages, with the same model and settings,
// idle no longer than that cache lasts.
func (a *Agent) expectsCacheRead(prev, sent sentRequest, idle time.Duration) bool {
	p, r := prev.req, sent.req
	return sent.extends && !prev.at.IsZero() && prev.usage.CacheRead+prev.usage.CacheWrite > 0 &&
		sameProvider(prev.provider, sent.provider) && p.Model == r.Model &&
		p.Thinking == r.Thinking && p.Fast == r.Fast && p.LongCache == r.LongCache &&
		idle <= a.warmFor(prev)
}

// cacheMissNote is what to tell about m, "" when nothing.
func cacheMissNote(m *llm.CacheMiss) string {
	if !m.Changed() || m.Tokens < cacheMissReported {
		return ""
	}
	what := map[string]string{
		"tools_changed":    "the tool definitions changed",
		"system_changed":   "the system prompt changed",
		"messages_changed": "an earlier message changed",
		"model_changed":    "the model changed",
	}[m.Reason]
	if what == "" {
		what = m.Reason
	}
	return fmt.Sprintf("prompt cache missed: %s since the last request (%s tokens not read from the cache)", what, fmtK(m.Tokens))
}

func fmtK(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.0fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}
