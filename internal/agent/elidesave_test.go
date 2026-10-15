package agent

import (
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
)

// implicitProvider is a provider with an implicit prompt cache (Gemini, OpenAI).
type implicitProvider struct{ countingProvider }

func (*implicitProvider) ImplicitPrefixCache(string) bool { return true }

func TestNoWarmElisionWithAnImplicitCache(t *testing.T) {
	a := &Agent{Provider: &implicitProvider{}, UI: nopUI{}, ContextWindow: 1_000_000, AutoCompact: true, KeepRecent: 5000}
	a.SetHistory(toolHistory(60, 5000)) // ~300k tokens of bulky tool output
	a.LastCtx = 300_000
	if a.ElideAt() != 0 || a.needsElide() {
		t.Fatalf("implicit cache: ElideAt %d, needsElide %v", a.ElideAt(), a.needsElide())
	}
	// Compaction still bounds the context.
	if a.CompactAt() <= 0 || a.CompactAt() >= a.ContextWindow {
		t.Fatalf("CompactAt %d", a.CompactAt())
	}
}

func TestElisionMeasuresFromRealUsage(t *testing.T) {
	a := &Agent{Provider: &countingProvider{}, UI: nopUI{}, ContextWindow: 1_000_000, AutoCompact: true, KeepRecent: 5000}
	h := toolHistory(12, 10_000) // twelve 10k-token results
	// The provider measured three times what the text suggests (Gemini did).
	last := len(h) - 2
	h[last].Usage = &llm.Usage{Input: 3 * estimateTokens(h[:last+1])}
	a.SetHistory(h)
	before := a.EstimateContext()
	n, saved := a.elide(0)
	if n == 0 || saved < 50_000 {
		t.Fatalf("elided %d blocks, saved %d", n, saved)
	}
	after := a.EstimateContext()
	// Real size minus the cut, not a re-estimate of the text (which would be
	// about a third of it).
	if want := before - saved; after < want-100 || after > want+100 {
		t.Fatalf("after elision %d, want about %d (before %d, saved %d)", after, want, before, saved)
	}
	// The next response's usage replaces the correction.
	a.append(llm.Message{Role: llm.RoleAssistant, Usage: &llm.Usage{Input: 12_345}, Blocks: []llm.Block{{Type: llm.BlockText, Text: "ok"}}})
	if got := a.EstimateContext(); got != 12_345 {
		t.Fatalf("after a reply: %d", got)
	}
}

func TestElisionSkipsASmallCut(t *testing.T) {
	a := &Agent{Provider: &countingProvider{}, UI: nopUI{}, ContextWindow: 1_000_000, AutoCompact: true, KeepRecent: 5000}
	h := toolHistory(3, 1000) // three 1k-token results: a cut of ~3k
	h[len(h)-2].Usage = &llm.Usage{Input: 120_000}
	a.SetHistory(h)
	a.LastCtx = a.EstimateContext()
	if n, _ := a.elide(a.warmMinSave()); n != 0 {
		t.Fatalf("elided %d blocks for a tiny saving", n)
	}
	for _, m := range a.Messages {
		for _, b := range m.Blocks {
			if strings.HasPrefix(b.Content, elidedMarker) {
				t.Fatal("history rewritten for a tiny saving")
			}
		}
	}
	if a.Messages[len(a.Messages)-2].Usage == nil {
		t.Fatal("usage dropped although nothing changed")
	}
	if a.needsElide() {
		t.Fatal("tries again before the context grew")
	}
	if got := a.warmMinSave(); got != a.LastCtx/4 || got < 30_000 { // a quarter of ~121k
		t.Fatalf("warmMinSave %d for context %d", got, a.LastCtx)
	}
}
