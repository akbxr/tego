package agent

import (
	"fmt"
	"sort"

	"github.com/akbxr/tego/internal/llm"
)

// Server-side clearing of old tool results (Anthropic context editing,
// clear_tool_uses). Once the prompt passes clearTrigger tokens the oldest
// tool results are replaced by a placeholder. The cleared set only grows when
// the prompt passes the trigger again, by at least clearAtLeast tokens: each
// pass rewrites the prompt cache once (measured: the turns between passes
// keep hitting the cache), and the model re-reads about one in three cleared
// results, so a pass must remove a lot to pay for itself. Most of the
// saving comes from cache-cold elision (cold.go); replaying recorded Opus
// sessions, this trigger added the most on top of it.
const (
	clearTrigger = 200_000
	clearAtLeast = 80_000
	clearKeep    = 6 // most recent tool uses always kept
)

// clearPolicy is sent with every request; providers that cannot clear
// server-side ignore it.
func (a *Agent) clearPolicy() *llm.ClearPolicy {
	if !a.AutoCompact || a.ContextWindow <= 0 {
		return nil
	}
	trigger := min(clearTrigger, a.CompactAt()*elideSoftRatio/100)
	keep := make([]string, 0, len(elideKeep)+1)
	for name := range elideKeep {
		keep = append(keep, name)
	}
	// A search result loads deferred tools by reference; clearing it would
	// unload them.
	keep = append(keep, "tool_search")
	sort.Strings(keep)
	return &llm.ClearPolicy{Trigger: trigger, ClearAtLeast: min(clearAtLeast, trigger/2), Keep: clearKeep, Exclude: keep}
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
	if n := a.removeThinking(m.DroppedThinking); n > 0 {
		a.UI.OnInfo(fmt.Sprintf("history changed since earlier turns: removed %d thinking block(s) that no longer match it", n))
	}
}

// removeThinking deletes thinking blocks the provider dropped because the
// history before them changed (elision, compaction, rewind, a resumed
// session). Left in, they would be sent and dropped again on every request.
// The provider drops a block and all thinking after it, so this removes
// thinking from the end of the chain, which keeps later blocks valid. Only
// the agent's history changes: the session keeps the blocks, and a resume
// drops them once more.
func (a *Agent) removeThinking(refs []llm.BlockRef) int {
	byMsg := map[int][]int{}
	for _, r := range refs {
		if r.Msg < len(a.Messages) && r.Block < len(a.Messages[r.Msg].Blocks) && a.Messages[r.Msg].Blocks[r.Block].Type == llm.BlockThinking {
			byMsg[r.Msg] = append(byMsg[r.Msg], r.Block)
		}
	}
	n := 0
	for i, drop := range byMsg {
		skip := map[int]bool{}
		for _, j := range drop {
			skip[j] = true
		}
		old := a.Messages[i].Blocks
		kept := make([]llm.Block, 0, len(old)) // a copy: the session tree shares the old slice
		for j, b := range old {
			if !skip[j] {
				kept = append(kept, b)
			}
		}
		n += len(old) - len(kept)
		a.Messages[i].Blocks = kept
	}
	return n
}
