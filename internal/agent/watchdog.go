package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// Watchdog checks on a subagent every Every requests: a model reads what it
// did since the last check and says whether it is getting somewhere. The
// first "stuck" is passed on to the subagent; a second in a row makes it
// write its report and stop. A guard for subagents without a turn limit, and
// for those that would spend most of theirs going in circles.
type Watchdog struct {
	Every    int
	Provider llm.Provider
	Model    string
	Task     string // the subagent's task, to judge drift against

	ch      chan verdict // the check in flight, if any
	from    int          // len(Messages) at the last check
	strikes int          // "stuck" verdicts in a row
}

type verdict struct {
	stuck  bool
	reason string
	usage  *llm.Usage
}

const watchdogSystem = "You check on a coding subagent for the agent that started it. You get its task and what it did in its latest requests: " +
	"its words, its tool calls (→) and their results (✓ or ✗). Answer with one line, nothing else: PROGRESS, or STUCK: <the reason, under 25 words>.\n" +
	"STUCK: no substantive progress in these requests. It repeats the same actions or fails the same way, goes in circles, undoes its own work, " +
	"or works on something other than its task.\n" +
	"PROGRESS: anything else, including long work that moves: reading to understand, building, fixing one failure after another, testing. When unsure, PROGRESS."

// watchdogNote goes to a subagent after its first "stuck" verdict.
func watchdogNote(reason string) string {
	return "A check of your latest requests found no real progress: " + reason + ". " +
		"Step back and change your approach. If you can't finish the task, write your report now instead: what is done, what is left, what blocks you."
}

// watchdogTurn starts a check when one is due, and reports the verdict of a
// finished one: stop (with why) after a second "stuck" in a row.
func (a *Agent) watchdogTurn(ctx context.Context, turn int) (stop bool, why string) {
	w := a.Watchdog
	if w == nil || w.Every <= 0 {
		return false, ""
	}
	if w.ch != nil {
		select {
		case v := <-w.ch:
			w.ch = nil
			a.addUsage(v.usage)
			if !v.stuck {
				w.strikes = 0
				break
			}
			w.strikes++
			a.UI.OnInfo("watchdog: no progress: " + v.reason)
			if w.strikes >= 2 {
				return true, v.reason
			}
			a.remind(watchdogNote(v.reason))
		default:
		}
	}
	if w.ch == nil && turn > 1 && (turn-1)%w.Every == 0 {
		if w.from > len(a.Messages) { // compacted since
			w.from = 0
		}
		digest := watchdogDigest(a.Messages[w.from:])
		w.from = len(a.Messages)
		w.ch = make(chan verdict, 1)
		go func(ch chan verdict) { ch <- w.check(ctx, digest, turn-1) }(w.ch)
	}
	return false, ""
}

// check asks the model for a verdict; a failed check counts as progress.
func (w *Watchdog) check(ctx context.Context, digest string, turns int) verdict {
	prompt := fmt.Sprintf("<task>\n%s\n</task>\n\n<latest_requests made=\"%d in total\">\n%s\n</latest_requests>",
		clip(w.Task, 4000), turns, digest)
	req := &llm.Request{Model: w.Model, System: watchdogSystem, MaxTokens: 100, Class: llm.ClassAuxiliary,
		Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: prompt}}}}}
	msg, err := w.Provider.Stream(ctx, req, func(llm.Event) {})
	if err != nil || msg == nil {
		return verdict{}
	}
	return parseVerdict(textOf(msg), msg.Usage)
}

func parseVerdict(answer string, usage *llm.Usage) verdict {
	v := verdict{usage: usage}
	line, _, _ := strings.Cut(strings.TrimSpace(answer), "\n")
	line = strings.Trim(line, "*` ")
	if rest, ok := strings.CutPrefix(strings.ToUpper(line), "STUCK"); ok {
		v.stuck = true
		v.reason = strings.TrimSpace(strings.TrimLeft(line[len(line)-len(rest):], "*:-— "))
		if v.reason == "" {
			v.reason = "no reason given"
		}
	}
	return v
}

// watchdogDigest is a compact log of msgs: the subagent's words, its calls
// and how they went, the newest kept when it is long.
func watchdogDigest(msgs []llm.Message) string {
	var lines []string
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch {
			case b.Type == llm.BlockText && m.Role == llm.RoleAssistant:
				if t := strings.Join(strings.Fields(b.Text), " "); t != "" {
					lines = append(lines, clip(t, 300))
				}
			case b.Type == llm.BlockToolUse:
				lines = append(lines, "→ "+clip(tools.Summary(b.Name, b.Input), 200))
			case b.Type == llm.BlockToolResult:
				first, _, _ := strings.Cut(strings.TrimSpace(b.Content), "\n")
				if b.IsError {
					lines = append(lines, "  ✗ "+clip(first, 200))
				} else {
					lines = append(lines, "  ✓ "+clip(first, 120))
				}
			}
		}
	}
	const max = 12000
	n, i := 0, len(lines)
	for i > 0 && n+len(lines[i-1]) < max {
		i--
		n += len(lines[i]) + 1
	}
	if i > 0 {
		lines = append([]string{fmt.Sprintf("[%d earlier lines left out]", i)}, lines[i:]...)
	}
	return strings.Join(lines, "\n")
}
