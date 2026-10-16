// Package config loads ~/.tego/config.json and resolves model specs to providers.
package config

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/akbxr/tego/internal/auth"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/stats"
)

type Config struct {
	Model         string                         `json:"model,omitempty"`        // provider/model-id
	Thinking      string                         `json:"thinking,omitempty"`     // off|low|medium|high|xhigh|max
	Fast          bool                           `json:"fast,omitempty"`         // fast mode on models that have it (/fast)
	WebModel      string                         `json:"web_model,omitempty"`    // model for native web_search (default: provider-specific)
	Subagents     int                            `json:"subagents,omitempty"`    // subagents running at once (default 8); more tasks queue
	MCPTools      string                         `json:"mcp_tools,omitempty"`    // "auto" (default: deferred behind tool_search above 10 tools), "lazy", "eager"
	Accounts      string                         `json:"accounts,omitempty"`     // with several logins per provider: "fallback" (default: switch on limits) or "round-robin"
	Icons         string                         `json:"icons,omitempty"`        // status-line icons: "nerd" (Nerd Font glyphs) or "unicode" (default)
	Notify        string                         `json:"notify,omitempty"`       // "off" disables the notification (sound) when a turn finishes
	UpdateCheck   string                         `json:"update_check,omitempty"` // "off" stops the daily check for a newer release
	ContextWindow int                            `json:"context_window,omitempty"`
	AutoCompact   *bool                          `json:"auto_compact,omitempty"`
	CompactAt     int                            `json:"compact_at_percent,omitempty"`  // auto-compact above this % of the window (default: window - max(15%, 16k))
	KeepRecent    int                            `json:"compact_keep_tokens,omitempty"` // recent tokens kept verbatim when compacting (default 20000)
	Providers     map[string]*llm.ProviderConfig `json:"providers,omitempty"`
	// Prices override or add API prices ($ per million tokens) for tego
	// stats, keyed by model id or "provider/model".
	Prices map[string]stats.Price `json:"prices,omitempty"`

	Dir string `json:"-"` // ~/.tego
}

// Presets are the built-in providers. User config entries with the same
// name override individual fields.
var Presets = map[string]llm.ProviderConfig{
	"anthropic":          {Kind: "anthropic", KeyEnv: "ANTHROPIC_API_KEY"},
	"openai":             {Kind: "openai", KeyEnv: "OPENAI_API_KEY"},
	"openai-codex":       {Kind: "openai-codex"},
	"google-antigravity": {Kind: "google-antigravity"},
	"openrouter":         {Kind: "openai", BaseURL: "https://openrouter.ai/api/v1", KeyEnv: "OPENROUTER_API_KEY"},
	"groq":               {Kind: "openai", BaseURL: "https://api.groq.com/openai/v1", KeyEnv: "GROQ_API_KEY"},
	"deepseek":           {Kind: "openai", BaseURL: "https://api.deepseek.com/v1", KeyEnv: "DEEPSEEK_API_KEY"},
	"gemini":             {Kind: "openai", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", KeyEnv: "GEMINI_API_KEY"},
	"xai":                {Kind: "openai", BaseURL: "https://api.x.ai/v1", KeyEnv: "XAI_API_KEY"},
	"mistral":            {Kind: "openai", BaseURL: "https://api.mistral.ai/v1", KeyEnv: "MISTRAL_API_KEY"},
	"zai":                {Kind: "openai", BaseURL: "https://api.z.ai/api/coding/paas/v4", KeyEnv: "ZAI_API_KEY"},
	"opencode":           {Kind: "openai", BaseURL: "https://opencode.ai/zen/v1", KeyEnv: "OPENCODE_API_KEY"},
	"ollama":             {Kind: "openai", BaseURL: "http://localhost:11434/v1"},
}

// OAuthProviders support `tego login <name>` with a subscription.
var OAuthProviders = []string{"anthropic", "openai-codex", "google-antigravity"}

// Default model per provider when only the provider is known.
var defaultModels = map[string]string{
	"anthropic":          "claude-sonnet-5-5",
	"openai":             "gpt-5",
	"openai-codex":       "gpt-5.6",
	"google-antigravity": "gemini-3.8-flash",
	"openrouter":         "anthropic/claude-sonnet-4.5",
	"deepseek":           "deepseek-chat",
	"gemini":             "gemini-2.5-pro",
}

// DefaultModelFor is the fallback model id for a provider ("" if none).
func DefaultModelFor(provider string) string { return defaultModels[provider] }

func Dir() string {
	if d := os.Getenv("TEGO_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".tego")
}

func Load() (*Config, error) {
	dir := Dir()
	c := &Config{Dir: dir}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	return c, nil
}

// Save persists only the user-editable fields.
func (c *Config) Save() error {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.Dir, "config.json"), append(data, '\n'), 0o600)
}

func (c *Config) AutoCompactEnabled() bool { return c.AutoCompact == nil || *c.AutoCompact }

