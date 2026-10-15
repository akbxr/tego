package llm

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Claude subscription (OAuth) requests carry Claude Code's fingerprint:
// headers, beta list, billing header and metadata as Claude Code 2.1.296
// sends them (taken from its own traffic, interactive mode).

const (
	ClaudeCodeVersion     = "2.1.296" // advertised unless TEGO_CLAUDE_CODE_VERSION or a server rejection says otherwise
	claudeCodeSDKVersion  = "0.128.0" // @anthropic-ai/sdk bundled with that release
	claudeCodeInstruction = "You are Claude Code, Anthropic's official CLI for Claude."
	claudeDispatchID      = "v2d"
	claudeSubagentType    = "general-purpose" // Claude Code's built-in general subagent

	billingHeaderPrefix        = "x-anthropic-billing-header:"
	billingPlaceholder         = "cch=00000"
	billingSeed         uint64 = 0x4d659218e32a3268
	billingMaxDistance         = 150
	oauthToolPrefix            = "_"
)

var (
	// claudeAgentBetas go with conversation requests (main thread,
	// subagents, compaction). No fine-grained-tool-streaming: without it the
	// API validates tool arguments as JSON.
	claudeAgentBetas = strings.Join([]string{
		"claude-code-20250219", "oauth-2025-04-20", "interleaved-thinking-2025-05-14",
		"thinking-token-count-2026-05-13", "context-management-2025-06-27", "prompt-caching-scope-2026-01-05",
		"mid-conversation-system-2026-04-07", "per-turn-control-2026-07-01", "mid-conversation-tool-changes-2026-07-01",
		"inline-tools-2026-09-15", "advisor-tool-2026-03-01", "advanced-tool-use-2025-11-20", "effort-2025-11-24",
		"dangerous-tool-use-2026-09-03", "thinking-binding-controls-2026-08-01", "thinking-display-updates-2026-08-18",
		"afk-mode-2026-01-31", "extended-cache-ttl-2025-04-11", "cache-diagnosis-2026-04-07",
	}, ",")
	// claudeUtilityBetas go with side requests (titles, web search).
	claudeUtilityBetas = strings.Join([]string{
		"oauth-2025-04-20", "interleaved-thinking-2025-05-14", "redact-thinking-2026-02-12",
		"thinking-token-count-2026-05-13", "context-management-2025-06-27", "prompt-caching-scope-2026-01-05",
	}, ",")
)

// ccTurn is the Claude Code context of one request.
type ccTurn struct {
	sessionID   string
	class       string // ClassMain, ClassSubagent, ClassCompaction, ClassAuxiliary
	accountID   string // Anthropic account uuid ("" if unknown)
	promptID    string // main thread: id of the prompt being answered
	promptIndex int    // main thread: 1-based number of that prompt
	prevReq     string // main thread: request-id of the previous request
	prevMsg     string // id of the previous assistant message in this session
}

// ccSession is what a session remembers between requests.
type ccSession struct {
	prompts  int
	promptID string
	prevReq  string
	prevMsg  string
}

// humanPrompts counts user messages carrying text (tool results don't).
func humanPrompts(msgs []Message) int {
	n := 0
	for i := range msgs {
		if msgs[i].Role != RoleUser {
			continue
		}
		for _, b := range msgs[i].Blocks {
			if b.Type == BlockText && strings.TrimSpace(b.Text) != "" {
				n++
				break
			}
		}
	}
	return n
}

// turn builds the context of the next request of session sid. A main
// thread request with a new prompt gets a fresh prompt id.
func (p *anthropicProvider) turn(ctx context.Context, sid, class string, msgs []Message) *ccTurn {
	if class == "" {
		class = ClassMain
	}
	t := &ccTurn{sessionID: sid, class: class}
	if src, ok := p.oauth.(AccountIDSource); ok {
		t.accountID = src.AccountID(ctx)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions == nil {
		p.sessions = map[string]*ccSession{}
	}
	s := p.sessions[sid]
	if s == nil {
		s = &ccSession{}
		p.sessions[sid] = s
	}
	t.prevMsg = s.prevMsg
	if class == ClassMain {
		if n := humanPrompts(msgs); n != s.prompts || s.promptID == "" {
			s.prompts, s.promptID = n, newUUID()
		}
		t.promptID, t.promptIndex, t.prevReq = s.promptID, s.prompts, s.prevReq
	}
	return t
}

// finish records what the response told us about the session.
func (p *anthropicProvider) finish(t *ccTurn, requestID, messageID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[t.sessionID]
	if s == nil {
		return
	}
	if t.class == ClassMain && requestID != "" {
		s.prevReq = requestID
	}
	if messageID != "" {
		s.prevMsg = messageID
	}
}

// setClaudeCodeHeaders adds the per-request Claude Code headers.
func setClaudeCodeHeaders(h http.Header, t *ccTurn, version string) {
	betas := claudeAgentBetas
	if t.class == ClassAuxiliary {
		betas = claudeUtilityBetas
	}
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "claude-cli/"+version+" (external, cli)")
	h.Set("X-Claude-Code-Session-Id", t.sessionID)
	h.Set("X-Stainless-Arch", stainlessArch())
	h.Set("X-Stainless-Lang", "js")
	h.Set("X-Stainless-OS", stainlessOS())
	h.Set("X-Stainless-Package-Version", claudeCodeSDKVersion)
	h.Set("X-Stainless-Retry-Count", "0")
	h.Set("X-Stainless-Runtime", "node")
	h.Set("X-Stainless-Runtime-Version", "v26.3.0")
	h.Set("X-Stainless-Timeout", "600")
	h.Set("anthropic-beta", betas)
	h.Set("anthropic-dangerous-direct-browser-access", "true")
	h.Set("anthropic-dispatch-id", claudeDispatchID)
	h.Set("anthropic-version", anthropicVersion)
	h.Set("x-app", "cli")
	h.Set("x-claude-code-request-class", t.class)
	h.Set("x-client-request-id", newUUID())
	if t.promptID != "" {
		h.Set("x-claude-code-prompt-id", t.promptID)
	}
	if t.class == ClassSubagent {
		h.Set("x-claude-code-agent-type", claudeSubagentType)
	}
	h.Set("Connection", "keep-alive")
}

