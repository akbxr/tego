package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

const (
	anthropicVersion = "2023-06-01"
	// No fine-grained-tool-streaming beta: without it the API validates tool
	// arguments as JSON; with it, models sometimes stream malformed arrays.
	// Only write and edit opt in, per tool (eagerInputTools).
	anthropicAPIKeyBeta = "interleaved-thinking-2025-05-14,extended-cache-ttl-2025-04-11,context-management-2025-06-27,thinking-binding-controls-2026-08-01"
)

type anthropicProvider struct {
	name, base, key string
	headers         map[string]string
	oauth           TokenSource
	deviceID        string

	mu          sync.Mutex
	ccVersion   string // version adopted after a claude_code_version_too_old rejection
	fallbackSID string
	sessions    map[string]*ccSession
	fast        fastState // fast mode after a rate limit or refusal (fast.go)
}

func (p *anthropicProvider) Name() string { return p.name }

// ClearsToolResults: context editing (clear_tool_uses) and custom tool
// search (defer_loading + tool_reference) need Claude 4.5 or newer.
func (p *anthropicProvider) ClearsToolResults(model string) bool {
	return claudeGenAtLeast(model, 4, 5)
}
func (p *anthropicProvider) DefersTools(model string) bool { return claudeGenAtLeast(model, 4, 5) }

// KnownCacheTTL: every request marks its cache breakpoints with an explicit TTL.
func (p *anthropicProvider) KnownCacheTTL(string) bool { return true }

func (p *anthropicProvider) Stream(ctx context.Context, req *Request, onEvent func(Event)) (*Message, error) {
	return streamWithRetry(ctx, p.name, req, onEvent, func(ctx context.Context, emit func(Event)) (*Message, error) {
		var t *ccTurn
		if p.oauth != nil {
			t = p.turn(ctx, p.sessionID(req), req.Class, req.Messages)
		}
		fast := p.fastFor(req)
		send := func() (*http.Response, error) {
			return p.send(ctx, t, fast, func(version string) ([]byte, error) { return p.buildBodySpeed(req, version, t, fast) })
		}
		resp, err := send()
		note := ""
		if err != nil && fast {
			// A fast mode rate limit or refusal: carry on at standard speed
			// now rather than wait, as Claude Code does.
			var again bool
			if note, again = p.fastRejected(err); again {
				fast = false
				resp, err = send()
			}
		}
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var msgID string
		msg, err := parseAnthropicStream(ctx, resp.Body, p.oauth != nil, emit, &msgID)
		if msg != nil {
			msg.FastNote = note
		}
		if err != nil && fast {
			p.fastRejected(err) // overloaded mid-stream: the retry runs at standard speed
		}
		if t != nil {
			p.finish(t, resp.Header.Get("request-id"), msgID)
		}
		if msg != nil && len(msg.droppedPaths) > 0 {
			msg.DroppedThinking = droppedRefs(req.Messages, p.oauth != nil, msg.droppedPaths)
		}
		return msg, err
	})
}

// send posts the body built for the advertised Claude Code version, adopting
// a newer version once if the OAuth backend rejects it. t is the Claude Code
// context of an OAuth request (nil with an API key).
func (p *anthropicProvider) send(ctx context.Context, t *ccTurn, fast bool, build func(version string) ([]byte, error)) (*http.Response, error) {
	endpoint := p.base + "/v1/messages"
	if p.oauth != nil {
		endpoint += "?beta=true"
	}
	for adopted := false; ; adopted = true {
		var token string
		if p.oauth != nil {
			tok, err := p.oauth.Token(ctx)
			if err != nil {
				return nil, err
			}
			token = tok
		}
		version := p.claudeCodeVersion()
		body, err := build(version)
		if err != nil {
			return nil, err
		}
		h := p.header(token, version, t)
		if fast {
			betas := h.Get("anthropic-beta")
			if betas != "" {
				betas += ","
			}
			h.Set("anthropic-beta", betas+fastBeta)
		}
		resp, err := postJSON(ctx, p.name, endpoint, body, h)
		if err == nil {
			return resp, nil
		}
		var ae *APIError
		if p.oauth == nil || adopted || !errors.As(err, &ae) || !p.adoptVersion(ae.Body) {
			return nil, err
		}
	}
}

var requiredVersionRe = regexp.MustCompile(`(?i)version (\d+\.\d+\.\d+) or newer is required`)

