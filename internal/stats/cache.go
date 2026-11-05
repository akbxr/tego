package stats

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/mattn/go-runewidth"
)

// The cache report (tego stats cache) finds prompt-cache writes that could
// have been reads.
//
// Within one provider session every request repeats the previous one's
// prompt and adds a tail, so it should read that whole prompt (the
// previous context: input + cache read + cache write) from the cache and
// write only the tail. What it wrote of the part it should have read,
// min(cache_write, previous context − cache_read), is a rewrite. The tail
// (last reply, new tool results) lies beyond the previous context and is
// never counted. The first request of a session has nothing to read.
//
// The API's cache_miss alone can't tell: it often names the newest part
// of a prompt whose read was fine, so the token arithmetic decides whether
// a request missed and the reason only explains a miss.
//
// Each rewrite gets the first cause that fits:
//
//	keepalive    a keep-alive wrote: its only job is to read, so any
//	             rewrite is a bug, even after the TTL (it should not
//	             have been sent then)
//	expired      the previous request began a TTL or more before (5 min,
//	             1 h with long_cache): the cache lapsed while idle, as
//	             designed; expected, counted apart
//	shrunk       the context shrank by a fifth or more: compaction,
//	             elision, a rewind, a fork; a new history, expected
//	switch       the previous request used another provider or model
//	<reason>     the API's cache_miss (messages_changed, …)
//	unknown      none of these
//
// A rewrite's cost is its tokens at the cache-write price less the
// cache-read price: what reading them instead would have saved.

// Causes of a rewrite besides the API's own reasons.
const (
	CauseKeepAlive = "keepalive"
	CauseExpired   = "expired"
	CauseShrunk    = "shrunk"
	CauseSwitch    = "switch"
	CauseUnknown   = "unknown"
)

// minRewrite is the smallest rewrite counted: below it lie moved cache
// breakpoints and tail edits, and the provider's minimum cacheable prompt.
const minRewrite = 2048

// expectedCause reports whether cause is how the cache is meant to work.
func expectedCause(cause string) bool { return cause == CauseExpired || cause == CauseShrunk }

// CacheAmount counts requests and their tokens and cost.
type CacheAmount struct {
	Requests int     `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
}

func (a *CacheAmount) add(tokens int, cost float64) {
	a.Requests++
	a.Tokens += int64(tokens)
	a.Cost += cost
}

// CacheGroup is the rewrites of one cause or agent kind.
type CacheGroup struct {
	Key      string `json:"key"`
	Expected bool   `json:"expected,omitempty"` // expired or shrunk: not waste
	CacheAmount
}

// CacheMiss is one rewrite.
type CacheMiss struct {
	TS       int64  `json:"ts"`
	Folder   string `json:"folder,omitempty"`
	Session  string `json:"session,omitempty"`
	Class    string `json:"class"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Cause    string `json:"cause"`
	Reason   string `json:"reason,omitempty"` // the API's cache_miss
	// PrevContext is the previous request's prompt, all of which this one
	// could have read; 0 without one.
	PrevContext int   `json:"prev_context"`
	CacheRead   int   `json:"cache_read"`
	CacheWrite  int   `json:"cache_write"`
	Rewritten   int   `json:"rewritten"` // of CacheWrite, what could have been read
	GapMs       int64 `json:"gap_ms"`    // since the previous request began; -1 without one
	// Cost is Rewritten at the write price over the read price.
	Cost float64 `json:"cost"`
}

// CacheReport is what tego stats cache shows for one range.
type CacheReport struct {
	Range    string `json:"range"`
	From     int64  `json:"from"` // 0 for "all"
	To       int64  `json:"to"`
	First    int64  `json:"first_ts,omitempty"`
	Last     int64  `json:"last_ts,omitempty"`
	Requests int    `json:"requests"`
	Unpriced int    `json:"unpriced"` // requests with cache tokens whose model has no price
	// Read and Written are every cache read and write, at their prices.
	Read    CacheAmount `json:"read"`
	Written CacheAmount `json:"written"`
	// Wasted is the rewrites that are not expected; Expired and Shrunk the
	// ones that are. Their Cost is over the read price.
	Wasted     CacheAmount  `json:"wasted"`
	Expired    CacheAmount  `json:"expired"`
	Shrunk     CacheAmount  `json:"shrunk"`
	KeepAlives int          `json:"keepalives"` // keep-alives sent
	ByCause    []CacheGroup `json:"by_cause"`   // waste first, then expected
	ByClass    []CacheGroup `json:"by_agent"`   // waste only
	Top        []CacheMiss  `json:"top"`        // the costliest waste
}

