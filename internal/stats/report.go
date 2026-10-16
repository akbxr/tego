package stats

import (
	"fmt"
	"sort"
	"time"
)

// Ranges a report can cover.
var Ranges = []string{"24h", "7d", "30d", "90d", "all"}

// Totals aggregates requests. Derived fields are filled by finish.
type Totals struct {
	Requests    int     `json:"requests"`
	Failed      int     `json:"failed"`
	Input       int64   `json:"input_tokens"`
	Output      int64   `json:"output_tokens"`
	CacheRead   int64   `json:"cache_read_tokens"`
	CacheWrite  int64   `json:"cache_write_tokens"`
	Cost        float64 `json:"cost"`          // API-equivalent estimate, priced requests only
	Spend       Spend   `json:"cost_by_token"` // Cost split by token kind
	Unpriced    int     `json:"unpriced"`      // requests whose model has no price
	WebSearches int64   `json:"web_searches"`  // searches native web search ran
	First       int64   `json:"first_ts,omitempty"`
	Last        int64   `json:"last_ts,omitempty"`

	ErrorRate    float64 `json:"error_rate"`
	CacheRate    float64 `json:"cache_rate"`    // share of input tokens read from cache
	CacheSavings float64 `json:"cache_savings"` // share of the input-side cost caching saved
	CacheSaved   float64 `json:"cache_saved"`   // dollars caching saved
	AvgDuration  float64 `json:"avg_duration_ms"`
	AvgTTFT      float64 `json:"avg_ttft_ms"`
	// AvgTPS is output speed while generating: all output tokens over all
	// generation time, of the requests whose generation could be timed (see
	// genWindow). 0 when none could, e.g. a provider that buffers answers.
	AvgTPS float64 `json:"avg_tokens_per_second"`

	uncachedCost    float64
	durSum, ttftSum float64
	durN, ttftN     int
	genOut, genMs   int64
}

// genTokens is the output generated inside r's generation window.
func genTokens(r *Record) int {
	if r.GenOutput > 0 {
		return r.GenOutput
	}
	return r.Output
}

// minGenWindowMs: output that arrives faster than this came in one burst
// (a buffering provider, or a short answer after a long silent think), so
// its speed says nothing about the model.
const minGenWindowMs = 500

// genWindow is the generation time (ms) to measure r's output speed over,
// 0 if it can't be measured. Generation begins at GenStartMs; older records
// only have the first visible delta, which after hidden thinking comes
// late and made speeds look like thousands of tokens per second.
func genWindow(r *Record) int64 {
	start := r.GenStartMs
	if start == 0 {
		start = r.TTFTMs
	}
	if start <= 0 || genTokens(r) <= 0 || r.Error != "" {
		return 0
	}
	if gen := r.DurationMs - start; gen >= minGenWindowMs {
		return gen
	}
	return 0
}

func (t *Totals) add(r *Record, p Price, priced bool) {
	t.Requests++
	if r.Error != "" {
		t.Failed++
	}
	t.Input += int64(r.Input)
	t.Output += int64(r.Output)
	t.CacheRead += int64(r.CacheRead)
	t.CacheWrite += int64(r.CacheWrite)
	t.WebSearches += int64(r.WebSearches)
	if priced {
		s, unc := priceRequest(p, r)
		t.Spend.add(s)
		t.Cost += s.Total()
		t.uncachedCost += unc
	} else if r.Input+r.Output+r.CacheRead+r.CacheWrite > 0 {
		t.Unpriced++
	}
	if t.First == 0 || r.TS < t.First {
		t.First = r.TS
	}
	t.Last = max(t.Last, r.TS)
	if r.DurationMs > 0 {
		t.durSum += float64(r.DurationMs)
		t.durN++
	}
	if r.TTFTMs > 0 {
		t.ttftSum += float64(r.TTFTMs)
		t.ttftN++
	}
	if gen := genWindow(r); gen > 0 {
		t.genOut += int64(genTokens(r))
		t.genMs += gen
	}
}

func (t *Totals) finish() {
	if t.Requests > 0 {
		t.ErrorRate = float64(t.Failed) / float64(t.Requests)
	}
	if in := t.Input + t.CacheRead; in > 0 {
		t.CacheRate = float64(t.CacheRead) / float64(in)
	}
	if t.uncachedCost > 0 {
		inputSide := t.Spend.Input + t.Spend.CacheRead + t.Spend.CacheWrite
		t.CacheSavings = 1 - inputSide/t.uncachedCost
		t.CacheSaved = t.uncachedCost - inputSide
	}
	if t.durN > 0 {
		t.AvgDuration = t.durSum / float64(t.durN)
	}
	if t.ttftN > 0 {
		t.AvgTTFT = t.ttftSum / float64(t.ttftN)
	}
	if t.genMs > 0 {
		t.AvgTPS = float64(t.genOut) / (float64(t.genMs) / 1000)
	}
}

