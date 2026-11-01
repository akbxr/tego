package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// judge answers watchdog checks with a fixed verdict.
type judge struct {
	mu      sync.Mutex
	answer  string
	prompts []string
}

func (j *judge) Name() string { return "judge" }

func (j *judge) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if req.System != watchdogSystem || req.Class != llm.ClassAuxiliary || len(req.Tools) != 0 {
		panic("not a watchdog request")
	}
	j.prompts = append(j.prompts, req.Messages[0].Blocks[0].Text)
	return &llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: j.answer}},
		Usage: &llm.Usage{Input: 1000, Output: 10}}, nil
}

func watchedTask(t *testing.T, prov llm.Provider, j *judge, every int) *TaskTool {
	return &TaskTool{Parent: &Agent{Provider: prov, UI: nopUI{}}, Env: &tools.Env{Cwd: t.TempDir()}, Watchdog: every,
		Small: func() (SmallModel, bool) { return SmallModel{Provider: j, Model: "small"}, true }}
}

func runWorker(t *testing.T, task *TaskTool) string {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"tasks": []map[string]string{{"name": "Worker", "prompt": "fix the build"}}})
	out, err := task.Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Stuck twice in a row: told once, then it writes its report and stops.
func TestWatchdogStopsAStuckSubagent(t *testing.T) {
	prov := &busyProvider{stopAt: 400} // would stop by itself much later
	j := &judge{answer: "STUCK: it runs the same glob over and over"}
	out := runWorker(t, watchedTask(t, prov, j, 3))
	if !strings.Contains(out, "failed: stopped by the watchdog, no progress: it runs the same glob over and over") {
		t.Fatalf("report:\n%s", out)
	}
	if prov.requests >= 400 {
		t.Fatal("ran to the end")
	}
	var warned, final bool
	for _, s := range prov.lastSeen {
		warned = warned || strings.Contains(s, "A check of your latest requests found no real progress")
		final = final || strings.Contains(s, "A second check found no real progress") && strings.Contains(s, "This is your last response")
	}
	if !warned || !final {
		t.Fatalf("warned %v, final %v", warned, final)
	}
	// It judged the task and the calls made since the last check.
	if len(j.prompts) < 2 || !strings.Contains(j.prompts[0], "<task>\nfix the build\n</task>") || !strings.Contains(j.prompts[0], "→ glob") {
		t.Fatalf("prompts %q", j.prompts)
	}
	if strings.Contains(j.prompts[1], "*.x1 ") || strings.Contains(j.prompts[1], "*.x1\n") {
		t.Fatalf("second check saw the first's requests again:\n%s", j.prompts[1])
	}
}

// Progress: it goes on to the end, checked along the way.
func TestWatchdogLetsProgressGoOn(t *testing.T) {
	prov := &busyProvider{stopAt: 30}
	j := &judge{answer: "PROGRESS"}
	out := runWorker(t, watchedTask(t, prov, j, 5))
	if prov.requests != 30 || !strings.Contains(out, "(done)\nall done") || len(j.prompts) == 0 {
		t.Fatalf("%d requests, %d checks:\n%s", prov.requests, len(j.prompts), out)
	}
}

func TestParseVerdict(t *testing.T) {
	for answer, want := range map[string]verdict{
		"PROGRESS":                     {},
		"progress, it fixed two tests": {},
		"STUCK: same error again":      {stuck: true, reason: "same error again"},
		"**STUCK** - drifted to docs":  {stuck: true, reason: "drifted to docs"},
		"stuck — loops\nmore":          {stuck: true, reason: "loops"},
		"STUCK":                        {stuck: true, reason: "no reason given"},
		"":                             {},
	} {
		if got := parseVerdict(answer, nil); got != want {
			t.Errorf("%q: %+v, want %+v", answer, got, want)
		}
	}
}

func TestWatchdogDigestKeepsTheNewest(t *testing.T) {
	var msgs []llm.Message
	for i := range 400 {
		in, _ := json.Marshal(map[string]string{"command": "make " + strings.Repeat("x", 50) + string(rune('a'+i%26))})
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "trying again"}, {Type: llm.BlockToolUse, Name: "bash", Input: in}}},
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, Content: "error: no rule\nmore", IsError: true}}})
	}
	d := watchdogDigest(msgs)
	if len(d) > 12500 || !strings.HasPrefix(d, "[") || !strings.Contains(d, "earlier lines left out]") ||
		!strings.HasSuffix(d, "  ✗ error: no rule") || !strings.Contains(d, "→ ") {
		t.Fatalf("digest (%d bytes):\n%s", len(d), d[:300])
	}
}