func (p *anthropicProvider) adoptVersion(msg string) bool {
	if os.Getenv("TEGO_CLAUDE_CODE_VERSION") != "" || !strings.Contains(msg, "claude_code_version_too_old") {
		return false
	}
	m := requiredVersionRe.FindStringSubmatch(msg)
	if m == nil || compareVersions(m[1], p.claudeCodeVersion()) <= 0 {
		return false
	}
	p.mu.Lock()
	p.ccVersion = m[1]
	p.mu.Unlock()
	return true
}

func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range 3 {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

func (p *anthropicProvider) claudeCodeVersion() string {
	if v := os.Getenv("TEGO_CLAUDE_CODE_VERSION"); v != "" {
		return v
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ccVersion != "" {
		return p.ccVersion
	}
	return ClaudeCodeVersion
}

func (p *anthropicProvider) sessionID(req *Request) string {
	if req.SessionID != "" {
		return req.SessionID
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fallbackSID == "" {
		p.fallbackSID = newUUID()
	}
	return p.fallbackSID
}

func (p *anthropicProvider) header(token, version string, t *ccTurn) http.Header {
	h := http.Header{}
	if p.oauth == nil {
		h.Set("Content-Type", "application/json")
		h.Set("Accept", "application/json")
		h.Set("x-api-key", p.key)
		h.Set("anthropic-version", anthropicVersion)
		h.Set("anthropic-beta", anthropicAPIKeyBeta)
		setHeaders(h, p.headers)
		return h
	}
	// Caller-supplied headers first; the Claude Code fingerprint wins.
	setHeaders(h, p.headers)
	h.Set("Authorization", "Bearer "+token)
	setClaudeCodeHeaders(h, t, version)
	return h
}

func stainlessArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "x86"
	}
	return "other::" + runtime.GOARCH
}

func stainlessOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "freebsd":
		return "FreeBSD"
	}
	return "Other::" + runtime.GOOS
}

// ---- request body ----

type aCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

var (
	ephemeral   = &aCacheControl{Type: "ephemeral"}
	ephemeral1h = &aCacheControl{Type: "ephemeral", TTL: "1h"}
)

type aSystemBlock struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	CacheControl *aCacheControl `json:"cache_control,omitempty"`
}

// aContent is one content block. Content is a string, or for tool results
// carrying images a []aContent of text and image blocks.
type aContent struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Thinking     *string         `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	Data         string          `json:"data,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      any             `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	Source       *aImageSource   `json:"source,omitempty"`
	ToolName     string          `json:"tool_name,omitempty"` // tool_reference
	CacheControl *aCacheControl  `json:"cache_control,omitempty"`
}

type aImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

func anthropicImage(b *Block) aContent {
	return aContent{Type: "image", Source: &aImageSource{Type: "base64", MediaType: b.MediaType, Data: b.Data}}
}

// anthropicToolResult renders a tool result; with images its content
// becomes an array of a text block followed by the image blocks. A result
// that loads deferred tools becomes tool_reference blocks for the ones in
// deferred (wire names; a reference to a tool missing from the request is
// rejected, so unknown ones fall back to the text).
func anthropicToolResult(b *Block, oauth bool, deferred map[string]bool) aContent {
	c := aContent{Type: "tool_result", ToolUseID: b.ToolUseID, IsError: b.IsError}
	var refs []aContent
	for _, name := range b.ToolRefs {
		if w := wireToolName(name, oauth); deferred[w] {
			refs = append(refs, aContent{Type: "tool_reference", ToolName: w})
		}
	}
	if len(refs) > 0 && !b.IsError {
		c.Content = refs
		return c
	}
	if len(b.Images) == 0 {
		if b.Content != "" {
			c.Content = b.Content
		}
		return c
	}
	parts := make([]aContent, 0, len(b.Images)+1)
	if b.Content != "" {
		parts = append(parts, aContent{Type: "text", Text: b.Content})
	}
	for i := range b.Images {
		if b.Images[i].Type == BlockImage {
			parts = append(parts, anthropicImage(&b.Images[i]))
		}
	}
	c.Content = parts
	return c
}

type aMessage struct {
	Role    Role       `json:"role"`
	Content []aContent `json:"content"`
}

type aTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	DeferLoading bool            `json:"defer_loading,omitempty"`
	CacheControl *aCacheControl  `json:"cache_control,omitempty"`
	// EagerInput streams this tool's input as the model writes it instead
	// of after validating each parameter (fine-grained tool streaming).
	EagerInput bool `json:"eager_input_streaming,omitempty"`
}

