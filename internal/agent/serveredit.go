package agent

import (
	"fmt"
	"sort"

	"github.com/akbxr/tego/internal/llm"
)

// Server-side clearing of old tool results (Anthropic context editing,
// clear_tool_uses), as Claude Code does it: the trigger counts tool uses.
// Once the prompt passes clearTrigger tokens, tego sets the trigger just
// below the number of tool uses so far; the server clears all but the last
// clearKeep results, and then nothing more until that many tool uses have
// piled up again, so the turns in between keep hitting the cache. Each
// pass rewrites the cache once, so a pass must also free clearAtLeast
// tokens. When the cache has expired anyway, the trigger is set afresh.
//
// The trigger was a token count before: most of a long session (text,
// thinking, edit inputs) cannot be cleared, so the prompt stayed above it,
// the server cleared a little more on every new tool use, and sessions past
// 600k tokens rewrote the whole cache again and again (127 times in three
// days, ~$450 of cache writes at API prices).
const (
	clearTrigger = 200_000 // tokens before the first pass
	clearAtLeast = 80_000
	clearKeep    = 6  // most recent tool uses always kept
	clearBelow   = 3  // the trigger is this far below the tool uses so far
	clearMinUses = 20 // fewer clearable tool uses: not worth a pass
)

// clearPolicy is sent with every request; providers that cannot clear
// server-side ignore it. nil until the prompt passes clearTrigger.
func (a *Agent) clearPolicy() *llm.ClearPolicy {
	if !a.AutoCompact || a.ContextWindow <= 0 {
		return nil
	}
	at := min(clearTrigger, a.CompactAt()*elideSoftRatio/100)
	// Set the trigger the first time the prompt is past at, and afresh once
	// the cache has expired; otherwise never touch it.
	if (a.clearUses == 0 || a.cacheIdle() > 0) && a.LastCtx > at {
		if n := a.clearableSince(0) - clearBelow; n > clearKeep && n >= clearMinUses {
			a.clearUses = n
		}
	}
	if a.clearUses == 0 {
		return nil
	}
	return &llm.ClearPolicy{TriggerToolUses: a.clearUses, ClearAtLeast: min(clearAtLeast, at/2), Keep: clearKeep, Exclude: clearExempt()}
}

// clearExempt are the tools whose results are never cleared.
func clearExempt() []string {
	keep := make([]string, 0, len(elideKeep)+1)
	for name := range elideKeep {
		keep = append(keep, name)
	}
	// A search result loads deferred tools by reference; clearing it would
	// unload them.
	keep = append(keep, "tool_search")
	sort.Strings(keep)
	return keep
}

// clearableSince counts the tool calls in Messages[from:] whose results the
// server may clear.
func (a *Agent) clearableSince(from int) int {
	n := 0
	for _, m := range a.Messages[min(max(from, 0), len(a.Messages)):] {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolUse && !elideKeep[b.Name] && b.Name != "tool_search" {
				n++
			}
		}
	}
	return n
}

// restoreClearTrigger picks up the trigger a resumed (or rewound) history
// was sent with, so its next request clears the same results and hits the
// cache, and what the server had cleared by then: the clearing is not news
// again, and reads before it are not taken as still visible.
func (a *Agent) restoreClearTrigger() {
	a.clearUses, a.cleared, a.clearMark = 0, 0, 0
	trigger := false
	for i := len(a.Messages) - 1; i >= 0; i-- {
		m := a.Messages[i]
		if m.Role != llm.RoleAssistant || m.Usage == nil {
			continue
		}
		if !trigger {
			a.clearUses, trigger = m.ClearTrigger, true
		}
		if m.ClearedToolUses > 0 {
			a.cleared, a.clearMark = m.ClearedToolUses, i
			return
		}
	}
}

// noteServerEdits reports server-side edits once each: the server
// re-applies the clearing on every request, so only growth is news.
func (a *Agent) noteServerEdits(m *llm.Message) {
	if m.ClearedToolUses > a.cleared {
		a.UI.OnInfo(fmt.Sprintf("old tool output cleared by the provider: %d tool result(s) so far (~%.1fk tokens)",
			m.ClearedToolUses, float64(m.ClearedTokens)/1000))
	}
	if m.ClearedToolUses > 0 || m.ClearedTokens > 0 {
		a.cleared = m.ClearedToolUses
		a.clearMark = len(a.Messages) // any result sent so far may be gone
	}
	// Thinking the provider dropped (the history before it changed) stays
	// in the history: the provider drops it the same way on every request,
	// at no token cost, while removing it would change the prefix the
	// request just cached and write all of it again.
}
