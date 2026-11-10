package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/auth"
	"github.com/akbxr/tego/internal/llm"
)

func TestSubagentMaxTurns(t *testing.T) {
	for raw, want := range map[string]int{`{}`: 150, `{"subagent_max_turns":0}`: 0, `{"subagent_max_turns":500}`: 500, `{"subagent_max_turns":-1}`: 150} {
		var c Config
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatal(err)
		}
		if got := c.SubagentMaxTurns(); got != want {
			t.Errorf("%s: %d, want %d", raw, got, want)
		}
	}
}

func TestSubagentWatchdog(t *testing.T) {
	for raw, want := range map[string]int{`{}`: 40, `{"subagent_watchdog":0}`: 0, `{"subagent_watchdog":25}`: 25} {
		var c Config
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatal(err)
		}
		if got := c.SubagentWatchdog(); got != want {
			t.Errorf("%s: %d, want %d", raw, got, want)
		}
	}
}

func TestContextWindowFor(t *testing.T) {
	c := &Config{}
	cases := []struct {
		provider, model string
		want            int
	}{
		{"anthropic", "claude-opus-5-5", 1_000_000},
		{"anthropic", "claude-opus-4-6", 1_000_000},
		{"anthropic", "claude-opus-4-5-20251101", 200_000},
		{"anthropic", "claude-haiku-4-5", 200_000},
		{"anthropic", "claude-haiku-5-5", 1_000_000},
		{"anthropic", "claude-sonnet-4-5", 1_000_000},
		{"anthropic", "claude-sonnet-4-20250514", 200_000},
		{"anthropic", "claude-3-5-sonnet-20241022", 200_000},
		{"openrouter", "anthropic/claude-sonnet-5", 1_000_000},
		{"openai", "gpt-5", 400_000},
		{"openai", "gpt-5.6", 1_050_000},
		{"openai", "gpt-5.4-mini", 400_000},
		{"openai", "gpt-5.1-chat-latest", 128_000},
		{"openai-codex", "gpt-5.6", 272_000},
		{"ollama", "qwen3", 32_000},
		{"google-antigravity", "gemini-3.8-flash", 1_000_000},
		{"google-antigravity", "claude-opus-4-6", 250_000},
		{"google-antigravity", "gpt-oss-120b", 131_072},
	}
	for _, tc := range cases {
		if got := c.ContextWindowFor(tc.provider, tc.model); got != tc.want {
			t.Errorf("%s/%s = %d, want %d", tc.provider, tc.model, got, tc.want)
		}
	}
	c.ContextWindow = 64_000
	if got := c.ContextWindowFor("anthropic", "claude-opus-5-5"); got != 64_000 {
		t.Errorf("config override ignored: %d", got)
	}
}

func TestAntigravityProvider(t *testing.T) {
	t.Setenv("TEGO_MODEL", "")
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "GEMINI_API_KEY", "DEEPSEEK_API_KEY"} {
		t.Setenv(k, "")
	}
	store, err := auth.Open(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{}
	if c.Available("google-antigravity", store) {
		t.Fatal("available without login")
	}
	if _, err := c.NewProvider("google-antigravity", store); err == nil || !strings.Contains(err.Error(), "tego login google-antigravity") {
		t.Fatalf("NewProvider without login: %v", err)
	}
	_, _, _ = store.Add("google-antigravity", auth.Credential{Type: "api_key", Key: "k"})
	if c.Available("google-antigravity", store) {
		t.Fatal("an API key must not make google-antigravity available")
	}
	_ = store.Delete("google-antigravity")
	_, _, _ = store.Add("google-antigravity", auth.Credential{Type: "oauth", Access: "a", Refresh: "r", ProjectID: "p"})
	if !c.Available("google-antigravity", store) {
		t.Fatal("not available after login")
	}
	if m, err := c.DefaultModel(store); err != nil || m != "google-antigravity/gemini-3.8-flash" {
		t.Fatalf("DefaultModel = %q, %v", m, err)
	}
	p, err := c.NewProvider("google-antigravity", store)
	if err != nil || p.Name() != "google-antigravity" {
		t.Fatalf("NewProvider = %v, %v", p, err)
	}
	_ = store.Delete("google-antigravity")
	_, _, _ = store.Add("google-antigravity", auth.Credential{Type: "oauth", Access: "a", Refresh: "r"})
	if _, err := c.NewProvider("google-antigravity", store); err == nil {
		t.Fatal("NewProvider without a project should fail")
	}
}

