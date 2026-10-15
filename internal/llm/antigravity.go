package llm

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Google Antigravity (Cloud Code Assist with a Google account): the request
// envelope, headers and endpoint order of the google-antigravity provider.
const (
	AntigravityKind = "google-antigravity"

	antigravityDaily   = "https://daily-cloudcode-pa.googleapis.com"
	antigravitySandbox = "https://daily-cloudcode-pa.sandbox.googleapis.com"

	antigravityDefaultVersion = "2.19.1"
	antigravityManifestURL    = "https://antigravity-hub-auto-updater-974169037036.us-central1.run.app/manifest/latest-arm64-mac.yml"
	antigravityClaudeBeta     = "interleaved-thinking-2025-05-14"
	skipThoughtSignature      = "skip_thought_signature_validator"
	imageOmittedText          = "[image omitted: model does not support vision]"
)

var antigravityEndpoints = []string{antigravityDaily, antigravitySandbox}

// ---- client version / User-Agent ----

var agVersion struct {
	mu       sync.Mutex
	value    string
	failedAt time.Time
}

// antigravityVersion returns the current Antigravity hub version from its
// auto-update manifest (fetched once per process), falling back to
// a built-in version. TEGO_ANTIGRAVITY_VERSION overrides it.
func antigravityVersion(ctx context.Context) string {
	if v := os.Getenv("TEGO_ANTIGRAVITY_VERSION"); v != "" {
		return v
	}
	agVersion.mu.Lock()
	defer agVersion.mu.Unlock()
	if agVersion.value != "" {
		return agVersion.value
	}
	if !agVersion.failedAt.IsZero() && time.Since(agVersion.failedAt) < 10*time.Minute {
		return antigravityDefaultVersion
	}
	if v := fetchAntigravityVersion(ctx); v != "" {
		agVersion.value = v
		return v
	}
	agVersion.failedAt = time.Now()
	return antigravityDefaultVersion
}

var manifestVersionRe = regexp.MustCompile(`(?m)^\s*version\s*:\s*["']?(\d+\.\d+\.\d+)["']?\s*(?:#.*)?$`)

func fetchAntigravityVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, antigravityManifestURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", "electron-builder")
	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if m := manifestVersionRe.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// AntigravityUserAgent is the User-Agent sent to Cloud Code Assist for
// Antigravity (chat, model discovery and project provisioning).
func AntigravityUserAgent(ctx context.Context) string {
	return "antigravity/hub/" + antigravityVersion(ctx) + " (aidev_client; os_type=darwin; arch=arm64; cl=963137146)"
}

// ---- provider ----

type antigravityProvider struct {
	name      string
	endpoints []string
	project   string
	headers   map[string]string
	oauth     TokenSource

	mu       sync.Mutex
	sessions map[string]*agSession
}

// agSession is the per-conversation state kept for Antigravity
// request ids and labels.
type agSession struct {
	agentID, trajectoryID, sessionID string
	step                             int
	lastExecutionID                  string
	lastGoodEndpoint                 string
}

func newAntigravityProvider(name, base, project string, headers map[string]string, oauth TokenSource) *antigravityProvider {
	endpoints := antigravityEndpoints
	if base != "" && base != antigravityDaily && base != antigravitySandbox {
		endpoints = []string{base}
	}
	return &antigravityProvider{name: name, endpoints: endpoints, project: project, headers: headers, oauth: oauth, sessions: map[string]*agSession{}}
}

func (p *antigravityProvider) Name() string { return p.name }

// ImplicitPrefixCache: Antigravity's cache is implicit; a benchmark found it
// still warm after a 6 minute pause, and missing at random after seconds.
func (p *antigravityProvider) ImplicitPrefixCache(string) bool { return true }

// agTurn is the per-request snapshot of session state.
type agTurn struct {
	sessionID, requestID string
	labels               map[string]string
	endpoints            []string
	state                *agSession // nil when the request has no session id
}

