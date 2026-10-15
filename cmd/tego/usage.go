package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/akbxr/tego/internal/auth"
	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/llm"
)

// accountUsage is the usage report (or error) of one stored account.
type accountUsage struct {
	label  string
	report *llm.UsageReport
	err    error
}

type providerUsage struct {
	name     string // provider id
	title    string // display name, e.g. "Anthropic"
	accounts []accountUsage
}

var usageKinds = map[string]string{
	"anthropic":          "Anthropic",
	"openai-codex":       "OpenAI Codex",
	"google-antigravity": "Google Antigravity",
}

// fetchUsage queries every subscription account in the store concurrently.
func fetchUsage(ctx context.Context, cfg *config.Config, store *auth.Store) []providerUsage {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out []providerUsage
	var wg sync.WaitGroup
	for _, provider := range store.Providers() {
		kind := cfg.Kind(provider)
		if _, ok := usageKinds[kind]; !ok {
			continue
		}
		var accts []auth.Credential
		for _, c := range store.Accounts(provider) {
			if c.Type == "oauth" {
				accts = append(accts, c)
			}
		}
		if len(accts) == 0 {
			continue
		}
		title := usageKinds[kind]
		if provider != kind {
			title = provider // a user-defined provider of a subscription kind
		}
		pu := providerUsage{name: provider, title: title, accounts: make([]accountUsage, len(accts))}
		for i, c := range accts {
			wg.Add(1)
			go func(slot *accountUsage, c auth.Credential) {
				defer wg.Done()
				slot.label = c.Label()
				token, err := store.TokenSource(provider, c.ID).Token(ctx)
				if err == nil {
					slot.report, err = llm.FetchUsage(ctx, kind, token, c.ProjectID)
				}
				slot.err = err
				if slot.report != nil && c.Email == "" && slot.report.Email != "" {
					slot.label = slot.report.Email
				}
			}(&pu.accounts[i], c)
		}
		out = append(out, pu)
	}
	wg.Wait()
	return out
}

// renderUsage prints reports in the `usage` layout: one block per
// provider, a bar per limit, and a capacity summary across accounts.
// active maps provider -> label of the account in use (may be nil).
func renderUsage(w io.Writer, c palette, all []providerUsage, took time.Duration, active map[string]string) {
	fmt.Fprintf(w, "%sUsage%s%s · fetched in %s%s\n", c.bold, c.reset, c.dim, took.Round(100*time.Millisecond), c.reset)
	if len(all) == 0 {
		fmt.Fprintf(w, "\n%sno subscription logins (usage is reported for Claude Pro/Max, ChatGPT and Google Antigravity accounts)%s\n", c.dim, c.reset)
		return
	}
	const barWidth = 28
	now := time.Now()
	for _, pu := range all {
		fmt.Fprintf(w, "\n%s%s%s%s — %s%s\n", c.bold, c.cyan, pu.title, c.reset+c.dim, plural(len(pu.accounts), "account"), c.reset)

		// Every label seen in this provider, in first-seen order, so accounts
		// missing a limit show it as "not reported".
		var labels []string
		seen := map[string]bool{}
		labelWidth := 0
		for _, a := range pu.accounts {
			if a.report == nil {
				continue
			}
			for _, l := range a.report.Limits {
				if !seen[l.Label] {
					seen[l.Label] = true
					labels = append(labels, l.Label)
				}
				labelWidth = max(labelWidth, runewidth.StringWidth(l.Label))
			}
		}

		for _, a := range pu.accounts {
			head := c.bold + a.label + c.reset
			if a.report != nil && a.report.Org != "" {
				head += c.dim + " · " + a.report.Org + c.reset
			}
			if active[pu.name] == a.label {
				head += " " + c.green + "◎ in use" + c.reset
			}
			if a.err != nil {
				fmt.Fprintf(w, "  %s●%s %s %s· %s%s\n", c.red, c.reset, head, c.red, firstLine(a.err.Error()), c.reset)
				continue
			}
			worst := 0.0
			for _, l := range a.report.Limits {
				worst = max(worst, l.Used)
			}
			fmt.Fprintf(w, "  %s●%s %s\n", usageColor(c, worst), c.reset, head)
			have := map[string]bool{}
			for _, l := range a.report.Limits {
				have[l.Label] = true
				pad := strings.Repeat(" ", labelWidth-runewidth.StringWidth(l.Label))
				if l.Used < 0 {
					fmt.Fprintf(w, "      %s○ %s%s  %s  not reported%s\n", c.dim, l.Label, pad, strings.Repeat("·", barWidth), c.reset)
					continue
				}
				filled := int(math.Round(l.Used * barWidth))
				col := usageColor(c, l.Used)
				detail := fmt.Sprintf("%.1f%% used", l.Used*100)
				if l.Detail != "" {
					detail = l.Detail + " · " + detail
				}
				if !l.ResetsAt.IsZero() && l.ResetsAt.After(now) {
					detail += " · resets in " + shortDuration(l.ResetsAt.Sub(now))
				}
				fmt.Fprintf(w, "      %s●%s %s%s  %s%s%s%s%s  %s\n", col, c.reset, l.Label, pad,
					col, strings.Repeat("█", filled), c.dim, strings.Repeat("░", barWidth-filled), c.reset, detail)
			}
			for _, label := range labels {
				if !have[label] {
					pad := strings.Repeat(" ", labelWidth-runewidth.StringWidth(label))
					fmt.Fprintf(w, "      %s○ %s%s  %s  not reported%s\n", c.dim, label, pad, strings.Repeat("·", barWidth), c.reset)
				}
			}
		}
		if line := capacityLine(pu); line != "" {
			fmt.Fprintf(w, "  %scapacity: %s%s\n", c.dim, line, c.reset)
		}
	}
}

