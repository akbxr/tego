package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// openaiProvider speaks the Chat Completions API (OpenAI and the many
// OpenAI-compatible endpoints: OpenRouter, Groq, DeepSeek, Ollama, ...).
type openaiProvider struct {
	name, base, key string
	headers         map[string]string
	oauth           TokenSource
}

func (p *openaiProvider) Name() string { return p.name }

// implicitCacheHosts serve an implicit prompt cache. Other OpenAI-compatible
// endpoints (local servers, routers) may cache nothing, and there trimming
// old history is the only way to keep a long session small.
var implicitCacheHosts = []string{"api.openai.com", "generativelanguage.googleapis.com", "api.deepseek.com", "api.x.ai"}

func (p *openaiProvider) ImplicitPrefixCache(string) bool {
	for _, h := range implicitCacheHosts {
		if strings.Contains(p.base, h) {
			return true
		}
	}
	return false
}

func (p *openaiProvider) Stream(ctx context.Context, req *Request, onEvent func(Event)) (*Message, error) {
	body, err := openaiBody(req)
	if err != nil {
		return nil, err
	}
	return streamWithRetry(ctx, p.name, req, onEvent, func(ctx context.Context, emit func(Event)) (*Message, error) {
		key := p.key
		if p.oauth != nil {
			t, err := p.oauth.Token(ctx)
			if err != nil {
				return nil, err
			}
			key = t
		}
		h := http.Header{}
		h.Set("Content-Type", "application/json")
		h.Set("Accept", "text/event-stream")
		if key != "" {
			h.Set("Authorization", "Bearer "+key)
		}
		setHeaders(h, p.headers)
		resp, err := postJSON(ctx, p.name, p.base+"/chat/completions", body, h)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return parseOpenAIStream(ctx, p.name, resp.Body, emit)
	})
}

type oaFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type oaTool struct {
	Type     string     `json:"type"`
	Function oaFunction `json:"function"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// oaMessage.Content is a string, or a []oaPart for user messages with images.
type oaMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaImageURL struct {
	URL string `json:"url"`
}

type oaPart struct {
	Type     string      `json:"type"`
	Text     string      `json:"text,omitempty"`
	ImageURL *oaImageURL `json:"image_url,omitempty"`
}

// dataURL renders an image block as a data: URL.
func dataURL(b *Block) string {
	return "data:" + b.MediaType + ";base64," + b.Data
}

func appendOpenAIImages(parts []oaPart, imgs []Block) []oaPart {
	for i := range imgs {
		if imgs[i].Type == BlockImage {
			parts = append(parts, oaPart{Type: "image_url", ImageURL: &oaImageURL{URL: dataURL(&imgs[i])}})
		}
	}
	return parts
}

type oaRequest struct {
	Model               string      `json:"model"`
	Messages            []oaMessage `json:"messages"`
	Tools               []oaTool    `json:"tools,omitempty"`
	Stream              bool        `json:"stream"`
	StreamOptions       any         `json:"stream_options,omitempty"`
	MaxCompletionTokens int         `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string      `json:"reasoning_effort,omitempty"`
}

// openaiMessages converts history. Tool messages cannot carry images, so
// each tool result with images is followed (after the whole run of tool
// messages, which must directly follow the assistant's tool calls) by a user
// message holding them.
func openaiMessages(out []oaMessage, msgs []Message) []oaMessage {
	var pending []oaMessage // image messages for tool results in the current run
	for i := range msgs {
		m := &msgs[i]
		n := len(out)
		out = appendOpenAIMessages(out, m, &pending)
		if len(pending) == 0 || len(out) == n {
			continue
		}
		// Flush pending images before the first non-tool message just appended.
		j := n
		for j < len(out) && out[j].Role == "tool" {
			j++
		}
		if j < len(out) {
			out = append(out[:j], append(pending, out[j:]...)...)
			pending = pending[:0]
		}
	}
	return append(out, pending...)
}

