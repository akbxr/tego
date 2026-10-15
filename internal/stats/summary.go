package stats

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
)

type sgr struct{ reset, dim, bold, green, yellow, red, cyan, magenta string }

// Compact formats counts like 1.5K, 642M.
func Compact(n int64) string {
	f := float64(n)
	switch {
	case n >= 1e9:
		return trimZero(fmt.Sprintf("%.1f", f/1e9)) + "B"
	case n >= 1e6:
		return trimZero(fmt.Sprintf("%.1f", f/1e6)) + "M"
	case n >= 1e3:
		return trimZero(fmt.Sprintf("%.1f", f/1e3)) + "K"
	}
	return fmt.Sprint(n)
}

func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// Money formats a cost estimate.
func Money(v float64) string {
	switch {
	case v == 0:
		return "$0"
	case v < 0.01:
		return "<$0.01"
	case v < 100:
		return fmt.Sprintf("$%.2f", v)
	}
	return fmt.Sprintf("$%.0f", v)
}

// Seconds formats milliseconds as 8.9s or 450ms.
func Seconds(ms float64) string {
	if ms < 1000 {
		return fmt.Sprintf("%.0fms", ms)
	}
	return fmt.Sprintf("%.1fs", ms/1000)
}

// RangeLabel describes a report's range for headings.
func RangeLabel(rng string) string {
	switch rng {
	case "24h":
		return "last 24 hours"
	case "7d":
		return "last 7 days"
	case "30d":
		return "last 30 days"
	case "90d":
		return "last 90 days"
	}
	return "all time"
}

// FolderLabel shortens a working directory for display.
func FolderLabel(dir string) string {
	if dir == "" {
		return "(unknown)"
	}
	if home, err := os.UserHomeDir(); err == nil && (dir == home || strings.HasPrefix(dir, home+string(filepath.Separator))) {
		return "~" + dir[len(home):]
	}
	return dir
}

