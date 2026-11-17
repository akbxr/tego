package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
)

func TestParseGoalVerdict(t *testing.T) {
	for in, want := range map[string]GoalVerdict{
		"MET: go test ./... printed ok for every package": {GoalMet, "go test ./... printed ok for every package"},
		"**NOT_MET**: the lint step was never run":        {GoalNotMet, "the lint step was never run"},
		"NOT MET - two tests still fail":                  {GoalNotMet, "two tests still fail"},
		"IMPOSSIBLE: it asks for both 0 and 1 files":      {GoalImpossible, "it asks for both 0 and 1 files"},
		"Looking at the record.\nMET: the build exits 0":  {GoalMet, "the build exits 0"},
		"MET": {GoalMet, "no reason given"},
	} {
		got, err := parseGoalVerdict(in)
		if err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "METHOD: none", "The goal looks fine."} {
		if v, err := parseGoalVerdict(in); err == nil {
			t.Errorf("%q: %+v", in, v)
		}
	}
}

// goalJudge answers a goal check and keeps what it was asked.
type goalJudge struct {
	answer string
	req    *llm.Request
}

func (p *goalJudge) Name() string { return "fake" }
func (p *goalJudge) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	p.req = req
	return &llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{text(p.answer)}, Usage: &llm.Usage{Input: 900, Output: 12}}, nil
}

// The check reads the condition and the record: the user's words, the
// agent's, its calls and the end of their output, where a test run says
// how it went. Reminders stay out.
func TestCheckGoal(t *testing.T) {
	long := strings.Repeat("=== RUN TestX\n", 200) + "FAIL\tgithub.com/x/y\t0.3s"
	a := &Agent{Messages: []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{text("make the tests pass")}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{text("Running them."), use("1", "bash", `{"command":"go test ./..."}`)}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "1", Content: long, IsError: true}, text("<system-reminder>todo</system-reminder>")}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{text("All tests pass now.")}},
	}}
	judge := &goalJudge{answer: "NOT_MET: the only test run shown failed (FAIL github.com/x/y)"}
	v, err := a.CheckGoal(context.Background(), SmallModel{Provider: judge, Model: "small"}, "go test ./... passes", 2)
	if err != nil || v.State != GoalNotMet || !strings.Contains(v.Reason, "failed") {
		t.Fatalf("%+v %v", v, err)
	}
	req := judge.req
	sent := req.Messages[0].Blocks[0].Text
	for _, want := range []string{"<goal>\ngo test ./... passes\n</goal>", `turns_checked_before="2"`, "user: make the tests pass",
		"agent: Running them.", "→ ", "✗ === RUN TestX", "FAIL\tgithub.com/x/y", "agent: All tests pass now."} {
		if !strings.Contains(sent, want) {
			t.Errorf("record lacks %q:\n%s", want, sent)
		}
	}
	if strings.Contains(sent, "todo") || req.Class != llm.ClassAuxiliary || req.Model != "small" || req.Thinking != "" {
		t.Errorf("request %+v:\n%s", req, sent)
	}
	if u := a.Usage(); u.Input != 900 {
		t.Errorf("usage %+v", u)
	}
}

func TestLastTurnUsedTools(t *testing.T) {
	user := llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{text("go")}}
	call := llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{use("1", "bash", `{}`)}}
	result := llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "1"}, text("steered in")}}
	reminder := llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{text("<system-reminder>x</system-reminder>")}}
	answer := llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{text("done")}}
	for name, c := range map[string]struct {
		msgs []llm.Message
		want bool
	}{
		"calls":          {[]llm.Message{user, call, result, reminder, answer}, true},
		"answer only":    {[]llm.Message{user, call, result, answer, user, answer}, false},
		"reminder first": {[]llm.Message{user, answer, reminder, answer}, false},
		"empty":          {nil, false},
	} {
		if got := (&Agent{Messages: c.msgs}).LastTurnUsedTools(); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}
