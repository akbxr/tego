package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	imgs "github.com/akbxr/tego/internal/images"
	"github.com/akbxr/tego/internal/llm"
)

const (
	callTimeout    = 10 * time.Minute
	maxNameLen     = 64
	maxDescription = 1024
)

// mcpTool adapts one remote tool to tools.ImageTool.
type mcpTool struct {
	s        *server
	remote   string
	def      llm.Tool
	readOnly bool
	headers  []paramHeader
}

func (t *mcpTool) Def() llm.Tool { return t.def }

// ServerInstructions is the server's name and its instructions to the
// model ("" without).
func (t *mcpTool) ServerInstructions() (string, string) {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	return t.s.cfg.Name, t.s.instructions
}

// ReadOnlyHint is the server's claim that the tool changes nothing, which
// lets it run in plan mode. (Not tools.ReadOnly: that would also run it in
// parallel batches, which a server may not expect.)
func (t *mcpTool) ReadOnlyHint() bool { return t.readOnly }

func (t *mcpTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	text, images, err := t.RunImages(ctx, input)
	if len(images) > 0 {
		text += fmt.Sprintf("\n[%d image(s) omitted]", len(images))
	}
	return text, err
}

func (t *mcpTool) RunImages(ctx context.Context, input json.RawMessage) (string, []llm.Block, error) {
	c, err := t.s.acquire() // starts it if it is idle
	if err != nil {
		return "", nil, err
	}
	defer t.s.release()
	if len(input) == 0 || string(input) == "null" {
		input = json.RawMessage("{}")
	}
	r, err := t.call(ctx, c, input)
	if err != nil {
		return "", nil, t.callErr(ctx, err)
	}
	if r.ResultType == "task" {
		return t.awaitTask(ctx, c, r, input)
	}
	return r.output()
}

// maxInputRounds caps the times one call may ask the user (input_required).
const maxInputRounds = 8

// call makes the tools/call: when the server asks the user something
// first (input_required, MCP 2026-07-28), it is asked and the call made
// again with the answers; when the server's headers changed, once more
// with the new ones.
func (t *mcpTool) call(ctx context.Context, c *conn, input json.RawMessage) (*callResult, error) {
	params := map[string]any{"name": t.remote, "arguments": input}
	headers := t.headers
	retried := false
	for round := 0; ; round++ {
		cctx, cancel := context.WithTimeout(withParamHeaders(ctx, headerValues(headers, input)), callTimeout)
		var r callResult
		err := c.call(cctx, "tools/call", params, &r)
		cancel()
		if rpcCode(err) == codeHeaderMismatch && !retried {
			retried = true
			t.s.refetch()
			headers = t.s.toolHeaders(t.remote)
			continue
		}
		if err != nil || r.ResultType != "input_required" {
			return &r, err
		}
		if round >= maxInputRounds {
			return nil, fmt.Errorf("mcp %s: the server asked for input %d times in one call; gave up", t.s.cfg.Name, round+1)
		}
		answers, err := t.s.answer(ctx, r.InputRequests, nil)
		if err != nil {
			return nil, err
		}
		params = map[string]any{"name": t.remote, "arguments": input, "inputResponses": answers}
		if r.RequestState != "" {
			params["requestState"] = r.RequestState
		}
	}
}

// callErr is how a failed call reads to the model. A server that needs the
// user to finish a step in the browser first (2025-11-25's -32042) gets it
// shown to them.
func (t *mcpTool) callErr(ctx context.Context, err error) error {
	var re *rpcError
	if !errors.As(err, &re) {
		return err
	}
	if re.Code == codeURLRequired {
		var data struct {
			Elicitations []json.RawMessage `json:"elicitations"`
		}
		_ = json.Unmarshal(re.Data, &data)
		opened := 0
		for _, e := range data.Elicitations {
			if r, err := t.s.elicit(ctx, e); err == nil && r.Action == ElicitAccept {
				opened++
			}
		}
		if opened > 0 && opened == len(data.Elicitations) {
			return fmt.Errorf("mcp %s: the server needs the user to finish a step in their browser first; it was opened for them. Call the tool again once they say they finished", t.s.cfg.Name)
		}
		return fmt.Errorf("mcp %s: %s (it needs the user to finish a step in their browser, which they did not open)", t.s.cfg.Name, re.Message)
	}
	return fmt.Errorf("mcp %s: %s", t.s.cfg.Name, re.Message)
}

// output is a finished call's result for the model.
func (r *callResult) output() (string, []llm.Block, error) {
	text, images := r.render()
	if r.IsError {
		if text == "" {
			text = "tool reported an error"
		}
		return "", nil, errors.New(text)
	}
	if text == "" && len(images) == 0 {
		text = "(no output)"
	}
	return text, images, nil
}

// inputRequest is one thing a modern server asks of the client before it
// goes on: elicitation/create, roots/list.
type inputRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// maxInputRequests caps the requests one answer takes.
const maxInputRequests = 16