// Summary writes the report for a terminal.
func Summary(w io.Writer, rep *Report, color bool) {
	c := sgr{}
	if color {
		c = sgr{"\x1b[0m", "\x1b[2m", "\x1b[1m", "\x1b[32m", "\x1b[33m", "\x1b[31m", "\x1b[36m", "\x1b[35m"}
	}
	o := &rep.Overall
	head := func(s string) { fmt.Fprintf(w, "\n%s%s%s\n", c.bold+c.cyan, s, c.reset) }
	row := func(label, value string) { fmt.Fprintf(w, "  %s%-12s%s %s\n", c.dim, label, c.reset, value) }

	fmt.Fprintf(w, "%stego stats%s %s· %s%s\n", c.bold, c.reset, c.dim, RangeLabel(rep.Range), c.reset)
	if o.Requests == 0 {
		fmt.Fprintf(w, "\nNo requests recorded in this range.\n")
		return
	}
	if o.First > 0 {
		fmt.Fprintf(w, "%s%s – %s%s\n", c.dim, time.UnixMilli(o.First).Format("Jan 02 2006 15:04"), time.UnixMilli(o.Last).Format("Jan 02 2006 15:04"), c.reset)
	}

	head("Overall")
	errs := plural(o.Failed, "error")
	if o.Failed > 0 {
		errs = c.red + errs + c.reset + fmt.Sprintf(", %.1f%%", o.ErrorRate*100)
	}
	row("Requests", fmt.Sprintf("%s (%s)", Compact(int64(o.Requests)), errs))
	row("Tokens", fmt.Sprintf("%s  %sinput %s · output %s · cache read %s · cache write %s%s", Compact(o.Tokens()), c.dim,
		Compact(o.Input), Compact(o.Output), Compact(o.CacheRead), Compact(o.CacheWrite), c.reset))
	row("Cache", fmt.Sprintf("%.1f%% of input from cache · saves %.1f%% of input cost", o.CacheRate*100, o.CacheSavings*100))
	cost := c.green + Money(o.Cost) + c.reset + " API-equivalent estimate"
	if o.Unpriced > 0 {
		cost += fmt.Sprintf(" %s(%d unpriced requests)%s", c.dim, o.Unpriced, c.reset)
	}
	row("Cost", cost)
	if s := o.Spend; o.Cost > 0 {
		row("", fmt.Sprintf("%scache reads %s · cache writes %s · fresh input %s · output %s%s", c.dim,
			Money(s.CacheRead), Money(s.CacheWrite), Money(s.Input), Money(s.Output), c.reset))
	}
	if o.AvgDuration > 0 {
		lat := "avg " + Seconds(o.AvgDuration)
		if o.AvgTTFT > 0 {
			lat += " · first token " + Seconds(o.AvgTTFT)
		}
		if o.AvgTPS > 0 {
			lat += fmt.Sprintf(" · %.0f tok/s", o.AvgTPS)
		}
		row("Latency", lat)
	}

	if len(rep.Series) > 1 {
		unit := "Daily"
		layout := "Jan 02"
		if rep.Bucket == "hour" {
			unit, layout = "Hourly", "15:04"
		}
		head(unit)
		series := rep.Series
		for len(series) > 1 && series[0].Requests == 0 { // the dashboard shows the whole range
			series = series[1:]
		}
		if len(series) > 30 {
			series = series[len(series)-30:]
		}
		peak := 0
		for _, b := range series {
			peak = max(peak, b.Requests)
		}
		const width = 28
		for _, b := range series {
			n := 0
			if peak > 0 {
				n = (b.Requests*width + peak - 1) / peak
			}
			bar := c.magenta + strings.Repeat("█", n) + c.reset + c.dim + strings.Repeat("░", width-n) + c.reset
			line := fmt.Sprintf("  %s%s%s %s %5s req", c.dim, time.UnixMilli(b.Start).Format(layout), c.reset, bar, Compact(int64(b.Requests)))
			if b.Cost > 0 {
				line += "  " + Money(b.Cost)
			}
			if b.Errors > 0 {
				line += "  " + c.red + plural(b.Errors, "error") + c.reset
			}
			fmt.Fprintln(w, line)
		}
	}

	groups := func(title string, gs []Group, label func(string) string) {
		if len(gs) == 0 {
			return
		}
		head(title)
		const top = 10
		var rest Group
		if len(gs) > top {
			for _, g := range gs[top:] {
				rest.Requests += g.Requests
				rest.Cost += g.Cost
			}
			rest.Key = fmt.Sprintf("… %d more", len(gs)-top)
			gs = gs[:top]
		}
		wide := 0
		for _, g := range gs {
			wide = max(wide, runewidth.StringWidth(label(g.Key)))
		}
		wide = min(wide, 48)
		for _, g := range gs {
			name := runewidth.Truncate(label(g.Key), wide, "…")
			line := fmt.Sprintf("  %s%s  %6s req  %7s  %s%s tokens%s", name, strings.Repeat(" ", wide-runewidth.StringWidth(name)),
				Compact(int64(g.Requests)), Money(g.Cost), c.dim, Compact(g.Tokens()), c.reset)
			if g.Input+g.CacheRead > 0 {
				line += fmt.Sprintf(" %s· cache %.0f%%%s", c.dim, g.CacheRate*100, c.reset)
			}
			if g.Failed > 0 {
				line += "  " + c.red + plural(g.Failed, "error") + c.reset
			}
			fmt.Fprintln(w, line)
		}
		if rest.Key != "" {
			fmt.Fprintf(w, "  %s%s%s  %6s req  %7s%s\n", c.dim, rest.Key, strings.Repeat(" ", max(wide-runewidth.StringWidth(rest.Key), 0)),
				Compact(int64(rest.Requests)), Money(rest.Cost), c.reset)
		}
	}
	same := func(s string) string { return s }
	groups("By model", rep.ByModel, same)
	groups("By agent", rep.ByClass, same)
	groups("By folder", rep.ByFolder, FolderLabel)

	if len(rep.ByTool) > 0 {
		head("Tools")
		for i, t := range rep.ByTool {
			if i == 15 {
				fmt.Fprintf(w, "  %s… %d more%s\n", c.dim, len(rep.ByTool)-15, c.reset)
				break
			}
			word := "calls"
			if t.Calls == 1 {
				word = "call"
			}
			line := fmt.Sprintf("  %-24s %6s %s", runewidth.Truncate(t.Tool, 24, "…"), Compact(int64(t.Calls)), word)
			if t.Errors > 0 {
				line += fmt.Sprintf("  %s%s (%.0f%%)%s", c.red, plural(t.Errors, "error"), float64(t.Errors)*100/float64(t.Calls), c.reset)
			}
			fmt.Fprintln(w, line)
		}
	}

	if len(rep.Errors) > 0 {
		head("Recent errors")
		for i, r := range rep.Errors {
			if i == 5 {
				break
			}
			msg := strings.Join(strings.Fields(r.Error), " ")
			msg = runewidth.Truncate(msg, 90, "…")
			fmt.Fprintf(w, "  %s%s%s  %s/%s  %s%s%s\n", c.dim, time.UnixMilli(r.TS).Format("Jan 02 15:04"), c.reset, r.Provider, r.Model, c.red, msg, c.reset)
		}
	}
}