// eagerInputTools stream their arguments as they are written, so the UI can
// show a long file or replacement while it is produced: buffered, a write's
// content arrived only when complete. They're string-heavy, and malformed
// JSON (the price of skipping validation) is caught by ToolInput and sent
// back to the model as an error. Other tools keep validated streaming.
var eagerInputTools = map[string]bool{"write": true, "edit": true}

type aThinking struct {
	Type         string         `json:"type"`
	BudgetTokens int            `json:"budget_tokens,omitempty"`
	Display      string         `json:"display,omitempty"`
	BlockBinding *aBlockBinding `json:"block_binding,omitempty"`
}

// aBlockBinding: thinking blocks are bound to the exact prefix they were
// produced after. drop_block makes the API drop (rather than reject with a
// 400) earlier thinking whose prefix tego has since changed: elision,
// compaction, rewind, a changed tool set or system prompt, a resumed session.
type aBlockBinding struct {
	PrefixMismatchBehavior string `json:"prefix_mismatch_behavior"`
}

var dropMismatchedThinking = &aBlockBinding{PrefixMismatchBehavior: "drop_block"}

// anthropicBindsThinking reports models whose thinking blocks are bound to
// their prefix (Claude 5.5 and the Fable models).
func anthropicBindsThinking(model string) bool {
	return claudeGenAtLeast(model, 5, 5) || strings.Contains(strings.ToLower(model), "fable")
}

type aOutputConfig struct {
	Effort string `json:"effort"`
}

type aMetadata struct {
	UserID string `json:"user_id"`
}

type aContextManagement struct {
	Edits []aContextEdit `json:"edits"`
}

type aContextEdit struct {
	Type         string       `json:"type"`
	Trigger      *aTokenCount `json:"trigger,omitempty"`
	Keep         any          `json:"keep,omitempty"`
	ClearAtLeast *aTokenCount `json:"clear_at_least,omitempty"`
	ExcludeTools []string     `json:"exclude_tools,omitempty"`
}

type aTokenCount struct {
	Type  string `json:"type"` // input_tokens | tool_uses
	Value int    `json:"value"`
}

// keepAllThinking is Claude Code's context_management with adaptive
// thinking: earlier thinking blocks stay in context. It must come before
// other edits.
var keepAllThinking = aContextEdit{Type: "clear_thinking_20251015", Keep: "all"}

// clearToolUses is the server-side clearing edit for policy c.
func clearToolUses(c *ClearPolicy, oauth bool) aContextEdit {
	e := aContextEdit{Type: "clear_tool_uses_20250919",
		Trigger: &aTokenCount{Type: "tool_uses", Value: c.TriggerToolUses},
		Keep:    &aTokenCount{Type: "tool_uses", Value: c.Keep}}
	if c.ClearAtLeast > 0 {
		e.ClearAtLeast = &aTokenCount{Type: "input_tokens", Value: c.ClearAtLeast}
	}
	for _, n := range c.Exclude {
		e.ExcludeTools = append(e.ExcludeTools, wireToolName(n, oauth))
	}
	return e
}

type aDiagnostics struct {
	PreviousMessageID *string `json:"previous_message_id"`
}

// aRequest fields are in Claude Code's order. "system" must serialise as
// `"system":[{"type":"text","text":"x-anthropic-billing-header:...` for the
// OAuth billing patch to find it.
type aRequest struct {
	Model             string              `json:"model"`
	Messages          []aMessage          `json:"messages"`
	System            []aSystemBlock      `json:"system,omitempty"`
	Tools             []aTool             `json:"tools,omitempty"`
	Metadata          *aMetadata          `json:"metadata,omitempty"`
	MaxTokens         int                 `json:"max_tokens"`
	Thinking          *aThinking          `json:"thinking,omitempty"`
	ContextManagement *aContextManagement `json:"context_management,omitempty"`
	OutputConfig      *aOutputConfig      `json:"output_config,omitempty"`
	Diagnostics       *aDiagnostics       `json:"diagnostics,omitempty"`
	Speed             string              `json:"speed,omitempty"` // "fast" for fast mode
	Stream            bool                `json:"stream"`
}