// capacityLine sums, per window, how much of one account's quota is used
// across the accounts that report it (each account counts its most-used
// limit in that window), e.g. "5h → 0.43/5 accounts used (4.57× quota left)".
func capacityLine(pu providerUsage) string {
	type window struct {
		key      string
		used     float64
		accounts int
	}
	var order []*window
	byKey := map[string]*window{}
	for _, a := range pu.accounts {
		if a.report == nil {
			continue
		}
		most := map[string]float64{}
		for _, l := range a.report.Limits {
			if l.Used < 0 || l.Window == "extra" {
				continue
			}
			key := l.Window
			if l.Tier != "" {
				key += " (" + l.Tier + ")"
			}
			if _, ok := byKey[key]; !ok {
				byKey[key] = &window{key: key}
				order = append(order, byKey[key])
			}
			if u, ok := most[key]; !ok || l.Used > u {
				most[key] = l.Used
			}
		}
		for key, u := range most {
			byKey[key].used += u
			byKey[key].accounts++
		}
	}
	// Shortest window first (5h before 7d).
	sort.SliceStable(order, func(i, j int) bool { return windowHours(order[i].key) < windowHours(order[j].key) })
	parts := make([]string, 0, len(order))
	for _, win := range order {
		parts = append(parts, fmt.Sprintf("%s → %.2f/%s used (%.2f× quota left)", win.key, win.used, plural(win.accounts, "account"), float64(win.accounts)-win.used))
	}
	return strings.Join(parts, " · ")
}

// windowHours reads the length of a window key like "5h", "7d (Fable)";
// unknown windows sort last.
func windowHours(key string) int {
	id, _, _ := strings.Cut(key, " ")
	if len(id) >= 2 {
		if n, err := strconv.Atoi(id[:len(id)-1]); err == nil {
			switch id[len(id)-1] {
			case 'h':
				return n
			case 'd':
				return n * 24
			}
		}
	}
	return math.MaxInt
}

func usageColor(c palette, used float64) string {
	switch {
	case used >= 1:
		return c.red
	case used >= 0.8:
		return c.yellow
	}
	return c.green
}

// shortDuration formats durations as 45s, 27m36s, 4h47m, 5d13h.
func shortDuration(d time.Duration) string {
	s := int(d.Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%ds", s/60, s%60)
	case s < 86400:
		return fmt.Sprintf("%dh%dm", s/3600, s%3600/60)
	}
	return fmt.Sprintf("%dd%dh", s/86400, s%86400/3600)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// usageCommand is /usage in the TUI.
func (a *app) usageCommand() {
	a.ui.Dim("fetching usage…")
	start := time.Now()
	all := fetchUsage(context.Background(), a.cfg, a.store)
	active := map[string]string{}
	if pool, ok := a.ag.Provider.(*llm.Pool); ok {
		for _, st := range pool.Status() {
			if st.Active {
				active[a.ag.ProviderName] = st.Label
			}
		}
	}
	renderUsage(a.out, a.ui.c, all, time.Since(start), active)
}
