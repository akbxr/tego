// Package llm defines the provider-neutral conversation model and the
// Provider interface implemented by each backend.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type BlockType string

const (
	BlockText       BlockType = "text"
	BlockThinking   BlockType = "thinking"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	BlockImage      BlockType = "image"
)

// Block is one content block. Fields used depend on Type:
//   - text:        Text, Signature (Gemini thoughtSignature)
//   - thinking:    Text, Signature (Anthropic signature or Gemini thoughtSignature),
//     Redacted (Anthropic redacted_thinking data),
//     Encrypted (OpenAI Codex reasoning item: encrypted_content), ItemID (Codex reasoning item id)
//   - tool_use:    ID, Name, Input, Signature (Gemini thoughtSignature)
//   - tool_result: ToolUseID, Content, IsError, Images (image blocks returned by the tool),
//     ToolRefs (deferred tools the result loads; providers that defer tools
//     send references instead of Content)
//   - image:       MediaType (image/png|jpeg|gif|webp), Data (base64, no data: prefix);
//     appears in user messages and inside tool_result Images
type Block struct {
	Type      BlockType       `json:"type"`
	Text      string          `json:"text,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Redacted  string          `json:"redacted,omitempty"`
	Encrypted string          `json:"encrypted,omitempty"`
	ItemID    string          `json:"item_id,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Images    []Block         `json:"images,omitempty"`
	ToolRefs  []string        `json:"tool_refs,omitempty"`
	MediaType string          `json:"media_type,omitempty"`
	Data      string          `json:"data,omitempty"`
}

// Message is a single conversation turn. Tool results are sent as a user
// message whose blocks are all tool_result blocks.
type Message struct {
	Role       Role    `json:"role"`
	Blocks     []Block `json:"content"`
	Provider   string  `json:"provider,omitempty"`
	Model      string  `json:"model,omitempty"`
	Usage      *Usage  `json:"usage,omitempty"`
	StopReason string  `json:"stop_reason,omitempty"`
	// Server-side context edits applied to the request that produced this
	// message (Anthropic): old tool uses cleared, and earlier thinking
	// blocks dropped because the history before them changed.
	ClearedToolUses int `json:"cleared_tool_uses,omitempty"`
	ClearedTokens   int `json:"cleared_tokens,omitempty"`
	ThinkingDropped int `json:"thinking_dropped,omitempty"`
	// ClearTrigger is the ClearPolicy.TriggerToolUses the request was sent
	// with, so a resumed session sends the same one (any change moves what
	// the server clears, and so rewrites the prompt cache).
	ClearTrigger int `json:"clear_trigger,omitempty"`
	// DroppedThinking locates the dropped blocks in Request.Messages, so
	// the caller can remove them from its history: otherwise they are sent,
	// and dropped, again on every request.
	DroppedThinking []BlockRef `json:"-"`
	// GenOutput is the part of Usage.Output generated after the stream
	// started, for providers that send nothing until hidden thinking is
	// done (Antigravity): output speed is timed over it. 0 = all of Output.
	GenOutput    int      `json:"-"`
	droppedPaths []string // wire paths of the dropped blocks (Anthropic)
	// FastNote says why a request that asked for fast mode ran at standard
	// speed (rate limited, not enabled for the account); "" otherwise. Set
	// on the request where that started, not on every one after.
	FastNote string `json:"-"`
}

// BlockRef is block Block of message Msg in a request's messages.
type BlockRef struct{ Msg, Block int }

// Usage token counts. Input excludes cache reads/writes.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
	// Fast is set when the response ran in fast mode, which bills every
	// token class at twice the standard price.
	Fast bool `json:"fast,omitempty"`
}

// ContextTokens is the prompt size the request consumed.
func (u *Usage) ContextTokens() int {
	if u == nil {
		return 0
	}
	return u.Input + u.CacheRead + u.CacheWrite + u.Output
}

// Tool is a tool definition advertised to the model. Schema is a JSON Schema object.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"input_schema"`
	// Deferred tools stay out of the model's context until a tool result
	// references them (Block.ToolRefs); only sent to providers whose
	// DefersTools reports true.
	Deferred bool `json:"-"`
}

// Thinking levels. xhigh and max exist on the newest models only (Claude
// Opus 4.7+, the 4.6 models for max; GPT-5.2+ for xhigh, GPT-5.6+ for max);
// each provider clamps a level a model lacks to the nearest one below.
const (
	ThinkingOff    = "off"
	ThinkingLow    = "low"
	ThinkingMedium = "medium"
	ThinkingHigh   = "high"
	ThinkingXHigh  = "xhigh"
	ThinkingMax    = "max"
)

