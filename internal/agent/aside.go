package agent

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/akbxr/tego/internal/llm"
)

// asideKeep is how many earlier side questions a side question sees.
const asideKeep = 6

// asideState is what side questions (/btw) need: the last request a turn
// sent, to ask about while the turn runs, and the side thread so far.
type asideState struct {
	sent   *llm.Request // messages as sent, each with its own blocks
	reply  *llm.Message // the answer to sent, while its tool calls run
	thread []asidePair
}

type asidePair struct{ question, answer string }

// noteAside keeps the request a turn sends, for a side question asked
// before the next one. Only an agent with Asides set keeps it.
func (a *Agent) noteAside(req *llm.Request) {
	if !a.Asides {
		return
	}
	r := *req
	r.Messages = make([]llm.Message, len(req.Messages))
	for i, m := range req.Messages {
		// Its own blocks: elision rewrites the history's in place.
		m.Blocks = slices.Clone(m.Blocks)
		r.Messages[i] = m
	}
	a.asideMu.Lock()
	a.aside.sent, a.aside.reply = &r, nil
	a.asideMu.Unlock()
}

// noteAsideReply keeps the answer to the request noteAside kept: while its
// tool calls run, a side question sees what the model is doing.
func (a *Agent) noteAsideReply(m *llm.Message) {
	if !a.Asides {
		return
	}
	r := *m
	r.Blocks = slices.Clone(m.Blocks)
	a.asideMu.Lock()
	if a.aside.sent != nil {
		a.aside.reply = &r
	}
	a.asideMu.Unlock()
}

// asideRunning answers the tool calls of a reply whose calls still run.
const asideRunning = "[still running: the user asked the side question below meanwhile]"

// asideNotRun answers a tool call a side question's answer made.
const asideNotRun = "Not run: a side question is answered from the conversation only. Answer it in plain text now."

// forgetAsides drops the side thread and what the last turn sent: the
// session changed.
func (a *Agent) forgetAsides() {
	a.asideMu.Lock()
	a.aside = asideState{}
	a.asideMu.Unlock()
}

// AsideRequest builds the request for a side question: the conversation
// as the last request sent it while a turn runs (busy), or as the next
// prompt will send it, then the question. The same model, system prompt,
// tools and thinking, and a cache breakpoint where the conversation ends,
// so the request reads the turn's cache and writes nothing to it. Call it
// from the goroutine that starts turns; AskAside sends it.
func (a *Agent) AsideRequest(ctx context.Context, question string, busy bool) (*llm.Request, error) {
	if a.Provider == nil {
		return nil, errors.New("no model")
	}
	a.asideMu.Lock()
	sent, reply := a.aside.sent, a.aside.reply
	thread := slices.Clone(a.aside.thread)
	a.asideMu.Unlock()
	var req llm.Request
	var tail []llm.Message // after the cached conversation, before the question
	var results []llm.Block
	switch {
	case busy && sent == nil:
		return nil, errors.New("the turn has not sent anything to the model yet; ask again in a moment")
	case busy:
		req = *sent
		req.Messages = slices.Clone(sent.Messages)
		if reply != nil {
			// Its calls are running: they go in as such, so the answer can
			// say what the model is doing.
			tail = append(tail, *reply)
			for _, b := range reply.Blocks {
				if b.Type == llm.BlockToolUse {
					results = append(results, llm.Block{Type: llm.BlockToolResult, ToolUseID: b.ID, Content: asideRunning})
				}
			}
		}
	default:
		if sent != nil && sent.Model == a.Model {
			req = *sent // its system prompt and tools are the ones cached
		} else {
			req = llm.Request{Model: a.Model, System: a.System, Tools: a.toolDefs(), SessionID: a.SessionID, LongCache: a.LongCache}
		}
		req.Thinking, req.Fast, req.MaxTokens = a.Thinking, a.Fast, a.MaxOutputTokens
		msgs := make([]llm.Message, len(a.Messages))
		for i, m := range a.Messages {
			m.Blocks = slices.Clone(m.Blocks)
			msgs[i] = m
		}
		req.Messages = a.mediaBudget(a.audioAsText(ctx, a.videoAsFrames(ctx, msgs)), false)
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("there is no conversation to ask about yet")
	}
	req.Class = llm.ClassAuxiliary
	req.CachePrefix = len(req.Messages)
	req.Messages = append(req.Messages, tail...)
	req.Messages = append(req.Messages, llm.Message{Role: llm.RoleUser,
		Blocks: append(results, llm.Block{Type: llm.BlockText, Text: asidePrompt(question, thread, busy)})})
	return &req, nil
}

func asidePrompt(question string, thread []asidePair, busy bool) string {
	var b strings.Builder
	b.WriteString("<btw>\nThe user asks a side question")
	if busy {
		b.WriteString(" while you work on the task above")
	}
	b.WriteString(". Answer it briefly from what this conversation holds, without calling any tool, as plain text. " +
		"This exchange stays out of the conversation: ")
	if busy {
		b.WriteString("the work goes on afterwards exactly as before, so don't change plans or promise anything here.")
	} else {
		b.WriteString("the next prompt continues the conversation as if it had not happened.")
	}
	if len(thread) > 0 {
		b.WriteString("\n\nEarlier side questions and your answers:")
		for _, p := range thread {
			b.WriteString("\n\nQ: " + p.question + "\nA: " + p.answer)
		}
	}
	b.WriteString("\n\nQuestion: " + question + "\n</btw>")
	return b.String()
}

// asideCalls answers the tool calls in m: not run.
func asideCalls(m *llm.Message) []llm.Block {
	var out []llm.Block
	for _, b := range m.Blocks {
		if b.Type == llm.BlockToolUse {
			out = append(out, llm.Block{Type: llm.BlockToolResult, ToolUseID: b.ID, Content: asideNotRun, IsError: true})
		}
	}
	return out
}

// AskAside sends a side question built by AsideRequest and returns the
// answer's text, which joins the side thread.
func (a *Agent) AskAside(ctx context.Context, question string, req *llm.Request) (string, error) {
	msg, err := a.Provider.Stream(ctx, req, func(llm.Event) {})
	if err != nil {
		return "", err
	}
	answer := strings.TrimSpace(textOf(msg))
	if calls := asideCalls(msg); answer == "" && len(calls) > 0 {
		// It reached for a tool anyway: once, say it doesn't run.
		again := *req
		again.Messages = append(slices.Clip(req.Messages), *msg, llm.Message{Role: llm.RoleUser, Blocks: calls})
		if msg, err = a.Provider.Stream(ctx, &again, func(llm.Event) {}); err != nil {
			return "", err
		}
		answer = strings.TrimSpace(textOf(msg))
	}
	if answer == "" {
		var tools []string
		for _, b := range msg.Blocks {
			if b.Type == llm.BlockToolUse {
				tools = append(tools, b.Name)
			}
		}
		if len(tools) > 0 {
			return "", errors.New("the model wanted to call " + strings.Join(tools, ", ") + " to answer; /btw answers from the conversation only")
		}
		return "", errors.New("the model gave no answer")
	}
	a.asideMu.Lock()
	a.aside.thread = append(a.aside.thread, asidePair{question, answer})
	if n := len(a.aside.thread); n > asideKeep {
		a.aside.thread = slices.Clone(a.aside.thread[n-asideKeep:])
	}
	a.asideMu.Unlock()
	return answer, nil
}
