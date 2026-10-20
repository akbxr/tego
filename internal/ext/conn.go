package ext

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// The protocol is JSON-RPC 2.0, one message per line, both ways: tego asks
// an extension to initialize, handle events, run its tools and commands;
// the extension asks tego to show things and ask the user.

type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

var errClosed = errors.New("extension closed")

type conn struct {
	w   io.Writer
	wmu sync.Mutex

	mu      sync.Mutex
	next    int64
	pending map[int64]chan *message
	err     error // set once the connection is gone

	// handle answers a request from the extension; notify takes a
	// notification. Both run on their own goroutine.
	handle func(method string, params json.RawMessage) (any, error)
	notify func(method string, params json.RawMessage)
	done   chan struct{} // closed when the read loop ends
}

func newConn(w io.Writer) *conn {
	return &conn{w: w, pending: map[int64]chan *message{}, done: make(chan struct{})}
}

func (c *conn) write(m *message) error {
	m.JSONRPC = "2.0"
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(data, '\n'))
	return err
}

// call sends a request and waits for its answer, or ctx, or the end.
func (c *conn) call(ctx context.Context, method string, params, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.next++
	id := c.next
	ch := make(chan *message, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	idRaw := json.RawMessage(fmt.Sprint(id))
	if err := c.write(&message{ID: &idRaw, Method: method, Params: raw}); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m == nil {
			return c.closedErr()
		}
		if m.Error != nil {
			return m.Error
		}
		if result != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.closedErr()
	}
}

// notifyExt sends a notification (no answer).
func (c *conn) notifyExt(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(&message{Method: method, Params: raw})
}

func (c *conn) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return errClosed
}

// readLoop reads messages until r ends, then fails what is pending.
func (c *conn) readLoop(r io.Reader) {
	br := bufio.NewReader(r)
	var readErr error
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			c.dispatch(line)
		}
		if err != nil {
			readErr = err
			break
		}
	}
	c.mu.Lock()
	if c.err == nil {
		c.err = errClosed
		if readErr != nil && readErr != io.EOF {
			c.err = readErr
		}
	}
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.done)
}

func (c *conn) dispatch(line []byte) {
	var m message
	if json.Unmarshal(line, &m) != nil {
		return // not JSON: stray output
	}
	switch {
	case m.Method != "" && m.ID != nil:
		go func() {
			var res any
			var err error
			if c.handle != nil {
				res, err = c.handle(m.Method, m.Params)
			} else {
				err = fmt.Errorf("method not found: %s", m.Method)
			}
			out := &message{ID: m.ID}
			if err != nil {
				out.Error = &rpcError{Code: -32000, Message: err.Error()}
			} else {
				out.Result, _ = json.Marshal(res)
				if res == nil {
					out.Result = json.RawMessage("{}")
				}
			}
			_ = c.write(out)
		}()
	case m.Method != "":
		if c.notify != nil {
			go c.notify(m.Method, m.Params)
		}
	case m.ID != nil:
		var id int64
		if json.Unmarshal(*m.ID, &id) != nil {
			return
		}
		c.mu.Lock()
		ch := c.pending[id]
		c.mu.Unlock()
		if ch != nil {
			ch <- &m
		}
	}
}
