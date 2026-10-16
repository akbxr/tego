// Package acp speaks the Agent Client Protocol (agentclientprotocol.com):
// JSON-RPC 2.0 over stdio, one message per line, between an editor (the
// client) and an agent it runs as a subprocess. Both sides send requests.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// Error is a JSON-RPC error.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// JSON-RPC and ACP error codes.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeAuthRequired   = -32000
	CodeNotFound       = -32002 // resource not found
)

// ErrClosed is returned by calls on a closed connection.
var ErrClosed = errors.New("acp: connection closed")

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Reply is a handler result with work to do once it has been sent, such as
// notifications about the session the reply creates.
type Reply struct {
	Result any
	Then   func()
}

// Handler serves an incoming request (id != nil) or notification. A
// request's result is sent as its response; a returned *Error is sent as
// is, other errors as internal errors. Requests are served concurrently,
// notifications in order on the reading goroutine, so they must be quick.
type Handler func(ctx context.Context, method string, params json.RawMessage, notification bool) (any, error)

// Conn is one side of a connection.
type Conn struct {
	w       io.Writer
	wmu     sync.Mutex
	handler Handler
	next    atomic.Int64
	mu      sync.Mutex
	pending map[string]chan *message
	closed  chan struct{}
	once    sync.Once
}

// NewConn makes a connection writing to w and serving incoming requests
// with h; Serve reads them.
func NewConn(w io.Writer, h Handler) *Conn {
	return &Conn{w: w, handler: h, pending: map[string]chan *message{}, closed: make(chan struct{})}
}

// Serve reads messages from r until it ends or ctx is done. Requests in
// flight then see their context cancelled.
func (c *Conn) Serve(ctx context.Context, r io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.close()
	var wg sync.WaitGroup
	defer wg.Wait()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			c.send(&message{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &Error{Code: CodeParse, Message: "parse error"}})
			continue
		}
		switch {
		case m.Method == "" && m.ID != nil: // a response to one of our requests
			c.mu.Lock()
			ch := c.pending[string(m.ID)]
			delete(c.pending, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- &m
			}
		case m.ID == nil: // a notification
			if c.handler != nil {
				_, _ = c.handler(ctx, m.Method, m.Params, true)
			}
		default:
			wg.Add(1)
			go func(m message) {
				defer wg.Done()
				c.serve(ctx, &m)
			}(m)
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func (c *Conn) serve(ctx context.Context, m *message) {
	var res any
	var err error
	if c.handler == nil {
		err = &Error{Code: CodeMethodNotFound, Message: "method not found: " + m.Method}
	} else {
		res, err = c.handler(ctx, m.Method, m.Params, false)
	}
	var then func()
	if r, ok := res.(Reply); ok {
		res, then = r.Result, r.Then
	}
	out := &message{JSONRPC: "2.0", ID: m.ID}
	if err != nil {
		var e *Error
		if !errors.As(err, &e) {
			e = &Error{Code: CodeInternal, Message: err.Error()}
		}
		out.Error = e
	} else {
		if res == nil {
			res = struct{}{}
		}
		data, merr := json.Marshal(res)
		if merr != nil {
			out.Error = &Error{Code: CodeInternal, Message: merr.Error()}
		} else {
			out.Result = data
		}
	}
	c.send(out)
	if then != nil && out.Error == nil {
		then()
	}
}

func (c *Conn) send(m *message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	_, err = c.w.Write(data)
	return err
}

// Notify sends a notification.
func (c *Conn) Notify(method string, params any) error {
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.send(&message{JSONRPC: "2.0", Method: method, Params: data})
}

// Call sends a request and decodes its result into result (if not nil).
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id := json.RawMessage(strconv.FormatInt(c.next.Add(1), 10))
	ch := make(chan *message, 1)
	c.mu.Lock()
	c.pending[string(id)] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
	}()
	if err := c.send(&message{JSONRPC: "2.0", ID: id, Method: method, Params: data}); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if result != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrClosed
	}
}

func (c *Conn) close() { c.once.Do(func() { close(c.closed) }) }
