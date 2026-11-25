package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MCP 2026-07-28 ("modern") has no handshake: every request carries the
// protocol version and what the client can do in its _meta, and server/
// discover says what the server can do. tego asks a server for it first and
// falls back to initialize (the protocol up to 2025-11-25) when the server
// doesn't know it. Servers ask the user something by answering a call with
// input_required (the client calls again with the answers), and may run a
// call as a task the client polls (the tasks extension).

const (
	modernVersion = "2026-07-28"
	legacyVersion = "2025-11-25"
	tasksExt      = "io.modelcontextprotocol/tasks"

	metaVersion = "io.modelcontextprotocol/protocolVersion"
	metaCaps    = "io.modelcontextprotocol/clientCapabilities"
	metaClient  = "io.modelcontextprotocol/clientInfo"
)

// discoverWait bounds server/discover before a server that ignores it gets
// initialize; generous, as bunx and uvx may fetch the server first.
var discoverWait = 10 * time.Second

// ClientVersion is the version tego gives servers (set by main).
var ClientVersion = "dev"

// Errors of note: a modern server's headers changed (-32020), and a server
// of 2025-11-25 needs the user to open a page first (-32042).
const (
	codeHeaderMismatch = -32020
	codeURLRequired    = -32042
)

type discoverResult struct {
	SupportedVersions []string `json:"supportedVersions"`
	Capabilities      struct {
		Tools *struct {
			ListChanged bool `json:"listChanged"`
		} `json:"tools"`
		Extensions map[string]json.RawMessage `json:"extensions"`
	} `json:"capabilities"`
	Instructions string `json:"instructions"`
}

// handshake is what a connection learned of its server.
type handshake struct {
	version      string
	instructions string
	listChanged  bool
	tasks        bool // modern, and the server runs calls as tasks
}

// errNoProbe: the server died at server/discover; start it again and
// initialize it.
var errNoProbe = errors.New("server exited at server/discover")

// handshake finds the protocol and performs its handshake: server/discover,
// else initialize. probe false skips discover (a server known to not take
// it). ignored reports a legacy server that let discover time out.
func (c *conn) handshake(ctx context.Context, probe bool) (h *handshake, ignored bool, err error) {
	if probe {
		if h, ignored, err = c.discover(ctx); h != nil || err != nil {
			return h, false, err
		}
		if c.dead() {
			return nil, false, errNoProbe
		}
	}
	r, err := c.initialize(ctx)
	if err != nil {
		return nil, false, err
	}
	return r.handshake(), ignored, nil
}

// discover asks for server/discover. It returns the handshake of a modern
// server; nil and no error for a server to initialize instead (timedOut:
// it gave no answer in time); an error when the server can't be reached.
func (c *conn) discover(ctx context.Context) (*handshake, bool, error) {
	c.setModern(true)
	dctx, cancel := context.WithTimeout(ctx, discoverWait)
	defer cancel()
	var r discoverResult
	err := c.call(dctx, "server/discover", nil, &r)
	if err == nil && supports(r.SupportedVersions, modernVersion) {
		h := &handshake{version: modernVersion, instructions: r.Instructions, tasks: r.Capabilities.Extensions[tasksExt] != nil}
		if r.Capabilities.Tools != nil {
			h.listChanged = r.Capabilities.Tools.ListChanged
		}
		return h, false, nil
	}
	c.setModern(false)
	var se *statusError
	switch {
	case ctx.Err() != nil:
		return nil, false, ctx.Err()
	case errors.As(err, &se) && (se.code == 401 || se.code == 403):
		return nil, false, err // initialize would be refused the same
	case errors.Is(err, context.DeadlineExceeded):
		return nil, true, nil
	}
	// An error, or a modern server without this version: initialize.
	return nil, false, nil
}

// setModern switches c, and an HTTP transport's headers, to the modern
// protocol or back.
func (c *conn) setModern(on bool) {
	c.modern.Store(on)
	if h, ok := c.t.(interface{ setModern(bool) }); ok {
		h.setModern(on)
	}
}

func supports(versions []string, v string) bool {
	for _, x := range versions {
		if x == v {
			return true
		}
	}
	return false
}

func isRPC(err error) bool {
	var re *rpcError
	return errors.As(err, &re)
}

func rpcCode(err error) int {
	var re *rpcError
	if errors.As(err, &re) {
		return re.Code
	}
	return 0
}

func (r *initResult) handshake() *handshake {
	h := &handshake{version: r.ProtocolVersion, instructions: r.Instructions}
	if r.Capabilities.Tools != nil {
		h.listChanged = r.Capabilities.Tools.ListChanged
	}
	return h
}

// capabilities is what tego tells a server it can do.
func (c *conn) capabilities() map[string]any {
	caps := map[string]any{}
	if c.elicit {
		caps["elicitation"] = map[string]any{"form": map[string]any{}, "url": map[string]any{}}
	}
	if c.modern.Load() {
		if c.tasks.Load() {
			caps["extensions"] = map[string]any{tasksExt: map[string]any{}}
		}
	} else {
		caps["roots"] = map[string]any{"listChanged": false}
	}
	return caps
}

