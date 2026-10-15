// Package llm defines the provider-neutral conversation model and the
// Provider interface implemented by each backend.
package llm

import (
	"context"
	"encoding/json"
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
	// DroppedThinking locates the dropped blocks in Request.Messages, so
	// the caller can remove them from its history: otherwise they are sent,
	// and dropped, again on every request.
	DroppedThinking []BlockRef `json:"-"`
	// GenOutput is the part of Usage.Output generated after the stream
	// started, for providers that send nothing until hidden thinking is
	// done (Antigravity): output speed is timed over it. 0 = all of Output.
	GenOutput    int      `json:"-"`
	droppedPaths []string // wire paths of the dropped blocks (Anthropic)
}

// BlockRef is block Block of message Msg in a request's messages.
type BlockRef struct{ Msg, Block int }

// Usage token counts. Input excludes cache reads/writes.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
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

// Thinking levels.
const (
	ThinkingOff    = "off"
	ThinkingLow    = "low"
	ThinkingMedium = "medium"
	ThinkingHigh   = "high"
)

type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int    // 0 = provider default
	Thinking  string // off|low|medium|high ("" = off)
	SessionID string // stable per session; used for prompt-cache keys / headers
	LongCache bool   // keep the prompt cache for an hour instead of 5 minutes (Anthropic)
	Class     string // what the request is for: ClassMain ("" too), ClassSubagent, ClassCompaction, ClassAuxiliary
	// ClearToolResults asks providers whose ClearsToolResults reports true
	// to clear old tool results server-side; nil = never.
	ClearToolResults *ClearPolicy
}

// ClearPolicy is server-side clearing of old tool results: once the prompt
// exceeds Trigger tokens, the oldest tool results are replaced by a
// placeholder, at least ClearAtLeast tokens at a time (a clearing pass
// rewrites the prompt cache from the first cleared result on, so small
// passes cost more than they save), keeping the last Keep tool uses and the
// results of the Exclude tools.
type ClearPolicy struct {
	Trigger, ClearAtLeast, Keep int
	Exclude                     []string
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
)

type Event struct {
	Type     EventType
	Text     string
	ToolName string
	ToolID   string
}

// Provider streams one assistant turn. onEvent is called synchronously for
// deltas; the returned Message is the complete assistant message (Role
// assistant, Provider/Model/Usage/StopReason populated). StopReason is
// normalised to one of: "end_turn", "tool_use", "max_tokens", "error", "aborted".
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
