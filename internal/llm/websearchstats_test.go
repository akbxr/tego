package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// captureRequests records what providers report to the stats hook during
// the test.
func captureRequests(t *testing.T) func() []RequestStat {
	var mu sync.Mutex
	var got []RequestStat
	SetRequestHook(func(st RequestStat) {
		mu.Lock()
		got = append(got, st)
		mu.Unlock()
	})
	t.Cleanup(func() { SetRequestHook(nil) })
	return func() []RequestStat {
		mu.Lock()
		defer mu.Unlock()
		return append([]RequestStat(nil), got...)
	}
}

// A search that pauses once: one request for the stats, with the usage and
// searches of both responses.
func TestAnthropicWebSearchReportsItsRequest(t *testing.T) {
	replies := []string{
		`{"stop_reason":"pause_turn","content":[{"type":"server_tool_use","id":"s1","name":"web_search","input":{"query":"go"}},` +
			`{"type":"web_search_tool_result","tool_use_id":"s1","content":[{"type":"web_search_result","url":"https://go.dev","title":"Go"}]}],` +
			`"usage":{"input_tokens":100,"output_tokens":10,"server_tool_use":{"web_search_requests":1}}}`,
		`{"stop_reason":"end_turn","content":[{"type":"text","text":"Go 1.27"}],` +
			`"usage":{"input_tokens":3000,"output_tokens":50,"cache_read_input_tokens":200,"cache_creation_input_tokens":40,"server_tool_use":{"web_search_requests":1}}}`,
	}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(replies[min(n, len(replies)-1)]))
		n++
	}))
	defer srv.Close()
	got := captureRequests(t)
	p := &anthropicProvider{name: "anthropic", base: srv.URL, key: "k"}
	res, err := p.WebSearch(context.Background(), "claude-haiku-5-5", "latest go")
	if err != nil || !strings.Contains(res.Text, "Go 1.27") {
		t.Fatalf("result %+v, err %v", res, err)
	}
	stats := got()
	if len(stats) != 1 {
		t.Fatalf("reported %d requests, want 1: %+v", len(stats), stats)
	}
	st := stats[0]
	want := Usage{Input: 3100, Output: 60, CacheRead: 200, CacheWrite: 40}
	if st.Provider != "anthropic" || st.Model != "claude-haiku-5-5" || st.Class != ClassAuxiliary || st.Usage != want ||
		st.WebSearches != 2 || st.Err != "" || st.TTFT != 0 || st.GenStart != 0 {
		t.Fatalf("stat %+v", st)
	}
}

func TestAnthropicWebSearchReportsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`))
	}))
	defer srv.Close()
	got := captureRequests(t)
	p := &anthropicProvider{name: "anthropic", base: srv.URL, key: "k"}
	if _, err := p.WebSearch(context.Background(), "claude-haiku-5-5", "q"); err == nil {
		t.Fatal("no error")
	}
	if stats := got(); len(stats) != 1 || stats[0].Err == "" || stats[0].StopReason != "error" || stats[0].WebSearches != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestCodexWebStreamUsage(t *testing.T) {
	events := []string{
		`{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_1","action":{"type":"search","sources":[{"type":"url","url":"https://go.dev/dl/"}]}}}`,
		`{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_2","action":{"type":"search"}}}`,
		`{"type":"response.output_item.done","item":{"type":"message","id":"m1","content":[{"type":"output_text","text":"Go 1.27"}]}}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5000,"output_tokens":80,"input_tokens_details":{"cached_tokens":1200}}}}`,
	}
	var sse strings.Builder
	for _, e := range events {
		sse.WriteString("data: " + e + "\n\n")
	}
	res, err := parseCodexWebStream(context.Background(), "openai-codex", strings.NewReader(sse.String()))
	if err != nil {
		t.Fatal(err)
	}
	if want := (Usage{Input: 3800, CacheRead: 1200, Output: 80}); res.usage != want || res.searches != 2 {
		t.Fatalf("usage %+v, searches %d", res.usage, res.searches)
	}
}
