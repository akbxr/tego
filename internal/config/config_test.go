package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/auth"
)

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