// Tokens is every token the requests used.
func (t *Totals) Tokens() int64 { return t.Input + t.Output + t.CacheRead + t.CacheWrite }

// Group is Totals for one model, provider, folder or agent kind.
type Group struct {
	Key string `json:"key"`
	Totals
}

// ToolRow counts one tool's calls.
type ToolRow struct {
	Tool     string `json:"tool"`
	Calls    int    `json:"calls"`
	Errors   int    `json:"errors"`
	LastUsed int64  `json:"last_used"`
}

// Bucket is one hour or day of the time series.
type Bucket struct {
	Start      int64   `json:"start"`
	Requests   int     `json:"requests"`
	Errors     int     `json:"errors"`
	Input      int64   `json:"input_tokens"`
	Output     int64   `json:"output_tokens"`
	CacheRead  int64   `json:"cache_read_tokens"`
	CacheWrite int64   `json:"cache_write_tokens"`
	Cost       float64 `json:"cost"`
	Spend      Spend   `json:"cost_by_token"`
	// Models holds the cost (requests when nothing is priced) of each of
	// Report.TopModels in this bucket.
	Models []float64 `json:"models"`

	modelCost, modelReqs map[string]float64
}

// Tokens is every token the bucket's requests used.
func (b *Bucket) Tokens() int64 { return b.Input + b.Output + b.CacheRead + b.CacheWrite }

// Report is everything tego stats shows for one range.
type Report struct {
	Range      string    `json:"range"`
	From       int64     `json:"from"` // 0 for "all"
	To         int64     `json:"to"`
	Overall    Totals    `json:"overall"`
	Previous   *Totals   `json:"previous,omitempty"` // the same length of time before From
	ByModel    []Group   `json:"by_model"`
	ByProvider []Group   `json:"by_provider"`
	ByFolder   []Group   `json:"by_folder"`
	ByClass    []Group   `json:"by_agent"`
	ByTool     []ToolRow `json:"by_tool"`
	Bucket     string    `json:"bucket"` // "hour" or "day"
	Series     []Bucket  `json:"series"`
	// TopModels names the models of Bucket.Models: the five that cost
	// most, then "other" when there are more.
	TopModels []string `json:"top_models"`
	// Heat counts requests by weekday (Monday first) and hour, local time.
	Heat   [7][24]int `json:"heat"`
	Errors []Record   `json:"recent_errors"` // newest first
}

// ParseRange returns the start of rng ("" or "all": zero time).
func ParseRange(rng string, now time.Time) (time.Time, error) {
	switch rng {
	case "", "all":
		return time.Time{}, nil
	case "24h":
		return now.Add(-24 * time.Hour), nil
	case "7d", "30d", "90d":
		var days int
		fmt.Sscanf(rng, "%dd", &days)
		return now.AddDate(0, 0, -days), nil
	}
	return time.Time{}, fmt.Errorf("unknown range %q (24h, 7d, 30d, 90d or all)", rng)
}

const recentErrors = 20