func (p *antigravityProvider) turn(req *Request, t agTarget) agTurn {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[req.SessionID]
	if s == nil {
		s = &agSession{agentID: randomUUID(), trajectoryID: randomUUID()}
		if req.SessionID != "" {
			s.sessionID = randomSessionNumber()
			p.sessions[req.SessionID] = s
		} else {
			s.sessionID = hashedSessionNumber(firstUserText(req.Messages))
		}
	}
	if s.step == 0 {
		s.step = 1
	}
	s.step++
	labels := map[string]string{}
	if s.lastExecutionID != "" {
		labels["last_execution_id"] = s.lastExecutionID
	}
	labels["last_step_index"] = strconv.Itoa(s.step - 1)
	if t.modelEnum != "" {
		labels["model_enum"] = t.modelEnum
	}
	labels["trajectory_id"] = s.trajectoryID
	used := strconv.FormatBool(t.claude)
	labels["used_claude"] = used
	labels["used_claude_conservative"] = used

	endpoints := p.endpoints
	if s.lastGoodEndpoint != "" && len(endpoints) > 1 && s.lastGoodEndpoint != endpoints[0] {
		ordered := []string{s.lastGoodEndpoint}
		for _, e := range endpoints {
			if e != s.lastGoodEndpoint {
				ordered = append(ordered, e)
			}
		}
		endpoints = ordered
	}
	tr := agTurn{
		sessionID: s.sessionID,
		requestID: fmt.Sprintf("agent/%s/%d/%s/%d", s.agentID, time.Now().UnixMilli(), s.trajectoryID, s.step),
		labels:    labels,
		endpoints: endpoints,
	}
	if req.SessionID != "" {
		tr.state = s
	}
	return tr
}

func (p *antigravityProvider) remember(state *agSession, endpoint, executionID string) {
	if state == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.endpoints) > 1 {
		state.lastGoodEndpoint = endpoint
	}
	state.lastExecutionID = executionID
}

func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randomSessionNumber is a session id: "-" + a random integer below 9e18.
func randomSessionNumber() string {
	n, err := rand.Int(rand.Reader, big.NewInt(9_000_000_000_000_000_000))
	if err != nil {
		panic(err)
	}
	return "-" + n.String()
}

// hashedSessionNumber derives a stable session id from the first prompt
// (the fallback when there is no session state).
func hashedSessionNumber(text string) string {
	if strings.TrimSpace(text) == "" {
		return randomSessionNumber()
	}
	sum := sha256.Sum256([]byte(text))
	return "-" + strconv.FormatUint(binary.BigEndian.Uint64(sum[:8])&(1<<63-1), 10)
}

func (p *antigravityProvider) header(ctx context.Context, token string, t agTarget) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("User-Agent", AntigravityUserAgent(ctx))
	if t.claude && t.reasoning {
		h.Set("anthropic-beta", antigravityClaudeBeta)
	}
	setHeaders(h, p.headers)
	return h
}

func (p *antigravityProvider) Stream(ctx context.Context, req *Request, onEvent func(Event)) (*Message, error) {
	if p.project == "" {
		return nil, fmt.Errorf("%s: no Cloud Code project; run `tego login %s`", p.name, AntigravityKind)
	}
	t := resolveAntigravityModel(req.Model, req.Thinking, req.MaxTokens)
	tr := p.turn(req, t)
	body, err := antigravityBody(req, p.name, p.project, t, tr)
	if err != nil {
		return nil, err
	}
	return streamWithRetry(ctx, p.name, req, onEvent, func(ctx context.Context, emit func(Event)) (*Message, error) {
		token, err := p.oauth.Token(ctx)
		if err != nil {
			return nil, err
		}
		emitted := false
		track := func(e Event) {
			if e.Type != EvStarted { // a started stream can still fall back
				emitted = true
			}
			emit(e)
		}
		h := p.header(ctx, token, t)
		for i, ep := range tr.endpoints {
			last := i == len(tr.endpoints)-1
			resp, err := postJSON(ctx, p.name, ep+"/v1internal:streamGenerateContent?alt=sse", body, h)
			if err != nil {
				err = antigravityAPIError(err)
				if !last && ctx.Err() == nil && antigravityFallback(err) {
					continue
				}
				return nil, err
			}
			msg, execID, err := parseAntigravityStream(ctx, p.name, resp.Body, track)
			resp.Body.Close()
			if err != nil {
				if !last && !emitted && ctx.Err() == nil && antigravityFallback(err) {
					continue
				}
				return msg, err
			}
			p.remember(tr.state, ep, execID)
			return msg, nil
		}
		return nil, errors.New("antigravity: no endpoints")
	})
}

// antigravityFallback reports whether a failure before any output should
// move on to the next endpoint (408/429/5xx and transport errors).
func antigravityFallback(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == 408 || ae.Status == 429 || ae.Status >= 500
	}
	var se *streamError
	var ue *url.Error
	var ne net.Error
	return errors.As(err, &se) || errors.As(err, &ue) || errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF)
}

