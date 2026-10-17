package agent

import (
	"context"
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

// classes records the class of every request.
type classesProvider struct {
	implicitProvider
	classes []string
}

func (p *classesProvider) Stream(ctx context.Context, req *llm.Request, fn func(llm.Event)) (*llm.Message, error) {
	p.classes = append(p.classes, req.Class)
	return p.implicitProvider.Stream(ctx, req, fn)
}

func TestImplicitCacheElidesInsteadOfCompactingWhenThatIsEnough(t *testing.T) {
	prov := &classesProvider{}
	// 100k window: compaction above 85k, the soft threshold at 59.5k.
	ui := &infoUI{}
	a := &Agent{Provider: prov, UI: ui, ContextWindow: 100_000, AutoCompact: true, KeepRecent: 5000}
	h := toolHistory(30, 3000) // ~90k tokens, nearly all old tool output
	stripUsage(h)
	a.SetHistory(h)
	a.Messages = append(a.Messages, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "next"}}})
	if err := a.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(prov.classes, ",") != llm.ClassMain {
		t.Fatalf("requests %v, want only the turn itself (no summary)", prov.classes)
	}
	if est := a.EstimateContext(); est > a.CompactAt()*elideSoftRatio/100 {
		t.Fatalf("context after elision ~%d, above the soft threshold", est)
	}
	if !strings.Contains(strings.Join(ui.infos, "\n"), "instead of compacting") {
		t.Fatalf("infos: %q", ui.infos)
	}
}

func TestImplicitCacheStillCompactsWhenElisionIsNotEnough(t *testing.T) {
	prov := &classesProvider{}
	a := &Agent{Provider: prov, UI: nopUI{}, ContextWindow: 100_000, AutoCompact: true, KeepRecent: 5000}
	// ~90k tokens of conversation and only ~16k of tool output: eliding the
	// output would leave ~75k, above the soft threshold.
	h := toolHistory(4, 4000)
	for range 15 {
		h = append(h, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: strings.Repeat("q", 20_000)}}},
			llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: strings.Repeat("a", 4_000)}}})
	}
	stripUsage(h)
	a.SetHistory(h)
	a.Messages = append(a.Messages, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "next"}}})
	if err := a.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(prov.classes, ",") != llm.ClassCompaction+","+llm.ClassMain {
		t.Fatalf("requests %v, want a summary and then the turn", prov.classes)
	}
}

func TestExplicitCacheDoesNotElideAtTheCompactionThreshold(t *testing.T) {
	a := &Agent{Provider: &countingProvider{}, UI: nopUI{}, ContextWindow: 100_000, AutoCompact: true, KeepRecent: 5000}
	h := toolHistory(30, 3000)
	stripUsage(h)
	a.SetHistory(h)
	if !a.needsCompact() {
		t.Fatal("setup: should be above the compaction threshold")
	}
	if n, _ := a.elideBeforeCompact(); n != 0 {
		t.Fatalf("elided %d blocks: only implicit caches take this path (Anthropic elides earlier, cache-aware)", n)
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
