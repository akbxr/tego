package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/akbxr/tego/internal/llm"
)

// Compaction defaults: compact once the context exceeds
// window - max(15% of window, 16k reserve), keep the most recent ~20k tokens
// verbatim and summarise everything before them.
const (
	defaultKeepRecent = 20000
	minReserve        = 16384
)

// CompactAt is the context size (tokens) that triggers auto-compaction.
func (a *Agent) CompactAt() int {
	w := a.ContextWindow
	if w <= 0 {
		return 0
	}
	if a.CompactPercent > 0 && a.CompactPercent < 100 {
		return w * a.CompactPercent / 100
	}
	reserve := max(w*15/100, minReserve)
	if reserve >= w-w*15/100 { // tiny windows: the fixed reserve would eat everything
		reserve = w * 15 / 100
	}
	return w - reserve
}

func (a *Agent) needsCompact() bool {
	if !a.AutoCompact || a.ContextWindow <= 0 || len(a.Messages) < 2 {
		return false
	}
	return a.LastCtx > a.CompactAt()
}

// EstimateContext is the context the next request will use: the latest
// reported usage plus an estimate for messages added since, or a pure
// estimate (system prompt + tool definitions + history) when no trustworthy
// usage exists (fresh, resumed, compacted or rewound history).
func (a *Agent) EstimateContext() int {
	for i := len(a.Messages) - 1; i >= 0; i-- {
		m := a.Messages[i]
		if m.Role == llm.RoleAssistant && m.Usage != nil && m.Usage.ContextTokens() > 0 {
			return m.Usage.ContextTokens() + estimateTokens(a.Messages[i+1:])
		}
		if i < a.ctxRewriteAt && a.ctxRewrite > 0 && a.ctxRewriteAt <= len(a.Messages) {
			// Elision dropped the usage: the measured size minus what it cut.
			return a.ctxRewrite + estimateTokens(a.Messages[a.ctxRewriteAt:])
		}
	}
	return a.overheadTokens() + estimateTokens(a.Messages)
}

func (a *Agent) overheadTokens() int {
	n := len(a.System)
	for _, t := range a.toolDefs() {
		if !t.Deferred { // deferred schemas load only when referenced
			n += len(t.Name) + len(t.Description) + len(t.Schema)
		}
	}
	return n / 4
}

// stripUsage drops recorded usage from messages whose surrounding history
// was rewritten; their numbers describe a context that no longer exists.
func stripUsage(msgs []llm.Message) {
	for i := range msgs {
		msgs[i].Usage = nil
	}
}

// keepCut returns the index from which messages are kept verbatim: the
// earliest safe boundary (an assistant message or a typed prompt, so no
// tool call is separated from its result) whose suffix fits the keep
// budget. -1 means summarise everything.
func (a *Agent) keepCut() int {
	keep := a.KeepRecent
	if keep <= 0 {
		keep = defaultKeepRecent
	}
	if half := a.CompactAt() / 2; half > 0 && keep > half {
		keep = half
	}
	cut, acc := -1, 0
	for i := len(a.Messages) - 1; i >= 1; i-- {
		acc += estimateTokens(a.Messages[i : i+1])
		if acc > keep {
			break
		}
		if m := a.Messages[i]; m.Role == llm.RoleAssistant || isPrompt(m) {
			cut = i
		}
	}
	return cut
}

// summaryPrompt asks for a handoff with fixed sections, so nothing that the
// next context needs is left to chance (the structure follows Lindenbauer
// et al. 2025, "The Complexity Trap", and OpenHands' condenser). An earlier
// summary in the transcript is merged, not dropped, so facts survive any
// number of compactions.
const summaryPrompt = `You are compacting a coding-agent conversation so the work can continue in a fresh context window. The agent will see only your summary and the most recent messages. Write a dense, factual handoff of the transcript below with exactly these sections (write "none" for an empty one):

## User goals and instructions
What the user wants, and every explicit instruction, constraint or preference, quoted verbatim when the wording matters.

## Completed
Work done so far and the key decisions, each with its reason.

## Current state
What was in progress at the end of the transcript, exactly where it stopped.

## Key facts
What was learned that is still needed: code structure, APIs, commands that work, causes of errors.

## Code state
Files created or changed and how (paths, functions, signatures); changes that were tried and reverted.

## Tests and checks
Which tests, builds or commands were run and their latest results, including failures with their exact messages.

## Open problems
Bugs, failing checks, open questions and anything the user still has to answer.

## Next steps
The concrete steps left to finish, in order.

Rules: keep exact names, paths, numbers, versions and error messages; do not invent anything; mark what is uncertain. If the transcript starts with an earlier summary, merge it: keep everything in it that still holds and drop only what is done and no longer matters. Omit pleasantries and tool-call mechanics. Output only the summary.`

// filesHeading lists the files the agent changed, gathered from its edit and
// write calls rather than left to the summary, and carried over from one
// compaction to the next.
const filesHeading = "\n\nFiles changed so far (edit/write): "

// maxFilesListed bounds that list.
const maxFilesListed = 60