// answer fulfils a server's input requests, one at a time, and returns
// the answers by key. Keys in done were answered before (a task asks again
// until it sees the answer) and are skipped, and the answered keys added.
func (s *server) answer(ctx context.Context, reqs map[string]inputRequest, done map[string]bool) (map[string]any, error) {
	keys := make([]string, 0, len(reqs))
	for k := range reqs {
		if !done[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > maxInputRequests {
		keys = keys[:maxInputRequests]
	}
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		req := reqs[k]
		switch req.Method {
		case "elicitation/create":
			if s.m.elicit == nil {
				return nil, fmt.Errorf("mcp %s: the server asks the user something, which this session can't ask", s.cfg.Name)
			}
			r, err := s.elicit(ctx, req.Params)
			if err != nil {
				return nil, fmt.Errorf("mcp %s: %w", s.cfg.Name, err)
			}
			out[k] = r
		case "roots/list":
			out[k] = s.roots()
		default:
			return nil, fmt.Errorf("mcp %s: the server asks for %s, which tego doesn't do", s.cfg.Name, req.Method)
		}
		if done != nil {
			done[k] = true
		}
	}
	return out, nil
}

// toolHeaders is the header mirroring of s's tool remote now.
func (s *server) toolHeaders(remote string) []paramHeader {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ti := range s.list {
		if ti.remote == remote {
			return ti.headers
		}
	}
	return nil
}

type callResult struct {
	ResultType        string          `json:"resultType"`
	Content           []content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`

	// input_required
	InputRequests map[string]inputRequest `json:"inputRequests"`
	RequestState  string                  `json:"requestState"`

	// task
	taskState
}

type content struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
	URI      string `json:"uri"`
	Resource *struct {
		URI      string  `json:"uri"`
		MimeType string  `json:"mimeType"`
		Text     *string `json:"text"`
		Blob     string  `json:"blob"`
	} `json:"resource"`
}

// render converts tool result content to text (parts joined by blank lines)
// and image blocks.
func (r *callResult) render() (string, []llm.Block) {
	var (
		parts   []string
		images  []llm.Block
		hasText bool
	)
	for _, c := range r.Content {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
			hasText = true
		case "image":
			if supportedImage(c.MimeType) && c.Data != "" {
				// Sized as a read image is: a 4K render as sent made every
				// request with over 20 images fail.
				img, _, err := imgs.FromBlock(llm.Block{Type: llm.BlockImage, MediaType: c.MimeType, Data: c.Data}, "image")
				if err != nil {
					parts = append(parts, fmt.Sprintf("[image omitted: %v]", err))
					continue
				}
				images = append(images, img)
			} else {
				parts = append(parts, fmt.Sprintf("[image %s omitted: unsupported type]", c.MimeType))
			}
		case "audio":
			parts = append(parts, fmt.Sprintf("[audio %s omitted]", c.MimeType))
		case "resource":
			if c.Resource == nil {
				continue
			}
			if c.Resource.Text != nil {
				parts = append(parts, *c.Resource.Text)
				hasText = true
			} else {
				parts = append(parts, "[binary resource "+c.Resource.URI+"]")
			}
		case "resource_link":
			parts = append(parts, "[resource "+c.URI+"]")
		}
	}
	if !hasText && len(r.StructuredContent) > 0 && string(r.StructuredContent) != "null" {
		parts = append(parts, string(r.StructuredContent))
	}
	return strings.Join(parts, "\n\n"), images
}

func supportedImage(mt string) bool {
	switch mt {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// sanitize replaces every character outside [A-Za-z0-9_-] with '_'.
func sanitize(s string) string {
	ok := true
	for i := range len(s) {
		if !nameByte(s[i]) {
			ok = false
			break
		}
	}
	if ok {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < utf8.RuneSelf && nameByte(byte(r)) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func nameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func shortHash(server, tool string) string {
	h := fnv.New32a()
	h.Write([]byte(server))
	h.Write([]byte{0})
	h.Write([]byte(tool))
	return fmt.Sprintf("%08x", h.Sum32())
}

// toolName builds mcp__<server>__<tool>, sanitized and at most 64 bytes.
func toolName(server, tool string) string {
	full := "mcp__" + sanitize(server) + "__" + sanitize(tool)
	if len(full) <= maxNameLen {
		return full
	}
	return hashedName(full, server, tool)
}

func hashedName(base, server, tool string) string {
	const keep = maxNameLen - 9 // "_" + 8 hex
	if len(base) > keep {
		base = base[:keep]
	}
	return base + "_" + shortHash(server, tool)
}

type toolKey struct{ server, tool string }

// uniqueNames assigns a distinct model-facing name to every (server, tool)
// pair, in order. Collisions (from sanitizing or truncation) get a hash
// suffix derived from the raw names, then a counter as a last resort.
func uniqueNames(keys []toolKey) []string {
	out := make([]string, len(keys))
	used := make(map[string]bool, len(keys))
	for i, k := range keys {
		n := toolName(k.server, k.tool)
		if used[n] {
			h := hashedName(n, k.server, k.tool)
			n = h
			for j := 2; used[n]; j++ {
				suffix := fmt.Sprintf("_%d", j)
				n = h[:min(len(h), maxNameLen-len(suffix))] + suffix
			}
		}
		used[n] = true
		out[i] = n
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

var defaultSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// normalizeSchema returns s as a JSON Schema object with top-level
// "type":"object" and a "properties" member.
func normalizeSchema(s json.RawMessage) json.RawMessage {
	if len(s) == 0 || string(s) == "null" {
		return defaultSchema
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(s, &m) != nil || m == nil {
		return defaultSchema
	}
	changed := false
	if string(m["type"]) != `"object"` {
		m["type"] = json.RawMessage(`"object"`)
		changed = true
	}
	if _, ok := m["properties"]; !ok {
		m["properties"] = json.RawMessage(`{}`)
		changed = true
	}
	if !changed {
		return s
	}
	b, err := json.Marshal(m)
	if err != nil {
		return defaultSchema
	}
	return b
}

func description(rt *remoteTool) string {
	d := strings.TrimSpace(rt.Description)
	if d == "" {
		d = rt.Title
	}
	return truncate(d, maxDescription)
}
