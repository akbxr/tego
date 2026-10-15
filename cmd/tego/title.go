package main

import (
	"context"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/session"
)

// titleResult is a generated session title, delivered to the event loop.
type titleResult struct {
	sess  *session.Session
	title string
	err   error
}

// maybeTitle names the session with the model, once, from its first
// prompt. It runs in the background next to the turn; the
// result is applied by the event loop. Until then the status line shows the
// first prompt.
func (a *app) maybeTitle(prompt string) {
	sess := a.ag.Session
	if a.titleDone == nil || sess == nil || sess.Title != "" || a.titlingFor == sess || a.ag.Provider == nil {
		return
	}
	if first := firstPromptText(a.ag.Messages); first != "" {
		prompt = first // a resumed or forked session: name it after how it began
	}
	a.titlingFor = sess
	p, model := a.ag.Provider, a.titleModel()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		t, err := agent.GenerateTitle(ctx, p, model, prompt)
		a.titleDone <- titleResult{sess, t, err}
	}()
}

// titleModel is a small, fast model where the provider has one; otherwise
// the session's model (with thinking off).
func (a *app) titleModel() string {
	if a.cfg.Kind(a.ag.ProviderName) == "anthropic" {
		return "claude-haiku-4-5"
	}
	return a.ag.Model
}

func (a *app) finishTitle(r titleResult) {
	if a.titlingFor == r.sess {
		a.titlingFor = nil
	}
	if r.err != nil || r.title == "" || r.sess.Title != "" {
		return // keep the first-prompt title; a later prompt may try again
	}
	if err := r.sess.SetTitle(r.title); err != nil {
		a.ui.Dim("saving session title: %v", err)
	}
	if r.sess == a.ag.Session {
		a.refreshStatus()
	}
}

// firstPromptText is the first typed prompt in msgs, as the user wrote it.
func firstPromptText(msgs []llm.Message) string {
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Blocks {
			t := strings.TrimSpace(b.Text)
			if b.Type == llm.BlockText && t != "" && !strings.HasPrefix(t, "<context-summary>") &&
				!strings.HasPrefix(t, "<system-reminder>") && !strings.HasPrefix(t, mentionPrefix) && !strings.HasPrefix(t, skillAttachPrefix) {
				return displayPrompt(t)
			}
		}
	}
	return ""
}