type agErrorBody struct {
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
		Details []struct {
			Type       string `json:"@type"`
			Reason     string `json:"reason"`
			RetryDelay string `json:"retryDelay"`
			Metadata   struct {
				ValidationURL string `json:"validation_url"`
			} `json:"metadata"`
		} `json:"details"`
	} `json:"error"`
}

// antigravityAPIError rewrites a Google JSON error body into its message
// (plus status), picks up RetryInfo delays, and turns VALIDATION_REQUIRED
// into an account-verification hint.
func antigravityAPIError(err error) error {
	var ae *APIError
	if !errors.As(err, &ae) {
		return err
	}
	var eb agErrorBody
	if json.Unmarshal([]byte(ae.Body), &eb) != nil || eb.Error == nil || eb.Error.Message == "" {
		return err
	}
	msg := eb.Error.Message
	if eb.Error.Status != "" {
		msg += " (" + eb.Error.Status + ")"
	}
	for _, d := range eb.Error.Details {
		if d.Reason == "VALIDATION_REQUIRED" && d.Metadata.ValidationURL != "" {
			msg = "Account verification required. Visit " + d.Metadata.ValidationURL + " to continue, then retry your request."
		}
		if d.RetryDelay != "" && ae.RetryAfter == 0 {
			if dur, perr := time.ParseDuration(d.RetryDelay); perr == nil {
				ae.RetryAfter = dur
			}
		}
	}
	ae.Body = msg
	return ae
}

// ---- model resolution ----

// agTarget is how a user-facing model id and thinking level map onto the
// wire: Antigravity exposes thinking levels of some models as separate
// model ids (effort routing).
type agTarget struct {
	wire           string
	modelEnum      string
	claude         bool
	gptOSS         bool
	gemini3        bool // Gemini 3+: skip-signature sentinel and multimodal function responses
	images         bool
	reasoning      bool
	thinking       *agThinkingConfig
	maxOutput      int
	functionPartID bool
}

