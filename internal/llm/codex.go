package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Codex (ChatGPT Plus/Pro subscription) Responses backend: request shape
// and headers.
const (
	codexDefaultBase   = "https://chatgpt.com/backend-api"
	codexOriginator    = "omp"
	codexClientVersion = "0.159.0"
	codexUserAgent     = "omp/18.8.9"
	codexAuthClaim     = "https://api.openai.com/auth"
)

type codexProvider struct {
	name, url string
	headers   map[string]string
	oauth     TokenSource
}

func (p *codexProvider) Name() string { return p.name }

// ImplicitPrefixCache: OpenAI caches prompt prefixes on its own.
func (p *codexProvider) ImplicitPrefixCache(string) bool { return true }

// codexURL resolves the responses endpoint from an optional base URL.
func codexURL(base string) string {
	if base == "" {
		base = codexDefaultBase
	}
	switch {
	case strings.HasSuffix(base, "/codex/responses"):
		return base
	case strings.HasSuffix(base, "/codex"):
		return base + "/responses"
	}
	return base + "/codex/responses"
}

func (p *codexProvider) Stream(ctx context.Context, req *Request, onEvent func(Event)) (*Message, error) {
	body, err := codexBody(req)
	if err != nil {
		return nil, err
	}
	return streamWithRetry(ctx, p.name, req, onEvent, func(ctx context.Context, emit func(Event)) (*Message, error) {
		token, err := p.oauth.Token(ctx)
		if err != nil {
			return nil, err
		}
		resp, err := postJSON(ctx, p.name, p.url, body, p.header(token, req.SessionID))
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return parseCodexStream(ctx, p.name, resp.Body, emit)
	})
}

func (p *codexProvider) header(token, sessionID string) http.Header {
	h := http.Header{}
	setHeaders(h, p.headers)
	h.Set("Authorization", "Bearer "+token)
	claims := codexClaims(token)
	if claims.AccountID != "" {
		h.Set("chatgpt-account-id", claims.AccountID)
	}
	if r := claims.residency(); r != "" {
		h.Set("x-openai-internal-codex-residency", r)
	}
	h.Set("OpenAI-Beta", "responses=experimental")
	h.Set("originator", codexOriginator)
	h.Set("version", codexClientVersion)
	h.Set("User-Agent", codexUserAgent)
	h.Set("Accept", "text/event-stream")
	h.Set("Content-Type", "application/json")
	if sessionID != "" {
		h.Set("conversation_id", sessionID)
		h.Set("session_id", sessionID)
		h.Set("x-client-request-id", sessionID)
	}
	return h
}

type codexAuthClaims struct {
	AccountID        string `json:"chatgpt_account_id"`
	DataResidency    string `json:"chatgpt_data_residency"`
	ComputeResidency string `json:"chatgpt_compute_residency"`
}

func (c codexAuthClaims) residency() string {
	if r := strings.TrimSpace(c.DataResidency); r != "" {
		return r
	}
	return strings.TrimSpace(c.ComputeResidency)
}

// codexClaims decodes the ChatGPT auth claim from an access-token JWT
// (without verifying it).
func codexClaims(jwt string) codexAuthClaims {
	var out codexAuthClaims
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return out
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return out
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(payload, &claims) != nil {
		return out
	}
	_ = json.Unmarshal(claims[codexAuthClaim], &out)
	return out
}

type cxPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type cxMessageItem struct {
	Type    string     `json:"type"`
	Role    string     `json:"role"`
	Content []cxInPart `json:"content"`
}

// cxInPart is an input/output message part: text, or an input_image.
type cxInPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Detail   string `json:"detail,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

func codexImage(b *Block) cxInPart {
	return cxInPart{Type: "input_image", Detail: "auto", ImageURL: dataURL(b)}
}

type cxFunctionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// cxFunctionOutput.Output is a string, or a []cxInPart of
// input_text and input_image parts when the tool returned images.
type cxFunctionOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output any    `json:"output"`
}

func codexToolOutput(b *Block) cxFunctionOutput {
	o := cxFunctionOutput{Type: "function_call_output", CallID: b.ToolUseID, Output: b.Content}
	if len(b.Images) == 0 {
		return o
	}
	parts := make([]cxInPart, 0, len(b.Images)+1)
	if b.Content != "" {
		parts = append(parts, cxInPart{Type: "input_text", Text: b.Content})
	}
	for i := range b.Images {
		if b.Images[i].Type == BlockImage {
			parts = append(parts, codexImage(&b.Images[i]))
		}
	}
	o.Output = parts
	return o
}

type cxReasoningItem struct {
	Type             string   `json:"type"`
	Summary          []cxPart `json:"summary"`
	EncryptedContent string   `json:"encrypted_content"`
}

type cxTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type cxReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

type cxRequest struct {
	Model             string       `json:"model"`
	Instructions      string       `json:"instructions"`
	Input             []any        `json:"input"`
	Tools             []cxTool     `json:"tools,omitempty"`
	ToolChoice        string       `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool        `json:"parallel_tool_calls,omitempty"`
	Store             bool         `json:"store"`
	Stream            bool         `json:"stream"`
	Reasoning         *cxReasoning `json:"reasoning,omitempty"`
	Include           []string     `json:"include"`
	PromptCacheKey    string       `json:"prompt_cache_key,omitempty"`
}

