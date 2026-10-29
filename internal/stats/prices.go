package stats

import (
	"regexp"
	"strings"
)

//go:generate go run ./genprices

// Price is a model's API price in US dollars per million tokens.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"` // 5-minute cache writes; 0 = billed as input
	// Tiers price prompts longer than Above tokens (input plus cache), like
	// Gemini and GPT above 200k. Zero fields keep the base price.
	Tiers []Tier `json:"tiers,omitempty"`
}

// Tier is the price of prompts longer than Above tokens.
type Tier struct {
	Above      int     `json:"above"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// forPrompt is the price for a prompt of n tokens.
func (p Price) forPrompt(n int) Price {
	var t *Tier
	for i := range p.Tiers {
		if n > p.Tiers[i].Above && (t == nil || p.Tiers[i].Above > t.Above) {
			t = &p.Tiers[i]
		}
	}
	if t == nil {
		return p
	}
	or := func(v, base float64) float64 {
		if v != 0 {
			return v
		}
		return base
	}
	return Price{Input: or(t.Input, p.Input), Output: or(t.Output, p.Output),
		CacheRead: or(t.CacheRead, p.CacheRead), CacheWrite: or(t.CacheWrite, p.CacheWrite)}
}

var (
	dateSuffixRe   = regexp.MustCompile(`-(20\d{6}|\d{4}-\d{2}-\d{2})$`)
	effortSuffixRe = regexp.MustCompile(`-(low|medium|high|minimal|xhigh|tiered|thinking)$`)
	claudeDotRe    = regexp.MustCompile(`(\d)\.(\d)`)
)

// Prices resolves a model's price: config overrides first, then the
// models.dev catalog fetched by tego stats serve, then the built-in table.
// Override keys may be "model" or "provider/model".
type Prices struct {
	Overrides map[string]Price
	Catalog   map[string]Price
}

func (ps Prices) lookup(provider, model string) (Price, bool) {
	if p, ok := ps.Overrides[provider+"/"+model]; ok {
		return p, true
	}
	ids := priceIDs(model)
	for _, table := range [...]map[string]Price{ps.Overrides, ps.Catalog, builtinPrices()} {
		for _, id := range ids {
			if p, ok := table[id]; ok {
				return p, true
			}
		}
	}
	return Price{}, false
}

// CacheWritePremium reports whether writing the prompt cache costs more than
// plain input for model (Anthropic, OpenAI GPT-5.6): there, every rewrite
// of the history is expensive. Unknown models report false.
func (ps Prices) CacheWritePremium(provider, model string) bool {
	p, ok := ps.lookup(provider, model)
	return ok && p.CacheWrite > p.Input
}

// CacheReadRatio is a cache read's price as a share of an input token's
// (0.1 for most Claude models, 0.05 for Opus and Sonnet 5.5); 0 unknown.
func (ps Prices) CacheReadRatio(provider, model string) float64 {
	p, ok := ps.lookup(provider, model)
	if !ok || p.Input <= 0 || p.CacheRead <= 0 {
		return 0
	}
	return p.CacheRead / p.Input
}

// priceIDs lists the ids a model may be priced under: as given, without a
// vendor prefix (OpenRouter "anthropic/claude-sonnet-4.5"), a date
// suffix, an effort suffix (Antigravity "gemini-3.8-flash-high") or
// "-preview".
func priceIDs(model string) []string {
	m := strings.ToLower(model)
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	if strings.HasPrefix(m, "claude") {
		m = claudeDotRe.ReplaceAllString(m, "$1-$2") // claude-sonnet-4.5 → claude-sonnet-4-5
	}
	ids := []string{m}
	add := func(s string) {
		if s != ids[len(ids)-1] {
			ids = append(ids, s)
		}
	}
	m = dateSuffixRe.ReplaceAllString(m, "")
	add(m)
	m = effortSuffixRe.ReplaceAllString(m, "")
	add(m)
	add(strings.TrimSuffix(m, "-preview"))
	return ids
}

// Spend is what requests cost, split by the kind of token paid for.
type Spend struct {
	Input      float64 `json:"input"` // fresh (uncached) input
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	WebSearch  float64 `json:"web_search"` // the per-search fee of native web search
}

func (s Spend) Total() float64 { return s.Input + s.Output + s.CacheRead + s.CacheWrite + s.WebSearch }

func (s *Spend) add(o Spend) {
	s.Input += o.Input
	s.Output += o.Output
	s.CacheRead += o.CacheRead
	s.CacheWrite += o.CacheWrite
	s.WebSearch += o.WebSearch
}

// webSearchFee is the price of one search a native web search ran, on top
// of its tokens: $14 per 1,000 Google searches grounding a Gemini 3+
// answer (past 5,000 free a month, which a log can't tell apart), $10 per
// 1,000 on Anthropic and OpenAI.
func webSearchFee(model string) float64 {
	if strings.HasPrefix(model, "gemini") {
		return 14.0 / 1000
	}
	return 10.0 / 1000
}

// priceRequest prices one request, and what its input side would have
// cost without caching (every cached token billed as fresh input). 1-hour
// cache writes cost twice the input price.
func priceRequest(p Price, r *Record) (s Spend, uncached float64) {
	p = p.forPrompt(r.Input + r.CacheRead + r.CacheWrite)
	if r.Fast {
		// Fast mode bills every token class at twice the standard rate
		// (Opus 5.5: $8/$40 against $4/$20), cache multipliers on top.
		p.Input, p.Output, p.CacheRead, p.CacheWrite = 2*p.Input, 2*p.Output, 2*p.CacheRead, 2*p.CacheWrite
	}
	write := p.CacheWrite
	if write == 0 {
		write = p.Input
	}
	if r.LongCache && p.CacheWrite != 0 {
		write = 2 * p.Input
	}
	s = Spend{
		Input:      float64(r.Input) * p.Input / 1e6,
		Output:     float64(r.Output) * p.Output / 1e6,
		CacheRead:  float64(r.CacheRead) * p.CacheRead / 1e6,
		CacheWrite: float64(r.CacheWrite) * write / 1e6,
		WebSearch:  float64(r.WebSearches) * webSearchFee(r.Model),
	}
	return s, float64(r.Input+r.CacheRead+r.CacheWrite) * p.Input / 1e6
}
