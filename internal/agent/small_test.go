package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// modelRecorder answers every request at once and records which model it
// was for.
type modelRecorder struct {
	name   string
	mu     sync.Mutex
	models []string
}

func (m *modelRecorder) Name() string { return m.name }
func (m *modelRecorder) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	m.mu.Lock()
	m.models = append(m.models, req.Model)
	m.mu.Unlock()
	return &llm.Message{Role: llm.RoleAssistant, StopReason: "end_turn", Blocks: []llm.Block{text("report")}}, nil
}

func TestTaskSmallModel(t *testing.T) {
	mainP, smallP := &modelRecorder{name: "main"}, &modelRecorder{name: "small"}
	parent := &Agent{Provider: mainP, Model: "claude-opus-5-5", Fast: true, ContextWindow: 1_000_000, UI: nopUI{}}
	small := SmallModel{Provider: smallP, ProviderName: "anthropic", Model: "claude-haiku-5-5", Window: 200_000}
	task := &TaskTool{Parent: parent, Env: &tools.Env{Cwd: t.TempDir()}, Small: func() (SmallModel, bool) { return small, true }}

	if s := string(task.Def().Schema); !strings.Contains(s, `"model":{"type":"string","enum":["main","small"]`) || !strings.Contains(s, "claude-haiku-5-5") {
		t.Fatalf("schema without the model choice: %s", s)
	}
	in, _ := json.Marshal(map[string]any{"tasks": []map[string]string{
		{"name": "Find", "prompt": "find it", "model": "small"},
		{"name": "Design", "prompt": "design it"},
		{"name": "Fix", "prompt": "fix it", "model": "main"},
	}})
	if _, err := task.Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(smallP.models, []string{"claude-haiku-5-5"}) || len(mainP.models) != 2 || mainP.models[0] != "claude-opus-5-5" {
		t.Fatalf("small provider got %v, main %v", smallP.models, mainP.models)
	}
	subs := parent.Subagents()
	if subs[0].Model != "claude-haiku-5-5" || subs[0].Window != 200_000 || subs[1].Model != "" || subs[1].Window != 1_000_000 {
		t.Fatalf("statuses %+v / %+v", subs[0], subs[1])
	}

	// No small model: the schema doesn't offer it, and "small" runs on the parent's.
	task.Small = func() (SmallModel, bool) { return SmallModel{}, false }
	if strings.Contains(string(task.Def().Schema), `"model"`) {
		t.Fatal("schema offers a small model that isn't there")
	}
	mainP.models = nil
	in, _ = json.Marshal(map[string]any{"tasks": []map[string]string{{"name": "Find", "prompt": "x", "model": "small"}}})
	if _, err := task.Run(context.Background(), in); err != nil || !slices.Equal(mainP.models, []string{"claude-opus-5-5"}) {
		t.Fatalf("err %v, main got %v", err, mainP.models)
	}
}

// thinkingRecorder records the thinking level of each request.
type thinkingRecorder struct {
	modelRecorder
	thinking []string
}

func (m *thinkingRecorder) Stream(ctx context.Context, req *llm.Request, fn func(llm.Event)) (*llm.Message, error) {
	m.mu.Lock()
	m.thinking = append(m.thinking, req.Thinking)
	m.mu.Unlock()
	return m.modelRecorder.Stream(ctx, req, fn)
}

func TestTaskNamedModels(t *testing.T) {
	mainP := &modelRecorder{name: "main"}
	gptP := &thinkingRecorder{modelRecorder: modelRecorder{name: "gpt"}}
	parent := &Agent{Provider: mainP, ProviderName: "anthropic", Model: "claude-opus-5-5", Thinking: "medium", Fast: true, ContextWindow: 1_000_000, UI: nopUI{}}
	resolved := 0
	task := &TaskTool{Parent: parent, Env: &tools.Env{Cwd: t.TempDir()}, Models: func() []SubagentModel {
		return []SubagentModel{
			{Name: "gpt", Spec: "openai-codex/gpt-5.6-terra", Use: "review and debugging.", Thinking: "high", Resolve: func() (SmallModel, error) {
				resolved++
				return SmallModel{Provider: gptP, ProviderName: "openai-codex", Model: "gpt-5.6-terra", Window: 272_000}, nil
			}},
			{Name: "gemini", Spec: "google-antigravity/gemini-3.8-pro", Resolve: func() (SmallModel, error) {
				return SmallModel{}, errors.New("not logged in")
			}},
		}
	}}

	s := string(task.Def().Schema)
	for _, want := range []string{`"enum":["main","gpt","gemini"]`, "gpt: openai-codex/gpt-5.6-terra, review and debugging.", "gemini: google-antigravity/gemini-3.8-pro, when the user asks for it."} {
		if !strings.Contains(s, want) {
			t.Errorf("schema lacks %q: %s", want, s)
		}
	}
	in, _ := json.Marshal(map[string]any{"tasks": []map[string]string{
		{"name": "Review", "prompt": "review it", "model": "gpt"},
		{"name": "Debug", "prompt": "debug it", "model": "gpt"},
		{"name": "Read", "prompt": "read it", "model": "gemini"},
		{"name": "Odd", "prompt": "do it", "model": "llama"},
	}})
	out, err := task.Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != 1 {
		t.Errorf("gpt resolved %d times, want once per call", resolved)
	}
	if !slices.Equal(gptP.models, []string{"gpt-5.6-terra", "gpt-5.6-terra"}) || !slices.Equal(gptP.thinking, []string{"high", "high"}) {
		t.Errorf("gpt got models %v, thinking %v", gptP.models, gptP.thinking)
	}
	// The ones that can't run where asked run on the session's model, and say so.
	if len(mainP.models) != 2 {
		t.Errorf("main got %v", mainP.models)
	}
	for _, want := range []string{"## Read (done; on the session's model: model gemini (google-antigravity/gemini-3.8-pro) is unavailable: not logged in)",
		`## Odd (done; on the session's model: model "llama" is not configured)`, "## Review (done)"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	subs := parent.Subagents()
	if subs[0].Model != "gpt-5.6-terra" || subs[0].Window != 272_000 || subs[2].Model != "" || subs[2].Window != 1_000_000 {
		t.Errorf("statuses %+v / %+v", subs[0], subs[2])
	}
}
