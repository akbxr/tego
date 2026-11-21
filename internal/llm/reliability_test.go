package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// noRetryWait makes retry and pool waits return at once; it returns the
// waits asked for.
func noRetryWait(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	old := sleepCtx
	sleepCtx = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	t.Cleanup(func() { sleepCtx = old })
	return &waits
}

func withIdleTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := streamIdleTimeout()
	SetStreamIdleTimeout(d)
	t.Cleanup(func() { SetStreamIdleTimeout(old) })
}

const oaHi = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// silentServer answers through script, which gets the request's number
// (from 1); hang blocks until the client gives up on the request.
func silentServer(t *testing.T, script func(n int32, w http.ResponseWriter, hang func())) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		script(hits.Add(1), w, func() {
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-release:
			}
		})
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	return srv, &hits
}

// A stream that goes silent before anything reached the caller is cut by
// the idle watch and retried.
func TestIdleStreamIsRetriedBeforeOutput(t *testing.T) {
	withIdleTimeout(t, 100*time.Millisecond)
	waits := noRetryWait(t)
	srv, hits := silentServer(t, func(n int32, w http.ResponseWriter, hang func()) {
		if n == 1 {
			io.WriteString(w, ": keep-alive\n\n") // some bytes, then nothing
			hang()
			return
		}
		io.WriteString(w, oaHi)
	})
	p := &openaiProvider{name: "oa", base: srv.URL}
	start := time.Now()
	msg, err := p.Stream(context.Background(), prompt("s"), nil)
	if err != nil || len(msg.Blocks) != 1 || msg.Blocks[0].Text != "hi" {
		t.Fatalf("msg %+v, err %v", msg, err)
	}
	if hits.Load() != 2 || len(*waits) != 1 {
		t.Fatalf("%d requests, %d retry waits; want 2 and 1", hits.Load(), len(*waits))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v: the silent stream was not cut", d)
	}
}

// Once output has reached the caller, an idle stream is an error, marked
// as a stream error and as an idle timeout, and is not sent again.
func TestIdleStreamAfterOutputIsAnError(t *testing.T) {
	withIdleTimeout(t, 100*time.Millisecond)
	noRetryWait(t)
	srv, hits := silentServer(t, func(n int32, w http.ResponseWriter, hang func()) {
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n")
		hang()
	})
	p := &openaiProvider{name: "oa", base: srv.URL}
	var got strings.Builder
	msg, err := p.Stream(context.Background(), prompt("s"), func(e Event) { got.WriteString(e.Text) })
	var se *streamError
	if !errors.Is(err, ErrStreamIdle) || !errors.As(err, &se) || !strings.Contains(err.Error(), "idle stream timeout") {
		t.Fatalf("err = %v", err)
	}
	if msg.StopReason != "error" || got.String() != "par" || hits.Load() != 1 {
		t.Fatalf("stop %q, streamed %q, %d requests", msg.StopReason, got.String(), hits.Load())
	}
}

// The wait for the headers is not the idle watch's (thinking models can
// take long before them), and gaps shorter than the timeout pass.
func TestIdleWatchStartsWithTheBody(t *testing.T) {
	withIdleTimeout(t, 150*time.Millisecond)
	noRetryWait(t)
	srv, hits := silentServer(t, func(n int32, w http.ResponseWriter, hang func()) {
		time.Sleep(400 * time.Millisecond) // before the headers
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"h\"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(60 * time.Millisecond)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"i\"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(60 * time.Millisecond)
		io.WriteString(w, "data: [DONE]\n\n")
	})
	p := &openaiProvider{name: "oa", base: srv.URL}
	msg, err := p.Stream(context.Background(), prompt("s"), nil)
	if err != nil || msg.Blocks[0].Text != "hi" || hits.Load() != 1 {
		t.Fatalf("msg %+v, err %v, %d requests", msg, err, hits.Load())
	}
}

// Only time spent in Read counts: a caller that is slow to read again is
// not cut off, a Read that waits too long is.
func TestIdleBodyCountsOnlyTimeInRead(t *testing.T) {
	pr, pw := io.Pipe()
	var once sync.Once
	var cancels atomic.Int32
	// As the transport does on a cancelled request: the blocked Read ends.
	cancel := func() { once.Do(func() { pr.CloseWithError(context.Canceled) }); cancels.Add(1) }
	b := watchIdle(pr, cancel, 50*time.Millisecond)
	buf := make([]byte, 16)
	go pw.Write([]byte("abc"))
	if n, err := b.Read(buf); err != nil || string(buf[:n]) != "abc" {
		t.Fatalf("read %q, %v", buf[:n], err)
	}
	time.Sleep(200 * time.Millisecond) // busy elsewhere, not reading
	go pw.Write([]byte("def"))
	if n, err := b.Read(buf); err != nil || string(buf[:n]) != "def" {
		t.Fatalf("after a pause between reads: %q, %v", buf[:n], err)
	}
	start := time.Now()
	_, err := b.Read(buf) // nothing comes
	if !errors.Is(err, ErrStreamIdle) || time.Since(start) < 50*time.Millisecond {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	if _, err := b.Read(buf); !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("read after expiry: %v", err)
	}
	b.Close()
	b.Close()
	if n := cancels.Load(); n != 2 {
		t.Fatalf("cancel called %d times, want on expiry and on close", n)
	}
}