// firstUserText returns the first non-blank text block of a user message.
func firstUserText(msgs []Message) string {
	for i := range msgs {
		if msgs[i].Role != RoleUser {
			continue
		}
		for _, b := range msgs[i].Blocks {
			if b.Type == BlockText && strings.TrimSpace(b.Text) != "" {
				return b.Text
			}
		}
	}
	return ""
}

// jsCharAt mimics JavaScript `s[i] ?? "0"` (UTF-16 indexing). A lone
// surrogate half becomes U+FFFD once UTF-8 encoded, as in Bun's hashing.
func jsCharAt(s string, i int) string {
	pos := 0
	for _, r := range s {
		if r >= 0x10000 {
			if i == pos || i == pos+1 {
				return "\uFFFD"
			}
			pos += 2
			continue
		}
		if pos == i {
			return string(r)
		}
		pos++
	}
	return "0"
}

// billingHeader is the first system block: the version tagged with a hash
// of the first prompt, the entrypoint, the cch placeholder (filled in by
// patchBilling) and, for the main thread, the prompt and turn position.
func billingHeader(firstText, version string, t *ccTurn) string {
	sum := sha256.Sum256([]byte("59cf53e54c78" + jsCharAt(firstText, 4) + jsCharAt(firstText, 7) + jsCharAt(firstText, 20) + version))
	var b strings.Builder
	b.WriteString(billingHeaderPrefix + " cc_version=" + version + "." + hex.EncodeToString(sum[:2])[:3] + "; cc_entrypoint=cli; " + billingPlaceholder + ";")
	if t.class == ClassSubagent {
		b.WriteString(" cc_is_subagent=true;")
	}
	if t.class == ClassMain {
		if t.prevReq != "" {
			b.WriteString(" cc_prev_req=" + t.prevReq + ";")
		}
		if t.promptIndex > 0 {
			idx := strconv.Itoa(t.promptIndex)
			b.WriteString(" cc_prompt_id=" + t.promptID + "; cc_turn_origin=human; cc_prompt_index=" + idx + "; cc_turn_index=" + idx + ";")
		}
	}
	return b.String()
}

var billingAnchor = []byte(`"system":[{"type":"text","text":"` + billingHeaderPrefix)

// patchBilling replaces the cch=00000 placeholder with the low 20 bits of
// xxHash64 over the body as Claude Code hashes it (see billingHashInput)
// and reports whether it did.
func patchBilling(body []byte) bool {
	s := bytes.Index(body, billingAnchor)
	if s < 0 {
		return false
	}
	n := s + len(billingAnchor)
	o := bytes.Index(body[n:], []byte(billingPlaceholder))
	if o < 0 || o > billingMaxDistance {
		return false
	}
	sum := xxHash64(billingHashInput(body), billingSeed) & 0xfffff
	const digits = "0123456789abcdef"
	at := n + o + len("cch=")
	for i := 4; i >= 0; i-- {
		body[at+i] = digits[sum&0xf]
		sum >>= 4
	}
	return true
}

// billingHashInput is the body without what Claude Code leaves out of the
// cch hash: every "model" value (the key and quotes stay) and every
// "max_tokens" field with its comma.
func billingHashInput(body []byte) []byte {
	modelKey, maxKey := []byte(`"model":"`), []byte(`"max_tokens":`)
	out := make([]byte, 0, len(body))
	for rest := body; ; {
		i := bytes.Index(rest, modelKey)
		if i < 0 {
			out = append(out, rest...)
			break
		}
		i += len(modelKey)
		out = append(out, rest[:i]...)
		rest = rest[i:]
		if q := bytes.IndexByte(rest, '"'); q >= 0 {
			rest = rest[q:]
		}
	}
	for {
		i := bytes.Index(out, maxKey)
		if i < 0 {
			return out
		}
		end := i + len(maxKey)
		for end < len(out) && out[end] >= '0' && out[end] <= '9' {
			end++
		}
		switch {
		case end < len(out) && out[end] == ',':
			end++
		case i > 0 && out[i-1] == ',':
			i--
		}
		out = append(out[:i], out[end:]...)
	}
}

// claudeUserID builds metadata.user_id as Claude Code does.
func claudeUserID(deviceID string, t *ccTurn) string {
	b, _ := json.Marshal(struct {
		DeviceID  string `json:"device_id"`
		AccountID string `json:"account_uuid,omitempty"`
		SessionID string `json:"session_id"`
	}{deviceID, t.accountID, t.sessionID})
	return string(b)
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
