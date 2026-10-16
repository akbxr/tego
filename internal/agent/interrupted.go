package agent

import (
	"fmt"

	"github.com/akbxr/tego/internal/llm"
)

// Tool calls interrupted by tego stopping. A response's tool calls are
// recorded before they run, their results once all of them are done; when
// tego is killed, crashes or loses its terminal in between, the history ends
// with calls that have no results. Providers refuse such a history (every
// tool call must be answered by the next message), so the session could not
// go on. SetHistory answers them instead, as interrupted.

// interruptedResult is the result of a call tego stopped during.
const interruptedResult = "This call was interrupted: tego stopped before it finished, so its result is unknown. " +
	"It may have run partly or fully: check what it changed before running it again."

// answerInterrupted gives every tool call in msgs that has no result an
// interrupted error result, and returns the history and the number of calls
// answered. Calls at the end of the history are answered by a new message
// (tail, to be recorded in the session); calls followed by another message,
// as in sessions continued by older versions, get their results inserted
// right after them, in memory only, so every load inserts the same thing.
func answerInterrupted(msgs []llm.Message) (out []llm.Message, tail *llm.Message, n int) {
	out = msgs
	for i := 0; i < len(out); i++ {
		m := out[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		answered := map[string]bool{}
		if i+1 < len(out) && out[i+1].Role == llm.RoleUser {
			for _, b := range out[i+1].Blocks {
				if b.Type == llm.BlockToolResult {
					answered[b.ToolUseID] = true
				}
			}
		}
		var missing []llm.Block
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolUse && !answered[b.ID] {
				missing = append(missing, llm.Block{Type: llm.BlockToolResult, ToolUseID: b.ID, IsError: true, Content: interruptedResult})
			}
		}
		if len(missing) == 0 {
			continue
		}
		n += len(missing)
		if i+1 == len(out) {
			tail = &llm.Message{Role: llm.RoleUser, Blocks: missing}
			break
		}
		fixed := make([]llm.Message, 0, len(out)+1)
		fixed = append(fixed, out[:i+1]...)
		if next := out[i+1]; next.Role == llm.RoleUser && len(answered) > 0 {
			// Some calls were answered: the missing results join theirs.
			next.Blocks = append(append([]llm.Block(nil), next.Blocks...), missing...)
			fixed = append(fixed, next)
			out = append(fixed, out[i+2:]...)
		} else {
			fixed = append(fixed, llm.Message{Role: llm.RoleUser, Blocks: missing})
			out = append(fixed, out[i+1:]...)
			i++ // past the inserted results
		}
	}
	return out, tail, n
}

// noteInterrupted answers the calls of a.Messages that have no results (see
// answerInterrupted) and says so.
func (a *Agent) noteInterrupted() {
	msgs, tail, n := answerInterrupted(a.Messages)
	if n == 0 {
		return
	}
	a.Messages = msgs
	if tail != nil {
		a.Messages = append(a.Messages, *tail)
		if err := a.Session.Append(*tail); err != nil && a.UI != nil {
			a.UI.OnInfo("session write failed: " + err.Error())
		}
	}
	if a.UI != nil {
		a.UI.OnInfo(fmt.Sprintf("%d tool call(s) never finished: tego stopped while they ran. They are marked interrupted for the model.", n))
	}
}
