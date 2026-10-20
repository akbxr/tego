package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"
	"unicode/utf8"

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
}

func (t *mcpTool) Def() llm.Tool { return t.def }

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
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var r callResult
	err = c.call(ctx, "tools/call", map[string]any{"name": t.remote, "arguments": input}, &r)
	if err != nil {
		var re *rpcError
		if errors.As(err, &re) {
			return "", nil, fmt.Errorf("mcp %s: %s", t.s.cfg.Name, re.Message)
		}
		return "", nil, err
	}
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

type callResult struct {
	Content           []content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
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
				images = append(images, llm.Block{Type: llm.BlockImage, MediaType: c.MimeType, Data: c.Data})
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