var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// buildBody encodes req; t is the Claude Code context of an OAuth request
// (nil with an API key).
func (p *anthropicProvider) buildBody(req *Request, version string, t *ccTurn) ([]byte, error) {
	return p.buildBodySpeed(req, version, t, p.fastFor(req))
}

// buildBodySpeed is buildBody with the speed decided: after a fast mode
// rejection the same request goes out again at standard speed.
func (p *anthropicProvider) buildBodySpeed(req *Request, version string, t *ccTurn, fast bool) ([]byte, error) {
	oauth := t != nil
	r := aRequest{Model: req.Model, Stream: true}
	if fast {
		r.Speed = "fast"
	}
	cc := ephemeral
	if req.LongCache {
		// 1h entries cost 2x to write (vs 1.25x) but survive pauses between
		// prompts; every breakpoint uses the same TTL, as the API requires
		// longer TTLs to come first.
		cc = ephemeral1h
	}

	if oauth {
		r.System = append(r.System,
			aSystemBlock{Type: "text", Text: billingHeader(firstUserText(req.Messages), version, t)},
			aSystemBlock{Type: "text", Text: claudeCodeInstruction, CacheControl: cc},
		)
		r.Metadata = &aMetadata{UserID: claudeUserID(p.deviceID, t)}
		r.Diagnostics = &aDiagnostics{}
		if t.prevMsg != "" {
			r.Diagnostics.PreviousMessageID = &t.prevMsg
		}
	}
	if req.System != "" {
		r.System = append(r.System, aSystemBlock{Type: "text", Text: req.System})
	}
	if n := len(r.System); n > 0 {
		r.System[n-1].CacheControl = cc
	}

	tools, deferred := p.anthropicTools(req, oauth, cc)
	r.Tools = tools
	msgCC := cc
	if (req.Class == ClassCompaction || req.Class == ClassAuxiliary) && req.CachePrefix == 0 {
		// Sent once: caching their messages (a compaction's whole transcript)
		// costs 1.25-2x input for a write nothing reads (measured: a 161k
		// transcript went to the cache).
		msgCC = nil
	}
	if req.CachePrefix > 0 && req.CachePrefix < len(req.Messages) {
		r.Messages = anthropicMessagesCachedTo(req.Messages, req.CachePrefix, oauth, msgCC, deferred)
	} else {
		r.Messages = anthropicMessages(req.Messages, oauth, msgCC, deferred, nil)
	}

	r.Thinking, r.OutputConfig = anthropicThinking(req.Model, req.Thinking)
	r.MaxTokens = anthropicMaxTokens(req.Model, req.MaxTokens, req.Thinking, r.Thinking)
	if r.Thinking != nil && anthropicBindsThinking(req.Model) {
		r.Thinking.BlockBinding = dropMismatchedThinking
	}
	var edits []aContextEdit
	if oauth && r.Thinking != nil && r.Thinking.Type == "adaptive" {
		edits = append(edits, keepAllThinking)
	}
	if c := req.ClearToolResults; c != nil && p.ClearsToolResults(req.Model) {
		edits = append(edits, clearToolUses(c, oauth))
	}
	if len(edits) > 0 {
		r.ContextManagement = &aContextManagement{Edits: edits}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&r); err != nil {
		return nil, err
	}
	body := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	if oauth {
		patchBilling(body)
	}
	return body, nil
}

// anthropicTools encodes the tool list: deferred tools (when the model
// supports them) go last with defer_loading, outside the cached prefix, so
// the cache breakpoint sits on the last tool that is loaded up front. It
// returns the wire names of the deferred tools.
func (p *anthropicProvider) anthropicTools(req *Request, oauth bool, cc *aCacheControl) ([]aTool, map[string]bool) {
	if len(req.Tools) == 0 {
		return nil, nil
	}
	canDefer := p.DefersTools(req.Model)
	var loaded, later []aTool
	deferred := map[string]bool{}
	for _, t := range req.Tools {
		schema := t.Schema
		if len(schema) == 0 {
			schema = emptyObjectSchema
		}
		at := aTool{Name: wireToolName(t.Name, oauth), Description: t.Description, InputSchema: schema, EagerInput: eagerInputTools[t.Name]}
		if t.Deferred && canDefer {
			at.DeferLoading = true
			deferred[at.Name] = true
			later = append(later, at)
			continue
		}
		loaded = append(loaded, at)
	}
	if len(loaded) == 0 { // the API needs one tool loaded up front
		for i := range later {
			later[i].DeferLoading = false
		}
		loaded, later, deferred = later, nil, nil
	}
	loaded[len(loaded)-1].CacheControl = cc
	return append(loaded, later...), deferred
}

