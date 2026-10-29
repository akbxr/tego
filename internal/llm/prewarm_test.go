package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A keep-alive goes out unstreamed with max_tokens 0, its breakpoint on the
// last block it keeps (not the placeholder), and reports its usage.
func TestPrewarmBody(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		io.WriteString(w, `{"id":"msg_1","content":[],"stop_reason":"max_tokens","usage":{"input_tokens":9,"output_tokens":0,"cache_read_input_tokens":18000,"cache_creation_input_tokens":40}}`)
	}))
	defer srv.Close()
	var stats []RequestStat
	SetRequestHook(func(s RequestStat) { stats = append(stats, s) })
	defer SetRequestHook(nil)

	p := &anthropicProvider{name: "anthropic", base: srv.URL, key: "k"}
	msgs := []Message{
		{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "read the notes"}}},
		{Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: "done"}}},
		{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "warmup"}}},
	}
	req := &Request{Model: "claude-haiku-5-5", System: "sys", Messages: msgs, Thinking: "low",
		LongCache: true, Class: ClassKeepAlive, CachePrefix: 2, MaxTokens: 4096}
	if !CanPrewarm(p, req.Model) {
		t.Fatal("anthropic cannot prewarm")
	}
	u, err := Prewarm(context.Background(), p, req)
	if err != nil {
		t.Fatal(err)
	}
	if u.CacheRead != 18000 || u.CacheWrite != 40 || u.Input != 9 {
		t.Fatalf("usage %+v", u)
	}
	if body["stream"] != false || body["max_tokens"] != float64(0) {
		t.Fatalf("stream %v, max_tokens %v", body["stream"], body["max_tokens"])
	}
	wire, _ := json.Marshal(body["messages"])
	s := string(wire)
	if strings.Count(s, "cache_control") != 1 || !strings.Contains(s, `"cache_control":{"ttl":"1h","type":"ephemeral"},"text":"done"`) {
		t.Fatalf("breakpoint not on the last kept block: %s", s)
	}
	if len(stats) != 1 || stats[0].Class != ClassKeepAlive || stats[0].Usage.CacheRead != 18000 || stats[0].Err != "" {
		t.Fatalf("stats %+v", stats)
	}

	// Not a keep-alive request: refused before sending.
	req.Class = ClassMain
	if _, err := p.Prewarm(context.Background(), req); err == nil {
		t.Fatal("a main request was sent as a keep-alive")
	}
	// A fixed thinking budget cannot go with max_tokens 0.
	req.Class, req.Model, req.Thinking = ClassKeepAlive, "claude-sonnet-4-5", "high"
	if _, err := p.Prewarm(context.Background(), req); err == nil || !strings.Contains(err.Error(), "max_tokens 0") {
		t.Fatalf("legacy thinking: %v", err)
	}
}

// A pool keeps alive on the account the session used last.
func TestPoolPrewarmUsesTheSessionsAccount(t *testing.T) {
	a, b := &fakePrewarmer{name: "a"}, &fakePrewarmer{name: "b"}
	pool := NewPool("anthropic", StrategyFallback, []PoolMember{{Label: "a", Provider: a}, {Label: "b", Provider: b}})
	pool.sessions["s1"] = 1
	Prewarm(context.Background(), pool, &Request{SessionID: "s1"})
	Prewarm(context.Background(), pool, &Request{SessionID: "other"})
	if a.n != 1 || b.n != 1 {
		t.Fatalf("a %d, b %d", a.n, b.n)
	}
	if !CanPrewarm(pool, "m") || CanPrewarm(NewPool("x", "", []PoolMember{{Provider: &openaiProvider{}}}), "m") {
		t.Fatal("CanPrewarm")
	}
}

type fakePrewarmer struct {
	name string
	n    int
}

func (f *fakePrewarmer) Name() string { return f.name }
func (f *fakePrewarmer) Stream(context.Context, *Request, func(Event)) (*Message, error) {
	return nil, nil
}
func (f *fakePrewarmer) CanPrewarm(string) bool { return true }
func (f *fakePrewarmer) Prewarm(context.Context, *Request) (Usage, error) {
	f.n++
	return Usage{}, nil
}
