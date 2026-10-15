package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func serve(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAnthropicUsage(t *testing.T) {
	srv := serve(t, map[string]string{
		// five_hour named bucket; weekly only via the newer limits list,
		// including a model-scoped weekly limit; spend disabled.
		"/api/oauth/usage": `{"five_hour":{"utilization":43,"resets_at":"2030-01-01T05:00:00Z"},"seven_day":null,
			"limits":[{"kind":"weekly_all","percent":10,"resets_at":"2030-01-05T00:00:00Z"},
			          {"kind":"weekly_scoped","percent":0,"resets_at":"2030-01-06T00:00:00Z","scope":{"model":{"display_name":"Fable"}}}],
			"spend":{"enabled":false,"used":{"amount_minor":0,"exponent":2}}}`,
		"/api/oauth/profile": `{"account":{"email":"me@x.com"},"organization":{"name":"Unli"}}`,
	})
	old := anthropicOAuthAPI
	anthropicOAuthAPI = srv.URL + "/api/oauth"
	defer func() { anthropicOAuthAPI = old }()

	r, err := FetchUsage(context.Background(), "anthropic", "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Email != "me@x.com" || r.Org != "Unli" {
		t.Fatalf("identity = %q %q", r.Email, r.Org)
	}
	want := []struct {
		label, window, tier string
		used                float64
	}{
		{"Claude 5 Hour", "5h", "", 0.43},
		{"Claude 7 Day", "7d", "", 0.10},
		{"Claude 7 Day (Fable)", "7d", "Fable", 0},
	}
	if len(r.Limits) != len(want) {
		t.Fatalf("limits = %+v", r.Limits)
	}
	for i, w := range want {
		l := r.Limits[i]
		if l.Label != w.label || l.Window != w.window || l.Tier != w.tier || l.Used != w.used || l.ResetsAt.IsZero() {
			t.Errorf("limit %d = %+v, want %+v", i, l, w)
		}
	}
}

func TestAntigravityUsage(t *testing.T) {
	srv := serve(t, map[string]string{
		"/quota": `{"groups":[
			{"displayName":"Gemini Models","buckets":[
				{"bucketId":"gemini-weekly","window":"weekly","resetTime":"2030-01-05T00:00:00Z","remainingFraction":0.9}]},
			{"displayName":"Claude and GPT models","buckets":[
				{"bucketId":"3p-weekly","window":"weekly","resetTime":"2030-01-06T00:00:00Z"},
				{"bucketId":"3p-5h","window":"5h","resetTime":"2030-01-01T05:00:00Z","remainingFraction":1}]}]}`,
	})
	old := antigravityQuotaURLs
	antigravityQuotaURLs = []string{srv.URL + "/quota"}
	defer func() { antigravityQuotaURLs = old }()

	r, err := FetchUsage(context.Background(), "google-antigravity", "tok", "proj")
	if err != nil {
		t.Fatal(err)
	}
	// Sorted most-used first; a bucket without remainingFraction is exhausted
	// (proto3 drops zero values).
	got := []string{}
	for _, l := range r.Limits {
		got = append(got, l.Label)
	}
	if len(r.Limits) != 3 ||
		r.Limits[0].Label != "Claude & GPT (shared) (Weekly)" || r.Limits[0].Used != 1 || r.Limits[0].Window != "7d" ||
		r.Limits[1].Label != "Gemini (Weekly)" || r.Limits[1].Used < 0.099 || r.Limits[1].Used > 0.101 ||
		r.Limits[2].Label != "Claude & GPT (shared) (5 Hour)" || r.Limits[2].Used != 0 || r.Limits[2].Window != "5h" {
		t.Fatalf("limits = %v / %+v", got, r.Limits)
	}
	if _, err := FetchUsage(context.Background(), "google-antigravity", "tok", ""); err == nil {
		t.Fatal("expected an error without a project")
	}
}

func TestCodexUsage(t *testing.T) {
	srv := serve(t, map[string]string{
		"/wham/usage": `{"plan_type":"plus","rate_limit":{
			"primary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_after_seconds":600},
			"secondary_window":{"used_percent":5,"limit_window_seconds":604800,"reset_at":1893456000}},
			"additional_rate_limits":[{"limit_name":"codex-bengalfox","rate_limit":{"primary_window":{"used_percent":50,"limit_window_seconds":18000}}}]}`,
	})
	old := codexUsageURL
	codexUsageURL = srv.URL + "/wham/usage"
	defer func() { codexUsageURL = old }()

	r, err := FetchUsage(context.Background(), "openai-codex", "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Limits) != 3 {
		t.Fatalf("limits = %+v", r.Limits)
	}
	p, s, x := r.Limits[0], r.Limits[1], r.Limits[2]
	if p.Label != "5 hours" || p.Window != "5h" || p.Used != 0.25 || time.Until(p.ResetsAt) > 11*time.Minute {
		t.Errorf("primary = %+v", p)
	}
	if s.Label != "7 days" || s.Window != "7d" || s.ResetsAt.Unix() != 1893456000 {
		t.Errorf("secondary = %+v", s)
	}
	if x.Label != "5 hours (Spark)" || x.Tier != "Spark" || x.Used != 0.5 {
		t.Errorf("additional = %+v", x)
	}
}