type agThinkingConfig struct {
	IncludeThoughts bool   `json:"includeThoughts"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
	ThinkingBudget  *int   `json:"thinkingBudget,omitempty"`
}

// Default thinking budgets for budget-mode Cloud Code models.
var agBudgets = map[string]int{ThinkingLow: 4096, ThinkingMedium: 8192, ThinkingHigh: 16384}

// agFlashBudgets caps low and medium thinking on the 3.6+ flash models, as
// pi does: their level alone lets one turn think 20k tokens, three times
// pi's output on the same tasks. High stays dynamic (the HIGH level).
var agFlashBudgets = map[string]int{ThinkingLow: 1000, ThinkingMedium: 4000}

// Wire profiles (model enum label, output cap).
var agWireProfiles = map[string]struct {
	enum   string
	maxOut int
}{
	"gemini-3.5-flash-extra-low": {"MODEL_PLACEHOLDER_M187", 65536},
	"gemini-3.5-flash-low":       {"MODEL_PLACEHOLDER_M20", 65536},
	"gemini-3-flash-agent":       {"MODEL_PLACEHOLDER_M132", 65536},
	"gemini-3.1-pro-low":         {"MODEL_PLACEHOLDER_M36", 65535},
	"gemini-pro-agent":           {"MODEL_PLACEHOLDER_M16", 65535},
	"claude-sonnet-4-6":          {"", 64000},
	"claude-opus-4-6-thinking":   {"", 64000},
}

var (
	agLevelFlashRe = regexp.MustCompile(`^gemini-(\d+)\.(\d+)-flash$`)
	agGeminiVerRe  = regexp.MustCompile(`^gemini-(\d+)`)
)

func budgetThinking(budget int, include bool) *agThinkingConfig {
	return &agThinkingConfig{IncludeThoughts: include, ThinkingBudget: &budget}
}

func levelThinking(level string) *agThinkingConfig {
	return &agThinkingConfig{IncludeThoughts: true, ThinkingLevel: level}
}

func thinkingOn(level string) bool {
	_, ok := agBudgets[level]
	return ok
}

// resolveAntigravityModel follows the catalog for the Antigravity models.
// Unknown ids are sent unchanged, with thinking chosen by model family.
func resolveAntigravityModel(model, thinking string, maxTokens int) agTarget {
	id := strings.ToLower(strings.TrimSpace(model))
	on := thinkingOn(thinking)
	t := agTarget{wire: model, reasoning: true, images: true}
	budget := -1 // budget-mode thinking tokens; -1 = level mode or none

	geminiMajor := 0
	if m := agGeminiVerRe.FindStringSubmatch(id); m != nil {
		geminiMajor, _ = strconv.Atoi(m[1])
	}
	t.gemini3 = geminiMajor >= 3

	switch {
	case agLevelFlashRe.MatchString(id) && flashLevelModel(id):
		// gemini-3.6+-flash: each effort is its own model id; thinking off
		// falls back to the lowest effort.
		eff := thinking
		if !on {
			eff = ThinkingLow
		}
		t.wire = model + "-" + eff
		if b, ok := agFlashBudgets[eff]; ok {
			budget = b
		} else {
			t.thinking = levelThinking(strings.ToUpper(eff))
		}
	case id == "gemini-3.5-flash" || id == "gemini-3-flash":
		switch thinking {
		case ThinkingMedium:
			t.wire, budget = "gemini-3.5-flash-low", 4000
		case ThinkingHigh:
			t.wire, budget = "gemini-3-flash-agent", 10000
		case ThinkingLow:
			t.wire, budget = "gemini-3.5-flash-extra-low", 1000
		default:
			t.wire, t.thinking = "gemini-3.5-flash-extra-low", budgetThinking(0, false)
		}
	case id == "gemini-3.1-pro":
		switch thinking {
		case ThinkingHigh:
			t.wire, budget = "gemini-pro-agent", 10001
		case ThinkingLow, ThinkingMedium:
			t.wire, budget = "gemini-3.1-pro-low", 1001
		default:
			t.wire, t.thinking = "gemini-3.1-pro-low", budgetThinking(0, false)
		}
	case id == "gemini-2.5-flash":
		if on {
			t.wire, budget = "gemini-2.5-flash-thinking", agBudgets[thinking]
		}
	case id == "claude-opus-4-6":
		t.wire = "claude-opus-4-6-thinking"
		if on {
			budget = agBudgets[thinking]
		}
	case id == "gpt-oss-120b":
		t.wire = "gpt-oss-120b-medium"
		if on {
			budget = agBudgets[thinking]
		}
	case t.gemini3:
		// Other Gemini 3 models (flash-lite, pro): thinking levels, never off.
		level := "MINIMAL"
		if on {
			level = strings.ToUpper(thinking)
		}
		t.thinking = levelThinking(level)
	default:
		if on {
			budget = agBudgets[thinking]
		}
	}

	wire := strings.ToLower(t.wire)
	t.claude = strings.HasPrefix(wire, "claude-")
	t.gptOSS = strings.HasPrefix(wire, "gpt-oss-")
	t.images = !t.gptOSS
	t.functionPartID = t.claude || t.gptOSS

	t.maxOutput = 65536
	switch {
	case t.claude:
		t.maxOutput = 64000
	case t.gptOSS:
		t.maxOutput = 32768
	case strings.HasPrefix(wire, "gemini-2.") || strings.Contains(wire, "-lite"):
		t.maxOutput = 65535
	}
	if prof, ok := agWireProfiles[wire]; ok {
		t.modelEnum = prof.enum
		t.maxOutput = prof.maxOut
	}
	limit := t.maxOutput
	if maxTokens > 0 {
		limit = min(maxTokens, t.maxOutput)
	}
	if budget >= 0 {
		if maxTokens > 0 {
			limit = min(maxTokens+budget, t.maxOutput)
		}
		if limit <= budget {
			budget = max(0, limit-1024)
		}
		t.thinking = budgetThinking(budget, true)
	}
	t.maxOutput = limit
	return t
}

// flashLevelModel reports gemini-X.Y-flash ids from 3.6 on, which
// Antigravity serves as -low/-medium/-high variants.
func flashLevelModel(id string) bool {
	m := agLevelFlashRe.FindStringSubmatch(id)
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major > 3 || major == 3 && minor >= 6
}

// ---- request body ----

type agBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type agFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
	ID   string          `json:"id,omitempty"`
}

type agFunctionResponse struct {
	Name     string            `json:"name"`
	Response map[string]string `json:"response"`
	Parts    []agPart          `json:"parts,omitempty"`
	ID       string            `json:"id,omitempty"`
}

type agPart struct {
	Thought          bool                `json:"thought,omitempty"`
	Text             string              `json:"text,omitempty"`
	ThoughtSignature string              `json:"thoughtSignature,omitempty"`
	InlineData       *agBlob             `json:"inlineData,omitempty"`
	FunctionCall     *agFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *agFunctionResponse `json:"functionResponse,omitempty"`
}

type agContent struct {
	Role  string   `json:"role,omitempty"`
	Parts []agPart `json:"parts"`
}

type agFunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type agTool struct {
	FunctionDeclarations []agFunctionDecl `json:"functionDeclarations"`
}

type agToolConfig struct {
	FunctionCallingConfig struct {
		Mode string `json:"mode"`
	} `json:"functionCallingConfig"`
}

type agGenerationConfig struct {
	MaxOutputTokens int               `json:"maxOutputTokens,omitempty"`
	ThinkingConfig  *agThinkingConfig `json:"thinkingConfig,omitempty"`
}

type agInnerRequest struct {
	Contents          []agContent         `json:"contents"`
	SystemInstruction *agContent          `json:"systemInstruction,omitempty"`
	Tools             []agTool            `json:"tools,omitempty"`
	ToolConfig        *agToolConfig       `json:"toolConfig,omitempty"`
	Labels            map[string]string   `json:"labels,omitempty"`
	GenerationConfig  *agGenerationConfig `json:"generationConfig,omitempty"`
	SessionID         string              `json:"sessionId,omitempty"`
}

type agRequest struct {
	Project     string         `json:"project"`
	RequestID   string         `json:"requestId"`
	Request     agInnerRequest `json:"request"`
	Model       string         `json:"model"`
	UserAgent   string         `json:"userAgent"`
	RequestType string         `json:"requestType"`
}

func antigravityBody(req *Request, provider, project string, t agTarget, tr agTurn) ([]byte, error) {
	inner := agInnerRequest{
		Contents:  antigravityContents(req.Messages, provider, req.Model, t),
		Labels:    tr.labels,
		SessionID: tr.sessionID,
	}
	if inner.Contents == nil {
		inner.Contents = []agContent{}
	}
	if req.System != "" {
		inner.SystemInstruction = &agContent{Role: "user", Parts: []agPart{{Text: req.System}}}
	}
	if len(req.Tools) > 0 {
		decls := make([]agFunctionDecl, len(req.Tools))
		for i, tool := range req.Tools {
			decls[i] = agFunctionDecl{Name: tool.Name, Description: tool.Description, Parameters: cleanSchemaForCCA(tool.Schema)}
		}
		inner.Tools = []agTool{{FunctionDeclarations: decls}}
		tc := &agToolConfig{}
		tc.FunctionCallingConfig.Mode = "VALIDATED"
		inner.ToolConfig = tc
	}
	inner.GenerationConfig = &agGenerationConfig{MaxOutputTokens: t.maxOutput, ThinkingConfig: t.thinking}
	r := agRequest{
		Project:     project,
		RequestID:   tr.requestID,
		Request:     inner,
		Model:       t.wire,
		UserAgent:   "antigravity",
		RequestType: "agent",
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&r); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

var base64Re = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)

// replayableSignature: only signatures produced by this provider and model
// are sent back, and only when they look like base64.
func replayableSignature(same bool, sig string) string {
	if same && sig != "" && len(sig)%4 == 0 && base64Re.MatchString(sig) {
		return sig
	}
	return ""
}

var toolIDCleanRe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func cleanToolID(id string) string {
	id = toolIDCleanRe.ReplaceAllString(id, "_")
	if len(id) > 64 {
		id = id[:64]
	}
	return id
}

func inlineImage(b *Block) agPart {
	return agPart{InlineData: &agBlob{MimeType: b.MediaType, Data: b.Data}}
}

// antigravityContents converts history to Gemini contents. Thinking and
// signatures are replayed only for turns this provider/model produced;
// foreign thinking is dropped. Gemini 3 needs a signature on the first
// function call of each model turn, so unsigned ones get the skip
// sentinel. Claude/GPT-OSS function parts carry call ids.
func antigravityContents(msgs []Message, provider, model string, t agTarget) []agContent {
	var out []agContent
	names := map[string]string{} // tool call id -> name
	for i := range msgs {
		m := &msgs[i]
		if m.Role == RoleAssistant {
			same := m.Provider == provider && m.Model == model
			var parts []agPart
			first := true
			for j := range m.Blocks {
				b := &m.Blocks[j]
				switch b.Type {
				case BlockText:
					if strings.TrimSpace(b.Text) == "" {
						continue
					}
					parts = append(parts, agPart{Text: b.Text, ThoughtSignature: replayableSignature(same, b.Signature)})
				case BlockThinking:
					sig := replayableSignature(same, b.Signature)
					if sig == "" || strings.TrimSpace(b.Text) == "" {
						continue
					}
					parts = append(parts, agPart{Thought: true, Text: b.Text, ThoughtSignature: sig})
				case BlockToolUse:
					names[b.ID] = b.Name
					sig := replayableSignature(same, b.Signature)
					if sig == "" && first && t.gemini3 {
						sig = skipThoughtSignature
					}
					first = false
					args := bytes.TrimSpace(b.Input)
					if len(args) == 0 || !json.Valid(args) {
						args = []byte("{}")
					}
					fc := &agFunctionCall{Name: b.Name, Args: json.RawMessage(args)}
					if t.functionPartID {
						fc.ID = cleanToolID(b.ID)
					}
					parts = append(parts, agPart{FunctionCall: fc, ThoughtSignature: sig})
				}
			}
			if len(parts) > 0 {
				out = append(out, agContent{Role: "model", Parts: parts})
			}
			continue
		}

		var user, imageFollowUp []agPart
		for j := range m.Blocks {
			b := &m.Blocks[j]
			switch b.Type {
			case BlockText:
				if strings.TrimSpace(b.Text) != "" {
					user = append(user, agPart{Text: b.Text})
				}
			case BlockImage:
				if t.images {
					user = append(user, inlineImage(b))
				} else {
					user = append(user, agPart{Text: imageOmittedText})
				}
			case BlockToolResult:
				fr, extra := antigravityToolResult(b, names, t)
				part := agPart{FunctionResponse: fr}
				if n := len(out); n > 0 && out[n-1].Role == "user" && hasFunctionResponse(out[n-1].Parts) {
					out[n-1].Parts = append(out[n-1].Parts, part)
				} else {
					out = append(out, agContent{Role: "user", Parts: []agPart{part}})
				}
				imageFollowUp = append(imageFollowUp, extra...)
			}
		}
		if len(imageFollowUp) > 0 {
			out = append(out, agContent{Role: "user", Parts: append([]agPart{{Text: "Tool result image:"}}, imageFollowUp...)})
		}
		if len(user) > 0 {
			out = append(out, agContent{Role: "user", Parts: user})
		}
	}
	return out
}

func hasFunctionResponse(parts []agPart) bool {
	for i := range parts {
		if parts[i].FunctionResponse != nil {
			return true
		}
	}
	return false
}

// antigravityToolResult builds a functionResponse. Gemini 3 takes images
// inside the response; other vision models get them in a following user
// turn (returned as extra); text-only models get a placeholder.
func antigravityToolResult(b *Block, names map[string]string, t agTarget) (*agFunctionResponse, []agPart) {
	var imgs []agPart
	for i := range b.Images {
		if b.Images[i].Type == BlockImage {
			imgs = append(imgs, inlineImage(&b.Images[i]))
		}
	}
	text := b.Content
	var extra []agPart
	fr := &agFunctionResponse{Name: names[b.ToolUseID]}
	switch {
	case len(imgs) > 0 && !t.images:
		if text != "" {
			text += "\n"
		}
		text += imageOmittedText
	case len(imgs) > 0:
		if text == "" {
			text = "(see attached image)"
		}
		if t.gemini3 {
			fr.Parts = imgs
		} else {
			extra = imgs
		}
	}
	if fr.Name == "" {
		fr.Name = "tool"
	}
	if b.IsError {
		fr.Response = map[string]string{"error": text}
	} else {
		fr.Response = map[string]string{"output": text}
	}
	if t.functionPartID {
		fr.ID = cleanToolID(b.ToolUseID)
	}
	return fr, extra
}

// ---- response stream ----

type agStreamPart struct {
	Text             string `json:"text"`
	Thought          bool   `json:"thought"`
	ThoughtSignature string `json:"thoughtSignature"`
	FunctionCall     *struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall"`
}

type agUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
}

type agStreamChunk struct {
	Response *struct {
		ResponseID string `json:"responseId"`
		Candidates []struct {
			Content *struct {
				Parts []agStreamPart `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata  *agUsageMetadata `json:"usageMetadata"`
		PromptFeedback *struct {
			BlockReason        string `json:"blockReason"`
			BlockReasonMessage string `json:"blockReasonMessage"`
		} `json:"promptFeedback"`
	} `json:"response"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func agUsage(u *agUsageMetadata) *Usage {
	prompt := u.PromptTokenCount
	if prompt == 0 {
		prompt = max(0, u.TotalTokenCount-u.CandidatesTokenCount-u.ThoughtsTokenCount)
	}
	cached := min(u.CachedContentTokenCount, prompt)
	return &Usage{Input: prompt - cached, CacheRead: cached, Output: u.CandidatesTokenCount + u.ThoughtsTokenCount}
}

var agToolCallSeq atomic.Int64

// parseAntigravityStream reads the SSE stream into an assistant message and
// returns the response id (the last_execution_id label).
func parseAntigravityStream(ctx context.Context, provider string, body io.Reader, emit func(Event)) (msg *Message, responseID string, err error) {
	msg = &Message{Role: RoleAssistant, Usage: &Usage{}}
	acc := newBlockAccumulator()
	defer func() { acc.finalize(msg, err != nil) }()
	cur := -1 // index of the open text/thinking block
	finish := ""
	meaningful, thought := false, false
	ids := map[string]bool{}

	open := func(typ BlockType) int {
		if cur >= 0 && msg.Blocks[cur].Type == typ {
			return cur
		}
		msg.Blocks = append(msg.Blocks, Block{Type: typ})
		cur = len(msg.Blocks) - 1
		return cur
	}
	sign := func(bi int, sig string) {
		if bi >= 0 && sig != "" {
			msg.Blocks[bi].Signature = sig
		}
	}

	sse := newSSEReader(body)
	for {
		e, rerr := sse.Next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return msg, responseID, ctx.Err()
			}
			return msg, responseID, &streamError{rerr}
		}
		emit(Event{Type: EvStarted}) // Antigravity buffers: this is usually the end
		var ch agStreamChunk
		if err := json.Unmarshal(e.Data, &ch); err != nil {
			return msg, responseID, fmt.Errorf("%s: bad stream event %q: %w", provider, truncate(string(e.Data), 200), err)
		}
		if ch.Error != nil {
			text := ch.Error.Message
			if text == "" {
				text = ch.Error.Status
			}
			status := ch.Error.Code
			if status < 400 {
				status = 500
			}
			return msg, responseID, &APIError{Provider: provider, Status: status, Body: "Cloud Code Assist stream error: " + text}
		}
		r := ch.Response
		if r == nil {
			continue
		}
		if r.ResponseID != "" {
			responseID = r.ResponseID
		}
		if len(r.Candidates) == 0 && r.PromptFeedback != nil && r.PromptFeedback.BlockReason != "" {
			text := "Request blocked by Google (" + r.PromptFeedback.BlockReason + ")"
			if r.PromptFeedback.BlockReasonMessage != "" {
				text += ": " + r.PromptFeedback.BlockReasonMessage
			}
			return msg, responseID, &APIError{Provider: provider, Status: 400, Body: text}
		}
		if len(r.Candidates) > 0 {
			c := r.Candidates[0]
			if c.Content != nil {
				for _, part := range c.Content.Parts {
					switch {
					case part.Text != "" && part.Thought:
						bi := open(BlockThinking)
						acc.addText(bi, part.Text)
						sign(bi, part.ThoughtSignature)
						thought = true
						emit(Event{Type: EvThinking, Text: part.Text})
					case part.Text != "":
						bi := open(BlockText)
						acc.addText(bi, part.Text)
						sign(bi, part.ThoughtSignature)
						if strings.TrimSpace(part.Text) != "" {
							meaningful = true
						}
						emit(Event{Type: EvText, Text: part.Text})
					case part.ThoughtSignature != "" && part.FunctionCall == nil:
						sign(cur, part.ThoughtSignature)
					}
					if fc := part.FunctionCall; fc != nil {
						cur = -1
						name := fc.Name
						id := fc.ID
						if id == "" || ids[id] {
							n := name
							if n == "" {
								n = "tool"
							}
							id = fmt.Sprintf("%s_%d_%d", n, time.Now().UnixMilli(), agToolCallSeq.Add(1))
						}
						ids[id] = true
						msg.Blocks = append(msg.Blocks, Block{Type: BlockToolUse, ID: id, Name: name, Signature: part.ThoughtSignature})
						bi := len(msg.Blocks) - 1
						emit(Event{Type: EvToolCall, ToolName: name, ToolID: id})
						args := fc.Args
						if string(bytes.TrimSpace(args)) == "null" {
							args = nil
						}
						acc.finishTool(msg, bi, append([]byte{}, args...))
						meaningful = true
					}
				}
			}
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		}
		if r.UsageMetadata != nil {
			msg.Usage = agUsage(r.UsageMetadata)
			// Antigravity sends nothing while the model thinks, so the
			// stream only times the answer itself.
			msg.GenOutput = r.UsageMetadata.CandidatesTokenCount
		}
	}

	if ctx.Err() != nil {
		return msg, responseID, ctx.Err()
	}
	hasTools := false
	for _, b := range msg.Blocks {
		if b.Type == BlockToolUse {
			hasTools = true
		}
	}
	switch {
	case finish == "" && !meaningful && !thought:
		return msg, responseID, &APIError{Provider: provider, Status: 503, Body: "Cloud Code Assist API returned an empty response"}
	case finish == "":
		return msg, responseID, &streamError{errors.New("Cloud Code Assist stream ended without a finish reason")}
	case finish != "STOP" && finish != "MAX_TOKENS":
		return msg, responseID, fmt.Errorf("%s: generation failed with finish reason %s", provider, finish)
	case finish == "MAX_TOKENS":
		// Even with tool calls: Gemini sends arguments as whole objects, so
		// a call cut off by the limit still parses. The agent must know.
		msg.StopReason = "max_tokens"
	case hasTools:
		msg.StopReason = "tool_use"
	case !meaningful && thought:
		return msg, responseID, fmt.Errorf("%s: Cloud Code Assist API returned a thought-only response without final output", provider)
	case !meaningful:
		return msg, responseID, &APIError{Provider: provider, Status: 503, Body: "Cloud Code Assist API returned an empty response"}
	default:
		msg.StopReason = "end_turn"
	}
	return msg, responseID, nil
}

// ---- model listing ----

// agListAliases maps Antigravity wire ids onto the ids tego shows.
var agListAliases = map[string]string{
	"gemini-3.5-flash-extra-low": "gemini-3.5-flash",
	"gemini-3.5-flash-low":       "gemini-3.5-flash",
	"gemini-3-flash-agent":       "gemini-3.5-flash",
	"gemini-3.1-pro-low":         "gemini-3.1-pro",
	"gemini-3.1-pro-high":        "gemini-3.1-pro",
	"gemini-pro-agent":           "gemini-3.1-pro",
	"gemini-2.5-flash-thinking":  "gemini-2.5-flash",
	"claude-opus-4-6-thinking":   "claude-opus-4-6",
	"gpt-oss-120b-medium":        "gpt-oss-120b",
}

// Raw ids hidden from the Antigravity catalog.
var agListSkip = map[string]bool{"chat_20706": true, "chat_23310": true, "gemini-2.5-pro": true}

var agEffortSuffixRe = regexp.MustCompile(`^(.+)-(low|medium|high|tiered)$`)

// ListModels queries v1internal:fetchAvailableModels, dropping internal,
// tab-completion and image-generation models and folding the per-effort
// variants into one id each.
func (p *antigravityProvider) ListModels(ctx context.Context) ([]string, error) {
	token, err := p.oauth.Token(ctx)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", AntigravityUserAgent(ctx))
	setHeaders(h, p.headers)
	var lastErr error
	for _, ep := range p.endpoints {
		resp, err := postJSON(ctx, p.name, ep+"/v1internal:fetchAvailableModels", []byte("{}"), h)
		if err != nil {
			lastErr = antigravityAPIError(err)
			if ctx.Err() != nil {
				break
			}
			continue
		}
		var payload agModelsPayload
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("%s: bad fetchAvailableModels response: %w", p.name, err)
			continue
		}
		return payload.ids(), nil
	}
	return nil, lastErr
}

type agModelsPayload struct {
	Models map[string]struct {
		IsInternal bool `json:"isInternal"`
	} `json:"models"`
	ImageGenerationModelIDs []string `json:"imageGenerationModelIds"`
}

func (pl *agModelsPayload) ids() []string {
	skip := map[string]bool{}
	for _, id := range pl.ImageGenerationModelIDs {
		skip[id] = true
	}
	seen := map[string]bool{}
	var out []string
	for id, m := range pl.Models {
		if m.IsInternal || agListSkip[id] || skip[id] || strings.HasPrefix(id, "tab_") || strings.HasPrefix(id, "chat_") {
			continue
		}
		public := id
		if a, ok := agListAliases[id]; ok {
			public = a
		} else if s := agEffortSuffixRe.FindStringSubmatch(id); s != nil && flashLevelModel(s[1]) {
			public = s[1]
		}
		if !seen[public] {
			seen[public] = true
			out = append(out, public)
		}
	}
	sort.Strings(out)
	return out
}
