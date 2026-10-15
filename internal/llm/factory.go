package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// New builds a Provider for cfg.Kind ("anthropic", "openai", "openai-codex",
// "google-antigravity").
// The API key is cfg.APIKey, else the value of the cfg.KeyEnv environment
// variable. OAuth-backed configs (cfg.OAuth != nil) do not need a key.
func New(cfg ProviderConfig) (Provider, error) {
	key := cfg.APIKey
	if key == "" && cfg.KeyEnv != "" {
		key = os.Getenv(cfg.KeyEnv)
	}
	name := cfg.Name
	if name == "" {
		name = cfg.Kind
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	switch cfg.Kind {
	case "anthropic":
		if key == "" && cfg.OAuth == nil {
			return nil, fmt.Errorf("provider %s: no API key (set %s or log in)", name, keyHint(cfg))
		}
		if base == "" {
			base = "https://api.anthropic.com"
		}
		return &anthropicProvider{name: name, base: base, key: key, headers: cfg.Headers, oauth: cfg.OAuth, deviceID: cfg.DeviceID}, nil
	case "openai":
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		return &openaiProvider{name: name, base: base, key: key, headers: cfg.Headers, oauth: cfg.OAuth}, nil
	case "openai-codex":
		if cfg.OAuth == nil {
			return nil, fmt.Errorf("provider %s: openai-codex requires an OAuth login", name)
		}
		return &codexProvider{name: name, url: codexURL(base), headers: cfg.Headers, oauth: cfg.OAuth}, nil
	case AntigravityKind:
		if cfg.OAuth == nil || cfg.ProjectID == "" {
			return nil, fmt.Errorf("provider %s: google-antigravity requires `tego login google-antigravity`", name)
		}
		return newAntigravityProvider(name, base, cfg.ProjectID, cfg.Headers, cfg.OAuth), nil
	default:
		return nil, fmt.Errorf("provider %s: unknown kind %q", name, cfg.Kind)
	}
}

func keyHint(cfg ProviderConfig) string {
	if cfg.KeyEnv != "" {
		return cfg.KeyEnv
	}
	return "api_key"
}

// httpClient is shared by all providers. There is no overall timeout because
// streams can legitimately run for many minutes; connection setup and the
// wait for response headers are bounded instead.
var httpClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 3 * time.Minute,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
	},
}

// APIError is an HTTP or in-stream error reported by a provider.
type APIError struct {
	Provider   string
	Status     int    // HTTP status (in-stream errors are mapped to an equivalent status)
	Body       string // response body snippet / error message
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.Status, e.Body)
}

func (e *APIError) retryable() bool {
	switch {
	case e.Status == 429:
		b := strings.ToLower(e.Body)
		return !strings.Contains(b, "quota") && !strings.Contains(b, "usage_limit")
	case e.Status == 408, e.Status == 409, e.Status >= 500:
		return true
	}
	return false
}

const maxBodySnippet = 2000

// newAPIError drains (a bounded prefix of) resp.Body into an APIError.
func newAPIError(provider string, resp *http.Response) *APIError {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodySnippet))
	resp.Body.Close()
	return &APIError{
		Provider:   provider,
		Status:     resp.StatusCode,
		Body:       strings.TrimSpace(string(b)),
		RetryAfter: parseRetryAfter(resp.Header),
	}
}

