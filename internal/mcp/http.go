package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var httpClient = &http.Client{} // no timeout: SSE streams are long-lived

// statusError is a non-2xx HTTP response.
type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	switch e.code {
	case http.StatusUnauthorized:
		return "unauthorized (HTTP 401): the server requires authentication; set \"headers\" in mcp.json (e.g. \"Authorization\": \"Bearer ${TOKEN}\")"
	case http.StatusForbidden:
		return "forbidden (HTTP 403): check the credentials in \"headers\"" + bodySuffix(e.body)
	}
	return fmt.Sprintf("HTTP %d%s", e.code, bodySuffix(e.body))
}

func bodySuffix(b string) string {
	b = strings.TrimSpace(b)
	if b == "" {
		return ""
	}
	return ": " + truncate(b, 200)
}

func readStatusError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return &statusError{code: resp.StatusCode, body: string(b)}
}

// fallbackToSSE reports whether an initialize failure on a streamable HTTP
// endpoint suggests a legacy HTTP+SSE server.
func fallbackToSSE(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.code >= 400 && se.code < 500 &&
		se.code != http.StatusUnauthorized && se.code != http.StatusForbidden
}

func isEventStream(resp *http.Response) bool {
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return mt == "text/event-stream"
}

func setHeaders(req *http.Request, headers map[string]string) {
	for k, v := range headers {
		req.Header.Set(k, v)
	}
}

// ---- streamable HTTP ----

type httpTransport struct {
	c       *conn
	url     string
	headers map[string]string

	ctx    context.Context // lives until close; parents the GET stream
	cancel context.CancelFunc

	mu      sync.Mutex
	session string
	version string
}

func newHTTPTransport(c *conn, cfg ServerConfig) *httpTransport {
	ctx, cancel := context.WithCancel(context.Background())
	t := &httpTransport{c: c, url: cfg.URL, headers: cfg.Headers, ctx: ctx, cancel: cancel}
	c.t = t
	return t
}

func (t *httpTransport) kind() string { return "http" }

func (t *httpTransport) sessionHeaders(req *http.Request) {
	setHeaders(req, t.headers)
	t.mu.Lock()
	if t.session != "" {
		req.Header.Set("Mcp-Session-Id", t.session)
	}
	if t.version != "" {
		req.Header.Set("MCP-Protocol-Version", t.version)
	}
	t.mu.Unlock()
}

func (t *httpTransport) send(ctx context.Context, msg []byte, id int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(msg))
	if err != nil {
		return err
	}
	t.sessionHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.session = sid
		t.mu.Unlock()
	}
	if resp.StatusCode == http.StatusNotFound && t.hasSession() {
		return fmt.Errorf("session expired (HTTP 404)")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return readStatusError(resp)
	}
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		if isEventStream(resp) {
			rd := newSSEReader(resp.Body)
			for {
				ev, err := rd.next()
				if err != nil {
					break
				}
				if ev.event == "" || ev.event == "message" {
					t.c.handle(ev.data)
				}
				if id != 0 && !t.c.hasPending(id) {
					break // got our response; don't wait for the server to close
				}
			}
		} else if body, err := io.ReadAll(resp.Body); err == nil {
			t.c.handle(body)
		}
	}
	if id != 0 && t.c.hasPending(id) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("server sent no response (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func (t *httpTransport) hasSession() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.session != ""
}

func (t *httpTransport) initialized(version string) {
	t.mu.Lock()
	t.version = version
	t.mu.Unlock()
	go t.listen()
}

// listen holds the optional GET stream for server-initiated messages. It is
// best-effort: servers that don't offer one (405) are fine.
func (t *httpTransport) listen() {
	for {
		start := time.Now()
		req, err := http.NewRequestWithContext(t.ctx, http.MethodGet, t.url, nil)
		if err != nil {
			return
		}
		t.sessionHeaders(req)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := httpClient.Do(req)
		if err != nil {
			return
		}
		if resp.StatusCode != http.StatusOK || !isEventStream(resp) {
			resp.Body.Close()
			return
		}
		rd := newSSEReader(resp.Body)
		for {
			ev, err := rd.next()
			if err != nil {
				break
			}
			if ev.event == "" || ev.event == "message" {
				t.c.handle(ev.data)
			}
		}
		resp.Body.Close()
		// Reconnect only streams that stayed up a while, so a server
		// that closes immediately doesn't make us spin.
		if t.ctx.Err() != nil || time.Since(start) < 5*time.Second {
			return
		}
	}
}