// Build aggregates records (sorted by time) within rng.
func Build(recs []Record, rng string, now time.Time, prices Prices) (*Report, error) {
	from, err := ParseRange(rng, now)
	if err != nil {
		return nil, err
	}
	if rng == "" {
		rng = "all"
	}
	rep := &Report{Range: rng, To: now.UnixMilli(), Bucket: "day"}
	if !from.IsZero() {
		rep.From = from.UnixMilli()
	}
	if rng == "24h" {
		rep.Bucket = "hour"
	}
	groups := [4]map[string]*Group{{}, {}, {}, {}}
	tools := map[string]*ToolRow{}
	buckets := map[int64]*Bucket{}
	add := func(m map[string]*Group, key string, r *Record, p Price, ok bool) {
		g := m[key]
		if g == nil {
			g = &Group{Key: key}
			m[key] = g
		}
		g.add(r, p, ok)
	}
	prevFrom := rep.From - (rep.To - rep.From)
	for i := range recs {
		r := &recs[i]
		if r.TS < rep.From {
			if rep.From > 0 && r.TS >= prevFrom && r.Kind != "tool" {
				if rep.Previous == nil {
					rep.Previous = &Totals{}
				}
				p, ok := prices.lookup(r.Provider, r.Model)
				rep.Previous.add(r, p, ok)
			}
			continue
		}
		if r.Kind == "tool" {
			t := tools[r.Tool]
			if t == nil {
				t = &ToolRow{Tool: r.Tool}
				tools[r.Tool] = t
			}
			t.Calls++
			if r.ToolError {
				t.Errors++
			}
			t.LastUsed = max(t.LastUsed, r.TS)
			continue
		}
		p, ok := prices.lookup(r.Provider, r.Model)
		rep.Overall.add(r, p, ok)
		add(groups[0], r.Provider+"/"+r.Model, r, p, ok)
		add(groups[1], r.Provider, r, p, ok)
		add(groups[2], r.Folder, r, p, ok)
		add(groups[3], r.Class, r, p, ok)
		t := time.UnixMilli(r.TS)
		rep.Heat[(int(t.Weekday())+6)%7][t.Hour()]++
		start := bucketStart(t, rep.Bucket)
		b := buckets[start]
		if b == nil {
			b = &Bucket{Start: start, modelCost: map[string]float64{}, modelReqs: map[string]float64{}}
			buckets[start] = b
		}
		b.Requests++
		if r.Error != "" {
			b.Errors++
			rep.Errors = append(rep.Errors, *r)
		}
		b.Input += int64(r.Input)
		b.Output += int64(r.Output)
		b.CacheRead += int64(r.CacheRead)
		b.CacheWrite += int64(r.CacheWrite)
		model := r.Provider + "/" + r.Model
		if ok {
			s, _ := priceRequest(p, r)
			b.Spend.add(s)
			b.Cost += s.Total()
			b.modelCost[model] += s.Total()
		}
		b.modelReqs[model]++
	}
	rep.Overall.finish()
	if rep.Previous != nil {
		rep.Previous.finish()
	}
	lists := [4]*[]Group{&rep.ByModel, &rep.ByProvider, &rep.ByFolder, &rep.ByClass}
	for i, m := range groups {
		for _, g := range m {
			g.finish()
			*lists[i] = append(*lists[i], *g)
		}
		sort.Slice(*lists[i], func(a, b int) bool {
			x, y := (*lists[i])[a], (*lists[i])[b]
			if x.Cost != y.Cost {
				return x.Cost > y.Cost
			}
			return x.Requests > y.Requests
		})
	}
	for _, t := range tools {
		rep.ByTool = append(rep.ByTool, *t)
	}
	sort.Slice(rep.ByTool, func(a, b int) bool { return rep.ByTool[a].Calls > rep.ByTool[b].Calls })
	rep.Series = fillSeries(buckets, rep, now)
	rep.modelSeries()
	for i, j := 0, len(rep.Errors)-1; i < j; i, j = i+1, j-1 {
		rep.Errors[i], rep.Errors[j] = rep.Errors[j], rep.Errors[i]
	}
	if len(rep.Errors) > recentErrors {
		rep.Errors = rep.Errors[:recentErrors]
	}
	return rep, nil
}

// modelSeries fills TopModels and Bucket.Models: cost per model when
// anything is priced, else requests per model.
func (rep *Report) modelSeries() {
	const top = 5
	for i, g := range rep.ByModel { // sorted by cost, then requests
		if i == top {
			rep.TopModels = append(rep.TopModels, "other")
			break
		}
		rep.TopModels = append(rep.TopModels, g.Key)
	}
	for i := range rep.Series {
		b := &rep.Series[i]
		values := b.modelCost
		if rep.Overall.Cost == 0 {
			values = b.modelReqs
		}
		b.Models = make([]float64, len(rep.TopModels))
		for j, g := range rep.ByModel {
			b.Models[min(j, top)] += values[g.Key]
		}
		b.modelCost, b.modelReqs = nil, nil
	}
}

func bucketStart(t time.Time, unit string) int64 {
	if unit == "hour" {
		return t.Truncate(time.Hour).UnixMilli()
	}
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).UnixMilli()
}

// fillSeries returns every bucket from the range start (or the first
// record) to now, empty ones included, so charts have no gaps.
func fillSeries(buckets map[int64]*Bucket, rep *Report, now time.Time) []Bucket {
	start := rep.From
	if start == 0 {
		start = rep.Overall.First
	}
	if start == 0 {
		return nil
	}
	var out []Bucket
	t := time.UnixMilli(bucketStart(time.UnixMilli(start), rep.Bucket))
	for !t.After(now) {
		k := t.UnixMilli()
		if b := buckets[k]; b != nil {
			out = append(out, *b)
		} else {
			out = append(out, Bucket{Start: k})
		}
		if rep.Bucket == "hour" {
			t = t.Add(time.Hour)
		} else {
			t = t.AddDate(0, 0, 1)
		}
	}
	return out
}
