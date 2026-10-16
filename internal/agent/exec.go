package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

const (
	// maxToolResult caps any single tool result (read pages itself at 50 KB;
	// this catches MCP tools and anything else that floods the context).
	maxToolResult = 64 << 10
	// maxParallelReads bounds read-only tool calls run at once (as in
	// Fan et al. 2026, which allows up to eight per step).
	maxParallelReads = 8
	// Stuck detection thresholds (Fan et al. 2026): remind after this many
	// identical calls (or identical failing calls) in a row, stop after
	// stuckStop identical failing calls.
	stuckRemind = 5
	stuckStop   = 8
)

// runCalls executes one model turn's tool calls and returns their results
// in call order. Consecutive read-only calls (read, grep, glob, web search,
// tool_search) run concurrently; anything that may change state runs alone,
// in order. The UI sees every call and its result in the order they run;
// the results come back in call order.
//
// Todo calls run before the others: a list update sent with other calls
// describes the work they start (the todo tool asks for exactly that), so
// the list must show while they run, not after a long command or subagents.
func (a *Agent) runCalls(ctx context.Context, calls []llm.Block) []llm.Block {
	results := make([]llm.Block, len(calls))
	var rest []int // the calls left after the todo calls, in order
	for i, c := range calls {
		if c.Name == "todo" {
			results[i] = a.execute(ctx, c)
		} else {
			rest = append(rest, i)
		}
	}
	for n := 0; n < len(rest); {
		m := n + 1
		if a.readOnly(calls[rest[n]]) {
			for m < len(rest) && a.readOnly(calls[rest[m]]) {
				m++
			}
		}
		if m-n == 1 {
			results[rest[n]] = a.execute(ctx, calls[rest[n]])
			n = m
			continue
		}
		var wg sync.WaitGroup
		sem := make(chan struct{}, maxParallelReads)
		for _, k := range rest[n:m] {
			a.callStart(calls[k])
		}
		for _, k := range rest[n:m] {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				results[k] = a.runOne(ctx, calls[k])
			}(k)
		}
		wg.Wait()
		for _, k := range rest[n:m] {
			a.UI.OnToolCall(calls[k].Name, calls[k].Input)
			a.UI.OnToolResult(calls[k].Name, calls[k].Input, results[k].Content, results[k].IsError)
			a.callEnd(calls[k], results[k])
			a.toolStat(calls[k].Name, results[k].IsError)
		}
		n = m
	}
	return results
}

func isInvalid(in json.RawMessage) bool {
	_, ok := llm.InvalidInput(in)
	return ok
}

func (a *Agent) readOnly(call llm.Block) bool {
	if isInvalid(call.Input) {
		return false
	}
	ro, ok := a.tool(call.Name).(tools.ReadOnly)
	return ok && ro.ReadOnly()
}

// execute runs one call with the UI notified before and after.
func (a *Agent) execute(ctx context.Context, call llm.Block) llm.Block {
	a.callStart(call)
	t, res, ok := a.prepare(ctx, call)
	if ok {
		a.UI.OnToolCall(call.Name, call.Input)
		res = a.run(ctx, t, call)
	}
	a.UI.OnToolResult(call.Name, call.Input, res.Content, res.IsError)
	a.callEnd(call, res)
	a.toolStat(call.Name, res.IsError)
	return res
}

// CallUI is a UI that follows tool calls by id (an editor through ACP):
// each is announced before it runs, and before the user may be asked
// about it, then marked running, then ended with its result. agent is the
// subagent making the call, "" for the main agent.
type CallUI interface {
	OnCallStart(agent, id, name string, input json.RawMessage)
	OnCallRunning(agent, id string)
	OnCallEnd(agent, id, name string, input json.RawMessage, output string, isErr bool)
}

func (a *Agent) callStart(call llm.Block) {
	if cu, ok := a.UI.(CallUI); ok {
		cu.OnCallStart(a.Name, call.ID, call.Name, call.Input)
	}
}

func (a *Agent) callRunning(call llm.Block) {
	if cu, ok := a.UI.(CallUI); ok {
		cu.OnCallRunning(a.Name, call.ID)
	}
}

func (a *Agent) callEnd(call llm.Block, res llm.Block) {
	if cu, ok := a.UI.(CallUI); ok {
		cu.OnCallEnd(a.Name, call.ID, call.Name, call.Input, res.Content, res.IsError)
	}
}

func (a *Agent) toolStat(name string, isErr bool) {
	if a.ToolStat != nil {
		a.ToolStat(name, isErr, a.Subagent)
	}
}

// ProgressUI is a UI that shows a running tool's output as it arrives
// (bash). Subagents never report to it: their tools run in the background.
type ProgressUI interface {
	OnToolOutput(name string, chunk []byte)
}