// ThinkingLevels lists the levels from least to most thinking.
var ThinkingLevels = []string{ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh, ThinkingXHigh, ThinkingMax}

// ThinkingRank orders levels (off 0 … max 5); unknown levels are -1.
func ThinkingRank(level string) int {
	for i, l := range ThinkingLevels {
		if l == level {
			return i
		}
	}
	return -1
}

// EffectiveThinking is the level model runs at when level is asked for on
// provider p: providers clamp xhigh and max to what a model supports. It
// only differs from level for those two; "" when unknown.
func EffectiveThinking(p Provider, model, level string) string {
	if ThinkingRank(level) <= ThinkingRank(ThinkingHigh) {
		return level
	}
	if pool, ok := p.(*Pool); ok && len(pool.members) > 0 {
		p = pool.members[0].Provider
	}
	switch p.(type) {
	case *anthropicProvider:
		if anthropicLegacyThinking(model) {
			return ThinkingHigh
		}
		return anthropicEffort(model, level)
	case *codexProvider, *openaiProvider:
		if e := openaiEffort(model, level); e != "" {
			return e
		}
	case *antigravityProvider:
		return ThinkingHigh
	}
	return ""
}

// clampThinking returns level, or the highest of the levels a model
// supports below it ("" when none is).
func clampThinking(level string, supported ...string) string {
	r := ThinkingRank(level)
	best, bestRank := "", -1
	for _, s := range supported {
		if sr := ThinkingRank(s); sr <= r && sr > bestRank {
			best, bestRank = s, sr
		}
	}
	return best
}

type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int    // 0 = provider default
	Thinking  string // off|low|medium|high|xhigh|max ("" = off)
	SessionID string // stable per session; used for prompt-cache keys / headers
	LongCache bool   // keep the prompt cache for an hour instead of 5 minutes (Anthropic)
	// Fast asks for fast mode where the model has it (Claude Opus 4.8, 5
	// and 5.5 on Anthropic): up to 2.5x the output speed at twice the price.
	// Elsewhere it is ignored.
	Fast  bool
	Class string // what the request is for: ClassMain ("" too), ClassSubagent, ClassCompaction, ClassAuxiliary
	// ClearToolResults asks providers whose ClearsToolResults reports true
	// to clear old tool results server-side; nil = never.
	ClearToolResults *ClearPolicy
	// CachePrefix > 0 says the first CachePrefix messages, and everything
	// before them, repeat an earlier request byte for byte (a compaction
	// that reads the conversation's cache). Providers with explicit cache
	// breakpoints put the message breakpoint where that request had it, so
	// this one reads it exactly and writes nothing new.
	CachePrefix int
}

// ClearPolicy is server-side clearing of old tool results. Reading the
// history from the start, each time more than TriggerToolUses tool uses
// have not been cleared, the results of all but the last Keep of them are
// replaced by a placeholder, if that frees at least ClearAtLeast tokens.
// The results of the Exclude tools are left alone. With the same policy,
// what is cleared does not change as the conversation grows until the
// trigger is passed again (measured), so the prompt cache keeps hitting.
type ClearPolicy struct {
	TriggerToolUses, ClearAtLeast, Keep int
	Exclude                             []string
}

// ContextFeatures is implemented by providers with server-side context
// features for some models.
type ContextFeatures interface {
	ClearsToolResults(model string) bool // honours Request.ClearToolResults
	DefersTools(model string) bool       // honours Tool.Deferred and Block.ToolRefs
}

// ClearsToolResults reports whether p clears old tool results server-side for model.
func ClearsToolResults(p Provider, model string) bool {
	f, ok := p.(ContextFeatures)
	return ok && f.ClearsToolResults(model)
}

// DefersTools reports whether p supports deferred tools for model.
func DefersTools(p Provider, model string) bool {
	f, ok := p.(ContextFeatures)
	return ok && f.DefersTools(model)
}

// CacheExpiry is implemented by providers whose prompt cache lives for an
// exact time after its last use: Anthropic's cache_control TTL (5 minutes,
// or 1 hour with Request.LongCache). The client can then tell for sure when
// it has expired. Implicit caches (OpenAI, Gemini, Antigravity, xAI,
// DeepSeek) are best-effort: an entry may outlive or die before any fixed
// time, so a guess could throw away a cache that is still warm.
type CacheExpiry interface {
	KnownCacheTTL(model string) bool
}

// KnownCacheTTL reports whether p's prompt cache for model expires exactly
// when its TTL says.
func KnownCacheTTL(p Provider, model string) bool {
	f, ok := p.(CacheExpiry)
	return ok && f.KnownCacheTTL(model)
}