// anthropicMessages converts conversation history: thinking blocks without an
// Anthropic signature (from other providers, including Gemini/Claude
// thoughtSignatures from google-antigravity) are dropped, as are images outside user
// messages; empty messages are skipped and consecutive same-role messages
// merged. The last block of the final user message gets a cache breakpoint.
// origins (may be nil) receives, per wire message and content block, the
// block of msgs it came from.
func anthropicMessages(msgs []Message, oauth bool, cc *aCacheControl, deferred map[string]bool, origins *[][]BlockRef) []aMessage {
	out := make([]aMessage, 0, len(msgs))
	var from [][]BlockRef
	for i := range msgs {
		m := &msgs[i]
		var content []aContent
		var src []BlockRef
		for j := range m.Blocks {
			if n := len(content); n > len(src) {
				src = append(src, BlockRef{i, j - 1})
			}
			b := &m.Blocks[j]
			switch b.Type {
			case BlockText:
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				content = append(content, aContent{Type: "text", Text: b.Text})
			case BlockThinking:
				if m.Role != RoleAssistant {
					continue
				}
				if b.Redacted != "" {
					content = append(content, aContent{Type: "redacted_thinking", Data: b.Redacted})
				} else if b.Signature != "" && m.Provider != AntigravityKind {
					text := b.Text
					content = append(content, aContent{Type: "thinking", Thinking: &text, Signature: b.Signature})
				}
			case BlockToolUse:
				input := b.Input
				if len(bytes.TrimSpace(input)) == 0 {
					input = json.RawMessage("{}")
				}
				content = append(content, aContent{Type: "tool_use", ID: b.ID, Name: wireToolName(b.Name, oauth), Input: input})
			case BlockToolResult:
				content = append(content, anthropicToolResult(b, oauth, deferred))
			case BlockImage:
				if m.Role == RoleUser {
					content = append(content, anthropicImage(b))
				}
			}
		}
		if n := len(content); n > len(src) {
			src = append(src, BlockRef{i, len(m.Blocks) - 1})
		}
		if len(content) == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			out[n-1].Content = append(out[n-1].Content, content...)
			from[n-1] = append(from[n-1], src...)
			continue
		}
		out = append(out, aMessage{Role: m.Role, Content: content})
		from = append(from, src)
	}
	if origins != nil {
		*origins = from
	}
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role == RoleUser {
			c := out[i].Content
			c[len(c)-1].CacheControl = cc
			break
		}
	}
	return out
}

// anthropicMessagesCachedTo encodes msgs with the one message breakpoint
// where a request of just msgs[:prefix] had it, on the last block those
// messages make, rather than at the end: the request reads that cache
// entry exactly, and what follows is billed as plain input, not written
// to a cache nothing will read.
func anthropicMessagesCachedTo(msgs []Message, prefix int, oauth bool, cc *aCacheControl, deferred map[string]bool) []aMessage {
	var origins [][]BlockRef
	out := anthropicMessages(msgs, oauth, nil, deferred, &origins)
	for i := len(out) - 1; i >= 0; i-- {
		for k := len(out[i].Content) - 1; k >= 0; k-- {
			if k < len(origins[i]) && origins[i][k].Msg < prefix {
				out[i].Content[k].CacheControl = cc
				return out
			}
		}
	}
	return out
}

// Server tools keep their names; everything else gets the Claude Code prefix.
var anthropicServerTools = map[string]bool{"web_search": true, "code_execution": true, "text_editor": true, "computer": true}

func wireToolName(name string, oauth bool) string {
	if !oauth || anthropicServerTools[strings.ToLower(name)] {
		return name
	}
	return oauthToolPrefix + name
}

func stripToolPrefix(name string, oauth bool) string {
	if oauth && strings.HasPrefix(name, oauthToolPrefix) {
		return name[len(oauthToolPrefix):]
	}
	return name
}