// withMeta is params with the _meta every modern request carries.
func (c *conn) withMeta(params any) any {
	var m map[string]any
	switch p := params.(type) {
	case nil:
		m = map[string]any{}
	case map[string]any:
		m = make(map[string]any, len(p)+1)
		for k, v := range p {
			m[k] = v
		}
	default:
		b, err := json.Marshal(p)
		if err != nil || json.Unmarshal(b, &m) != nil || m == nil {
			return params
		}
	}
	m["_meta"] = map[string]any{
		metaVersion: modernVersion,
		metaCaps:    c.capabilities(),
		metaClient:  map[string]any{"name": "tego", "version": ClientVersion},
	}
	return m
}

// listen holds subscriptions/listen open for tools/list_changed, which a
// modern server sends only there; best-effort, as long as the connection
// lives.
func (c *conn) listen() {
	for c.life.Err() == nil {
		start := time.Now()
		err := c.call(c.life, "subscriptions/listen", map[string]any{"notifications": map[string]any{"toolsListChanged": true}}, nil)
		if isRPC(err) || time.Since(start) < 5*time.Second {
			return // not offered, or it won't stay up
		}
	}
}

// ---- HTTP header mirroring (x-mcp-header) ----

// paramHeader mirrors one argument of a tool into a header of its HTTP
// requests: Mcp-Param-<name>, for servers that route on it.
type paramHeader struct {
	name string
	path []string // property names from the schema's top
	typ  string   // "string", "integer", "boolean"
}

var tokenRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// paramHeaders reads a tool's x-mcp-header annotations; ok is false when
// one is invalid, and the tool must then be left out.
func paramHeaders(schema json.RawMessage) (hs []paramHeader, ok bool) {
	var walk func(raw json.RawMessage, path []string) bool
	seen := map[string]bool{}
	walk = func(raw json.RawMessage, path []string) bool {
		var p struct {
			Type       any                        `json:"type"`
			Header     *string                    `json:"x-mcp-header"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if json.Unmarshal(raw, &p) != nil {
			return true
		}
		if p.Header != nil && len(path) > 0 {
			name := *p.Header
			typ, _ := p.Type.(string)
			if !tokenRe.MatchString(name) || seen[strings.ToLower(name)] || (typ != "string" && typ != "integer" && typ != "boolean") {
				return false
			}
			seen[strings.ToLower(name)] = true
			hs = append(hs, paramHeader{name: name, path: append([]string(nil), path...), typ: typ})
		}
		for k, sub := range p.Properties {
			if !walk(sub, append(path, k)) {
				return false
			}
		}
		return true
	}
	if !walk(schema, nil) {
		return nil, false
	}
	return hs, true
}

// values is the headers args gives hs.
func headerValues(hs []paramHeader, args json.RawMessage) map[string]string {
	if len(hs) == 0 {
		return nil
	}
	var root any
	if json.Unmarshal(args, &root) != nil {
		return nil
	}
	out := map[string]string{}
	for _, h := range hs {
		v := root
		for _, k := range h.path {
			m, _ := v.(map[string]any)
			v = m[k]
		}
		var s string
		switch x := v.(type) {
		case string:
			if h.typ != "string" {
				continue
			}
			s = x
		case float64:
			if h.typ != "integer" || x != float64(int64(x)) {
				continue
			}
			s = strconv.FormatInt(int64(x), 10)
		case bool:
			if h.typ != "boolean" {
				continue
			}
			s = strconv.FormatBool(x)
		default:
			continue
		}
		out["Mcp-Param-"+h.name] = headerValue(s)
	}
	return out
}

var sentinelRe = regexp.MustCompile(`^=\?base64\?.*\?=$`)

// headerValue is s as a header value: as it is when it is plain visible
// ASCII, else in the base64 sentinel =?base64?…?=.
func headerValue(s string) string {
	plain := s == strings.TrimSpace(s) && !sentinelRe.MatchString(s)
	for i := 0; plain && i < len(s); i++ {
		plain = s[i] == '\t' || (s[i] >= 0x20 && s[i] <= 0x7e)
	}
	if plain {
		return s
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}

// reqInfo is what a modern HTTP request says in its headers.
type reqInfo struct {
	method, name string
	params       map[string]string // Mcp-Param-* headers
}

type reqInfoKey struct{}
type paramHeadersKey struct{}

// withParamHeaders gives the request a call makes its Mcp-Param headers.
func withParamHeaders(ctx context.Context, h map[string]string) context.Context {
	if len(h) == 0 {
		return ctx
	}
	return context.WithValue(ctx, paramHeadersKey{}, h)
}

func requestInfo(ctx context.Context) *reqInfo {
	ri, _ := ctx.Value(reqInfoKey{}).(*reqInfo)
	return ri
}

// withRequest notes method and the name it acts on in ctx, for the HTTP
// transport's headers.
func withRequest(ctx context.Context, method string, params any) context.Context {
	ri := &reqInfo{method: method}
	if m, ok := params.(map[string]any); ok {
		for _, k := range []string{"name", "uri"} {
			if s, ok := m[k].(string); ok && (method == "tools/call" || method == "resources/read" || method == "prompts/get") {
				ri.name = s
				break
			}
		}
	}
	ri.params, _ = ctx.Value(paramHeadersKey{}).(map[string]string)
	return context.WithValue(ctx, reqInfoKey{}, ri)
}

func (h *handshake) String() string {
	if h == nil {
		return ""
	}
	return fmt.Sprintf("%s tasks=%v", h.version, h.tasks)
}
