package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// UsageLimit is one subscription limit window of an account.
type UsageLimit struct {
	Label    string    // e.g. "Claude 5 Hour", "Gemini (Weekly)"
	Window   string    // short window id for capacity sums: "5h", "7d", "Monthly"…
	Tier     string    // model tier the limit is scoped to ("Fable"), "" for shared limits
	Used     float64   // fraction used, 0..1; negative when not reported
	ResetsAt time.Time // zero when unknown
	Detail   string    // optional amount text, e.g. "$3.20 / $50.00"
}

// UsageReport is the usage of one account.
type UsageReport struct {
	Email  string // account email when the endpoint reports it
	Org    string // organisation name (Anthropic)
	Limits []UsageLimit
}

// FetchUsage queries a subscription's usage endpoint. kind is the provider
// kind ("anthropic", "openai-codex", "google-antigravity"); token is a
// valid OAuth access token; projectID is the Antigravity project.
func FetchUsage(ctx context.Context, kind, token, projectID string) (*UsageReport, error) {
	switch kind {
	case "anthropic":
		return anthropicUsage(ctx, token)
	case "openai-codex":
		return codexUsage(ctx, token)
	case "google-antigravity":
		return antigravityUsage(ctx, token, projectID)
	}
	return nil, fmt.Errorf("usage is not available for %s", kind)
}

func usageRequest(ctx context.Context, method, url string, body io.Reader, h http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header = h
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 200))
	}
	return json.Unmarshal(raw, out)
}

// ---- Anthropic: GET /api/oauth/usage (+ /api/oauth/profile for the org) ----