// cacheTop is how many rewrites CacheReport.Top lists.
const cacheTop = 10

// promptTokens is everything r sent: the context a following request can read.
func promptTokens(r *Record) int { return r.Input + r.CacheRead + r.CacheWrite }

// cacheTTL is how long the cache prev wrote or read lives.
func cacheTTL(prev *Record) int64 {
	if prev.LongCache {
		return time.Hour.Milliseconds()
	}
	return (5 * time.Minute).Milliseconds()
}

// rewrite classifies r against prev, the previous request with usage in
// its session (nil: none): cause "" when r is not a rewrite.
func rewrite(prev, r *Record) (cause string, tokens int, gap int64) {
	if prev == nil {
		// Nothing to compare with, but a keep-alive that writes a lot
		// (a resumed session, a lost record) still missed.
		if r.Class == llm.ClassKeepAlive && r.CacheWrite >= minRewrite {
			return CauseKeepAlive, r.CacheWrite, -1
		}
		return "", 0, -1
	}
	gap = r.TS - prev.TS
	tokens = min(r.CacheWrite, promptTokens(prev)-r.CacheRead)
	if tokens < minRewrite {
		return "", 0, gap
	}
	switch {
	case r.Class == llm.ClassKeepAlive:
		cause = CauseKeepAlive
	case gap >= cacheTTL(prev):
		cause = CauseExpired
	case promptTokens(r)*5 < promptTokens(prev)*4:
		cause = CauseShrunk
	case r.Provider != prev.Provider || r.Model != prev.Model:
		cause = CauseSwitch
	case r.CacheMiss != "":
		cause = r.CacheMiss
	default:
		cause = CauseUnknown
	}
	return cause, tokens, gap
}

// BuildCache finds the rewrites within rng in recs (sorted by time). The
// sessions are followed from the first record, so a range's first request
// is compared with the one before the range.
func BuildCache(recs []Record, rng string, now time.Time, prices Prices) (*CacheReport, error) {
	from, err := ParseRange(rng, now)
	if err != nil {
		return nil, err
	}
	if rng == "" {
		rng = "all"
	}
	rep := &CacheReport{Range: rng, To: now.UnixMilli()}
	if !from.IsZero() {
		rep.From = from.UnixMilli()
	}
	type session struct {
		prev   *Record
		folder string // keep-alives log none
	}
	sessions := map[string]*session{}
	causes, classes := map[string]*CacheGroup{}, map[string]*CacheGroup{}
	group := func(m map[string]*CacheGroup, key string, expected bool) *CacheGroup {
		g := m[key]
		if g == nil {
			g = &CacheGroup{Key: key, Expected: expected}
			m[key] = g
		}
		return g
	}
	var misses []CacheMiss
	for i := range recs {
		r := &recs[i]
		if r.Kind != "request" {
			continue
		}
		s := &session{}
		if r.Session != "" {
			if sessions[r.Session] == nil {
				sessions[r.Session] = s
			}
			s = sessions[r.Session]
		}
		if r.Folder != "" {
			s.folder = r.Folder
		}
		prev := s.prev
		// A failed request with no usage says nothing about the cache.
		if r.Session != "" && promptTokens(r) > 0 {
			s.prev = r
		}
		if r.TS < rep.From {
			continue
		}
		rep.Requests++
		if rep.First == 0 {
			rep.First = r.TS
		}
		rep.Last = r.TS
		if r.Class == llm.ClassKeepAlive {
			rep.KeepAlives++
		}
		p, priced := prices.lookup(r.Provider, r.Model)
		p = billed(p, r)
		if !priced && r.CacheRead+r.CacheWrite > 0 {
			rep.Unpriced++
		}
		if r.CacheRead > 0 {
			rep.Read.add(r.CacheRead, float64(r.CacheRead)*p.CacheRead/1e6)
		}
		if r.CacheWrite > 0 {
			rep.Written.add(r.CacheWrite, float64(r.CacheWrite)*p.CacheWrite/1e6)
		}
		if r.Session == "" && r.Class != llm.ClassKeepAlive {
			continue // no session to compare with (auxiliary requests)
		}
		cause, tokens, gap := rewrite(prev, r)
		if cause == "" {
			continue
		}
		cost := float64(tokens) * max(p.CacheWrite-p.CacheRead, 0) / 1e6
		expected := expectedCause(cause)
		group(causes, cause, expected).add(tokens, cost)
		switch cause {
		case CauseExpired:
			rep.Expired.add(tokens, cost)
			continue
		case CauseShrunk:
			rep.Shrunk.add(tokens, cost)
			continue
		}
		rep.Wasted.add(tokens, cost)
		group(classes, r.Class, false).add(tokens, cost)
		m := CacheMiss{TS: r.TS, Folder: s.folder, Session: r.Session, Class: r.Class, Provider: r.Provider, Model: r.Model,
			Cause: cause, Reason: r.CacheMiss, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite, Rewritten: tokens, GapMs: gap, Cost: cost}
		if prev != nil {
			m.PrevContext = promptTokens(prev)
		}
		misses = append(misses, m)
	}
	for _, g := range causes {
		rep.ByCause = append(rep.ByCause, *g)
	}
	for _, g := range classes {
		rep.ByClass = append(rep.ByClass, *g)
	}
	sortCacheGroups(rep.ByCause)
	sortCacheGroups(rep.ByClass)
	sort.SliceStable(misses, func(a, b int) bool {
		x, y := misses[a], misses[b]
		if x.Cost != y.Cost {
			return x.Cost > y.Cost
		}
		return x.Rewritten > y.Rewritten
	})
	rep.Top = misses[:min(len(misses), cacheTop)]
	return rep, nil
}