func TestIdleCompactAt(t *testing.T) {
	off, on := false, true
	for _, c := range []struct {
		cfg  Config
		want int
	}{
		{Config{}, DefaultIdleCompactTokens},
		{Config{IdleCompact: &on}, DefaultIdleCompactTokens},
		{Config{IdleTokens: 150_000}, 150_000},
		{Config{IdleCompact: &off, IdleTokens: 150_000}, 0},
	} {
		if got := c.cfg.IdleCompactAt(); got != c.want {
			t.Errorf("%+v: %d, want %d", c.cfg, got, c.want)
		}
	}
}

func TestSmallModelSpec(t *testing.T) {
	c := &Config{Providers: map[string]*llm.ProviderConfig{"proxy": {Kind: "anthropic", BaseURL: "http://x"}}}
	for _, tc := range []struct{ small, provider, want string }{
		{"", "anthropic", "anthropic/claude-haiku-5-5"},
		{"", "google-antigravity", "google-antigravity/gemini-3.5-flash-lite"},
		{"", "proxy", "proxy/claude-haiku-5-5"}, // a gateway for Anthropic's API
		{"", "openai-codex", ""},                // no default: the session's model
		{"off", "anthropic", ""},
		{"claude-sonnet-5-5", "anthropic", "anthropic/claude-sonnet-5-5"}, // a model of the session's provider
		{"openrouter/qwen/qwen3-coder", "anthropic", "openrouter/qwen/qwen3-coder"},
	} {
		c.SmallModel = tc.small
		if got := c.SmallModelSpec(tc.provider); got != tc.want {
			t.Errorf("small_model %q on %s: %q, want %q", tc.small, tc.provider, got, tc.want)
		}
	}
}

// A save writes what this process changed and keeps the rest of the file:
// another session's changes, keys this tego doesn't know, and the order.
func TestSaveKeepsTheFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TEGO_HOME", dir)
	path := filepath.Join(dir, "config.json")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	write(`{"thinking":"high","model":"anthropic/claude-opus-5-5","subagent_max_turns":0,"icons":"nerd"}`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// Meanwhile: another session turns thinking up, the user adds two keys.
	write(`{"thinking":"xhigh","model":"anthropic/claude-opus-5-5","subagent_max_turns":0,"icons":"nerd","session_messages":true,"from_a_newer_tego":{"a":[1,2]}}`)
	c.Model = "anthropic/claude-fable-5-2"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	want := `{
  "thinking": "xhigh",
  "model": "anthropic/claude-fable-5-2",
  "subagent_max_turns": 0,
  "icons": "nerd",
  "session_messages": true,
  "from_a_newer_tego": {
    "a": [
      1,
      2
    ]
  }
}
`
	if got := read(); got != want {
		t.Fatalf("saved:\n%s", got)
	}
	// A key set back to its default goes; a new one is added at the end.
	c.Icons, c.Accounts = "", "expiring"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if got := read(); strings.Contains(got, "icons") || !strings.Contains(got, "\"from_a_newer_tego\": {\n    \"a\": [\n      1,\n      2\n    ]\n  },\n  \"accounts\": \"expiring\"\n}") {
		t.Fatalf("saved:\n%s", got)
	}
	// Its own earlier change is not written again over a later one.
	write(`{"model":"openai/gpt-5.6"}`)
	c.Thinking = "low"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "{\n  \"model\": \"openai/gpt-5.6\",\n  \"thinking\": \"low\"\n}\n" {
		t.Fatalf("saved:\n%s", got)
	}
	// No file, and one that is not JSON: the changes alone.
	os.Remove(path)
	fresh, _ := Load()
	fresh.Model = "a/b"
	if err := fresh.Save(); err != nil || read() != "{\n  \"model\": \"a/b\"\n}\n" {
		t.Fatalf("%v\n%s", err, read())
	}
}