// changedFiles lists the paths that edit or write calls in msgs changed,
// after those of an earlier summary's list, in order and without repeats.
func changedFiles(msgs []llm.Message) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p = strings.TrimSpace(p); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	failed := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolResult && b.IsError {
				failed[b.ToolUseID] = true
			}
		}
	}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch {
			case b.Type == llm.BlockText && strings.HasPrefix(b.Text, contextSummaryTag):
				if i := strings.Index(b.Text, filesHeading); i >= 0 {
					line := b.Text[i+len(filesHeading):]
					if j := strings.IndexByte(line, '\n'); j >= 0 {
						line = line[:j]
					}
					line, _, _ = strings.Cut(line, " and ")
					for _, p := range strings.Split(line, ", ") {
						add(p)
					}
				}
			case b.Type == llm.BlockToolUse && (b.Name == "edit" || b.Name == "write") && !failed[b.ID]:
				var in struct {
					Path string `json:"path"`
				}
				if json.Unmarshal(b.Input, &in) == nil {
					add(in.Path)
				}
			}
		}
	}
	return out
}

// Compact summarises older history and keeps the most recent messages
// verbatim. focus optionally steers the summary (/compact <focus>).
func (a *Agent) Compact(ctx context.Context, focus string) error {
	if a.Provider == nil {
		return ErrNoModel
	}
	if len(a.Messages) < 2 {
		return errors.New("nothing to compact")
	}
	head, tail := a.Messages, []llm.Message(nil)
	if cut := a.keepCut(); cut > 0 {
		head, tail = a.Messages[:cut], a.Messages[cut:]
	}
	instr := summaryPrompt
	if focus != "" {
		instr += "\n\nFocus especially on: " + focus
	}
	req := &llm.Request{
		Model: a.Model, System: instr, SessionID: a.SessionID, Class: llm.ClassCompaction,
		Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText,
			Text: "<transcript>\n" + a.fitTranscript(renderTranscript(head)) + "\n</transcript>\n\nWrite the handoff summary now."}}}},
	}
	msg, err := a.Provider.Stream(ctx, req, func(llm.Event) {})
	if err != nil {
		return err
	}
	a.addUsage(msg.Usage)
	summary := strings.TrimSpace(textOf(msg))
	if summary == "" {
		return errors.New("model returned an empty summary")
	}
	// All of the history, kept tail included: the list says "so far".
	if files := changedFiles(a.Messages); len(files) > 0 {
		shown := files[max(len(files)-maxFilesListed, 0):] // the most recent ones
		summary += filesHeading + strings.Join(shown, ", ")
		if n := len(files) - len(shown); n > 0 {
			summary += fmt.Sprintf(" and %d earlier", n)
		}
	}
	// The todo calls are summarised away with the head: carry the exact list
	// over so the model and the UI keep agreeing (and a resume restores it).
	if todos := a.Todos(); len(todos) > 0 {
		summary += todoListHeading + strings.TrimRight(RenderTodos(todos), "\n")
	}
	wrapped := contextSummaryTag + "\nEarlier conversation was compacted to save context. Summary:\n\n" + summary + "\n</context-summary>"

	var next []llm.Message
	switch {
	case tail == nil:
		cont := wrapped
		if last := a.Messages[len(a.Messages)-1]; last.Role == llm.RoleUser && !isPrompt(last) {
			cont += "\n\nContinue the task from where it left off."
		}
		next = []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: cont}}}}
	case tail[0].Role == llm.RoleUser: // a typed prompt: put the summary in front of it
		first := tail[0]
		first.Blocks = append([]llm.Block{{Type: llm.BlockText, Text: wrapped}}, first.Blocks...)
		next = append([]llm.Message{first}, tail[1:]...)
	default: // an assistant message: the summary becomes the user turn before it
		next = append([]llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: wrapped}}}}, tail...)
	}
	stripUsage(next)

	before := a.LastCtx
	if before == 0 {
		before = a.EstimateContext()
	}
	a.Messages = next
	// Indices into the old history are meaningless now.
	a.cp, a.pendingRewind, a.elideFloor, a.cleared, a.clearMark = checkpointState{}, nil, 0, 0, 0
	a.ctxRewrite, a.ctxRewriteAt = 0, 0
	a.LastCtx = a.EstimateContext()
	if err := a.Session.Compacted(next); err != nil {
		a.UI.OnInfo("session write failed: " + err.Error())
	}
	a.UI.OnInfo(fmt.Sprintf("compacted ~%.1fk → ~%.1fk tokens (kept %d recent messages)", float64(before)/1000, float64(a.LastCtx)/1000, len(tail)))
	return nil
}

// fitTranscript keeps the summarisation request inside the context window:
// beyond the budget it keeps the start (earlier summaries, the goal) and
// the most recent part.
func (a *Agent) fitTranscript(t string) string {
	limit := 400_000 // chars, ~100k tokens
	if w := a.ContextWindow; w > 0 && w*3 < limit {
		limit = w * 3
	}
	if len(t) <= limit {
		return t
	}
	headN := limit / 5
	tailN := limit - headN
	return t[:headN] + "\n\n…[middle of the transcript omitted]…\n\n" + t[len(t)-tailN:]
}

// isContextOverflow recognises provider errors for a request larger than
// the model's context window.
func isContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, p := range []string{
		"prompt is too long", "context_length_exceeded", "maximum context length",
		"context window", "input is too long", "too many tokens", "exceeds the context",
		"request too large", "input length and `max_tokens` exceed",
	} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
