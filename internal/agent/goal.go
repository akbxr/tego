package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// The verdicts of a goal check.
const (
	GoalMet        = "met"
	GoalNotMet     = "not met"
	GoalImpossible = "impossible"
)

// GoalVerdict is what a check of a /goal condition found.
type GoalVerdict struct {
	State  string // GoalMet, GoalNotMet or GoalImpossible
	Reason string
}

const goalSystem = "You check whether a goal holds, for a coding agent's user. You get the goal and a record of the agent's latest work: " +
	"the user's messages, the agent's words, its tool calls (→) and their results (✓ or ✗). You can't run anything: judge only from the record.\n" +
	"Answer with one line, nothing else: MET, NOT_MET or IMPOSSIBLE, a colon, then one sentence.\n" +
	"MET: the record shows it holds, by evidence such as command output or test results. A claim without evidence is not enough. " +
	"Say what shows it.\n" +
	"NOT_MET: anything else. Say what is still missing or unproven, as guidance for the agent's next turn.\n" +
	"IMPOSSIBLE: only when it can never hold, as it contradicts itself or needs what the agent can't have or do. " +
	"Not when it is hard, or stuck once. Say why."

// CheckGoal asks m whether cond holds, judging from the latest messages;
// turns is how many turns were checked before.
func (a *Agent) CheckGoal(ctx context.Context, m SmallModel, cond string, turns int) (GoalVerdict, error) {
	prompt := fmt.Sprintf("<goal>\n%s\n</goal>\n\n<record turns_checked_before=\"%d\">\n%s\n</record>",
		clip(cond, 4000), turns, goalDigest(a.Messages))
	req := &llm.Request{Model: m.Model, System: goalSystem, MaxTokens: 300, Class: llm.ClassAuxiliary,
		Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: prompt}}}}}
	msg, err := m.Provider.Stream(ctx, req, func(llm.Event) {})
	if err != nil {
		return GoalVerdict{}, err
	}
	if msg == nil {
		return GoalVerdict{}, errors.New("no answer")
	}
	a.addUsage(msg.Usage)
	return parseGoalVerdict(textOf(msg))
}

func parseGoalVerdict(answer string) (GoalVerdict, error) {
	for _, line := range strings.Split(strings.TrimSpace(answer), "\n") {
		line = strings.Trim(line, "*`# ")
		up := strings.ToUpper(line)
		for _, v := range []struct{ word, state string }{
			{"NOT_MET", GoalNotMet}, {"NOT MET", GoalNotMet}, {"IMPOSSIBLE", GoalImpossible}, {"MET", GoalMet},
		} {
			if rest, ok := strings.CutPrefix(up, v.word); ok && (rest == "" || strings.ContainsAny(rest[:1], ":-—. *")) {
				reason := strings.TrimSpace(strings.TrimLeft(line[len(v.word):], "*:-—. "))
				if reason == "" {
					reason = "no reason given"
				}
				return GoalVerdict{State: v.state, Reason: reason}, nil
			}
		}
	}
	return GoalVerdict{}, fmt.Errorf("no verdict in the answer %q", clip(strings.TrimSpace(answer), 200))
}

// goalDigest is the record a goal check reads: the newest of msgs, with
// the user's messages, the agent's words in full up to a point, its calls,
// and the start and end of each result, where test runs and builds say
// how they went.
func goalDigest(msgs []llm.Message) string {
	var lines []string
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch {
			case b.Type == llm.BlockText && m.Role == llm.RoleUser:
				if t := strings.TrimSpace(b.Text); t != "" && !strings.HasPrefix(t, "<system-reminder>") {
					lines = append(lines, "user: "+clip(t, 1000))
				}
			case b.Type == llm.BlockText && m.Role == llm.RoleAssistant:
				if t := strings.TrimSpace(b.Text); t != "" {
					lines = append(lines, "agent: "+clip(t, 2500))
				}
			case b.Type == llm.BlockToolUse:
				lines = append(lines, "→ "+clip(tools.Summary(b.Name, b.Input), 300))
			case b.Type == llm.BlockToolResult:
				mark := "✓"
				if b.IsError {
					mark = "✗"
				}
				lines = append(lines, "  "+mark+" "+clip(strings.TrimSpace(b.Content), 800))
			}
		}
	}
	const max = 30000
	n, i := 0, len(lines)
	for i > 0 && n+len(lines[i-1]) < max {
		i--
		n += len(lines[i]) + 1
	}
	if i > 0 {
		lines = append([]string{fmt.Sprintf("[%d earlier entries left out]", i)}, lines[i:]...)
	}
	return strings.Join(lines, "\n")
}

// LastTurnUsedTools reports whether the latest turn made a tool call: from
// the end back to the message that started it.
func (a *Agent) LastTurnUsedTools() bool {
	for i := len(a.Messages) - 1; i >= 0; i-- {
		m := a.Messages[i]
		if m.Role == llm.RoleAssistant {
			for _, b := range m.Blocks {
				if b.Type == llm.BlockToolUse {
					return true
				}
			}
			continue
		}
		// Results (with what was steered in beside them) and reminders come
		// within a turn; anything else started it.
		started := false
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolResult {
				started = false
				break
			}
			if b.Type != llm.BlockText || !strings.HasPrefix(b.Text, "<system-reminder>") {
				started = true
			}
		}
		if started {
			return false
		}
	}
	return false
}