// sortCacheGroups puts waste before expected rewrites, costliest first.
func sortCacheGroups(gs []CacheGroup) {
	sort.Slice(gs, func(a, b int) bool {
		x, y := gs[a], gs[b]
		if x.Expected != y.Expected {
			return !x.Expected
		}
		if x.Cost != y.Cost {
			return x.Cost > y.Cost
		}
		if x.Tokens != y.Tokens {
			return x.Tokens > y.Tokens
		}
		return x.Key < y.Key
	})
}

// causeLabel names a cause for the terminal; API reasons stay as sent.
func causeLabel(cause string) string {
	switch cause {
	case CauseKeepAlive:
		return "keep-alive miss"
	case CauseExpired:
		return "expired after idle"
	case CauseShrunk:
		return "history shrank"
	case CauseSwitch:
		return "model switch"
	case CauseUnknown:
		return "unknown"
	}
	return "api: " + cause
}

// Gap formats a span in milliseconds as 45s, 12m, 3h05m or 2d04h; "–" if negative.
func Gap(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case ms < 0:
		return "–"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
}

// tail keeps the last path elements of dir that fit in w cells:
// …/projects/tego. A last element too long on its own loses its start.
func tail(dir string, w int) string {
	if runewidth.StringWidth(dir) <= w {
		return dir
	}
	sep := string(filepath.Separator)
	parts := strings.Split(dir, sep)
	out := parts[len(parts)-1]
	for i := len(parts) - 2; i >= 0; i-- {
		if runewidth.StringWidth("…"+sep+parts[i]+sep+out) > w {
			break
		}
		out = parts[i] + sep + out
	}
	if sw := runewidth.StringWidth(out); sw+2 > w {
		return runewidth.TruncateLeft(out, sw-w+1, "…")
	}
	return "…" + sep + out
}

// pad fills s with spaces to w cells, truncating it if wider.
func pad(s string, w int) string {
	s = runewidth.Truncate(s, w, "…")
	return s + strings.Repeat(" ", w-runewidth.StringWidth(s))
}

