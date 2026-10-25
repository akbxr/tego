// Package lsp runs language servers to report the errors an edit leaves in
// a file: types, imports, undefined names, beyond what a syntax check sees.
// Only what diagnostics need is implemented: no completion, hover or
// navigation. It uses no dependencies.
package lsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// conn speaks JSON-RPC 2.0 with LSP's Content-Length framing.
type conn struct {
	w   io.Writer
	wmu sync.Mutex

	mu      sync.Mutex
	nextID  int
	pending map[int]chan response
	closed  error // set when the reader stops; pending calls fail with it

	// notify handles a notification; request answers a request from the
	// server. Both run on the reader goroutine.
	notify  func(method string, params json.RawMessage)
	request func(method string, params json.RawMessage) any
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (%d)", e.Message, e.Code) }

type response struct {
	result json.RawMessage
	err    error
}

func newConn(w io.Writer) *conn {
	return &conn{w: w, pending: map[int]chan response{}}
}

// call sends a request and returns a channel that gets its response.
func (c *conn) call(method string, params any) (<-chan response, error) {
	ch := make(chan response, 1)
	c.mu.Lock()
	if c.closed != nil {
		c.mu.Unlock()
		return nil, c.closed
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	return ch, nil
}

func (c *conn) notifyServer(method string, params any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *conn) send(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.w.Write(body)
	return err
}

// read handles messages from r until it ends, then fails pending calls.
func (c *conn) read(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	var err error
	for {
		var body []byte
		if body, err = readMessage(br); err != nil {
			break
		}
		var m message
		if json.Unmarshal(body, &m) != nil {
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0: // a request from the server
			var result any
			if c.request != nil {
				result = c.request(m.Method, m.Params)
			}
			c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		case m.Method != "":
			if c.notify != nil {
				c.notify(m.Method, m.Params)
			}
		default:
			id, perr := strconv.Atoi(string(m.ID))
			if perr != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				if m.Error != nil {
					ch <- response{err: m.Error}
				} else {
					ch <- response{result: m.Result}
				}
			}
		}
	}
	if errors.Is(err, io.EOF) {
		err = errors.New("language server exited")
	}
	c.mu.Lock()
	c.closed = err
	for id, ch := range c.pending {
		ch <- response{err: err}
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

// readMessage reads one framed message body.
func readMessage(br *bufio.Reader) ([]byte, error) {
	n := -1
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if n < 0 {
				continue // stray blank line before the headers
			}
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			if n, err = strconv.Atoi(strings.TrimSpace(v)); err != nil || n < 0 {
				return nil, fmt.Errorf("bad Content-Length %q", v)
			}
		}
	}
	body := make([]byte, n)
	_, err := io.ReadFull(br, body)
	return body, err
}
