package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// diagServer answers each request with message id m<n>, a cache miss
// reason when miss is set, and records each request's diagnostics.
func diagServer(t *testing.T, miss *string) (*httptest.Server, *[]any) {
	t.Helper()
	var got []any
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		d, ok := body["diagnostics"]
		if !ok {
			d = "absent"
		}
		got = append(got, d)
		n++
		diag := ""
		if *miss != "" {
			diag = `,"diagnostics":{"cache_miss_reason":{"type":"` + *miss + `","cache_missed_input_tokens":41850}}`
		}
		if body["stream"] == false {
			fmt.Fprintf(w, `{"id":"m%d","content":[],"stop_reason":"max_tokens","usage":{"input_tokens":1}%s}`, n, diag)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m%d\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1}%s}}\n\n", n, diag)
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// On the Claude API with a key, each request names the session's previous
// main or subagent response, not a compaction's or a keep-alive's, and the
// miss the API reports comes back on the message and in the stats.
func TestCacheDiagnosticsAPIKey(t *testing.T) {
	miss := ""
	srv, got := diagServer(t, &miss)
	old := diagnosticsBase
	diagnosticsBase = srv.URL
	defer func() { diagnosticsBase = old }()
	var stats []RequestStat
	SetRequestHook(func(s RequestStat) { stats = append(stats, s) })
	defer SetRequestHook(nil)

	p := &anthropicProvider{name: "anthropic", base: srv.URL, key: "k"}
	msgs := []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "hi"}}}}
	send := func(class string) *Message {
		t.Helper()
		m, err := p.Stream(context.Background(), &Request{Model: "claude-haiku-5-5", SessionID: "s", Class: class, Messages: msgs}, func(Event) {})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	send(ClassMain)       // m1
	send(ClassCompaction) // m2: not compared with
	miss = "tools_changed"
	m := send(ClassMain) // m3, compared with m1
	keep := append(msgs, Message{Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: "ok"}}},
		Message{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "warmup"}}})
	if _, err := p.Prewarm(context.Background(), &Request{Model: "claude-haiku-5-5", SessionID: "s", Class: ClassKeepAlive, Messages: keep, CachePrefix: 2}); err != nil {
		t.Fatal(err) // m4: not compared with
	}
	send(ClassMain) // compared with m3

	want := []string{`{"previous_message_id":null}`, `{"previous_message_id":"m1"}`, `{"previous_message_id":"m1"}`, `{"previous_message_id":"m3"}`, `{"previous_message_id":"m3"}`}
	for i, d := range *got {
		b, _ := json.Marshal(d)
		if i >= len(want) || string(b) != want[i] {
			t.Errorf("request %d diagnostics %s, want %v", i+1, b, want)
		}
	}
	if m.CacheMiss == nil || m.CacheMiss.Reason != "tools_changed" || m.CacheMiss.Tokens != 41850 || !m.CacheMiss.Changed() {
		t.Fatalf("miss %+v", m.CacheMiss)
	}
	if len(stats) != 5 || stats[2].CacheMiss == nil || stats[0].CacheMiss != nil || stats[3].CacheMiss == nil {
		t.Fatalf("stats %+v", stats)
	}
}

// On OAuth, too, only main and subagent responses are compared with.
func TestCacheDiagnosticsOAuthClasses(t *testing.T) {
	p := &anthropicProvider{}
	for _, c := range []struct{ class, id, want string }{
		{ClassMain, "m1", "m1"}, {ClassCompaction, "m2", "m1"}, {ClassAuxiliary, "m3", "m1"}, {ClassSubagent, "m4", "m4"},
	} {
		p.finish(p.turn(context.Background(), "s", c.class, nil), "", c.id)
		if got := p.prevMessage("s"); got != c.want {
			t.Fatalf("after %s: %q, want %q", c.class, got, c.want)
		}
	}
}

// Another Anthropic-compatible endpoint is not sent the field.
func TestCacheDiagnosticsOnlyOnTheClaudeAPI(t *testing.T) {
	miss := ""
	srv, got := diagServer(t, &miss)
	p := &anthropicProvider{name: "kimi", base: srv.URL, key: "k"}
	for range 2 {
		if _, err := p.Stream(context.Background(), &Request{Model: "kimi-k2", SessionID: "s",
			Messages: []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "hi"}}}}}, func(Event) {}); err != nil {
			t.Fatal(err)
		}
	}
	if len(*got) != 2 || (*got)[0] != "absent" || (*got)[1] != "absent" {
		t.Fatalf("diagnostics %v", *got)
	}
	if (&CacheMiss{Reason: "unavailable"}).Changed() || (*CacheMiss)(nil).Changed() {
		t.Fatal("Changed")
	}
}