func codexBody(req *Request) ([]byte, error) {
	r := cxRequest{
		Model:          req.Model,
		Instructions:   req.System,
		Input:          codexInput(req.Messages),
		Stream:         true,
		Include:        []string{"reasoning.encrypted_content"},
		PromptCacheKey: req.SessionID,
	}
	if r.Input == nil {
		r.Input = []any{}
	}
	if eff := openaiEffort(req.Model, req.Thinking); eff != "" {
		r.Reasoning = &cxReasoning{Effort: eff, Summary: "auto"}
	}
	if len(req.Tools) > 0 {
		parallel := true
		r.ToolChoice, r.ParallelToolCalls = "auto", &parallel
		r.Tools = make([]cxTool, len(req.Tools))
		for i, t := range req.Tools {
			schema := t.Schema
			if len(schema) == 0 {
				schema = emptyObjectSchema
			}
			r.Tools[i] = cxTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: schema}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&r); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// codexInput converts history to Responses input items. Thinking blocks are
// replayed only when they carry Codex encrypted reasoning.
func codexInput(msgs []Message) []any {
	var items []any
	for i := range msgs {
		m := &msgs[i]
		var parts []cxInPart // pending consecutive text/image parts
		role, partType := "user", "input_text"
		if m.Role == RoleAssistant {
			role, partType = "assistant", "output_text"
		}
		flush := func() {
			if len(parts) > 0 {
				items = append(items, cxMessageItem{Type: "message", Role: role, Content: parts})
				parts = nil
			}
		}
		for j := range m.Blocks {
			b := &m.Blocks[j]
			switch b.Type {
			case BlockText:
				if b.Text != "" {
					parts = append(parts, cxInPart{Type: partType, Text: b.Text})
				}
			case BlockImage:
				if m.Role == RoleUser {
					parts = append(parts, codexImage(b))
				}
			case BlockThinking:
				if m.Role != RoleAssistant || b.Encrypted == "" {
					continue
				}
				flush()
				summary := []cxPart{}
				if b.Text != "" {
					summary = append(summary, cxPart{Type: "summary_text", Text: b.Text})
				}
				items = append(items, cxReasoningItem{Type: "reasoning", Summary: summary, EncryptedContent: b.Encrypted})
			case BlockToolUse:
				flush()
				args := strings.TrimSpace(string(b.Input))
				if args == "" {
					args = "{}"
				}
				items = append(items, cxFunctionCall{Type: "function_call", CallID: b.ID, Name: b.Name, Arguments: args})
			case BlockToolResult:
				flush()
				items = append(items, codexToolOutput(b))
			}
		}
		flush()
	}
	return items
}

// ---- response stream ----

type cxOutputItem struct {
	ID               string   `json:"id"`
	Type             string   `json:"type"`
	CallID           string   `json:"call_id"`
	Name             string   `json:"name"`
	Arguments        string   `json:"arguments"`
	EncryptedContent string   `json:"encrypted_content"`
	Summary          []cxPart `json:"summary"`
	Content          []cxPart `json:"content"`
}

type cxErrorBody struct {
	Code    string `json:"code"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

type cxEvent struct {
	Type         string        `json:"type"`
	ItemID       string        `json:"item_id"`
	SummaryIndex int           `json:"summary_index"`
	Delta        string        `json:"delta"`
	Item         *cxOutputItem `json:"item"`
	Response     *struct {
		Status string `json:"status"`
		Usage  *struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
		Error             *cxErrorBody `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Error   *cxErrorBody `json:"error"`
}

func codexErrorStatus(code string) int {
	c := strings.ToLower(code)
	switch {
	case strings.Contains(c, "rate_limit"), strings.Contains(c, "usage_limit"), strings.Contains(c, "quota"):
		return 429
	case strings.Contains(c, "server_error"), strings.Contains(c, "overloaded"), strings.Contains(c, "unavailable"), c == "":
		return 500
	}
	return 400
}

func codexError(provider string, e *cxErrorBody, fallback string) *APIError {
	code, text := "", fallback
	if e != nil {
		code = e.Code
		if code == "" {
			code = e.Type
		}
		if e.Message != "" {
			text = e.Message
		}
	}
	if code != "" {
		text = code + ": " + text
	}
	return &APIError{Provider: provider, Status: codexErrorStatus(code), Body: text}
}

func parseCodexStream(ctx context.Context, provider string, body io.Reader, emit func(Event)) (msg *Message, err error) {
	msg = &Message{Role: RoleAssistant, Usage: &Usage{}}
	acc := newBlockAccumulator()
	byItem := map[string]int{} // output item id -> msg.Blocks index
	defer func() { acc.finalize(msg, err != nil) }()

	// block returns the block for an item, creating it on first sight.
	block := func(itemID string, b Block) int {
		if bi, ok := byItem[itemID]; ok && itemID != "" {
			return bi
		}
		bi := len(msg.Blocks)
		msg.Blocks = append(msg.Blocks, b)
		byItem[itemID] = bi
		return bi
	}

	sse := newSSEReader(body)
	var ev cxEvent
	for {
		e, rerr := sse.Next()
		if rerr != nil {
			if ctx.Err() != nil {
				return msg, ctx.Err()
			}
			if rerr == io.EOF {
				rerr = io.ErrUnexpectedEOF
			}
			return msg, &streamError{rerr}
		}
		ev = cxEvent{}
		if err := json.Unmarshal(e.Data, &ev); err != nil {
			return msg, fmt.Errorf("%s: bad stream event %q: %w", provider, truncate(string(e.Data), 200), err)
		}
		switch ev.Type {
		case "response.output_item.added":
			emit(Event{Type: EvStarted}) // the first output item: reasoning has begun
			it := ev.Item
			if it == nil {
				continue
			}
			switch it.Type {
			case "message":
				block(it.ID, Block{Type: BlockText})
			case "reasoning":
				block(it.ID, Block{Type: BlockThinking, ItemID: it.ID})
			case "function_call":
				block(it.ID, Block{Type: BlockToolUse, ID: it.CallID, Name: it.Name})
				emit(Event{Type: EvToolCall, ToolName: it.Name, ToolID: it.CallID})
			}
		case "response.output_text.delta":
			bi := block(ev.ItemID, Block{Type: BlockText})
			acc.addText(bi, ev.Delta)
			emit(Event{Type: EvText, Text: ev.Delta})
		case "response.reasoning_summary_part.added":
			bi := block(ev.ItemID, Block{Type: BlockThinking, ItemID: ev.ItemID})
			if ev.SummaryIndex > 0 && acc.textLen(bi) > 0 {
				acc.addText(bi, "\n\n")
				emit(Event{Type: EvThinking, Text: "\n\n"})
			}
		case "response.reasoning_summary_text.delta":
			bi := block(ev.ItemID, Block{Type: BlockThinking, ItemID: ev.ItemID})
			acc.addText(bi, ev.Delta)
			emit(Event{Type: EvThinking, Text: ev.Delta})
		case "response.function_call_arguments.delta":
			if bi, ok := byItem[ev.ItemID]; ok {
				acc.addArgs(bi, ev.Delta)
			}
		case "response.output_item.done":
			it := ev.Item
			if it == nil {
				continue
			}
			switch it.Type {
			case "message":
				bi := block(it.ID, Block{Type: BlockText})
				if acc.textLen(bi) == 0 {
					for _, c := range it.Content {
						if c.Type == "output_text" {
							acc.addText(bi, c.Text)
						}
					}
				}
			case "reasoning":
				bi := block(it.ID, Block{Type: BlockThinking, ItemID: it.ID})
				msg.Blocks[bi].Encrypted = it.EncryptedContent
				if acc.textLen(bi) == 0 {
					for j, s := range it.Summary {
						if j > 0 {
							acc.addText(bi, "\n\n")
						}
						acc.addText(bi, s.Text)
					}
				}
			case "function_call":
				bi, ok := byItem[it.ID]
				if !ok {
					bi = block(it.ID, Block{Type: BlockToolUse, ID: it.CallID, Name: it.Name})
					emit(Event{Type: EvToolCall, ToolName: it.Name, ToolID: it.CallID})
				}
				b := &msg.Blocks[bi]
				if it.CallID != "" {
					b.ID = it.CallID
				}
				if it.Name != "" {
					b.Name = it.Name
				}
				var full []byte
				if it.Arguments != "" {
					full = []byte(it.Arguments)
				}
				acc.finishTool(msg, bi, full)
			}
		case "response.completed", "response.done", "response.incomplete":
			r := ev.Response
			if r != nil && r.Usage != nil {
				cached := 0
				if r.Usage.InputTokensDetails != nil {
					cached = r.Usage.InputTokensDetails.CachedTokens
				}
				msg.Usage.Input = r.Usage.InputTokens - cached
				msg.Usage.CacheRead = cached
				msg.Usage.Output = r.Usage.OutputTokens
			}
			msg.StopReason = "end_turn"
			for bi, b := range msg.Blocks {
				if b.Type == BlockToolUse {
					if !acc.complete[bi] {
						acc.finishTool(msg, bi, nil)
					}
					msg.StopReason = "tool_use"
				}
			}
			if r != nil && (r.Status == "incomplete" || ev.Type == "response.incomplete") {
				if r.IncompleteDetails == nil || r.IncompleteDetails.Reason == "max_output_tokens" {
					msg.StopReason = "max_tokens"
				}
			}
			return msg, nil
		case "response.failed":
			var eb *cxErrorBody
			if ev.Response != nil {
				eb = ev.Response.Error
			}
			return msg, codexError(provider, eb, "response failed")
		case "error":
			eb := ev.Error
			if eb == nil {
				eb = &cxErrorBody{Code: ev.Code, Message: ev.Message}
			}
			return msg, codexError(provider, eb, truncate(string(e.Data), 500))
		}
	}
}