// Prewarmer is implemented by providers that can read a request's prompt
// into the cache without a response (Anthropic's max_tokens 0): reading a
// cached prefix keeps it alive for another TTL, at the cache read price.
// The request is built as the next one would be, with Class ClassKeepAlive
// and CachePrefix at the end of the history it keeps; its last message is
// a placeholder the model reads but does not answer.
type Prewarmer interface {
	CanPrewarm(model string) bool
	Prewarm(ctx context.Context, req *Request) (Usage, error)
}

// CanPrewarm reports whether p can keep model's cache alive.
func CanPrewarm(p Provider, model string) bool {
	w, ok := p.(Prewarmer)
	return ok && w.CanPrewarm(model)
}

// Prewarm reads req's prompt into p's cache; an error when p cannot.
func Prewarm(ctx context.Context, p Provider, req *Request) (Usage, error) {
	w, ok := p.(Prewarmer)
	if !ok {
		return Usage{}, fmt.Errorf("%s cannot keep its prompt cache alive", p.Name())
	}
	return w.Prewarm(ctx, req)
}

// ImplicitCache is implemented by providers whose prompt cache is implicit
// and best-effort (Gemini, OpenAI, DeepSeek, xAI): it keeps a repeated prefix
// for an unknown time, without markers. Rewriting old history to shrink it
// throws that cache away, so harnesses leave the history alone there (pi
// never prunes, omp only prunes the part not cached yet).
type ImplicitCache interface {
	ImplicitPrefixCache(model string) bool
}

// HasImplicitCache reports whether p caches prompt prefixes implicitly.
func HasImplicitCache(p Provider, model string) bool {
	f, ok := p.(ImplicitCache)
	return ok && f.ImplicitPrefixCache(model)
}

// Request classes, as Claude Code labels its requests.
const (
	ClassMain       = "main"
	ClassSubagent   = "subagent"
	ClassCompaction = "compaction"
	ClassAuxiliary  = "auxiliary" // side requests: titles, web search
	ClassKeepAlive  = "keepalive" // a prompt read into the cache while idle (Prewarmer)
)

type EventType int

const (
	EvText     EventType = iota // Text = delta
	EvThinking                  // Text = delta
	EvToolCall                  // ToolName, ToolID: model started emitting a tool call
	// EvStarted marks that the server began generating (Anthropic
	// message_start, the first chunk elsewhere). It times the output speed:
	// a model can think for seconds before its first visible delta. It is
	// not forwarded to onEvent and does not count as delivered for retries.
	EvStarted
	// EvToolDelta carries a piece of a tool call's JSON arguments as the
	// model writes them: ToolName, ToolID, Text = the raw JSON chunk. Sent
	// after the call's EvToolCall, so it is never the first delivered event.
	EvToolDelta
)

type Event struct {
	Type     EventType
	Text     string
	ToolName string
	ToolID   string
}

// StopContextFull: the response ended because the context window filled
// up, not at its own output limit.
const StopContextFull = "context_full"

// Provider streams one assistant turn. onEvent is called synchronously for
// deltas; the returned Message is the complete assistant message (Role
// assistant, Provider/Model/Usage/StopReason populated). StopReason is
// normalised to one of: "end_turn", "tool_use", "max_tokens", "error", "aborted",
// or StopContextFull.
type Provider interface {
	Name() string
	Stream(ctx context.Context, req *Request, onEvent func(Event)) (*Message, error)
}

// TokenSource yields a valid bearer token, refreshing if needed.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// AccountIDSource is implemented by token sources that know the
// provider-side account id (Anthropic's account uuid); "" when unknown.
type AccountIDSource interface {
	AccountID(ctx context.Context) string
}

// ProviderConfig describes one configured provider instance.
type ProviderConfig struct {
	Name    string            `json:"-"`
	Kind    string            `json:"kind"` // "anthropic" | "openai" | "openai-codex" | "google-antigravity"
	BaseURL string            `json:"base_url,omitempty"`
	APIKey  string            `json:"api_key,omitempty"`
	KeyEnv  string            `json:"api_key_env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// OAuth, when set, supplies bearer tokens (subscription login) and
	// switches the provider into its OAuth request shape.
	OAuth TokenSource `json:"-"`
	// ProjectID is the Cloud Code Assist project of a google-antigravity
	// login (taken from the stored credential).
	ProjectID string `json:"-"`
	// DeviceID is this installation's stable random id (64 hex chars),
	// sent as metadata.user_id.device_id on Claude subscription requests.
	DeviceID string `json:"-"`
}
