package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// A tool's work in the background: its report comes as a background
// subagent's does, task_status shows it and cancels it, and it takes no
// messages.
func TestBackgroundJobs(t *testing.T) {
	f := newBGFixture(t, 2)
	jobs := f.bg.Jobs(f.task.Parent)
	release := make(chan struct{})
	id := jobs.Start("mcp blender/render", `{"frame":1}`, func(ctx context.Context, progress func(string)) (string, error) {
		progress("task working: frame 1 of 2")
		select {
		case <-release:
			return "rendered to /tmp/f1.png", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	if id != "t1" || f.bg.Running() != 1 {
		t.Fatalf("id %s, running %d", id, f.bg.Running())
	}
	if _, err := f.status.Run(context.Background(), json.RawMessage(`{"ids":["t1"],"message":"faster"}`)); err == nil || !strings.Contains(err.Error(), "takes no messages") {
		t.Fatalf("message to a job: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(f.taskStatus(t, `{}`), "last: task working: frame 1 of 2") {
		if time.Now().After(deadline) {
			t.Fatalf("status: %s", f.taskStatus(t, `{}`))
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	f.waitDone(t, "t1")
	got := f.bg.Take()
	if !strings.Contains(got, "from subagents and MCP servers") || !strings.Contains(got, "## t1 mcp blender/render (done)\nCall: {\"frame\":1}\n\nrendered to /tmp/f1.png") {
		t.Fatalf("report: %s", got)
	}

	id = jobs.Start("mcp blender/render", "{}", func(ctx context.Context, _ func(string)) (string, error) {
		<-ctx.Done()
		return "", errors.New("mcp blender: " + ctx.Err().Error())
	})
	if s := f.taskStatus(t, `{"ids":["`+id+`"],"cancel":true}`); !strings.Contains(s, "Stopped 1 task(s).") || !strings.Contains(s, id+" mcp blender/render: cancelled") {
		t.Fatalf("cancel: %s", s)
	}
}

// A subagent's tool calls get no Jobs: they wait for their work.
func TestSubagentsGetNoJobs(t *testing.T) {
	var seen []bool
	probe := ctxTool(func(ctx context.Context) { seen = append(seen, tools.JobsFrom(ctx) != nil) })
	main := &Agent{UI: nopUI{}, Jobs: &recordJobs{}}
	main.run(context.Background(), probe, llm.Block{ID: "c1"})
	sub := &Agent{UI: nopUI{}, Subagent: true}
	sub.run(tools.WithJobs(context.Background(), &recordJobs{}), probe, llm.Block{ID: "c2"})
	if len(seen) != 2 || !seen[0] || seen[1] {
		t.Fatalf("jobs seen: %v", seen)
	}
}

type ctxTool func(ctx context.Context)

func (p ctxTool) Def() llm.Tool { return llm.Tool{Name: "probe"} }
func (p ctxTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	p(ctx)
	return "ok", nil
}

type recordJobs struct{}

func (recordJobs) Start(string, string, func(context.Context, func(string)) (string, error)) string {
	return ""
}
