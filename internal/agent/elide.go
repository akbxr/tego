package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/akbxr/tego/internal/llm"
)

// Staged context management (Fan et al., "An Empirical Study of Harness
// Design for Coding Agents", 2026): above a soft threshold, bulky old tool
// output is replaced by short stubs; only if that is not enough does the
// hard threshold trigger LLM summarisation. Elision is rule-based and free,
// so summaries become rarer. It runs once per crossing of the soft
// threshold (not every turn), so the provider's prompt cache is rebuilt
// only when the history actually shrinks. Elided content is not
// recoverable through a tool: the paper found models almost never recall
// it; re-running the tool (read, grep, …) gets fresh output instead.
const (
	elideSoftRatio = 70   // percent of CompactAt (≈60% of the window at the defaults)
	elideMinChars  = 2000 // smaller tool output is kept: a stub would save little
	elidedMarker   = "[elided "
	// elideMaxAt caps the soft threshold on large windows (Complexity Trap,
	// Lindenbauer et al. 2025: masking old observations halves cost);
	// elideRegrow is how much the context must grow again after a pass
	// before the next one (at most a tenth of the window), so a context
	// that hovers above the threshold does not rebuild the cache every turn.
	elideMaxAt  = 100_000
	elideRegrow = 20_000
	// A warm pass must save this much (and elideMinShare of the context):
	// it rewrites the cache from that point on, so a small cut never pays
	// back. omp uses the same 20k floor.
	elideMinSave  = 20_000
	elideMinShare = 4 // a quarter
	// A cold pass costs nothing (the cache is rebuilt anyway) but drops the
	// thinking after the cut, so it too must save something worthwhile.
	coldMinSave = 5_000
)

// spilledRE finds the file in which bash kept the whole of a cut output
// (internal/tools/bash.go).
var spilledRE = regexp.MustCompile(`are in (\S+) \(read it with offset/limit\)`)

// elideKeep lists tools whose results stay verbatim: subagent reports and
// rewind reports are distilled results that cannot be re-created by
// re-running a cheap tool.
var elideKeep = map[string]bool{"task": true, "rewind": true, "todo": true, "checkpoint": true}

// ElideAt is the context size (tokens) above which old tool output is
// elided while the prompt cache is warm; 0 disables it. Re-reading a huge
// context every turn costs more than rebuilding the cache once, so on large
// windows elision starts at a fixed size rather than at a share of the
// window: elideMaxAt where cache writes cost what input costs (Gemini,
// DeepSeek, xAI), the later clearTrigger where they cost a premium
// (GPT-5.6), since each pass rewrites the cache. Where the provider clears
// old tool results itself (clearPolicy), client-side elision is only the
// late backstop: rewriting the history also invalidates thinking. Where the
// cache is implicit (Gemini, OpenAI) it is off: like pi and omp, tego leaves
// the cached history alone there and lets compaction bound it.
func (a *Agent) ElideAt() int {
	at := a.CompactAt() * elideSoftRatio / 100
	if a.Provider != nil && llm.HasImplicitCache(a.Provider, a.Model) {
		return 0
	}
	if a.Provider != nil && llm.ClearsToolResults(a.Provider, a.Model) {
		return at
	}
	if a.writePremium() {
		return min(at, clearTrigger)
	}
	return min(at, elideMaxAt)
}

// elideGap is how much the context must grow after a warm pass before the
// next one.
func (a *Agent) elideGap() int {
	gap := elideRegrow
	if a.writePremium() {
		gap = clearAtLeast
	}
	return min(a.ContextWindow/10, gap)
}

func (a *Agent) writePremium() bool {
	return a.CacheWritePremium != nil && a.CacheWritePremium(a.ProviderName, a.Model)
}

func (a *Agent) needsElide() bool {
	if !a.AutoCompact || a.ContextWindow <= 0 || len(a.Messages) < 2 {
		return false
	}
	at := a.ElideAt()
	return at > 0 && a.LastCtx > max(at, a.elideFloor)
}