// anthropicLegacyThinking reports models that only support budget-based
// extended thinking (no adaptive thinking / effort).
func anthropicLegacyThinking(model string) bool {
	m := strings.ToLower(model)
	if strings.HasPrefix(m, "claude-3") {
		return true
	}
	for _, s := range []string{"-4-0", "-4-1", "-4-5", "opus-4-2025", "sonnet-4-2025"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

var claudeGenRe = regexp.MustCompile(`(?:opus|sonnet|haiku)-(\d+)(?:-(\d{1,2}))?(?:$|\D)`)

// claudeGenAtLeast reports whether the model generation (e.g. opus-4-7 ->
// 4.7) is at least major.minor. Unparseable ids return false.
func claudeGenAtLeast(model string, major, minor int) bool {
	m := claudeGenRe.FindStringSubmatch(strings.ToLower(model))
	if m == nil {
		return false
	}
	x, _ := strconv.Atoi(m[1])
	y := 0
	if m[2] != "" {
		y, _ = strconv.Atoi(m[2])
	}
	return x > major || (x == major && y >= minor)
}

var thinkingBudgets = map[string]int{ThinkingLow: 4000, ThinkingMedium: 12000, ThinkingHigh: 24000,
	ThinkingXHigh: 32000, ThinkingMax: 48000}

// anthropicEffort is the effort level model accepts for level, from the
// table in platform.claude.com/docs/en/build-with-claude/effort:
//   - max: Opus and Sonnet 4.6+, Haiku 5.5+, every Fable and Mythos model
//   - xhigh: Opus 4.7+, Sonnet and Haiku 5+ (Haiku from 5.5), Fable and
//     Mythos 5+ (not Mythos Preview)
//
// "Not every model that supports max supports xhigh" (Opus and Sonnet 4.6).
func anthropicEffort(model, level string) string {
	m := strings.ToLower(model)
	family := func(f string) bool { return strings.Contains(m, f) }
	frontier := family("fable") || family("mythos")
	levels := []string{ThinkingLow, ThinkingMedium, ThinkingHigh}
	switch {
	case frontier,
		(family("opus") || family("sonnet")) && claudeGenAtLeast(model, 4, 6),
		family("haiku") && claudeGenAtLeast(model, 5, 5):
		levels = append(levels, ThinkingMax)
	}
	switch {
	case frontier && !family("preview"),
		family("opus") && claudeGenAtLeast(model, 4, 7),
		family("sonnet") && claudeGenAtLeast(model, 5, 0),
		family("haiku") && claudeGenAtLeast(model, 5, 5):
		levels = append(levels, ThinkingXHigh)
	}
	return clampThinking(level, levels...)
}

func anthropicThinking(model, level string) (*aThinking, *aOutputConfig) {
	budget, ok := thinkingBudgets[level]
	if !ok {
		return nil, nil
	}
	if anthropicLegacyThinking(model) {
		return &aThinking{Type: "enabled", BudgetTokens: budget}, nil
	}
	t := &aThinking{Type: "adaptive"}
	if claudeGenAtLeast(model, 4, 7) {
		// Newer models omit thinking text unless asked for a summary.
		t.Display = "summarized"
	}
	return t, &aOutputConfig{Effort: anthropicEffort(model, level)}
}

// anthropicOutputCap is the most output model can write in one response
// (the models overview), or 0 when it isn't known.
func anthropicOutputCap(model string) int {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "claude-3-5"):
		return 8192
	case strings.HasPrefix(m, "claude-3-") && !strings.HasPrefix(m, "claude-3-7"):
		return 4096
	case strings.Contains(m, "fable") || strings.Contains(m, "mythos"),
		claudeGenAtLeast(m, 5, 0),
		strings.Contains(m, "opus") && claudeGenAtLeast(m, 4, 6):
		return 128000
	case strings.Contains(m, "opus-4") && !claudeGenAtLeast(m, 4, 5):
		return 32000 // Opus 4 and 4.1
	case strings.HasPrefix(m, "claude-"):
		return 64000 // 3.7, and the other 4.x models
	}
	return 0
}

// anthropicMaxTokens is a request's max_tokens: requested (the config's
// max_output_tokens) when set, or a default by model and thinking level.
// Thinking and the answer share it, so it leaves room for both: a long
// think that used it up would end the response before any answer or tool
// call. A response is billed by what it writes, not by its limit.
func anthropicMaxTokens(model string, requested int, level string, thinking *aThinking) int {
	limit := anthropicOutputCap(model)
	deep := ThinkingRank(level) >= ThinkingRank(ThinkingXHigh)
	mt := requested
	if mt <= 0 {
		switch {
		case limit == 0: // not a Claude model we know: the old defaults
			mt = 32000
			if deep {
				mt = 64000
			}
		case limit >= 128000:
			mt = 64000
			if deep {
				mt = limit
			}
		case thinking == nil && strings.Contains(strings.ToLower(model), "haiku"):
			mt = 16000
		case deep:
			mt = 64000 // the docs: at least 64k at xhigh and max
		default:
			mt = 32000
		}
	}
	if thinking != nil && thinking.BudgetTokens > 0 && mt <= thinking.BudgetTokens {
		mt = thinking.BudgetTokens + 4096
	}
	if limit > 0 {
		mt = min(mt, limit)
	}
	return mt
}

