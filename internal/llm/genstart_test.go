package llm

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestStreamWithRetryTimesGeneration(t *testing.T) {
	var got RequestStat
	SetRequestHook(func(st RequestStat) { got = st })
	defer SetRequestHook(nil)

	var forwarded []Event
	tries := 0
	msg, err := streamWithRetry(context.Background(), "p", &Request{Model: "m"}, func(e Event) { forwarded = append(forwarded, e) },
		func(ctx context.Context, emit func(Event)) (*Message, error) {
			tries++
			emit(Event{Type: EvStarted})
			if tries == 1 {
				// A stream that started and then broke must still be retried:
				// EvStarted is not output the user has seen.
				return nil, &streamError{io.ErrUnexpectedEOF}
			}
			time.Sleep(30 * time.Millisecond) // thinking before the first visible delta
			emit(Event{Type: EvText, Text: "hi"})
			return &Message{Role: RoleAssistant, StopReason: "end_turn", Usage: &Usage{Output: 3}}, nil
		})
	if err != nil || msg.StopReason != "end_turn" || tries != 2 {
		t.Fatalf("err %v, stop %q, tries %d", err, msg.StopReason, tries)
	}
	if len(forwarded) != 1 || forwarded[0].Type != EvText {
		t.Fatalf("forwarded %+v", forwarded)
	}
	if got.GenStart <= 0 || got.TTFT-got.GenStart < 25*time.Millisecond || got.Duration < got.TTFT {
		t.Fatalf("gen start %v, ttft %v, duration %v", got.GenStart, got.TTFT, got.Duration)
	}
}

func TestGenStartFallsBackToFirstEvent(t *testing.T) {
	var got RequestStat
	SetRequestHook(func(st RequestStat) { got = st })
	defer SetRequestHook(nil)
	_, _ = streamWithRetry(context.Background(), "p", &Request{Model: "m"}, nil,
		func(ctx context.Context, emit func(Event)) (*Message, error) {
			time.Sleep(10 * time.Millisecond)
			emit(Event{Type: EvText, Text: "x"}) // a provider without a start signal
			return &Message{Role: RoleAssistant}, nil
		})
	if got.GenStart == 0 || got.GenStart != got.TTFT {
		t.Fatalf("gen start %v, ttft %v", got.GenStart, got.TTFT)
	}
}