func parseRetryAfter(h http.Header) time.Duration {
	if v := h.Get("retry-after-ms"); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms >= 0 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	v := h.Get("retry-after")
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil && s >= 0 {
		return time.Duration(s * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// streamError marks a transport failure while reading an in-flight stream.
type streamError struct{ err error }

func (e *streamError) Error() string { return "stream interrupted: " + e.err.Error() }
func (e *streamError) Unwrap() error { return e.err }

const (
	maxRetries    = 3
	maxRetryAfter = 60 * time.Second
)

// attemptFunc performs one request+stream. emit must be used for all events
// so the retry loop knows whether anything reached the caller.
type attemptFunc func(ctx context.Context, emit func(Event)) (*Message, error)

// RequestStat describes one finished model request (after retries), for
// usage statistics.
type RequestStat struct {
	Provider, Model string
	Class           string // Request.Class ("" = main)
	SessionID       string
	LongCache       bool // cache writes used the 1-hour TTL
	Start           time.Time
	Duration        time.Duration // of the last attempt
	TTFT            time.Duration // until the first streamed event; 0 if none
	// GenStart is when generation began (EvStarted, else the first event);
	// Duration-GenStart is the generation window that output speed is
	// measured over. 0 if unknown.
	GenStart   time.Duration
	GenOutput  int // the output generated in that window (0 = Usage.Output)
	Usage      Usage
	StopReason string
	Err        string   // "" on success
	Tools      []string // tool calls the answer made
}

var requestHook atomic.Pointer[func(RequestStat)]

// SetRequestHook makes every provider report each finished request to fn
// (nil stops it). fn runs on the requesting goroutine and must be quick.
func SetRequestHook(fn func(RequestStat)) {
	if fn == nil {
		requestHook.Store(nil)
		return
	}
	requestHook.Store(&fn)
}

func reportRequest(provider string, req *Request, start, first, started time.Time, msg *Message, err error) {
	hook := requestHook.Load()
	if hook == nil {
		return
	}
	st := RequestStat{Provider: provider, Model: req.Model, Class: req.Class, SessionID: req.SessionID, LongCache: req.LongCache,
		Start: start, Duration: time.Since(start), StopReason: msg.StopReason}
	if !first.IsZero() {
		st.TTFT = first.Sub(start)
	}
	if started.IsZero() || (!first.IsZero() && first.Before(started)) {
		started = first
	}
	if !started.IsZero() {
		st.GenStart = started.Sub(start)
	}
	if msg.Usage != nil {
		st.Usage = *msg.Usage
	}
	st.GenOutput = msg.GenOutput
	if err != nil && msg.StopReason != "aborted" {
		st.Err = err.Error()
	}
	for _, b := range msg.Blocks {
		if b.Type == BlockToolUse {
			st.Tools = append(st.Tools, b.Name)
		}
	}
	(*hook)(st)
}

// streamWithRetry runs attempt, retrying transient failures (429/5xx/529,
// network errors) up to maxRetries times as long as no event has been
// delivered to onEvent yet. It normalises cancellation to StopReason
// "aborted" + ctx.Err() and other failures to StopReason "error", and
// reports the request to the stats hook.
func streamWithRetry(ctx context.Context, provider string, req *Request, onEvent func(Event), attempt attemptFunc) (msg *Message, err error) {
	delivered := false
	var start, first, started time.Time
	emit := func(e Event) {
		if e.Type == EvStarted {
			if started.IsZero() {
				started = time.Now()
			}
			return
		}
		if !delivered {
			first = time.Now()
		}
		delivered = true
		if onEvent != nil {
			onEvent(e)
		}
	}
	defer func() { reportRequest(provider, req, start, first, started, msg, err) }()
	for try := 0; ; try++ {
		start, started = time.Now(), time.Time{}
		msg, err = attempt(ctx, emit)
		if msg == nil {
			msg = &Message{Role: RoleAssistant}
		}
		msg.Provider, msg.Model = provider, req.Model
		if err == nil {
			return msg, nil
		}
		if ctx.Err() != nil {
			msg.StopReason = "aborted"
			return msg, ctx.Err()
		}
		delay, ok := retryDelay(err, try)
		if ok && failFastLimits(ctx) && IsAccountLimit(err) {
			ok = false // an account pool switches accounts instead of waiting
		}
		if delivered || !ok {
			msg.StopReason = "error"
			return msg, err
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			msg.StopReason = "aborted"
			return msg, ctx.Err()
		case <-t.C:
		}
	}
}

func retryDelay(err error, try int) (time.Duration, bool) {
	if try >= maxRetries {
		return 0, false
	}
	backoff := time.Duration(1<<try)*time.Second + time.Duration(rand.Int64N(int64(500*time.Millisecond)))
	var ae *APIError
	if errors.As(err, &ae) {
		if !ae.retryable() {
			return 0, false
		}
		if ae.RetryAfter > maxRetryAfter {
			return 0, false
		}
		if ae.RetryAfter > 0 {
			return ae.RetryAfter, true
		}
		return backoff, true
	}
	var se *streamError
	var ue *url.Error
	var ne net.Error
	if errors.As(err, &se) || errors.As(err, &ue) || errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF) {
		return backoff, true
	}
	return 0, false
}

// postJSON sends body to url and returns the response if it is 2xx; other
// statuses are converted to *APIError.
func postJSON(ctx context.Context, provider, endpoint string, body []byte, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = header
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, newAPIError(provider, resp)
	}
	return resp, nil
}

func setHeaders(h http.Header, extra map[string]string) {
	for k, v := range extra {
		h.Set(k, v)
	}
}