// ProviderNames lists presets plus user-defined providers.
func (c *Config) ProviderNames() []string {
	set := map[string]bool{}
	for n := range Presets {
		set[n] = true
	}
	for n := range c.Providers {
		set[n] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (c *Config) providerConfig(name string) (llm.ProviderConfig, bool) {
	pc, ok := Presets[name]
	if u, uok := c.Providers[name]; uok && u != nil {
		ok = true
		if u.Kind != "" {
			pc.Kind = u.Kind
		}
		if u.BaseURL != "" {
			pc.BaseURL = u.BaseURL
		}
		if u.APIKey != "" {
			pc.APIKey = u.APIKey
		}
		if u.KeyEnv != "" {
			pc.KeyEnv = u.KeyEnv
		}
		if len(u.Headers) > 0 {
			pc.Headers = u.Headers
		}
	}
	if pc.Kind == "" {
		pc.Kind = "openai"
	}
	pc.Name = name
	return pc, ok
}

// Kind returns the provider's wire kind ("anthropic", "openai",
// "openai-codex", "google-antigravity"), or "" for unknown providers.
func (c *Config) Kind(name string) string {
	if pc, ok := c.providerConfig(name); ok {
		return pc.Kind
	}
	return ""
}

// Available reports whether a provider has credentials (or needs none).
func (c *Config) Available(name string, store *auth.Store) bool {
	pc, ok := c.providerConfig(name)
	if !ok {
		return false
	}
	oauthOnly := pc.Kind == "openai-codex" || pc.Kind == "google-antigravity" // subscription login only
	for _, cred := range store.Accounts(name) {
		if cred.Type == "oauth" || (!oauthOnly && cred.Key != "") {
			return true
		}
	}
	if oauthOnly {
		return false
	}
	if pc.APIKey != "" || (pc.KeyEnv != "" && os.Getenv(pc.KeyEnv) != "") {
		return true
	}
	return pc.KeyEnv == "" && pc.BaseURL != "" // keyless endpoints (ollama, local proxies)
}

// ParseModel splits "provider/model". A bare model id gets a provider guessed
// from its name, falling back to the first available provider.
func (c *Config) ParseModel(spec string, store *auth.Store) (provider, model string, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", "", errors.New("empty model")
	}
	if p, m, ok := strings.Cut(spec, "/"); ok {
		if _, known := c.providerConfig(p); known {
			if m == "" {
				m = defaultModels[p]
			}
			return p, m, nil
		}
	}
	if _, known := c.providerConfig(spec); known {
		if m := defaultModels[spec]; m != "" {
			return spec, m, nil
		}
		return "", "", fmt.Errorf("provider %q needs an explicit model: %s/<model-id>", spec, spec)
	}
	lower := strings.ToLower(spec)
	var guesses []string
	switch {
	case strings.HasPrefix(lower, "claude"):
		guesses = []string{"anthropic", "openrouter"}
	case strings.HasPrefix(lower, "gpt") || strings.HasPrefix(lower, "o1") || strings.HasPrefix(lower, "o3") || strings.HasPrefix(lower, "o4") || strings.HasPrefix(lower, "codex"):
		guesses = []string{"openai-codex", "openai", "openrouter"}
	case strings.HasPrefix(lower, "gemini"):
		guesses = []string{"gemini", "openrouter"}
	case strings.HasPrefix(lower, "deepseek"):
		guesses = []string{"deepseek", "openrouter"}
	case strings.HasPrefix(lower, "grok"):
		guesses = []string{"xai", "openrouter"}
	case strings.HasPrefix(lower, "glm"):
		guesses = []string{"zai", "openrouter"}
	}
	for _, g := range guesses {
		if c.Available(g, store) {
			return g, spec, nil
		}
	}
	if len(guesses) > 0 {
		return guesses[0], spec, nil
	}
	return "", "", fmt.Errorf("cannot infer provider for %q; use provider/model", spec)
}

// DefaultModel picks the configured model, else the first provider with credentials.
func (c *Config) DefaultModel(store *auth.Store) (string, error) {
	if c.Model != "" {
		return c.Model, nil
	}
	if m := os.Getenv("TEGO_MODEL"); m != "" {
		return m, nil
	}
	for _, p := range []string{"anthropic", "openai-codex", "google-antigravity", "openai", "openrouter", "gemini", "deepseek"} {
		if c.Available(p, store) {
			return p + "/" + defaultModels[p], nil
		}
	}
	return "", errors.New("no model configured and no credentials found.\n" +
		"  Log in:     tego login anthropic   (Claude Pro/Max)  |  tego login openai-codex  (ChatGPT)\n" +
		"              tego login google-antigravity  (Gemini / Claude via a Google account)\n" +
		"  Or set e.g. ANTHROPIC_API_KEY / OPENAI_API_KEY / OPENROUTER_API_KEY,\n" +
		"  or pick a model:  tego -m ollama/qwen3-coder")
}

// NewProvider builds a provider instance with resolved credentials. With
// several stored accounts it returns an *llm.Pool that switches accounts
// on limits (and rotates them with "accounts": "round-robin").
func (c *Config) NewProvider(name string, store *auth.Store) (llm.Provider, error) {
	pc, ok := c.providerConfig(name)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (known: %s)", name, strings.Join(c.ProviderNames(), ", "))
	}
	accts := store.Accounts(name)
	if len(accts) <= 1 {
		var cred *auth.Credential
		if len(accts) == 1 {
			cred = &accts[0]
		}
		return c.accountProvider(pc, name, cred, store)
	}
	members := make([]llm.PoolMember, 0, len(accts))
	var firstErr error
	for i := range accts {
		p, err := c.accountProvider(pc, name, &accts[i], store)
		if err != nil {
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		members = append(members, llm.PoolMember{Label: accts[i].Label(), Provider: p})
	}
	switch len(members) {
	case 0:
		return nil, firstErr
	case 1:
		return members[0].Provider, nil
	}
	return llm.NewPool(name, c.Accounts, members), nil
}

// accountProvider builds the provider for one account (cred nil: env key
// or keyless endpoint).
func (c *Config) accountProvider(pc llm.ProviderConfig, name string, cred *auth.Credential, store *auth.Store) (llm.Provider, error) {
	if cred != nil {
		switch cred.Type {
		case "oauth":
			pc.OAuth = store.TokenSource(name, cred.ID)
			pc.ProjectID = cred.ProjectID
			if pc.Kind == "anthropic" {
				pc.DeviceID = DeviceID(c.Dir)
			}
		case "api_key":
			if pc.APIKey == "" && (pc.KeyEnv == "" || os.Getenv(pc.KeyEnv) == "") {
				pc.APIKey = cred.Key
			}
		}
	}
	if pc.Kind == "openai-codex" && pc.OAuth == nil {
		return nil, errors.New("openai-codex requires `tego login openai-codex`")
	}
	if pc.Kind == "google-antigravity" && (pc.OAuth == nil || pc.ProjectID == "") {
		return nil, errors.New("google-antigravity requires `tego login google-antigravity`")
	}
	return llm.New(pc)
}

var (
	deviceMu  sync.Mutex
	deviceIDs = map[string]string{}
)

// DeviceID returns this installation's random 64-hex-char id, kept in
// <dir>/device_id and created on first use (Claude Code keeps the same kind
// of id as its userID).
func DeviceID(dir string) string {
	deviceMu.Lock()
	defer deviceMu.Unlock()
	if id, ok := deviceIDs[dir]; ok {
		return id
	}
	path := filepath.Join(dir, "device_id")
	id := ""
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); len(s) == 64 {
			if _, err := hex.DecodeString(s); err == nil {
				id = s
			}
		}
	}
	if id == "" {
		var b [32]byte
		_, _ = rand.Read(b[:])
		id = hex.EncodeToString(b[:])
		if err := os.MkdirAll(dir, 0o700); err == nil {
			_ = os.WriteFile(path, []byte(id+"\n"), 0o600)
		}
	}
	deviceIDs[dir] = id
	return id
}