type claudeBucket struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type claudeUsagePayload struct {
	FiveHour       *claudeBucket `json:"five_hour"`
	SevenDay       *claudeBucket `json:"seven_day"`
	SevenDayOpus   *claudeBucket `json:"seven_day_opus"`
	SevenDaySonnet *claudeBucket `json:"seven_day_sonnet"`
	Limits         []struct {
		Kind     string   `json:"kind"`
		Percent  *float64 `json:"percent"`
		ResetsAt string   `json:"resets_at"`
		Scope    *struct {
			Model *struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	} `json:"limits"`
	Spend *struct {
		Enabled bool `json:"enabled"`
		Used    *struct {
			AmountMinor int64 `json:"amount_minor"`
			Exponent    int   `json:"exponent"`
		} `json:"used"`
		Limit *struct {
			AmountMinor int64 `json:"amount_minor"`
			Exponent    int   `json:"exponent"`
		} `json:"limit"`
	} `json:"spend"`
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func (b *claudeBucket) limit(label, window, tier string) (UsageLimit, bool) {
	if b == nil || (b.Utilization == nil && b.ResetsAt == "") {
		return UsageLimit{}, false
	}
	l := UsageLimit{Label: label, Window: window, Tier: tier, Used: -1, ResetsAt: parseTime(b.ResetsAt)}
	if b.Utilization != nil {
		l.Used = min(max(*b.Utilization, 0), 100) / 100
	}
	return l, true
}

// Usage endpoints (variables so tests can point them at a local server).
var (
	anthropicOAuthAPI    = "https://api.anthropic.com/api/oauth"
	codexUsageURL        = codexDefaultBase + "/wham/usage"
	antigravityQuotaURLs = []string{
		"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
		"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	}
)

func anthropicUsage(ctx context.Context, token string) (*UsageReport, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("anthropic-beta", "oauth-2025-04-20")
	h.Set("Accept", "application/json")
	h.Set("User-Agent", "claude-cli/"+ClaudeCodeVersion+" (external, cli)")
	base := anthropicOAuthAPI

	var profile struct {
		Account struct {
			Email string `json:"email"`
		} `json:"account"`
		Organization struct {
			Name string `json:"name"`
		} `json:"organization"`
	}
	profileDone := make(chan struct{})
	go func() {
		defer close(profileDone)
		_ = usageRequest(ctx, http.MethodGet, base+"/profile", nil, h.Clone(), &profile)
	}()
	var p claudeUsagePayload
	err := usageRequest(ctx, http.MethodGet, base+"/usage", nil, h, &p)
	<-profileDone
	if err != nil {
		return nil, err
	}

	// Named buckets first; the newer "limits" list fills gaps.
	find := func(kind string) *claudeBucket {
		for _, l := range p.Limits {
			if l.Kind == kind {
				return &claudeBucket{Utilization: l.Percent, ResetsAt: l.ResetsAt}
			}
		}
		return nil
	}
	five, seven := p.FiveHour, p.SevenDay
	if five == nil {
		five = find("session")
	}
	if seven == nil {
		seven = find("weekly_all")
	}
	r := &UsageReport{Email: profile.Account.Email, Org: profile.Organization.Name}
	add := func(b *claudeBucket, label, window, tier string) {
		if l, ok := b.limit(label, window, tier); ok {
			r.Limits = append(r.Limits, l)
		}
	}
	add(five, "Claude 5 Hour", "5h", "")
	add(seven, "Claude 7 Day", "7d", "")
	add(p.SevenDayOpus, "Claude 7 Day (Opus)", "7d", "Opus")
	add(p.SevenDaySonnet, "Claude 7 Day (Sonnet)", "7d", "Sonnet")
	seen := map[string]bool{}
	for _, l := range p.Limits {
		if l.Kind != "weekly_scoped" || l.Scope == nil || l.Scope.Model == nil || l.Scope.Model.DisplayName == "" {
			continue
		}
		name := strings.TrimSpace(l.Scope.Model.DisplayName)
		if seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		add(&claudeBucket{Utilization: l.Percent, ResetsAt: l.ResetsAt}, "Claude 7 Day ("+name+")", "7d", name)
	}
	if s := p.Spend; s != nil && s.Enabled && s.Used != nil {
		used := money(s.Used.AmountMinor, s.Used.Exponent)
		l := UsageLimit{Label: "Claude Extra Usage", Window: "extra", Used: -1, Detail: fmt.Sprintf("$%.2f spent", used)}
		if s.Limit != nil {
			if lim := money(s.Limit.AmountMinor, s.Limit.Exponent); lim > 0 {
				l.Used = min(used/lim, 1)
				l.Detail = fmt.Sprintf("$%.2f / $%.2f", used, lim)
			}
		}
		r.Limits = append(r.Limits, l)
	}
	return r, nil
}

func money(minor int64, exp int) float64 {
	v := float64(minor)
	for range exp {
		v /= 10
	}
	return v
}

// ---- OpenAI Codex: GET https://chatgpt.com/backend-api/wham/usage ----

type codexWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds float64  `json:"limit_window_seconds"`
	ResetAfterSeconds  *float64 `json:"reset_after_seconds"`
	ResetAt            *float64 `json:"reset_at"`
}

type codexRateLimit struct {
	Primary   *codexWindow `json:"primary_window"`
	Secondary *codexWindow `json:"secondary_window"`
}

func (w *codexWindow) limit(now time.Time, fallback, suffix string) UsageLimit {
	label, id := fallback, fallback
	if s := w.LimitWindowSeconds; s > 0 {
		if s >= 86400 {
			n := int(s/86400 + 0.5)
			label, id = plural(n, "day"), fmt.Sprintf("%dd", n)
		} else {
			n := max(1, int(s/3600+0.5))
			label, id = plural(n, "hour"), fmt.Sprintf("%dh", n)
		}
	}
	l := UsageLimit{Label: label + suffix, Window: id, Used: -1}
	if w.UsedPercent != nil {
		l.Used = min(max(*w.UsedPercent, 0), 100) / 100
	}
	switch {
	case w.ResetAt != nil && *w.ResetAt > 1e12:
		l.ResetsAt = time.UnixMilli(int64(*w.ResetAt))
	case w.ResetAt != nil:
		l.ResetsAt = time.Unix(int64(*w.ResetAt), 0)
	case w.ResetAfterSeconds != nil:
		l.ResetsAt = now.Add(time.Duration(*w.ResetAfterSeconds * float64(time.Second)))
	}
	return l
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func codexUsage(ctx context.Context, token string) (*UsageReport, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	if id := codexClaims(token).AccountID; id != "" {
		h.Set("chatgpt-account-id", id)
	}
	h.Set("User-Agent", codexUserAgent)
	h.Set("Accept", "application/json")
	var p struct {
		Email      string          `json:"email"`
		PlanType   string          `json:"plan_type"`
		RateLimit  *codexRateLimit `json:"rate_limit"`
		Additional []struct {
			LimitName      string          `json:"limit_name"`
			MeteredFeature string          `json:"metered_feature"`
			RateLimit      *codexRateLimit `json:"rate_limit"`
		} `json:"additional_rate_limits"`
	}
	if err := usageRequest(ctx, http.MethodGet, codexUsageURL, nil, h, &p); err != nil {
		return nil, err
	}
	now := time.Now()
	r := &UsageReport{Email: p.Email, Org: p.PlanType}
	addRL := func(rl *codexRateLimit, suffix string) {
		if rl == nil {
			return
		}
		if rl.Primary != nil {
			r.Limits = append(r.Limits, rl.Primary.limit(now, "Primary window", suffix))
		}
		if rl.Secondary != nil {
			r.Limits = append(r.Limits, rl.Secondary.limit(now, "Secondary window", suffix))
		}
	}
	addRL(p.RateLimit, "")
	for _, a := range p.Additional {
		name := cmpOr(a.LimitName, a.MeteredFeature, "extra")
		if s := strings.ToLower(name + " " + a.MeteredFeature); strings.Contains(s, "spark") || strings.Contains(s, "bengalfox") {
			name = "Spark"
		}
		n := len(r.Limits)
		addRL(a.RateLimit, " ("+name+")")
		for i := n; i < len(r.Limits); i++ {
			r.Limits[i].Tier = name
		}
	}
	return r, nil
}

func cmpOr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---- Google Antigravity: POST v1internal:retrieveUserQuotaSummary ----

func antigravityUsage(ctx context.Context, token, projectID string) (*UsageReport, error) {
	if projectID == "" {
		return nil, fmt.Errorf("no Cloud Code Assist project for this account")
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", AntigravityUserAgent(ctx))
	body, _ := json.Marshal(map[string]string{"project": projectID})
	type bucket struct {
		BucketID          string   `json:"bucketId"`
		DisplayName       string   `json:"displayName"`
		Window            string   `json:"window"`
		ResetTime         string   `json:"resetTime"`
		RemainingFraction *float64 `json:"remainingFraction"`
		Disabled          bool     `json:"disabled"`
	}
	var p struct {
		Groups []struct {
			DisplayName string   `json:"displayName"`
			Buckets     []bucket `json:"buckets"`
		} `json:"groups"`
		Buckets []bucket `json:"buckets"`
	}
	var err error
	for _, url := range antigravityQuotaURLs {
		if err = usageRequest(ctx, http.MethodPost, url, strings.NewReader(string(body)), h.Clone(), &p); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	r := &UsageReport{}
	add := func(b bucket, group string) {
		if b.Disabled {
			return
		}
		window, wlabel := antigravityWindow(b.Window + " " + b.DisplayName)
		id, g := strings.ToLower(b.BucketID), strings.ToLower(group)
		name := cmpOr(group, b.DisplayName, "Usage")
		switch {
		case strings.HasPrefix(id, "gemini-") || strings.Contains(g, "gemini"):
			name = "Gemini"
		case strings.HasPrefix(id, "3p-") || strings.Contains(g, "claude") || strings.Contains(g, "gpt"):
			name = "Claude & GPT (shared)"
		}
		l := UsageLimit{Label: name + " (" + wlabel + ")", Window: window, Used: -1, ResetsAt: parseTime(b.ResetTime)}
		switch {
		case b.RemainingFraction != nil:
			l.Used = 1 - min(max(*b.RemainingFraction, 0), 1)
		case b.ResetTime != "":
			l.Used = 1 // proto3 omits a zero remainingFraction: exhausted
		}
		r.Limits = append(r.Limits, l)
	}
	for _, g := range p.Groups {
		for _, b := range g.Buckets {
			add(b, g.DisplayName)
		}
	}
	for _, b := range p.Buckets {
		add(b, "")
	}
	// Most-used first.
	sort.SliceStable(r.Limits, func(i, j int) bool { return r.Limits[i].Used > r.Limits[j].Used })
	return r, nil
}

func antigravityWindow(s string) (id, label string) {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "week") || strings.Contains(s, "7d"):
		return "7d", "Weekly"
	case strings.Contains(s, "5h") || strings.Contains(s, "five hour"):
		return "5h", "5 Hour"
	case strings.Contains(s, "day") || strings.Contains(s, "24h"):
		return "1d", "Daily"
	}
	return "default", "Default"
}