// ---- response stream ----

type aUsage struct {
	InputTokens   int    `json:"input_tokens"`
	OutputTokens  int    `json:"output_tokens"`
	CacheRead     int    `json:"cache_read_input_tokens"`
	CacheCreation int    `json:"cache_creation_input_tokens"`
	Speed         string `json:"speed"` // "fast" or "standard"
}

// aInputTransformation reports a change the API made to the request, e.g.
// {"type":"thinking_dropped","reason":"prefix_binding_mismatch"}.
type aInputTransformation struct {
	Type   string `json:"type"`
	Path   string `json:"path"` // e.g. messages.3.content.0
	Reason string `json:"reason"`
}

// droppedRefs maps wire paths ("messages.M.content.C") of dropped thinking
// blocks back to msgs, whose consecutive same-role messages and skipped
// blocks make wire indices differ from history indices.
func droppedRefs(msgs []Message, oauth bool, paths []string) []BlockRef {
	var origins [][]BlockRef
	anthropicMessages(msgs, oauth, nil, nil, &origins)
	var refs []BlockRef
	for _, p := range paths {
		var m, c int
		if _, err := fmt.Sscanf(p, "messages.%d.content.%d", &m, &c); err != nil || m >= len(origins) || c >= len(origins[m]) {
			continue
		}
		refs = append(refs, origins[m][c])
	}
	return refs
}

type aAppliedEdits struct {
	AppliedEdits []struct {
		Type               string `json:"type"`
		ClearedToolUses    int    `json:"cleared_tool_uses"`
		ClearedInputTokens int    `json:"cleared_input_tokens"`
	} `json:"applied_edits"`
}

// noteEdits records server-side context edits on msg.
func noteEdits(msg *Message, ts []aInputTransformation, cm *aAppliedEdits) {
	for _, t := range ts {
		if t.Type == "thinking_dropped" {
			msg.ThinkingDropped++
			msg.droppedPaths = append(msg.droppedPaths, t.Path)
		}
	}
	if cm == nil {
		return
	}
	for _, e := range cm.AppliedEdits {
		if strings.HasPrefix(e.Type, "clear_tool_uses") {
			msg.ClearedToolUses += e.ClearedToolUses
			msg.ClearedTokens += e.ClearedInputTokens
		}
	}
}

type aStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		ID                   string                 `json:"id"`
		Usage                aUsage                 `json:"usage"`
		InputTransformations []aInputTransformation `json:"input_transformations"`
	} `json:"message"`
	InputTransformations []aInputTransformation `json:"input_transformations"`
	ContextManagement    *aAppliedEdits         `json:"context_management"`
	ContentBlock         *struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
		Data      string `json:"data"`
		ID        string `json:"id"`
		Name      string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *aUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func anthropicStopReason(s string) string {
	switch s {
	case "tool_use":
		return "tool_use"
	case "max_tokens":
		return "max_tokens"
	case "model_context_window_exceeded":
		return StopContextFull
	}
	return "end_turn"
}

func anthropicErrorStatus(typ string) int {
	switch typ {
	case "overloaded_error":
		return 529
	case "rate_limit_error":
		return 429
	case "api_error", "internal_server_error":
		return 500
	case "authentication_error":
		return 401
	case "permission_error":
		return 403
	case "not_found_error":
		return 404
	case "request_too_large":
		return 413
	}
	return 400
}

