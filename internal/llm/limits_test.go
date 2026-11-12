package llm

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func unifiedHeader(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set("Anthropic-Ratelimit-Unified-"+kv[i], kv[i+1])
	}
	return h
}

func TestAnthropicHeaderLimits(t *testing.T) {
	// As a Fable answer of an OAuth account reports them.
	h := unifiedHeader("5h-Utilization", "0.85", "5h-Reset", "1794464400", "5h-Status", "allowed",
		"7d-Utilization", "0.16", "7d-Reset", "1794477600",
		"7d_oi-Utilization", "0.29", "7d_oi-Reset", "1794477600",
		"Status", "allowed", "Representative-Claim", "five_hour", "Fallback-Percentage", "0.5")
	got := anthropicHeaderLimits(h, "claude-fable-5-2")
	want := []UsageLimit{
		{Label: "Claude 5 Hour", Window: "5h", Used: 0.85, ResetsAt: time.Unix(1794464400, 0)},
		{Label: "Claude 7 Day", Window: "7d", Used: 0.16, ResetsAt: time.Unix(1794477600, 0)},
		{Label: "Claude 7 Day (Fable)", Window: "7d", Tier: "Fable", Used: 0.29, ResetsAt: time.Unix(1794477600, 0)},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	// A scoped limit names its tier by a code: without a model to take it
	// from, it is left out.
	if got := anthropicHeaderLimits(h, ""); len(got) != 2 {
		t.Errorf("no model: %+v", got)
	}
	if got := anthropicHeaderLimits(http.Header{"Request-Id": {"x"}}, "claude-opus-5-5"); got != nil {
		t.Errorf("an API key's answer: %+v", got)
	}
}

func TestNoteLimits(t *testing.T) {
	got := make(chan []UsageLimit, 2)
	p := &anthropicProvider{onLimits: func(l []UsageLimit) { got <- l }}
	h := unifiedHeader("5h-Utilization", "0.4", "7d-Utilization", "1.0", "7d-Status", "rejected")
	p.noteLimits("claude-opus-5-5", &http.Response{Header: h}, nil)
	// A refusal reports them too: the account is full for every process.
	p.noteLimits("claude-opus-5-5", nil, &APIError{Status: 429, Header: h})
	for range 2 {
		select {
		case l := <-got:
			if len(l) != 2 || l[1].Used != 1 {
				t.Errorf("limits %+v", l)
			}
		case <-time.After(time.Second):
			t.Fatal("no limits passed on")
		}
	}
	(&anthropicProvider{}).noteLimits("claude-opus-5-5", &http.Response{Header: h}, nil) // none wanted: no panic
}

func TestLimitStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "limits.json")
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	a := &LimitStore{path: path, now: clock}
	b := &LimitStore{path: path, now: clock} // another process
	const key = "anthropic/acct1"

	asked := 0
	fetch := func(err error) func(context.Context) (*UsageReport, error) {
		return func(context.Context) (*UsageReport, error) {
			asked++
			if err != nil {
				return nil, err
			}
			return &UsageReport{Org: "Org", Limits: []UsageLimit{
				{Label: "Claude 5 Hour", Window: "5h", Used: 0.5, ResetsAt: now.Add(2 * time.Hour)},
				{Label: "Claude 7 Day (Fable)", Window: "7d", Tier: "Fable", Used: 0.7, ResetsAt: now.Add(48 * time.Hour)},
			}}, nil
		}
	}

	// An answer reported the account: nobody asks the endpoint.
	a.Record(key, []UsageLimit{{Label: "Claude 5 Hour", Window: "5h", Used: 0.2, ResetsAt: now.Add(time.Hour)}}, "answer")
	k := b.Usage(context.Background(), key, time.Hour, fetch(nil))
	if asked != 0 || k.Report == nil || k.From != "answer" || k.Report.Limits[0].Used != 0.2 {
		t.Fatalf("fresh answer: asked %d, %+v", asked, k)
	}

	// An hour on: one process asks, the other takes what it got.
	now = now.Add(61 * time.Minute)
	k = a.Usage(context.Background(), key, time.Hour, fetch(nil))
	if asked != 1 || k.From != "endpoint" || k.Report.Org != "Org" || len(k.Report.Limits) != 2 {
		t.Fatalf("stale: asked %d, %+v", asked, k)
	}
	if k = b.Usage(context.Background(), key, time.Hour, fetch(nil)); asked != 1 || len(k.Report.Limits) != 2 {
		t.Fatalf("the other process asked again: %d", asked)
	}

	// The endpoint refuses: nobody asks it for an hour, and what is known
	// is still there.
	now = now.Add(2 * time.Hour)
	refused := &UsageError{Status: 429, Message: "Rate limited. Please try again later."}
	k = a.Usage(context.Background(), key, time.Hour, fetch(refused))
	if asked != 2 || k.Failed == nil || k.Report == nil || k.Blocked.IsZero() {
		t.Fatalf("refused: asked %d, %+v", asked, k)
	}
	now = now.Add(30 * time.Minute)
	if k = b.Usage(context.Background(), key, 0, fetch(nil)); asked != 2 || k.Refusal != refused.Error() {
		t.Fatalf("asked while refusing: %d, %+v", asked, k)
	}
	now = now.Add(31 * time.Minute)
	if b.Usage(context.Background(), key, time.Hour, fetch(nil)); asked != 3 {
		t.Fatalf("not asked after the backoff: %d", asked)
	}

	// A window that ended since it was reported: nothing of the next is used.
	now = now.Add(3 * time.Hour)
	for _, l := range a.Known(key).Report.Limits {
		if l.Window == "5h" && (l.Used != 0 || !l.ResetsAt.IsZero()) {
			t.Errorf("ended window: %+v", l)
		}
		if l.Window == "7d" && l.Used != 0.7 {
			t.Errorf("weekly: %+v", l)
		}
	}

	// No store: asked every time.
	var none *LimitStore
	none.Record(key, []UsageLimit{{Label: "x"}}, "answer")
	if k := none.Usage(context.Background(), key, time.Hour, fetch(nil)); asked != 4 || k.Report == nil {
		t.Errorf("nil store: asked %d", asked)
	}
}

func TestUsageErrorMessage(t *testing.T) {
	body := "{\n  \"error\": {\n    \"type\": \"rate_limit_error\",\n    \"message\": \"Rate limited. Please try again later.\"\n  }\n}"
	err := &UsageError{Status: 429, Message: usageErrorMessage([]byte(body))}
	if err.Error() != "HTTP 429: Rate limited. Please try again later." || !UsageRefused(err) {
		t.Errorf("%q", err)
	}
	if m := usageErrorMessage([]byte("<html>\n bad gateway\n</html>")); m != "<html> bad gateway </html>" {
		t.Errorf("%q", m)
	}
}