func TestOverloadRetriesBackOffLonger(t *testing.T) {
	waits := noRetryWait(t)
	tries := 0
	overloaded := func(ctx context.Context, emit func(Event)) (*Message, error) {
		tries++
		return nil, &APIError{Status: 529, Body: "overloaded_error: Overloaded"}
	}
	if _, err := streamWithRetry(context.Background(), "p", &Request{Model: "m"}, nil, overloaded); err == nil || tries != maxOverloadRetries+1 {
		t.Fatalf("tries %d, err %v", tries, err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second}
	if len(*waits) != len(want) {
		t.Fatalf("waits %v", *waits)
	}
	var total time.Duration
	for i, w := range want {
		if d := (*waits)[i]; d < w || d >= w+500*time.Millisecond {
			t.Errorf("wait %d = %v, want %v plus jitter", i, d, w)
		}
		total += (*waits)[i]
	}
	if total < 25*time.Second || total > 30*time.Second {
		t.Errorf("waited %v in all", total)
	}

	// A spike that passes is ridden out; a Retry-After is taken as given.
	*waits, tries = nil, 0
	msg, err := streamWithRetry(context.Background(), "p", &Request{Model: "m"}, nil, func(ctx context.Context, emit func(Event)) (*Message, error) {
		if tries++; tries < 4 {
			return nil, &APIError{Status: 503, RetryAfter: 3 * time.Second}
		}
		return &Message{Role: RoleAssistant, StopReason: "end_turn"}, nil
	})
	if err != nil || msg.StopReason != "end_turn" || fmt.Sprint(*waits) != "[3s 3s 3s]" {
		t.Fatalf("err %v, waits %v", err, *waits)
	}

	// The waits of one request are bounded together, and each long one is
	// told to whoever asked for it.
	*waits, tries = nil, 0
	var notes []string
	ctx := WithRetryNotice(context.Background(), func(s string) { notes = append(notes, s) })
	_, err = streamWithRetry(ctx, "anthropic", &Request{Model: "m"}, nil, func(ctx context.Context, emit func(Event)) (*Message, error) {
		tries++
		return nil, &APIError{Provider: "anthropic", Status: 529, RetryAfter: 40 * time.Second,
			Body: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}
	})
	if err == nil || tries != 3 || fmt.Sprint(*waits) != "[40s 40s]" {
		t.Fatalf("tries %d, waits %v, err %v", tries, *waits, err)
	}
	if len(notes) != 2 || notes[0] != "anthropic: HTTP 529 Overloaded; trying again in 40s (retry 1)" {
		t.Fatalf("notes %q", notes)
	}

	// Rate limits and transport errors keep the shorter budget; a silent
	// stream gets one more try.
	for _, c := range []struct {
		err  error
		want int
	}{
		{&APIError{Status: 429, RetryAfter: time.Millisecond}, maxRetries + 1},
		{&streamError{io.ErrUnexpectedEOF}, maxRetries + 1},
		{&streamError{fmt.Errorf("%w: no data", ErrStreamIdle)}, maxIdleRetries + 1},
		{&APIError{Status: 400}, 1},
	} {
		tries = 0
		streamWithRetry(context.Background(), "p", &Request{Model: "m"}, nil, func(ctx context.Context, emit func(Event)) (*Message, error) {
			tries++
			return nil, c.err
		})
		if tries != c.want {
			t.Errorf("%v: %d tries, want %d", c.err, tries, c.want)
		}
	}
}

// Calls streamed without ids get random ones: an index would repeat in
// the next turn. The events carry the same id as the call.
func TestOpenAICallsWithoutIDsGetUniqueOnes(t *testing.T) {
	stream := sse(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read","arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"name":"ls","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`)
	seen := map[string]bool{}
	for turn := range 2 {
		var evIDs []string
		msg, err := parseOpenAIStream(context.Background(), "oa", strings.NewReader(stream), func(e Event) {
			if e.Type == EvToolCall {
				evIDs = append(evIDs, e.ToolID)
			}
		})
		if err != nil || len(msg.Blocks) != 2 || msg.StopReason != "tool_use" {
			t.Fatalf("turn %d: msg %+v, err %v", turn, msg, err)
		}
		for i, b := range msg.Blocks {
			if !strings.HasPrefix(b.ID, "call_") || len(b.ID) < 20 || seen[b.ID] {
				t.Fatalf("turn %d: call id %q (seen %v)", turn, b.ID, seen)
			}
			seen[b.ID] = true
			if i >= len(evIDs) || evIDs[i] != b.ID {
				t.Fatalf("turn %d: event ids %v, call ids %q", turn, evIDs, b.ID)
			}
		}
	}
	// An id the server sends is kept.
	msg, err := parseOpenAIStream(context.Background(), "oa", strings.NewReader(sse(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_srv","function":{"name":"ls","arguments":"{}"}}]}}]}`,
		`[DONE]`)), func(Event) {})
	if err != nil || msg.Blocks[0].ID != "call_srv" {
		t.Fatalf("server id: %+v, %v", msg.Blocks, err)
	}
}

func TestAnthropicRefusalStopReason(t *testing.T) {
	stream := sse(
		`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I can"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`)
	msg, err := parseAnthropicStream(context.Background(), strings.NewReader(stream), false, func(Event) {}, nil)
	if err != nil || msg.StopReason != StopRefusal || msg.Blocks[0].Text != "I can" {
		t.Fatalf("msg %+v, err %v", msg, err)
	}
	for in, want := range map[string]string{"end_turn": "end_turn", "tool_use": "tool_use", "max_tokens": "max_tokens",
		"model_context_window_exceeded": StopContextFull, "refusal": StopRefusal, "pause_turn": "end_turn"} {
		if got := anthropicStopReason(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}
