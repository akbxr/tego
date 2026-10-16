package agent

import (
	"context"
	"encoding/json"
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
