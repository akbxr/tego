package main

import (
	"context"
	"strings"
	"time"
)

// btwTimeout bounds a side question's answer.
const btwTimeout = 5 * time.Minute

// btwArgs returns the question of a /btw line, and whether it is one.
func btwArgs(line string) (string, bool) {
	name, args, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	if !strings.HasPrefix(line, "/") || name != "btw" {
		return "", false
	}
	return strings.TrimSpace(args), true
}

// btwCommand answers a side question from the session's conversation, at
// once even while a turn runs, without adding to the conversation: the
// turn goes on as before, and the next prompt doesn't see it. Questions in
// a row form a side thread, which a new session or a rewind ends. It reads
// the conversation's prompt cache rather than writing it again.
func (a *app) btwCommand(question string, busy bool) {
	if question == "" {
		a.dim("/btw <question>: ask about this session on the side, even while it works; the answer stays out of the conversation")
		return
	}
	if a.ag == nil || a.ag.Provider == nil {
		a.ui.Errorf("btw: no model: /login, then /model")
		return
	}
	// Built here, where turns start: the history is not changing now.
	req, err := a.ag.AsideRequest(context.Background(), question, busy)
	if err != nil {
		a.ui.Errorf("btw: %v", err)
		return
	}
	a.ui.AsideAsked(question)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), btwTimeout)
		defer cancel()
		answer, err := a.ag.AskAside(ctx, question, req)
		if err != nil {
			a.ui.Errorf("btw: %v", err)
			return
		}
		a.ui.Aside(question, answer)
	}()
}