// elide stubs out bulky tool results (and bulky string arguments of tool
// calls, e.g. a written file's content) older than the recent window that
// compaction keeps verbatim, if that saves at least minSave tokens. It
// reports how many blocks it changed and the tokens saved (estimated from
// the removed text). The history is left as is when the cut is too small.
func (a *Agent) elide(minSave int) (changed, saved int) {
	end := a.keepCut()
	if end < 0 {
		// The recent window alone is over budget: elide everything but the
		// last tool round so the current step still sees its own output.
		end = len(a.Messages) - 1
	}
	names := map[string]string{} // tool_use id -> tool name
	for _, m := range a.Messages[:end] {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolUse {
				names[b.ID] = b.Name
			}
		}
	}
	next := map[int][]llm.Block{} // message index -> its new blocks, applied only if worth it
	for i := range end {
		m := a.Messages[i]
		var blocks []llm.Block // copy on first change: the session tree shares the originals
		for j, b := range m.Blocks {
			nb, ok := elideBlock(b, names)
			if !ok {
				continue
			}
			if blocks == nil {
				blocks = append([]llm.Block(nil), m.Blocks...)
			}
			blocks[j] = nb
			changed++
			saved += estimateTokens([]llm.Message{{Blocks: []llm.Block{b}}}) - estimateTokens([]llm.Message{{Blocks: []llm.Block{nb}}})
		}
		if blocks != nil {
			next[i] = blocks
		}
	}
	before := a.EstimateContext()
	if changed == 0 || saved < minSave {
		// Nothing worth a cache rebuild: wait until the context grows again.
		a.elideFloor = before + a.elideGap()
		return 0, 0
	}
	for i, blocks := range next {
		m := a.Messages[i]
		m.Blocks = blocks
		a.Messages[i] = m
	}
	// Recorded usage describes the longer history. Measure from it rather
	// than re-estimate the whole history from its text: that misses what
	// the text doesn't show (Gemini counted 100k where the text gave 32k),
	// which made one pass look like a 68k cut when it took 7k.
	stripUsage(a.Messages)
	a.ctxRewrite, a.ctxRewriteAt = max(before-saved, 0), len(a.Messages)
	a.elideFloor = a.EstimateContext() + a.elideGap()
	return changed, saved
}

// warmMinSave is the cut a warm pass needs: elideMinSave, and a quarter of
// the context, so the rebuilt cache is at most three times what is saved.
func (a *Agent) warmMinSave() int {
	return max(elideMinSave, a.LastCtx/elideMinShare)
}

func elideBlock(b llm.Block, names map[string]string) (llm.Block, bool) {
	switch b.Type {
	case llm.BlockToolResult:
		name := names[b.ToolUseID]
		if elideKeep[name] || strings.HasPrefix(b.Content, elidedMarker) {
			return b, false
		}
		if len(b.Content) < elideMinChars && len(b.Images) == 0 {
			return b, false
		}
		if name == "" {
			name = "tool"
		}
		lines := strings.Count(b.Content, "\n") + 1
		stub := fmt.Sprintf("%s%s output: %d lines, %s", elidedMarker, name, lines, kb(len(b.Content)))
		if len(b.Images) > 0 {
			stub += fmt.Sprintf(", %d image(s)", len(b.Images))
		}
		if m := spilledRE.FindStringSubmatch(b.Content); m != nil {
			// bash saved the whole output: point there rather than at a rerun.
			b.Content = stub + ". Old tool output is dropped to save context; read " + m[1] +
				" with offset/limit if you need it (it is kept for a day; after that, run the command again).]"
		} else {
			b.Content = stub + ". Old tool output is dropped to save context; run the tool again if you need it.]"
		}
		b.Images = nil
		return b, true
	case llm.BlockToolUse:
		// A Gemini thought signature is bound to the call as sent: leave it.
		if elideKeep[b.Name] || len(b.Input) < elideMinChars || b.Signature != "" {
			return b, false
		}
		var args map[string]any
		if json.Unmarshal(b.Input, &args) != nil {
			return b, false
		}
		changed := false
		for k, v := range args {
			if s, ok := v.(string); ok && len(s) >= elideMinChars/2 {
				args[k] = fmt.Sprintf("%s%s: %d lines, %s]", elidedMarker, k, strings.Count(s, "\n")+1, kb(len(s)))
				changed = true
			}
		}
		if !changed {
			return b, false
		}
		in, err := json.Marshal(args)
		if err != nil {
			return b, false
		}
		b.Input = in
		return b, true
	}
	return b, false
}

func kb(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}