func (t *httpTransport) close() {
	t.cancel()
	t.mu.Lock()
	sid := t.session
	t.mu.Unlock()
	if sid == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.url, nil)
	if err != nil {
		return
	}
	t.sessionHeaders(req)
	if resp, err := httpClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

// ---- legacy HTTP+SSE ----

type sseTransport struct {
	c        *conn
	headers  map[string]string
	cancel   context.CancelFunc
	endpoint string // set before ready is closed
	ready    chan struct{}
	dead     chan struct{}
	deadErr  error
}

// dialSSE opens the event stream and waits for the endpoint event.
func dialSSE(ctx context.Context, c *conn, cfg ServerConfig) (*sseTransport, error) {
	base, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(context.Background())
	t := &sseTransport{c: c, headers: cfg.Headers, cancel: cancel,
		ready: make(chan struct{}), dead: make(chan struct{})}
	stop := context.AfterFunc(ctx, cancel)
	defer stop()

	req, err := http.NewRequestWithContext(sctx, http.MethodGet, cfg.URL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	setHeaders(req, cfg.Headers)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := httpClient.Do(req)
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		err := readStatusError(resp)
		resp.Body.Close()
		cancel()
		return nil, err
	}
	c.t = t
	go t.read(resp.Body, base)
	select {
	case <-t.ready:
		return t, nil
	case <-t.dead:
		cancel()
		return nil, t.deadErr
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

func (t *sseTransport) read(body io.ReadCloser, base *url.URL) {
	defer body.Close()
	rd := newSSEReader(body)
	var err error
loop:
	for {
		var ev sseEvent
		if ev, err = rd.next(); err != nil {
			break
		}
		switch ev.event {
		case "endpoint":
			if t.endpoint != "" {
				continue
			}
			ep, perr := base.Parse(strings.TrimSpace(string(ev.data)))
			if perr != nil || ep.Scheme != base.Scheme || ep.Host != base.Host {
				err = fmt.Errorf("invalid endpoint %q", ev.data)
				break loop
			}
			t.endpoint = ep.String()
			close(t.ready)
		case "", "message":
			t.c.handle(ev.data)
		}
	}
	if err == nil || err == io.EOF {
		err = errors.New("event stream closed by server")
	}
	t.deadErr = err
	close(t.dead)
	t.c.fail(err)
}

func (t *sseTransport) kind() string { return "sse" }

func (t *sseTransport) send(ctx context.Context, msg []byte, _ int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(msg))
	if err != nil {
		return err
	}
	setHeaders(req, t.headers)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return readStatusError(resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return nil
}

func (t *sseTransport) close() { t.cancel() }

// ---- SSE reader ----

type sseEvent struct {
	event string
	data  []byte // valid until the next call to next
}

// sseReader is a minimal text/event-stream parser handling `event:` and
// `data:` fields; lines may be arbitrarily long.
type sseReader struct {
	r     *bufio.Reader
	line  []byte
	data  []byte
	event string
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(r, 32<<10)}
}

func (s *sseReader) readLine() ([]byte, error) {
	s.line = s.line[:0]
	for {
		chunk, err := s.r.ReadSlice('\n')
		s.line = append(s.line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil && (err != io.EOF || len(s.line) == 0) {
			return nil, err
		}
		l := bytes.TrimSuffix(s.line, []byte{'\n'})
		return bytes.TrimSuffix(l, []byte{'\r'}), nil
	}
}

func (s *sseReader) next() (sseEvent, error) {
	s.data = s.data[:0]
	s.event = ""
	hasData := false
	for {
		line, err := s.readLine()
		if err != nil {
			if err == io.EOF && hasData {
				return sseEvent{s.event, s.data}, nil
			}
			return sseEvent{}, err
		}
		if len(line) == 0 {
			if hasData {
				return sseEvent{s.event, s.data}, nil
			}
			s.event = ""
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value := line, []byte(nil)
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], line[i+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
		}
		switch string(field) {
		case "data":
			if hasData {
				s.data = append(s.data, '\n')
			}
			s.data = append(s.data, value...)
			hasData = true
		case "event":
			s.event = string(value)
		}
	}
}
