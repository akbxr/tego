package stats

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

//go:embed dashboard.html
var dashboardHTML string

//go:embed fonts/*.woff2
var fonts embed.FS

// Dashboard serves the stats page at / (?range=7d), the report as JSON at
// /api/stats and the page's fonts. Records are reloaded on every request,
// so a reload shows new activity.
type Dashboard struct {
	Dir       string           // tego home with stats.jsonl and sessions/
	Overrides map[string]Price // config prices

	catalog atomic.Pointer[catalogState]
}

type catalogState struct {
	prices map[string]Price
	at     time.Time
	live   bool // fetched by this process, not read from the cache
}

// SetCatalog switches the dashboard to models.dev prices fetched at at;
// live tells a fresh download from the cached copy.
func (d *Dashboard) SetCatalog(prices map[string]Price, at time.Time, live bool) {
	d.catalog.Store(&catalogState{prices, at, live})
}

// PriceSource describes where the dashboard's prices come from.
func (d *Dashboard) PriceSource() string {
	c := d.catalog.Load()
	switch {
	case c == nil:
		return "prices built in from models.dev, " + PricesDate
	case c.live:
		return "prices from models.dev, fetched " + c.at.Format("15:04")
	}
	return "prices from models.dev, cached " + c.at.Format("Jan 2 15:04")
}

func (d *Dashboard) prices() Prices {
	p := Prices{Overrides: d.Overrides}
	if c := d.catalog.Load(); c != nil {
		p.Catalog = c.prices
	}
	return p
}

func (d *Dashboard) report(r *http.Request) (*Report, error) {
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "30d"
	}
	recs, err := Load(d.Dir)
	if err != nil {
		return nil, err
	}
	return Build(recs, rng, time.Now(), d.prices())
}

var pageFuncs = template.FuncMap{
	"compact": func(n any) string {
		switch v := n.(type) {
		case int:
			return Compact(int64(v))
		case int64:
			return Compact(v)
		}
		return fmt.Sprint(n)
	},
	"money":   Money,
	"seconds": Seconds,
	"pct":     pct,
	"share": func(v, total float64) float64 {
		if total <= 0 {
			return 0
		}
		return math.Min(v/total, 1)
	},
	"width":  func(f float64) string { return fmt.Sprintf("%.2f%%", f*100) },
	"tps":    func(f float64) string { return fmt.Sprintf("%.0f", f) },
	"folder": FolderLabel,
	"when":   func(ms int64) string { return time.UnixMilli(ms).Format("Jan 2, 15:04") },
	"ago":    ago,
	// window is the span the report covers: the range itself, or for
	// "all" the first record until now.
	"window": func(rep *Report) string {
		from := rep.From
		if from == 0 {
			from = rep.Overall.First
		}
		return time.UnixMilli(from).Format("Jan 2, 15:04") + " – " + time.UnixMilli(rep.To).Format("Jan 2, 15:04")
	},
	"label": RangeLabel,
	"prev": func(rng string) string {
		return strings.Replace(RangeLabel(rng), "last", "previous", 1)
	},
	"agent":  agentName,
	"plural": plural,
	"float":  func(n int) float64 { return float64(n) },
	"tok":    func(t Totals) float64 { return float64(t.Tokens()) },
	"rate": func(n, of int) float64 {
		if of == 0 {
			return 0
		}
		return float64(n) / float64(of)
	},
	"split": func(key string) [2]string { // "provider/model" → provider, model
		p, m, _ := strings.Cut(key, "/")
		return [2]string{p, m}
	},
	"oneline":    func(s string) string { return strings.Join(strings.Fields(s), " ") },
	"modelColor": func(i int) string { return fmt.Sprintf("m%d", min(i, 5)+1) },
	"kinds":      kindRows,
	"spend":      groupKinds,
	"delta":      delta,
	"chart":      chart,
	"trend":      trend,
	"modelTrend": modelTrend,
	"dots":       dots,
	"readouts":   readouts,
	"latency":    latencyRows,
	"sub":        func(a, b int) int { return a - b },
}

func pct(f float64) string { return trimZero(fmt.Sprintf("%.1f", f*100)) + "%" }

