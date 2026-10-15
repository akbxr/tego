package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/akbxr/tego/internal/llm"
)

// checkpointState marks where an exploratory detour began. Rewinding keeps
// history up to the checkpoint call and its results, drops the exploration
// after it, and keeps the model's rewind call (with its report).
type checkpointState struct {
	active   bool
	idx      int // assistant message that called checkpoint
	goal     string
	reminded bool // nudged once to rewind before finishing
}

// CheckpointTool starts a context checkpoint.
type CheckpointTool struct{ Parent *Agent }

func (t *CheckpointTool) Def() llm.Tool {
	return llm.Tool{
		Name: "checkpoint",
		Description: "Start a context checkpoint before exploratory work with many intermediate tool calls (reading, grepping, trying things). " +
			"Later call rewind with a concise report: everything since the checkpoint is removed from context and replaced by the report. " +
			"Only one checkpoint at a time; you must rewind before finishing.",
		Schema: json.RawMessage(`{"type":"object","properties":{"goal":{"type":"string","description":"what the exploration should find out"}},"required":["goal"]}`),
	}
}

func (t *CheckpointTool) Run(_ context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Goal string `json:"goal"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	a := t.Parent
	if a.cp.active {
		return "", fmt.Errorf("a checkpoint is already active (goal: %s); call rewind first", a.cp.goal)
	}
	// The assistant message carrying this call was appended just before tools run.
	a.cp = checkpointState{active: true, idx: len(a.Messages) - 1, goal: strings.TrimSpace(in.Goal)}
	return "Checkpoint set. Explore freely; when done, call rewind with a concise report of your findings (facts, paths, line numbers, conclusions). The exploration will then be dropped from context and replaced by that report.", nil
}

// RewindTool ends the active checkpoint.
type RewindTool struct{ Parent *Agent }

func (t *RewindTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "rewind",
		Description: "End the active checkpoint: drop everything since it from context and keep only your report. Make the report self-contained; the details will be gone.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"report":{"type":"string","description":"concise findings to keep"}},"required":["report"]}`),
	}
}

func (t *RewindTool) Run(_ context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Report string `json:"report"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	a := t.Parent
	if !a.cp.active {
		return "", errors.New("no active checkpoint (none was started, or context was compacted since)")
	}
	if strings.TrimSpace(in.Report) == "" {
		return "", errors.New("report must not be empty")
	}
	// The assistant message carrying this call is the last one in history.
	idx := len(a.Messages) - 1
	req := &rewindReq{msgIdx: idx}
	for _, b := range a.Messages[idx].Blocks {
		if b.Type == llm.BlockToolUse && b.Name == "rewind" {
			req.callID = b.ID
		}
	}
	a.pendingRewind = req
	return "Rewinding to checkpoint.", nil
}

// rewindReq locates the rewind call so history can be rebuilt around it.
type rewindReq struct {
	msgIdx int // assistant message that called rewind
	callID string
}

// applyRewind performs a requested rewind after the current tool batch;
// it returns false when none is pending. The rebuilt history is:
//
//	… checkpoint call, its results, the rewind call (the model's own message,
//	so its report reads as its own work), "rewound" result
//
// and everything in between is dropped.
func (a *Agent) applyRewind() bool {
	if a.pendingRewind == nil {
		return false
	}
	rq, cp := *a.pendingRewind, a.cp
	a.pendingRewind, a.cp = nil, checkpointState{}
	if cp.idx < 0 || rq.msgIdx < cp.idx || rq.msgIdx+1 >= len(a.Messages) {
		return false
	}
	before := estimateTokens(a.Messages)

	var next []llm.Message
	dropped := 0
	if rq.msgIdx == cp.idx { // checkpoint and rewind in one batch
		next = append(next, a.Messages[:cp.idx+2]...)
	} else {
		next = append(next, a.Messages[:cp.idx+2]...)
		dropped = rq.msgIdx - (cp.idx + 2)
		// Keep the rewind message's thinking/text and the rewind call only;
		// other calls in that batch belong to the dropped exploration.
		call := a.Messages[rq.msgIdx]
		var blocks []llm.Block
		for _, b := range call.Blocks {
			if b.Type != llm.BlockToolUse || b.ID == rq.callID {
				blocks = append(blocks, b)
			}
		}
		call.Blocks = blocks
		call.Usage = nil // measured with the exploration still in context
		next = append(next, call, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: rq.callID}}})
	}
	note := fmt.Sprintf("Rewound to the checkpoint. Between the checkpoint and this rewind you really did the exploration (%d messages of tool calls and results); it was removed from context to save space. "+
		"The report in this rewind call is your own summary of that work: trust it and continue from it, do not redo the exploration.", dropped)
	if todos := a.Todos(); len(todos) > 0 {
		note += todoListHeading + RenderTodos(todos)
	}
	// Set the rewind result text (in the same-batch case it lives in the
	// checkpoint's results message).
	last := next[len(next)-1]
	last.Blocks = append([]llm.Block(nil), last.Blocks...)
	for i := range last.Blocks {
		if last.Blocks[i].Type == llm.BlockToolResult && last.Blocks[i].ToolUseID == rq.callID {
			last.Blocks[i].Content, last.Blocks[i].IsError = note, false
		}
	}
	next[len(next)-1] = last

	a.Messages = next
	a.ctxRewrite, a.ctxRewriteAt = 0, 0
	a.LastCtx = a.EstimateContext()
	if err := a.Session.Compacted(next); err != nil {
		a.UI.OnInfo("session write failed: " + err.Error())
	}
	a.UI.OnInfo(fmt.Sprintf("rewound %d messages (~%.1fk tokens) to checkpoint: %s", dropped, float64(before-estimateTokens(next))/1000, cp.goal))
	return true
}

// needsRewindNudge reports (once) that the model is finishing with a
// checkpoint still open, and appends the reminder.
func (a *Agent) needsRewindNudge() bool {
	if !a.cp.active {
		return false
	}
	if a.cp.reminded {
		a.cp = checkpointState{}
		return false
	}
	a.cp.reminded = true
	a.append(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText,
		Text: fmt.Sprintf("<system-reminder>You started a checkpoint (goal: %s) but have not called rewind. Call rewind now with a concise report, then finish.</system-reminder>", a.cp.goal)}}})
	return true
}