// openaiReasoningModel reports model ids that accept reasoning_effort.
func openaiReasoningModel(model string) bool {
	m := strings.ToLower(model)
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	for _, p := range []string{"o1", "o3", "o4", "gpt-5"} {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

func openaiBody(req *Request) ([]byte, error) {
	r := oaRequest{
		Model:               req.Model,
		Stream:              true,
		StreamOptions:       map[string]bool{"include_usage": true},
		MaxCompletionTokens: req.MaxTokens,
	}
	if _, ok := thinkingBudgets[req.Thinking]; ok && openaiReasoningModel(req.Model) {
		r.ReasoningEffort = req.Thinking
	}
	if req.System != "" {
		r.Messages = append(r.Messages, oaMessage{Role: "system", Content: req.System})
	}
	r.Messages = openaiMessages(r.Messages, req.Messages)
	for _, t := range req.Tools {
		schema := t.Schema
		if len(schema) == 0 {
			schema = emptyObjectSchema
		}
		r.Tools = append(r.Tools, oaTool{Type: "function", Function: oaFunction{Name: t.Name, Description: t.Description, Parameters: schema}})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&r); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

func appendOpenAIMessages(out []oaMessage, m *Message, pending *[]oaMessage) []oaMessage {
	var text strings.Builder
	if m.Role == RoleAssistant {
		var calls []oaToolCall
		for _, b := range m.Blocks {
			switch b.Type {
			case BlockText:
				text.WriteString(b.Text)
			case BlockToolUse:
				var c oaToolCall
				c.ID, c.Type = b.ID, "function"
				c.Function.Name = b.Name
				c.Function.Arguments = string(b.Input)
				if strings.TrimSpace(c.Function.Arguments) == "" {
					c.Function.Arguments = "{}"
				}
				calls = append(calls, c)
			}
		}
		if text.Len() == 0 && len(calls) == 0 {
			return out
		}
		msg := oaMessage{Role: "assistant", ToolCalls: calls}
		if text.Len() > 0 {
			msg.Content = text.String()
		}
		return append(out, msg)
	}
	// User: tool results become role=tool messages and precede any text;
	// parts[0] is reserved for the joined text when the message has images.
	var parts []oaPart
	for i := range m.Blocks {
		b := &m.Blocks[i]
		switch b.Type {
		case BlockToolResult:
			content := b.Content
			if b.IsError && content == "" {
				content = "error"
			}
			out = append(out, oaMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: content})
			if len(b.Images) > 0 {
				parts := []oaPart{{Type: "text", Text: "Images returned by tool call " + b.ToolUseID + ":"}}
				*pending = append(*pending, oaMessage{Role: "user", Content: appendOpenAIImages(parts, b.Images)})
			}
		case BlockText:
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(b.Text)
		case BlockImage:
			if parts == nil {
				parts = append(parts, oaPart{Type: "text"})
			}
			parts = appendOpenAIImages(parts, m.Blocks[i:i+1])
		}
	}
	if parts != nil {
		if text.Len() > 0 {
			parts[0].Text = text.String()
		} else {
			parts = parts[1:]
		}
		return append(out, oaMessage{Role: "user", Content: parts})
	}
	if text.Len() > 0 {
		out = append(out, oaMessage{Role: "user", Content: text.String()})
	}
	return out
}

type oaChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

type oaPartialCall struct {
	id, name string
	args     []byte
	notified bool
}

func parseOpenAIStream(ctx context.Context, provider string, body io.Reader, emit func(Event)) (msg *Message, err error) {
	msg = &Message{Role: RoleAssistant, Usage: &Usage{}}
	var thinking, text strings.Builder
	calls := map[int]*oaPartialCall{}
	finish := ""
	done := false
	defer func() {
		// Assemble blocks: thinking, text, then tool calls in index order.
		if thinking.Len() > 0 {
			msg.Blocks = append(msg.Blocks, Block{Type: BlockThinking, Text: thinking.String()})
		}
		if text.Len() > 0 {
			msg.Blocks = append(msg.Blocks, Block{Type: BlockText, Text: text.String()})
		}
		if err != nil {
			return // partially streamed tool calls are never complete
		}
		idx := make([]int, 0, len(calls))
		for i := range calls {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		for n, i := range idx {
			c := calls[i]
			id := c.id
			if id == "" {
				id = fmt.Sprintf("call_%d", n)
			}
			msg.Blocks = append(msg.Blocks, Block{Type: BlockToolUse, ID: id, Name: c.name, Input: ToolInput(c.args)})
		}
		if len(idx) > 0 && (finish == "" || finish == "tool_calls" || finish == "stop") {
			msg.StopReason = "tool_use"
		}
	}()
	sse := newSSEReader(body)
	var ch oaChunk
	for {
		e, rerr := sse.Next()
		if rerr != nil {
			if ctx.Err() != nil {
				return msg, ctx.Err()
			}
			if rerr == io.EOF {
				if done || finish != "" {
					break
				}
				rerr = io.ErrUnexpectedEOF
			}
			return msg, &streamError{rerr}
		}
		data := bytes.TrimSpace(e.Data)
		if string(data) == "[DONE]" {
			done = true
			break
		}
		if len(data) > 0 {
			emit(Event{Type: EvStarted})
		}
		ch = oaChunk{}
		if err := json.Unmarshal(data, &ch); err != nil {
			return msg, fmt.Errorf("%s: bad stream chunk %q: %w", provider, truncate(string(data), 200), err)
		}
		if ch.Error != nil {
			status := 500
			if c := fmt.Sprint(ch.Error.Code); strings.Contains(c, "429") || strings.Contains(strings.ToLower(c), "rate") {
				status = 429
			}
			return msg, &APIError{Provider: provider, Status: status, Body: ch.Error.Message}
		}
		if u := ch.Usage; u != nil {
			cached := 0
			if u.PromptTokensDetails != nil {
				cached = u.PromptTokensDetails.CachedTokens
			}
			msg.Usage.Input = u.PromptTokens - cached
			msg.Usage.CacheRead = cached
			msg.Usage.Output = u.CompletionTokens
		}
		for _, c := range ch.Choices {
			d := &c.Delta
			if r := d.ReasoningContent + d.Reasoning; r != "" {
				thinking.WriteString(r)
				emit(Event{Type: EvThinking, Text: r})
			}
			if d.Content != "" {
				text.WriteString(d.Content)
				emit(Event{Type: EvText, Text: d.Content})
			}
			for _, tc := range d.ToolCalls {
				pc := calls[tc.Index]
				if pc == nil {
					pc = &oaPartialCall{}
					calls[tc.Index] = pc
				}
				if tc.ID != "" {
					pc.id = tc.ID
				}
				if pc.name == "" {
					pc.name = tc.Function.Name
				}
				pc.args = append(pc.args, tc.Function.Arguments...)
				if !pc.notified && pc.name != "" {
					pc.notified = true
					emit(Event{Type: EvToolCall, ToolName: pc.name, ToolID: pc.id})
				}
			}
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		}
	}
	switch finish {
	case "length":
		msg.StopReason = "max_tokens"
	case "tool_calls", "function_call":
		msg.StopReason = "tool_use"
	default:
		msg.StopReason = "end_turn"
	}
	return msg, nil
}
