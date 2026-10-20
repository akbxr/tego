package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMCPIdleAfter(t *testing.T) {
	for v, want := range map[string]time.Duration{"": DefaultMCPIdle, "off": 0, "0": 0, "90s": 90 * time.Second, "soon": DefaultMCPIdle, "-5m": DefaultMCPIdle} {
		if got := (&Config{MCPIdle: v}).MCPIdleAfter(); got != want {
			t.Errorf("mcp_idle %q: %v, want %v", v, got, want)
		}
	}
}

func TestSubagentModelsLoad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TEGO_HOME", dir)
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"subagent_models": {
		"gpt": "openai-codex/gpt-5.6-terra",
		"gemini": {"model": "google-antigravity/gemini-3.8-pro", "use": "reading large codebases", "thinking": "high"},
		"haiku": "claude-haiku-5-5"
	}}`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range c.SubagentModelList() {
		got = append(got, m.Name+"="+SubagentModelSpec(m.SubagentModel, "anthropic")+"|"+m.Use+"|"+m.Thinking)
	}
	want := "gemini=google-antigravity/gemini-3.8-pro|reading large codebases|high gpt=openai-codex/gpt-5.6-terra|| haiku=anthropic/claude-haiku-5-5||"
	if strings.Join(got, " ") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(got, " "), want)
	}

	// Saving keeps the short form where it was used.
	data, _ := json.Marshal(c.SubModels)
	for _, s := range []string{`"gpt":"openai-codex/gpt-5.6-terra"`, `"gemini":{"model":"google-antigravity/gemini-3.8-pro","use":"reading large codebases","thinking":"high"}`} {
		if !strings.Contains(string(data), s) {
			t.Errorf("saved %s, lacks %s", data, s)
		}
	}

	for cfg, msg := range map[string]string{
		`{"subagent_models": {"small": "anthropic/claude-haiku-5-5"}}`:                        `"small" is taken`,
		`{"subagent_models": {"main": "anthropic/claude-haiku-5-5"}}`:                         `"main" is taken`,
		`{"subagent_models": {"My GPT": "openai-codex/gpt-5.6"}}`:                             `name "My GPT"`,
		`{"subagent_models": {"gpt": ""}}`:                                                    "model is empty",
		`{"subagent_models": {"gpt": {"model": "openai-codex/gpt-5.6", "thinking": "huge"}}}`: `invalid thinking level "huge"`,
	} {
		write(cfg)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: err %v, want %q", cfg, err, msg)
		}
	}
}