// CacheSummary writes the cache report for a terminal.
func CacheSummary(w io.Writer, rep *CacheReport, color bool) {
	c := sgr{}
	if color {
		c = sgr{"\x1b[0m", "\x1b[2m", "\x1b[1m", "\x1b[32m", "\x1b[33m", "\x1b[31m", "\x1b[36m", "\x1b[35m"}
	}
	head := func(s string) { fmt.Fprintf(w, "\n%s%s%s\n", c.bold+c.cyan, s, c.reset) }
	row := func(label, value string) { fmt.Fprintf(w, "  %s%-12s%s %s\n", c.dim, label, c.reset, value) }

	fmt.Fprintf(w, "%stego stats cache%s %s· %s%s\n", c.bold, c.reset, c.dim, RangeLabel(rep.Range), c.reset)
	if rep.Read.Tokens+rep.Written.Tokens == 0 {
		fmt.Fprintf(w, "\nNo prompt-cache use recorded in this range.\n")
		return
	}
	fmt.Fprintf(w, "%s%s – %s%s\n", c.dim, time.UnixMilli(rep.First).Format("Jan 02 2006 15:04"), time.UnixMilli(rep.Last).Format("Jan 02 2006 15:04"), c.reset)

	head("Prompt cache")
	row("Read", fmt.Sprintf("%7s tokens  %s", Compact(rep.Read.Tokens), Money(rep.Read.Cost)))
	row("Written", fmt.Sprintf("%7s tokens  %s", Compact(rep.Written.Tokens), Money(rep.Written.Cost)))
	waste := fmt.Sprintf("%7s tokens  %s", Compact(rep.Wasted.Tokens), Money(rep.Wasted.Cost))
	if rep.Wasted.Requests > 0 {
		waste = c.red + waste + c.reset
	}
	row("Wasted", waste+fmt.Sprintf("  %sin %s, over the read price%s", c.dim, plural(rep.Wasted.Requests, "rewrite"), c.reset))
	if share := float64(rep.Wasted.Tokens) / float64(max(rep.Written.Tokens, 1)); rep.Wasted.Tokens > 0 {
		row("", fmt.Sprintf("%s%.1f%% of everything written%s", c.dim, share*100, c.reset))
	}
	if e := rep.Expired; e.Requests > 0 {
		row("Expired", fmt.Sprintf("%7s tokens  %s  %safter %s, expected%s", Compact(e.Tokens), Money(e.Cost), c.dim, plural(e.Requests, "idle gap"), c.reset))
	}
	if s := rep.Shrunk; s.Requests > 0 {
		row("Shrank", fmt.Sprintf("%7s tokens  %s  %scompacted, elided or rewound %s, expected%s", Compact(s.Tokens), Money(s.Cost), c.dim, plural(s.Requests, "time"), c.reset))
	}
	if rep.KeepAlives > 0 {
		missed := 0
		for _, g := range rep.ByCause {
			if g.Key == CauseKeepAlive {
				missed = g.Requests
			}
		}
		line := fmt.Sprintf("%d sent, %d missed", rep.KeepAlives, missed)
		if missed > 0 {
			line = fmt.Sprintf("%d sent, %s%d missed%s", rep.KeepAlives, c.red, missed, c.reset)
		}
		row("Keep-alives", line)
	}
	if rep.Unpriced > 0 {
		row("", fmt.Sprintf("%s%s without a price count as $0%s", c.dim, plural(rep.Unpriced, "request"), c.reset))
	}

	groups := func(title string, gs []CacheGroup, label func(string) string) {
		if len(gs) == 0 {
			return
		}
		head(title)
		wide := 0
		for _, g := range gs {
			wide = max(wide, runewidth.StringWidth(label(g.Key)))
		}
		wide = min(wide, 32)
		for _, g := range gs {
			line := fmt.Sprintf("  %s  %5d  %7s  %8s", pad(label(g.Key), wide), g.Requests, Compact(g.Tokens), Money(g.Cost))
			if g.Expected {
				line = c.dim + line + "  expected" + c.reset
			}
			fmt.Fprintln(w, line)
		}
	}
	groups("By cause", rep.ByCause, causeLabel)
	groups("By agent", rep.ByClass, func(s string) string { return s })

	if len(rep.Top) > 0 {
		head("Costliest rewrites")
		fmt.Fprintf(w, "  %s%-12s  %-22s  %-10s  %-16s  %7s  %6s  %7s  %s%s\n", c.dim,
			"when", "folder", "agent", "model", "written", "gap", "cost", "cause", c.reset)
		for _, m := range rep.Top {
			cause := causeLabel(m.Cause)
			if m.Reason != "" && m.Reason != m.Cause {
				cause += " " + c.dim + "(" + m.Reason + ")" + c.reset
			}
			fmt.Fprintf(w, "  %s%s%s  %s  %s  %s  %7s  %6s  %7s  %s\n", c.dim, time.UnixMilli(m.TS).Format("Jan 02 15:04"), c.reset,
				pad(tail(FolderLabel(m.Folder), 22), 22), pad(m.Class, 10), pad(m.Model, 16),
				Compact(int64(m.CacheWrite)), Gap(m.GapMs), Money(m.Cost), cause)
		}
	}
}
