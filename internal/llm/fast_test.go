package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestFastModels(t *testing.T) {
	for m, want := range map[string]bool{
		"claude-opus-5-5": true, "claude-opus-5": true, "claude-opus-4-8": true, "claude-opus-4-8-20260601": true,
		"claude-opus-5-5[1m]": true, "claude-opus-4-7": false, "claude-opus-4-6": false, "claude-sonnet-5-5": false,
		"claude-haiku-5-5": false, "claude-opus-5-6": false,
	} {
		if got := fastModel(m); got != want {
			t.Errorf("%s: %v, want %v", m, got, want)
		}
	}
	if !SupportsFast(&anthropicProvider{}, "claude-opus-5-5") || SupportsFast(&antigravityProvider{}, "claude-opus-5-5") ||
		SupportsFast(&openaiProvider{base: "https://openrouter.ai/api/v1"}, "anthropic/claude-opus-5-5") {
		t.Fatal("SupportsFast: only Anthropic itself")
	}
}

// fastServer answers like the Messages API; reject decides the status for
// a request that asks for fast mode (0 = serve it).
type fastServer struct {
	mu     sync.Mutex
	speeds []string
	betas  []string
	reject func() (int, string)
}

func (f *fastServer) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Speed string `json:"speed"`
	}
	json.Unmarshal(body, &req)
	f.mu.Lock()
	f.speeds = append(f.speeds, req.Speed)
	f.betas = append(f.betas, r.Header.Get("anthropic-beta"))
	reject := f.reject
	f.mu.Unlock()
	if req.Speed == "fast" && reject != nil {
		if status, msg := reject(); status != 0 {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(status)
			io.WriteString(w, msg)
			return
		}
	}
	speed := req.Speed
	if speed == "" {
		speed = "standard"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, `event: message_start
data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":10,"output_tokens":1,"speed":"`+speed+`"}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`)
}

func fastRequest(model string) *Request {
	return &Request{Model: model, Fast: true, MaxTokens: 100,
		Messages: []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "hi"}}}}}
}

func TestFastModeRequest(t *testing.T) {
	f := &fastServer{}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	p := &anthropicProvider{name: "anthropic", base: srv.URL, key: "k"}

	msg, err := p.Stream(context.Background(), fastRequest("claude-opus-5-5"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.speeds[0] != "fast" || !strings.Contains(f.betas[0], fastBeta) || strings.HasPrefix(f.betas[0], ",") {
		t.Fatalf("speed %q, betas %q", f.speeds[0], f.betas[0])
	}
	if !msg.Usage.Fast || msg.FastNote != "" {
		t.Fatalf("usage %+v, note %q", msg.Usage, msg.FastNote)
	}
	// A model without fast mode: neither the field nor the header.
	if _, err := p.Stream(context.Background(), fastRequest("claude-sonnet-5-5"), nil); err != nil {
		t.Fatal(err)
	}
	if f.speeds[1] != "" || strings.Contains(f.betas[1], fastBeta) {
		t.Fatalf("sonnet: speed %q, betas %q", f.speeds[1], f.betas[1])
	}
}

func TestFastModeFallsBackWhenRateLimited(t *testing.T) {
	f := &fastServer{reject: func() (int, string) {
		return 429, `{"type":"error","error":{"type":"rate_limit_error","message":"fast mode rate limit"}}`
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	p := &anthropicProvider{name: "anthropic", base: srv.URL, key: "k"}

	msg, err := p.Stream(context.Background(), fastRequest("claude-opus-5-5"), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Sent again at once at standard speed, without waiting out Retry-After.
	if strings.Join(f.speeds, ",") != "fast," || msg.Usage.Fast || !strings.Contains(msg.FastNote, "rate limited") {
		t.Fatalf("speeds %v, usage %+v, note %q", f.speeds, msg.Usage, msg.FastNote)
	}
	// Resting: the next request doesn't ask, and says nothing new.
	msg, err = p.Stream(context.Background(), fastRequest("claude-opus-5-5"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.speeds[2] != "" || msg.FastNote != "" {
		t.Fatalf("while resting: speed %q, note %q", f.speeds[2], msg.FastNote)
	}
}

func TestFastModeOffWhenRefused(t *testing.T) {
	f := &fastServer{reject: func() (int, string) {
		return 400, `{"type":"error","error":{"type":"invalid_request_error","message":"Fast mode is not enabled for this organization"}}`
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	p := &anthropicProvider{name: "anthropic", base: srv.URL, key: "k"}
	msg, err := p.Stream(context.Background(), fastRequest("claude-opus-5-5"), nil)
	// The note quotes the API's message, not its JSON.
	if err != nil || msg.FastNote != "fast mode unavailable (Fast mode is not enabled for this organization): standard speed for this session" {
		t.Fatalf("err %v, note %q", err, msg.FastNote)
	}
	if got := shortReason("upstream  connect\nerror"); got != "upstream connect error" {
		t.Fatalf("plain body: %q", got)
	}
	if p.fastFor(fastRequest("claude-opus-5-5")) {
		t.Fatal("still asks for fast mode after a refusal")
	}
	// Other errors are not fast mode's: they fail as before.
	f2 := &fastServer{reject: func() (int, string) { return 400, `{"error":{"message":"messages: bad block"}}` }}
	srv2 := httptest.NewServer(http.HandlerFunc(f2.handler))
	defer srv2.Close()
	p2 := &anthropicProvider{name: "anthropic", base: srv2.URL, key: "k"}
	if _, err := p2.Stream(context.Background(), fastRequest("claude-opus-5-5"), nil); err == nil {
		t.Fatal("an unrelated 400 was retried at standard speed")
	}
}
