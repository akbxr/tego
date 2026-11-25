package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// Tasks (the MCP tasks extension): a server may answer a call with a task
// it runs on its own, which the client polls (tasks/get) until it ends,
// answers when it asks the user something (tasks/update), or cancels
// (tasks/cancel). A call whose task ends within taskGrace returns as any
// call; a longer one goes on in the background of the session (tools.Jobs),
// its result coming in a message when it ends. Without Jobs (a subagent,
// tego -p) the call waits for it.

var (
	// taskGrace is how long a call waits for its task before it goes to the
	// background.
	taskGrace = 5 * time.Second
	// taskPoll is the poll interval when the server suggests none; polls
	// are kept within taskPollMin..taskPollMax.
	taskPoll    = 2 * time.Second
	taskPollMin = 250 * time.Millisecond
	taskPollMax = 30 * time.Second
	// taskLostWait is how long a task the server says it doesn't know
	// (-32602) is asked after again, before its outcome is called unknown.
	taskLostWait = 5 * time.Minute
)

// taskState is a task as tools/call and tasks/get return it.
type taskState struct {
	TaskID         string          `json:"taskId"`
	Status         string          `json:"status"` // working, input_required, completed, failed, cancelled
	StatusMessage  string          `json:"statusMessage"`
	PollIntervalMs *int64          `json:"pollIntervalMs"`
	Result         json.RawMessage `json:"result"` // completed: a tools/call result
	Error          *rpcError       `json:"error"`  // failed
	// input_required: what it asks (the call's own field, shared)
}

type taskGet struct {
	taskState
	InputRequests map[string]inputRequest `json:"inputRequests"`
}

func (st *taskState) interval() time.Duration {
	d := taskPoll
	if st.PollIntervalMs != nil && *st.PollIntervalMs > 0 {
		d = time.Duration(*st.PollIntervalMs) * time.Millisecond
	}
	return min(max(d, taskPollMin), taskPollMax)
}

// errTaskRunning: the task still runs at the deadline poll was given.
var errTaskRunning = errors.New("task still running")

// task follows one task of s on c.
type task struct {
	s        *server
	c        *conn
	st       taskState
	asks     map[string]inputRequest // the input it asks for now
	answered map[string]bool
	lost     time.Time // when the server first said it doesn't know it
}

// awaitTask follows the task the call r started: it waits taskGrace, then
// goes on in the background where the session has Jobs, else waits.
func (t *mcpTool) awaitTask(ctx context.Context, c *conn, r *callResult, input json.RawMessage) (string, []llm.Block, error) {
	tk := &task{s: t.s, c: c, st: r.taskState, asks: r.InputRequests, answered: map[string]bool{}}
	if tk.st.TaskID == "" {
		return "", nil, fmt.Errorf("mcp %s: the server answered with a task without an id", t.s.cfg.Name)
	}
	say := progressLines(ctx)
	jobs := tools.JobsFrom(ctx)
	if jobs == nil {
		return tk.poll(ctx, time.Time{}, say)
	}
	text, images, err := tk.poll(ctx, time.Now().Add(taskGrace), say)
	if !errors.Is(err, errTaskRunning) {
		return text, images, err
	}
	// The job holds the server as a call does: an idle one isn't stopped
	// under it.
	if _, err := t.s.acquire(); err != nil {
		return "", nil, err
	}
	name := "mcp " + t.s.cfg.Name + "/" + t.remote
	id := jobs.Start(name, oneLine(string(input), 200), func(jctx context.Context, progress func(string)) (string, error) {
		defer t.s.release()
		text, images, err := tk.poll(jctx, time.Time{}, progress)
		if len(images) > 0 {
			text += fmt.Sprintf("\n[%d image(s) in the result are not shown here: call the tool again if you need them]", len(images))
		}
		return text, err
	})
	return fmt.Sprintf("The %s server runs this call as a long task (%s), which goes on in the background as %s: its result comes in a message when it ends. "+
		"task_status checks on it, waits for it or cancels it. Don't call the tool again for the same work meanwhile.", t.s.cfg.Name, tk.st.TaskID, id), nil, nil
}

