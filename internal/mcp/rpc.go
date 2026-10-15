package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const protocolVersion = "2025-06-18"

var errClosed = errors.New("connection closed")

// transport carries JSON-RPC messages. Incoming messages are fed to
// conn.handle by the transport itself.
type transport interface {
	// send delivers one message. id is the request id (0 for notifications
	// and responses). Transports that receive the response on the same
	// exchange (streamable HTTP) block until it has been dispatched.
	send(ctx context.Context, msg []byte, id int64) error
	close()
	kind() string
}

// initHook is implemented by transports that need the negotiated protocol
// version after initialization (streamable HTTP).
type initHook interface {
	initialized(version string)
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type outRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type outNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type outResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type reply struct {
	msg *message
	err error
}

// conn is a JSON-RPC 2.0 client over a transport.
type conn struct {
	t      transport
	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan reply
	err     error // set once the connection is dead

	// serve answers server->client requests; notify receives notifications.
	// Both are set before the transport starts and never change.
	serve  func(method string, params json.RawMessage) (any, *rpcError)
	notify func(method string, params json.RawMessage)
	onFail func(err error)
}

func newConn() *conn {
	return &conn{pending: map[int64]chan reply{}}
}

// call sends a request and decodes the result into result (if non-nil).
// On context cancellation it sends notifications/cancelled and returns.
func (c *conn) call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	b, err := json.Marshal(outRequest{"2.0", id, method, params})
	if err != nil {
		return err
	}
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.t.send(ctx, b, id); err != nil {
		c.drop(id)
		if ctx.Err() != nil {
			c.cancelled(id, method, ctx.Err())
			return ctx.Err()
		}
		return err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if r.msg.Error != nil {
			return r.msg.Error
		}
		if result != nil && len(r.msg.Result) > 0 {
			return json.Unmarshal(r.msg.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.drop(id)
		c.cancelled(id, method, ctx.Err())
		return ctx.Err()
	}
}

// cancelled tells the server, best-effort and asynchronously, that request
// id is no longer wanted.
func (c *conn) cancelled(id int64, method string, cause error) {
	if method == "initialize" || c.dead() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.notifyServer(ctx, "notifications/cancelled", map[string]any{
			"requestId": id,
			"reason":    cause.Error(),
		})
	}()
}

func (c *conn) notifyServer(ctx context.Context, method string, params any) error {
	b, err := json.Marshal(outNotification{"2.0", method, params})
	if err != nil {
		return err
	}
	return c.t.send(ctx, b, 0)
}

func (c *conn) drop(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *conn) hasPending(id int64) bool {
	c.mu.Lock()
	_, ok := c.pending[id]
	c.mu.Unlock()
	return ok
}

func (c *conn) dead() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err != nil
}

// fail marks the connection dead and fails every in-flight request. Only the
// first call has an effect.
func (c *conn) fail(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	for id, ch := range c.pending {
		ch <- reply{err: err}
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if c.onFail != nil {
		c.onFail(err)
	}
}

// close shuts the transport down and fails pending requests.
func (c *conn) close() {
	c.mu.Lock()
	if c.err == nil {
		c.err = errClosed
		for id, ch := range c.pending {
			ch <- reply{err: errClosed}
			delete(c.pending, id)
		}
	}
	c.mu.Unlock()
	if c.t != nil {
		c.t.close()
	}
}

// handle dispatches one incoming message or batch. data is not retained.
func (c *conn) handle(data []byte) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return
	}
	if data[0] == '[' {
		var batch []json.RawMessage
		if json.Unmarshal(data, &batch) != nil {
			return
		}
		for _, m := range batch {
			c.handleOne(m)
		}
		return
	}
	c.handleOne(data)
}

func (c *conn) handleOne(data []byte) {
	var m message
	if json.Unmarshal(data, &m) != nil {
		return
	}
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	switch {
	case m.Method != "" && hasID:
		go c.answer(&m)
	case m.Method != "":
		if c.notify != nil {
			c.notify(m.Method, m.Params)
		}
	case hasID:
		id, ok := parseID(m.ID)
		if !ok {
			return
		}
		c.mu.Lock()
		ch, ok := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ok {
			ch <- reply{msg: &m}
		}
	}
}

func parseID(raw json.RawMessage) (int64, bool) {
	s := string(raw)
	if len(s) >= 2 && s[0] == '"' {
		s = s[1 : len(s)-1]
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil
}

// answer responds to a server->client request.
func (c *conn) answer(m *message) {
	var (
		result any
		rerr   *rpcError
	)
	if c.serve != nil {
		result, rerr = c.serve(m.Method, m.Params)
	} else {
		rerr = &rpcError{Code: -32601, Message: "method not found: " + m.Method}
	}
	resp := outResponse{JSONRPC: "2.0", ID: m.ID, Error: rerr}
	if rerr == nil {
		if result == nil {
			result = struct{}{}
		}
		resp.Result = result
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = c.t.send(ctx, b, 0)
}

type initResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	Capabilities    struct {
		Tools *struct {
			ListChanged bool `json:"listChanged"`
		} `json:"tools"`
	} `json:"capabilities"`
	ServerInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
	Instructions string `json:"instructions"`
}

// initialize performs the MCP handshake.
func (c *conn) initialize(ctx context.Context) (*initResult, error) {
	var r initResult
	err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{"roots": map[string]any{"listChanged": false}},
		"clientInfo":      map[string]any{"name": "tego", "version": "0.1.0"},
	}, &r)
	if err != nil {
		return nil, err
	}
	if r.ProtocolVersion == "" {
		r.ProtocolVersion = protocolVersion
	}
	if h, ok := c.t.(initHook); ok {
		h.initialized(r.ProtocolVersion)
	}
	if err := c.notifyServer(ctx, "notifications/initialized", nil); err != nil {
		return nil, err
	}
	return &r, nil
}

type remoteTool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations struct {
		ReadOnlyHint bool `json:"readOnlyHint"`
	} `json:"annotations"`
}

// listTools fetches every page of tools/list.
func (c *conn) listTools(ctx context.Context) ([]remoteTool, error) {
	var (
		all    []remoteTool
		cursor string
	)
	for range 1000 {
		var params any
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}
		var r struct {
			Tools      []remoteTool `json:"tools"`
			NextCursor string       `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &r); err != nil {
			return nil, err
		}
		all = append(all, r.Tools...)
		if r.NextCursor == "" || r.NextCursor == cursor {
			break
		}
		cursor = r.NextCursor
	}
	return all, nil
}