// parseAnthropicStream reads one streamed message; msgID (may be nil)
// receives the API's message id.
func parseAnthropicStream(ctx context.Context, body io.Reader, oauth bool, emit func(Event), msgID *string) (msg *Message, err error) {
	msg = &Message{Role: RoleAssistant, Usage: &Usage{}}
	acc := newBlockAccumulator()
	blockOf := map[int]int{} // anthropic content index -> msg.Blocks index
	defer func() { acc.finalize(msg, err != nil) }()
	sse := newSSEReader(body)
	var ev aStreamEvent
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
		ev = aStreamEvent{}
		if err := json.Unmarshal(e.Data, &ev); err != nil {
			return msg, fmt.Errorf("anthropic: bad stream event %q: %w", truncate(string(e.Data), 200), err)
		}
		switch ev.Type {
		case "message_start":
			emit(Event{Type: EvStarted})
			if ev.Message != nil {
				u := ev.Message.Usage
				msg.Usage.Input = u.InputTokens
				msg.Usage.Output = u.OutputTokens
				msg.Usage.CacheRead = u.CacheRead
				msg.Usage.CacheWrite = u.CacheCreation
				msg.Usage.Fast = u.Speed == "fast"
				if msgID != nil {
					*msgID = ev.Message.ID
				}
				noteEdits(msg, ev.Message.InputTransformations, nil)
			}
			noteEdits(msg, ev.InputTransformations, ev.ContextManagement)
		case "content_block_start":
			cb := ev.ContentBlock
			if cb == nil {
				continue
			}
			var b Block
			text := ""
			switch cb.Type {
			case "text":
				b, text = Block{Type: BlockText}, cb.Text
			case "thinking":
				b, text = Block{Type: BlockThinking, Signature: cb.Signature}, cb.Thinking
			case "redacted_thinking":
				b = Block{Type: BlockThinking, Redacted: cb.Data}
			case "tool_use":
				b = Block{Type: BlockToolUse, ID: cb.ID, Name: stripToolPrefix(cb.Name, oauth)}
			default:
				continue
			}
			bi := len(msg.Blocks)
			blockOf[ev.Index] = bi
			msg.Blocks = append(msg.Blocks, b)
			acc.addText(bi, text)
			switch {
			case b.Type == BlockToolUse:
				emit(Event{Type: EvToolCall, ToolName: b.Name, ToolID: b.ID})
			case text == "":
			case b.Type == BlockText:
				emit(Event{Type: EvText, Text: text})
			default:
				emit(Event{Type: EvThinking, Text: text})
			}
		case "content_block_delta":
			bi, ok := blockOf[ev.Index]
			if !ok || ev.Delta == nil {
				continue
			}
			b := &msg.Blocks[bi]
			switch ev.Delta.Type {
			case "text_delta":
				acc.addText(bi, ev.Delta.Text)
				emit(Event{Type: EvText, Text: ev.Delta.Text})
			case "thinking_delta":
				acc.addText(bi, ev.Delta.Thinking)
				emit(Event{Type: EvThinking, Text: ev.Delta.Thinking})
			case "signature_delta":
				b.Signature += ev.Delta.Signature
			case "input_json_delta":
				acc.addArgs(bi, ev.Delta.PartialJSON)
				if b := &msg.Blocks[bi]; b.Type == BlockToolUse && ev.Delta.PartialJSON != "" {
					emit(Event{Type: EvToolDelta, ToolName: b.Name, ToolID: b.ID, Text: ev.Delta.PartialJSON})
				}
			}
		case "content_block_stop":
			if bi, ok := blockOf[ev.Index]; ok && msg.Blocks[bi].Type == BlockToolUse {
				acc.finishTool(msg, bi, nil)
			}
		case "message_delta":
			noteEdits(msg, ev.InputTransformations, ev.ContextManagement)
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				msg.StopReason = anthropicStopReason(ev.Delta.StopReason)
			}
			if u := ev.Usage; u != nil {
				if u.InputTokens > 0 {
					msg.Usage.Input = u.InputTokens
				}
				if u.CacheRead > 0 {
					msg.Usage.CacheRead = u.CacheRead
				}
				if u.CacheCreation > 0 {
					msg.Usage.CacheWrite = u.CacheCreation
				}
				if u.OutputTokens > 0 {
					msg.Usage.Output = u.OutputTokens
				}
				if u.Speed != "" {
					msg.Usage.Fast = u.Speed == "fast"
				}
			}
		case "message_stop":
			if msg.StopReason == "" {
				msg.StopReason = "end_turn"
			}
			return msg, nil
		case "error":
			typ, text := "error", string(e.Data)
			if ev.Error != nil {
				typ, text = ev.Error.Type, ev.Error.Type+": "+ev.Error.Message
			}
			return msg, &APIError{Provider: "anthropic", Status: anthropicErrorStatus(typ), Body: text}
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