// runOne is execute without UI notifications (for concurrent batches).
func (a *Agent) runOne(ctx context.Context, call llm.Block) llm.Block {
	t, res, ok := a.prepare(ctx, call)
	if ok {
		res = a.run(ctx, t, call)
	}
	return res
}

// prepare resolves the tool; when the call cannot run it returns the
// error result and false.
func (a *Agent) prepare(ctx context.Context, call llm.Block) (tools.Tool, llm.Block, bool) {
	res := llm.Block{Type: llm.BlockToolResult, ToolUseID: call.ID, IsError: true}
	if ctx.Err() != nil {
		res.Content = "aborted by user"
		return nil, res, false
	}
	if call.ID != "" && call.ID == a.cutCall {
		res.Content = fmt.Sprintf("Your response hit the output token limit while you were writing this %s call, so its arguments "+
			"may be cut short and it did not run. Send it again in smaller pieces: split large content over several calls "+
			"(write the first part, then add the rest with edit).", call.Name)
		return nil, res, false
	}
	if raw, bad := llm.InvalidInput(call.Input); bad {
		res.Content = fmt.Sprintf("the arguments of this %s call were not valid JSON (truncated or malformed), so it did not run. "+
			"You sent: %s\nResend the call with valid JSON arguments (arrays need [\"quoted\", \"items\"]); split very large inputs.", call.Name, clip(raw, 500))
		return nil, res, false
	}
	t := a.tool(call.Name)
	if t == nil {
		res.Content = fmt.Sprintf("unknown tool %q", call.Name)
		return nil, res, false
	}
	if why := a.planRefusal(t, call); why != "" {
		res.Content = why
		return nil, res, false
	}
	if why := a.permissionRefusal(ctx, t, call); why != "" {
		res.Content = why
		return nil, res, false
	}
	return t, llm.Block{}, true
}

func (a *Agent) run(ctx context.Context, t tools.Tool, call llm.Block) (res llm.Block) {
	a.callRunning(call)
	ctx = tools.WithCallID(ctx, call.ID)
	res = llm.Block{Type: llm.BlockToolResult, ToolUseID: call.ID}
	if call.Name == "read" {
		if note := a.unchangedRead(call); note != "" {
			res.Content = note
			return res
		}
		if st, ok := stampBeforeRead(call); ok {
			defer func() { a.keepStamp(call, st, res) }()
		}
	}
	var out string
	var err error
	if pu, ok := a.UI.(ProgressUI); ok && !a.Subagent {
		ctx = tools.WithProgress(ctx, func(b []byte) { pu.OnToolOutput(call.Name, b) })
	}
	if it, ok := t.(tools.ImageTool); ok {
		out, res.Images, err = it.RunImages(ctx, call.Input)
	} else if rt, ok := t.(refTool); ok {
		out, res.ToolRefs, err = rt.RunRefs(ctx, call.Input)
	} else {
		out, err = t.Run(ctx, call.Input)
	}
	if err != nil {
		res.IsError = true
		if out != "" {
			out += "\n"
		}
		out += err.Error()
	}
	if out == "" {
		out = "(no output)"
	}
	res.Content = capResult(out)
	return res
}

// capResult keeps the start and end of an oversized tool result.
func capResult(s string) string {
	if len(s) <= maxToolResult {
		return s
	}
	head, tail := maxToolResult*3/4, maxToolResult/4
	return strings.ToValidUTF8(s[:head], "") +
		fmt.Sprintf("\n\n[… %d KB omitted: the output was too long. Ask for less (a range, a narrower pattern, a filter) …]\n\n", (len(s)-head-tail)>>10) +
		strings.ToValidUTF8(s[len(s)-tail:], "")
}

// stuckState tracks a run of identical tool calls within one prompt.
type stuckState struct {
	key            string
	same, failures int
	reminded       bool
}

// checkStuck updates the streak with this turn's calls. It adds a one-time
// reminder to the repeated call's result and reports an error once the same
// failing call has been repeated stuckStop times.
func (a *Agent) checkStuck(calls, results []llm.Block) error {
	s := &a.stuck
	for i, c := range calls {
		key := c.Name + "\x00" + string(c.Input)
		if key != s.key {
			*s = stuckState{key: key}
		}
		s.same++
		if results[i].IsError {
			s.failures++
		} else {
			s.failures = 0
		}
		if s.failures >= stuckStop {
			return fmt.Errorf("stopped: the same failing %s call was repeated %d times in a row", c.Name, s.failures)
		}
		if !s.reminded && (s.same >= stuckRemind || s.failures >= stuckRemind) {
			s.reminded = true
			results[i].Content += fmt.Sprintf("\n\n<system-reminder>You have made this exact %s call %d times in a row. "+
				"Repeating it will not change the result: change your approach (different arguments, another tool, or re-read the error).</system-reminder>", c.Name, s.same)
		}
	}
	return nil
}