func (d *Dashboard) Handler() http.Handler {
	page := template.Must(template.New("page").Funcs(pageFuncs).Parse(dashboardHTML))
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		rep, err := d.report(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	})
	fontFiles := http.FileServer(http.FS(fonts))
	mux.HandleFunc("/fonts/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fontFiles.ServeHTTP(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		rep, err := d.report(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		prev := Totals{}
		if rep.Previous != nil {
			prev = *rep.Previous
		}
		var b strings.Builder
		data := map[string]any{"R": rep, "P": prev, "HasPrev": rep.Previous != nil, "Ranges": Ranges,
			"Now": time.Now(), "Dir": FolderLabel(d.Dir), "PriceSource": d.PriceSource()}
		if err := page.Execute(&b, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	})
	return mux
}

// Listen binds host:port, falling back to a free port when port is taken
// and was not asked for explicitly.
func Listen(host string, port int, explicit bool) (net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil && !explicit {
		ln, err = net.Listen("tcp", net.JoinHostPort(host, "0"))
	}
	return ln, err
}

func ago(ms int64) string {
	d := time.Since(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func agentName(class string) string {
	switch class {
	case llm.ClassMain:
		return "Main agent"
	case llm.ClassSubagent:
		return "Subagents"
	case llm.ClassCompaction:
		return "Compaction"
	case llm.ClassAuxiliary:
		return "Titles and search"
	}
	return class
}

// A token kind, in the order charts stack them.
type kind struct {
	Class, Name string
	tokens      func(*Totals) int64
	cost        func(Spend) float64
	bucket      func(*Bucket) int64
}

var tokenKinds = []kind{
	{"k-read", "Cache reads", func(t *Totals) int64 { return t.CacheRead }, func(s Spend) float64 { return s.CacheRead }, func(b *Bucket) int64 { return b.CacheRead }},
	{"k-write", "Cache writes", func(t *Totals) int64 { return t.CacheWrite }, func(s Spend) float64 { return s.CacheWrite }, func(b *Bucket) int64 { return b.CacheWrite }},
	{"k-in", "Fresh input", func(t *Totals) int64 { return t.Input }, func(s Spend) float64 { return s.Input }, func(b *Bucket) int64 { return b.Input }},
	{"k-out", "Output", func(t *Totals) int64 { return t.Output }, func(s Spend) float64 { return s.Output }, func(b *Bucket) int64 { return b.Output }},
}

// KindRow is one token kind's share of tokens and of cost.
type KindRow struct {
	Class, Name           string
	Tokens                int64
	Cost                  float64
	TokenShare, CostShare float64
}

func kindRows(t Totals) []KindRow {
	rows := make([]KindRow, len(tokenKinds))
	total, cost := float64(t.Tokens()), t.Spend.Total()
	for i, k := range tokenKinds {
		r := KindRow{Class: k.Class, Name: k.Name, Tokens: k.tokens(&t), Cost: k.cost(t.Spend)}
		if total > 0 {
			r.TokenShare = float64(r.Tokens) / total
		}
		if cost > 0 {
			r.CostShare = r.Cost / cost
		}
		rows[i] = r
	}
	return rows
}

// groupKinds is kindRows for one model, agent or folder.
func groupKinds(g Group) []KindRow { return kindRows(g.Totals) }

// delta compares a figure with the previous period. good is "up", "down"
// or "" (neutral) and colours the change.
func delta(cur, prev float64, hasPrev bool, good string) template.HTML {
	if !hasPrev {
		return `<span class="delta">no earlier data</span>`
	}
	var text, dir string
	switch {
	case prev == 0 && cur == 0:
		return `<span class="delta">unchanged</span>`
	case prev == 0:
		text, dir = "new", "up"
	default:
		ch := (cur - prev) / prev * 100
		switch {
		case math.Abs(ch) < 0.5:
			return `<span class="delta">unchanged</span>`
		case ch > 0:
			dir = "up"
		default:
			dir = "down"
		}
		text = fmt.Sprintf("%.0f%%", math.Abs(ch))
		if math.Abs(ch) >= 1000 {
			text = fmt.Sprintf("%.0f×", cur/prev)
		}
	}
	tone := ""
	if good != "" {
		tone = map[bool]string{true: " good", false: " bad"}[dir == good]
	}
	arrow := map[string]string{"up": "↑", "down": "↓"}[dir]
	return template.HTML(fmt.Sprintf(`<span class="delta%s">%s %s</span>`, tone, arrow, text))
}

// Readout is the figures the page shows for the whole range or, while the
// pointer is on the timeline, for one bucket.
type Readout struct {
	Label  string      `json:"label"`
	Cost   string      `json:"cost"`
	Req    string      `json:"requests"`
	Tokens string      `json:"tokens"`
	Cache  string      `json:"cache"`
	Failed string      `json:"failed"`
	Kinds  [][2]string `json:"kinds"`  // per token kind: cost, tokens
	Models []string    `json:"models"` // per Report.TopModels: cost (or requests)
}

func readout(label string, req, failed int, t *Totals, models []float64, money bool) Readout {
	r := Readout{Label: label, Cost: Money(t.Spend.Total()), Req: Compact(int64(req)), Tokens: Compact(t.Tokens()), Failed: Compact(int64(failed)), Cache: "–"}
	if in := t.Input + t.CacheRead; in > 0 {
		r.Cache = pct(float64(t.CacheRead) / float64(in))
	}
	for _, k := range tokenKinds {
		r.Kinds = append(r.Kinds, [2]string{Money(k.cost(t.Spend)), Compact(k.tokens(t))})
	}
	for _, v := range models {
		if money {
			r.Models = append(r.Models, Money(v))
		} else {
			r.Models = append(r.Models, Compact(int64(math.Round(v))))
		}
	}
	return r
}

// readouts is the JSON the timeline swaps in on hover: the range first,
// then every bucket.
func readouts(rep *Report) template.JS {
	money := rep.Overall.Cost > 0
	totals := make([]float64, len(rep.TopModels))
	layout := "Mon Jan 2"
	if rep.Bucket == "hour" {
		layout = "Mon Jan 2, 15:04"
	}
	out := make([]Readout, 0, len(rep.Series)+1)
	out = append(out, Readout{})
	for i := range rep.Series {
		b := &rep.Series[i]
		for j, v := range b.Models {
			totals[j] += v
		}
		t := Totals{Input: b.Input, Output: b.Output, CacheRead: b.CacheRead, CacheWrite: b.CacheWrite, Spend: b.Spend}
		out = append(out, readout(time.UnixMilli(b.Start).Format(layout), b.Requests, b.Errors, &t, b.Models, money))
	}
	out[0] = readout("", rep.Overall.Requests, rep.Overall.Failed, &rep.Overall, totals, money)
	b, _ := json.Marshal(out)
	return template.JS(b)
}

// layer is one stacked part of a timeline column.
type layer struct {
	Class string
	val   func(*Bucket) float64
}

// chart draws the series as a timeline of thin stacked columns. Columns
// carry data-i (the bucket's index) for the page's readouts.
func chart(rep *Report, which string) template.HTML {
	s := rep.Series
	if len(s) == 0 {
		return ""
	}
	var layers []layer
	switch which {
	case "requests":
		layers = []layer{
			{"s-ok", func(b *Bucket) float64 { return float64(b.Requests - b.Errors) }},
			{"s-bad", func(b *Bucket) float64 { return float64(b.Errors) }},
		}
	case "tokens":
		for _, k := range tokenKinds {
			layers = append(layers, layer{k.Class, func(b *Bucket) float64 { return float64(k.bucket(b)) }})
		}
	case "cost":
		for _, k := range tokenKinds {
			layers = append(layers, layer{k.Class, func(b *Bucket) float64 { return k.cost(b.Spend) }})
		}
	case "models":
		for i := range rep.TopModels {
			layers = append(layers, layer{fmt.Sprintf("m%d", min(i, 5)+1), func(b *Bucket) float64 { return b.Models[i] }})
		}
	}
	money := which == "cost" || (which == "models" && rep.Overall.Cost > 0)
	format := func(v float64) string { return Compact(int64(math.Round(v))) }
	if money {
		format = Money
	}
	const w, h, top, bottom, right, gap = 780.0, 320.0, 14.0, 26.0, 58.0, 1.5
	plotW, plotH := w-right, h-top-bottom
	peak := 0.0
	for i := range s {
		t := 0.0
		for _, l := range layers {
			t += l.val(&s[i])
		}
		peak = math.Max(peak, t)
	}
	if peak == 0 {
		peak = 1
	}
	step := plotW / float64(len(s))
	bw := math.Max(math.Min(step*0.5, 16), 2)
	layout := "Jan 2"
	if rep.Bucket == "hour" {
		layout = "15:04"
	}
	base := top + plotH
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="timeline" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s per %s">`, w, h, which, rep.Bucket)
	for i := range 4 { // value lines, labelled on the right
		y := top + plotH*float64(i)/4
		fmt.Fprintf(&b, `<line class="grid" x1="0" x2="%.0f" y1="%.1f" y2="%.1f"/><text class="tick" x="%.0f" y="%.1f">%s</text>`,
			plotW, y+.5, y+.5, plotW+10, y+4, template.HTMLEscapeString(format(peak*float64(4-i)/4)))
	}
	fmt.Fprintf(&b, `<line class="base" x1="0" x2="%.0f" y1="%.1f" y2="%.1f"/>`, plotW, base+.5, base+.5)
	every := int(math.Ceil(float64(len(s)) / 7))
	cum, total := 0.0, 0.0
	for i := range s {
		for _, l := range layers {
			total += l.val(&s[i])
		}
	}
	var line []string
	var lastX, lastY float64
	for i := range s {
		bk := &s[i]
		x := step*float64(i) + (step-bw)/2
		fmt.Fprintf(&b, `<g class="col" data-i="%d" style="--i:%d"><rect class="hit" x="%.1f" y="0" width="%.1f" height="%.0f"/>`, i+1, i, step*float64(i), step, h)
		y := base
		for _, l := range layers {
			v := l.val(bk)
			if v <= 0 {
				continue
			}
			cum += v
			hh := math.Max(plotH*v/peak, 2)
			y -= hh
			// A hairline of paper between parts keeps a stack readable.
			fmt.Fprintf(&b, `<rect class="seg %s" x="%.1f" y="%.2f" width="%.1f" height="%.2f" rx="%.1f"/>`, l.Class, x, y, bw, math.Max(hh-gap, 1), math.Min(bw/2, 2.5))
		}
		b.WriteString(`</g>`)
		if total > 0 {
			lastX, lastY = x+bw/2, base-plotH*cum/total
			line = append(line, fmt.Sprintf("%.1f,%.1f", lastX, lastY))
		}
		if i%every == 0 {
			fmt.Fprintf(&b, `<text class="tick" x="%.1f" y="%.0f" text-anchor="middle">%s</text>`, x+bw/2, h-6, time.UnixMilli(bk.Start).Format(layout))
		}
	}
	// The running total, scaled to the whole range, shows how the spend
	// built up; only for cost, where a running total means something.
	if which == "cost" && len(line) > 1 {
		fmt.Fprintf(&b, `<polyline class="cum" points="%s"/>`, strings.Join(line, " "))
		fmt.Fprintf(&b, `<circle class="cum-dot" cx="%.1f" cy="%.1f" r="3.5"/>`, lastX, lastY)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// trend is a small line of one metric over the range, for the figures at
// the top; rates leave gaps where there were no requests.
func trend(rep *Report, metric string) template.HTML {
	v := make([]float64, len(rep.Series))
	for i := range rep.Series {
		b := &rep.Series[i]
		switch metric {
		case "cost":
			v[i] = b.Cost
		case "requests":
			v[i] = float64(b.Requests)
		case "tokens":
			v[i] = float64(b.Tokens())
		case "cache":
			v[i] = math.NaN()
			if in := b.Input + b.CacheRead; in > 0 {
				v[i] = float64(b.CacheRead) / float64(in)
			}
		case "failures":
			v[i] = math.NaN()
			if b.Requests > 0 {
				v[i] = float64(b.Errors) / float64(b.Requests)
			}
		}
	}
	return trendLine(v, metric != "cache" && metric != "failures")
}

// modelTrend is the cost (or requests) of one of Report.TopModels over the range.
func modelTrend(rep *Report, i int) template.HTML {
	if i >= len(rep.TopModels) {
		return ""
	}
	v := make([]float64, len(rep.Series))
	for j := range rep.Series {
		v[j] = rep.Series[j].Models[i]
	}
	return trendLine(v, true)
}

func trendLine(v []float64, fill bool) template.HTML {
	if len(v) < 2 {
		return ""
	}
	const w, h = 240.0, 44.0
	peak := 0.0
	for _, x := range v {
		if !math.IsNaN(x) {
			peak = math.Max(peak, x)
		}
	}
	if peak == 0 {
		peak = 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="trend" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" aria-hidden="true">`, w, h)
	var run []string
	var x0, x1 float64
	flush := func() {
		if len(run) > 1 {
			pts := strings.Join(run, " ")
			if fill {
				fmt.Fprintf(&b, `<polygon class="area" points="%.1f,%.0f %s %.1f,%.0f"/>`, x0, h, pts, x1, h)
			}
			fmt.Fprintf(&b, `<polyline class="line" points="%s"/>`, pts)
		}
		run = run[:0]
	}
	for i, x := range v {
		px := w * float64(i) / float64(len(v)-1)
		if math.IsNaN(x) {
			flush()
			continue
		}
		if len(run) == 0 {
			x0 = px
		}
		x1 = px
		run = append(run, fmt.Sprintf("%.1f,%.1f", px, h-3-(h-6)*x/peak))
	}
	flush()
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// LatencyRow is one model's average response time, split at the first token.
type LatencyRow struct {
	Model               string
	Duration, TTFT, TPS float64 // ms, ms, tokens/s
	Share, WaitShare    float64 // of the slowest model; first-token part of this one
}

func latencyRows(rep *Report) []LatencyRow {
	var rows []LatencyRow
	slowest := 0.0
	for _, g := range rep.ByModel {
		if g.AvgDuration > 0 {
			rows = append(rows, LatencyRow{Model: g.Key, Duration: g.AvgDuration, TTFT: g.AvgTTFT, TPS: g.AvgTPS})
			slowest = math.Max(slowest, g.AvgDuration)
		}
	}
	for i := range rows {
		rows[i].Share = rows[i].Duration / slowest
		rows[i].WaitShare = math.Min(rows[i].TTFT/rows[i].Duration, 1)
	}
	return rows
}

var weekdays = [7]string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// heatLevel buckets n into GitHub's five shades: 0 for no requests, then
// 1-4 on a square-root scale of the busiest hour, so a few quiet hours
// still show next to a very busy one.
func heatLevel(n, peak int) int {
	if n <= 0 || peak <= 0 {
		return 0
	}
	return 1 + min(3, int(4*math.Sqrt(float64(n)/float64(peak))))
}

// dots draws requests by weekday and hour as a grid of squares shaded by
// how busy each hour was, like GitHub's contribution graph.
func dots(rep *Report) template.HTML {
	peak := 0
	for _, row := range rep.Heat {
		for _, n := range row {
			peak = max(peak, n)
		}
	}
	const cell, gap, left, top = 22.0, 4.0, 40.0, 4.0
	size := cell - gap
	w, h := left+24*cell, top+7*cell+44
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="dots" viewBox="0 0 %.0f %.0f" role="img" aria-label="Requests by weekday and hour">`, w, h)
	for d, row := range rep.Heat {
		y := top + cell*float64(d)
		fmt.Fprintf(&b, `<text class="tick" x="0" y="%.1f">%s</text>`, y+size/2+4, weekdays[d])
		for hr, n := range row {
			what := "no requests"
			if n > 0 {
				what = plural(n, "request")
			}
			fmt.Fprintf(&b, `<rect class="l%d" x="%.1f" y="%.1f" width="%.0f" height="%.0f" rx="3"><title>%s %02d:00 · %s</title></rect>`,
				heatLevel(n, peak), left+cell*float64(hr), y, size, size, time.Weekday((d+1)%7), hr, what)
		}
	}
	base := top + 7*cell + 14
	for hr := 0; hr < 24; hr += 6 {
		fmt.Fprintf(&b, `<text class="tick" x="%.1f" y="%.0f" text-anchor="middle">%02d:00</text>`, left+cell*float64(hr)+size/2, base, hr)
	}
	// Legend on its own line, right-aligned with the grid: Less ■■■■■ More.
	const sw = 11.0
	ly := base + 20
	lx := w - gap - 5*(sw+3) - 32
	fmt.Fprintf(&b, `<text class="tick" x="%.1f" y="%.0f" text-anchor="end">Less</text>`, lx-6, ly)
	for l := 0; l < 5; l++ {
		fmt.Fprintf(&b, `<rect class="l%d" x="%.1f" y="%.1f" width="%.0f" height="%.0f" rx="2"/>`, l, lx+float64(l)*(sw+3), ly-sw+1, sw, sw)
	}
	fmt.Fprintf(&b, `<text class="tick" x="%.1f" y="%.0f">More</text>`, lx+5*(sw+3)+3, ly)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
