package llm

import (
	"bytes"
	"context"
	"encoding/json"
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
	"sync"
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
			base = anthropicAPIBase
		}
		return &anthropicProvider{name: name, base: base, key: key, headers: cfg.Headers, oauth: cfg.OAuth, deviceID: cfg.DeviceID, onLimits: cfg.OnLimits}, nil
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
// streams can legitimately run for many minutes; connection setup, the wait
// for response headers and (in post) silences within a response body are
// bounded instead.
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
	Header     http.Header // the response's headers, when it came over HTTP
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
		Header:     resp.Header,
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

// ErrStreamIdle is in the error of a response that went silent: no byte
// arrived for the idle timeout (SetStreamIdleTimeout) while it was read, so
// the attempt was abandoned. The stream parsers wrap it in a streamError,
// which is retried when nothing had reached the caller yet.
var ErrStreamIdle = errors.New("idle stream timeout")

// DefaultStreamIdle is how long a response body may go silent unless the
// config (stream_idle_timeout) says otherwise. Silences are legitimate:
// some backends send their headers at once and then think in silence,
// reasoning summaries come a block at a time, and Claude holds back the
// input of a tool other than write and edit until each parameter is
// complete (a long bash heredoc). Five minutes, as Codex CLI waits, covers
// those, and still ends a connection that will never send another byte.
const DefaultStreamIdle = 5 * time.Minute

const defaultStreamIdle = DefaultStreamIdle

var streamIdle atomic.Int64 // a time.Duration

func init() { streamIdle.Store(int64(defaultStreamIdle)) }

// SetStreamIdleTimeout sets how long a response body may stay silent while
// it is read before the request is abandoned; d <= 0 turns the watch off.
// The wait for the headers is not counted: ResponseHeaderTimeout bounds it.
func SetStreamIdleTimeout(d time.Duration) { streamIdle.Store(int64(d)) }

func streamIdleTimeout() time.Duration { return time.Duration(streamIdle.Load()) }

// idleBody watches a response body: when a Read has waited idle for data,
// it cancels the request (which ends the blocked Read on HTTP/1 and HTTP/2)
// and reports ErrStreamIdle. Only time inside Read counts, so a caller busy
// with what it read is never cut off. Close also releases the request's
// context.
type idleBody struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
	idle   time.Duration
	timer  *time.Timer // nil: not watched
	once   sync.Once

	mu      sync.Mutex
	reading bool
	since   time.Time // when the Read in progress began
	expired bool
	closed  bool
}

func watchIdle(rc io.ReadCloser, cancel context.CancelFunc, idle time.Duration) *idleBody {
	b := &idleBody{rc: rc, cancel: cancel, idle: idle}
	if idle > 0 {
		b.timer = time.AfterFunc(idle, b.check)
	}
	return b
}

// check runs on the timer: it expires the body when the Read in progress
// has waited idle, else sets the timer for when it would have.
func (b *idleBody) check() {
	b.mu.Lock()
	if b.expired || b.closed {
		b.mu.Unlock()
		return
	}
	next := b.idle
	if b.reading {
		waited := time.Since(b.since)
		b.expired = waited >= b.idle
		next = b.idle - waited
	}
	if !b.expired {
		b.timer.Reset(next)
	}
	expired := b.expired
	b.mu.Unlock()
	if expired {
		b.cancel()
	}
}

func (b *idleBody) idleErr() error {
	return fmt.Errorf("%w: no data from the server for %s", ErrStreamIdle, b.idle)
}

func (b *idleBody) Read(p []byte) (int, error) {
	if b.timer == nil {
		return b.rc.Read(p)
	}
	b.mu.Lock()
	if b.expired {
		b.mu.Unlock()
		return 0, b.idleErr()
	}
	b.reading, b.since = true, time.Now()
	b.mu.Unlock()
	n, err := b.rc.Read(p)
	b.mu.Lock()
	b.reading = false
	expired := b.expired
	b.mu.Unlock()
	if expired && err != nil {
		err = b.idleErr() // not the cancellation it caused
	}
	return n, err
}

func (b *idleBody) Close() error {
	var err error
	b.once.Do(func() {
		if b.timer != nil {
			b.mu.Lock()
			b.closed = true
			b.timer.Stop()
			b.mu.Unlock()
		}
		err = b.rc.Close()
		b.cancel()
	})
	return err
}

const (
	// maxRetries bounds the retries of 429s, 408/409 and transport errors.
	maxRetries = 3
	// maxOverloadRetries bounds the retries of 5xx and 529 (overloaded): a
	// spike of those can outlast a few seconds. With backoff's waits that
	// is about 25 seconds in all.
	maxOverloadRetries = 5
	// maxIdleRetries: a server that went silent gets one more try, as each
	// such try can take minutes.
	maxIdleRetries = 1
	maxBackoff     = 10 * time.Second
	maxRetryAfter  = 60 * time.Second
	// maxRetryWait bounds the waits of one request's retries together: a
	// server asking for a minute each time would otherwise hold the turn for
	// five.
	maxRetryWait = 90 * time.Second
	// retryNoticeFrom: a wait this long or longer is told (WithRetryNotice).
	retryNoticeFrom = 2 * time.Second
)

type retryNoticeKey struct{}