// progressLines is a func that shows a task's status in the call's output
// as it changes (nil-safe).
func progressLines(ctx context.Context) func(string) {
	out := tools.Progress(ctx)
	return func(s string) {
		if out != nil && s != "" {
			out([]byte(s + "\n"))
		}
	}
}

// poll follows the task until it ends, until (zero: no limit) or ctx ends:
// then it cancels the task. say is told each new status.
func (tk *task) poll(ctx context.Context, until time.Time, say func(string)) (string, []llm.Block, error) {
	last := ""
	for {
		if msg := tk.describe(); msg != last {
			last = msg
			say(msg)
		}
		switch tk.st.Status {
		case "completed":
			if len(tk.st.Result) == 0 || string(tk.st.Result) == "null" {
				break // the first answer says so without the result: get it
			}
			var r callResult
			if err := json.Unmarshal(tk.st.Result, &r); err != nil {
				return "", nil, fmt.Errorf("mcp %s: the task's result: %w", tk.s.cfg.Name, err)
			}
			return r.output()
		case "failed":
			msg := "the task failed"
			if tk.st.Error != nil && tk.st.Error.Message != "" {
				msg += ": " + tk.st.Error.Message
			} else if tk.st.StatusMessage != "" {
				msg += ": " + tk.st.StatusMessage
			}
			return "", nil, fmt.Errorf("mcp %s: %s", tk.s.cfg.Name, msg)
		case "cancelled":
			return "", nil, fmt.Errorf("mcp %s: the server cancelled the task", tk.s.cfg.Name)
		case "input_required":
			if err := tk.update(ctx); err != nil {
				tk.cancel()
				return "", nil, err
			}
		}
		wait := tk.st.interval()
		if tk.st.Status == "completed" {
			wait = 0
		}
		if !until.IsZero() && time.Now().Add(wait).After(until) {
			return "", nil, errTaskRunning
		}
		select {
		case <-ctx.Done():
			tk.cancel()
			return "", nil, ctx.Err()
		case <-tk.c.life.Done():
			return "", nil, fmt.Errorf("mcp %s: the server stopped while its task ran; its outcome is unknown", tk.s.cfg.Name)
		case <-time.After(wait):
		}
		if err := tk.get(ctx); err != nil {
			if ctx.Err() != nil {
				tk.cancel()
				return "", nil, ctx.Err()
			}
			return "", nil, err
		}
	}
}

// describe is the task's status for the user.
func (tk *task) describe() string {
	s := "task " + strings.ReplaceAll(tk.st.Status, "_", " ")
	if tk.st.StatusMessage != "" {
		s += ": " + oneLine(tk.st.StatusMessage, 200)
	}
	return s
}

// get asks for the task's state. A task the server says it doesn't know is
// asked after again for taskLostWait.
func (tk *task) get(ctx context.Context) error {
	var r taskGet
	err := tk.c.call(ctx, "tasks/get", map[string]any{"taskId": tk.st.TaskID}, &r)
	if rpcCode(err) == -32602 {
		if tk.lost.IsZero() {
			tk.lost = time.Now()
		}
		if time.Since(tk.lost) > taskLostWait {
			return fmt.Errorf("mcp %s: the server no longer knows the task %s; its outcome is unknown", tk.s.cfg.Name, tk.st.TaskID)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("mcp %s: tasks/get: %w", tk.s.cfg.Name, err)
	}
	tk.lost = time.Time{}
	if r.TaskID == "" {
		r.TaskID = tk.st.TaskID
	}
	tk.st, tk.asks = r.taskState, r.InputRequests
	return nil
}

// update answers what the task asks (each key once) with tasks/update.
func (tk *task) update(ctx context.Context) error {
	answers, err := tk.s.answer(ctx, tk.asks, tk.answered)
	if err != nil || len(answers) == 0 {
		return err
	}
	return tk.c.call(ctx, "tasks/update", map[string]any{"taskId": tk.st.TaskID, "inputResponses": answers}, nil)
}

// cancel asks the server to stop the task, best-effort.
func (tk *task) cancel() {
	if tk.c.dead() {
		return
	}
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	_ = tk.c.call(ctx, "tasks/cancel", map[string]any{"taskId": tk.st.TaskID}, nil)
}

func oneLine(s string, n int) string {
	return truncate(strings.Join(strings.Fields(s), " "), n)
}