// ContextWindowFor returns the configured window or a model-based guess.
func (c *Config) ContextWindowFor(provider, model string) int {
	if c.ContextWindow > 0 {
		return c.ContextWindow
	}
	m := strings.ToLower(model)
	switch {
	case provider == "google-antigravity" && strings.HasPrefix(m, "claude"):
		return 250_000 // Claude served through Antigravity
	case provider == "google-antigravity" && strings.HasPrefix(m, "gpt-oss"):
		return 131_072
	case strings.Contains(m, "gemini"):
		return 1_000_000
	case strings.Contains(m, "claude"):
		return claudeWindow(m)
	case provider == "openai-codex" || strings.Contains(m, "codex"):
		return 272_000 // ChatGPT Codex backend
	case strings.Contains(m, "chat-latest"):
		return 128_000
	case strings.HasPrefix(m, "gpt-5"):
		if v := gptMinor(m); v >= 4 && !strings.Contains(m, "mini") && !strings.Contains(m, "nano") && !strings.Contains(m, "cyber") {
			return 1_050_000
		}
		return 400_000
	case strings.HasPrefix(m, "gpt-6"):
		return 1_050_000
	case strings.Contains(m, "gpt-4.1"):
		return 1_000_000
	case provider == "ollama":
		return 32_000
	}
	return 128_000
}

var claudeGenRe = regexp.MustCompile(`(opus|sonnet|haiku|fable|mythos)-(\d+)(?:-(\d{1,2}))?(?:$|-)`)

// claudeWindow: 1M tokens from the 4.6 generation on
// (and Sonnet 4.5), 200k for older models.
func claudeWindow(m string) int {
	g := claudeGenRe.FindStringSubmatch(m)
	if g == nil || strings.Contains(m, "claude-3") {
		return 200_000
	}
	major, _ := strconv.Atoi(g[2])
	minor := 0
	if g[3] != "" {
		minor, _ = strconv.Atoi(g[3])
	}
	switch {
	case major >= 5, major == 4 && minor >= 6:
		return 1_000_000
	case g[1] == "sonnet" && major == 4 && minor == 5:
		return 1_000_000
	}
	return 200_000
}

// gptMinor parses N from "gpt-5.N…" (0 when absent).
func gptMinor(m string) int {
	rest := strings.TrimPrefix(m, "gpt-5.")
	if rest == m {
		return 0
	}
	n := 0
	for _, r := range rest {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}