// WithRetryNotice makes the requests made with ctx tell fn before each
// wait of retryNoticeFrom or longer for a retry: what failed, and when the
// request is tried again. The wait is otherwise silent.
func WithRetryNotice(ctx context.Context, fn func(msg string)) context.Context {
	return context.WithValue(ctx, retryNoticeKey{}, fn)
}

func retryNotice(ctx context.Context, provider string, err error, delay time.Duration, try int) {
	fn, _ := ctx.Value(retryNoticeKey{}).(func(string))
	if fn == nil || delay < retryNoticeFrom {
		return
	}
	fn(fmt.Sprintf("%s: %s; trying again in %s (retry %d)", provider, retryReason(err, provider), delay.Round(time.Second), try+1))
}

// retryReason says briefly what failed: the status and the message of an
// API error's JSON body ({"error": {"message": …}} on every provider), else
// the error's first line.
func retryReason(err error, provider string) string {
	var ae *APIError
	if errors.As(err, &ae) {
		s := fmt.Sprintf("HTTP %d", ae.Status)
		var body struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(ae.Body), &body) == nil && body.Error.Message != "" {
			s += " " + truncate(firstLine(body.Error.Message), 80)
		}
		return s
	}
	return truncate(firstLine(strings.TrimPrefix(err.Error(), provider+": ")), 100)
}

// sleepCtx waits d, or until ctx ends (returning its error). Tests replace
// it to skip the waits.
var sleepCtx = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

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
	// WebSearches is the number of searches a native web search ran; they
	// are billed per search on top of the tokens.
	WebSearches int
	// CacheMiss is what the provider said broke the prompt cache (nil =
	// nothing, or it does not say).
	CacheMiss *CacheMiss
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
	st.GenOutput, st.CacheMiss = msg.GenOutput, msg.CacheMiss
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

// reportWebSearch reports a native web search (WebSearcher) to the stats
// hook: a side request, with usage u and the number of searches it ran.
// It is not streamed, so it has no time to first token and no output speed.
func reportWebSearch(provider, model string, start time.Time, u Usage, searches int, err error) {
	hook := requestHook.Load()
	if hook == nil {
		return
	}
	st := RequestStat{Provider: provider, Model: model, Class: ClassAuxiliary, Start: start, Duration: time.Since(start),
		Usage: u, StopReason: "end_turn", WebSearches: searches}
	if err != nil {
		st.StopReason, st.Err = "error", err.Error()
		if errors.Is(err, context.Canceled) {
			st.StopReason, st.Err = "aborted", ""
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
	var waited time.Duration
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
		if delivered || !ok || waited+delay > maxRetryWait {
			msg.StopReason = "error"
			return msg, err
		}
		waited += delay
		retryNotice(ctx, provider, err, delay, try)
		if sleepCtx(ctx, delay) != nil {
			msg.StopReason = "aborted"
			return msg, ctx.Err()
		}
	}
}

// backoff is the wait before retry try+1 when the server named none: 1, 2,
// 4, 8, then 10 seconds, plus up to half a second of jitter.
func backoff(try int) time.Duration {
	d := maxBackoff
	if try < 4 {
		d = min(time.Duration(1<<try)*time.Second, maxBackoff)
	}
	return d + time.Duration(rand.Int64N(int64(500*time.Millisecond)))
}

// retryDelay reports whether a request that failed with err after try
// retries is tried again, and after how long.
func retryDelay(err error, try int) (time.Duration, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		limit := maxRetries
		if ae.Status >= 500 {
			limit = maxOverloadRetries
		}
		if try >= limit || !ae.retryable() || ae.RetryAfter > maxRetryAfter {
			return 0, false
		}
		if ae.RetryAfter > 0 {
			return ae.RetryAfter, true
		}
		return backoff(try), true
	}
	if try >= maxRetries || errors.Is(err, ErrStreamIdle) && try >= maxIdleRetries {
		return 0, false
	}
	var se *streamError
	var ue *url.Error
	var ne net.Error
	if errors.As(err, &se) || errors.As(err, &ue) || errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF) {
		return backoff(try), true
	}
	return 0, false
}

// postJSON sends body to url and returns the response if it is 2xx; other
// statuses are converted to *APIError. A large body goes compressed to the
// hosts known to take that (gzipbody.go).
func postJSON(ctx context.Context, provider, endpoint string, body []byte, header http.Header) (*http.Response, error) {
	if gz := gzipFor(endpoint, body); gz != nil {
		h := header.Clone()
		if h == nil {
			h = http.Header{}
		}
		h.Set("Content-Encoding", "gzip")
		resp, err := post(ctx, provider, endpoint, gz, h)
		var ae *APIError
		if !errors.As(err, &ae) || !gzipRefused(ae) {
			return resp, err
		}
		// Perhaps the compression it refused: try once plain, and if that
		// goes through, keep to plain for this host.
		resp, err = post(ctx, provider, endpoint, body, header)
		if err == nil {
			noGzip(endpoint)
		}
		return resp, err
	}
	return post(ctx, provider, endpoint, body, header)
}

// post sends one request. The body of the response is watched for silence
// (idleBody) once it is read; the wait for the headers before that is
// ResponseHeaderTimeout's.
func post(ctx context.Context, provider, endpoint string, body []byte, header http.Header) (*http.Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header = header
	resp, err := httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = watchIdle(resp.Body, cancel, streamIdleTimeout())
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
