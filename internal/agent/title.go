package agent

import (
	"context"
	"errors"
	"strings"

	"github.com/akbxr/tego/internal/llm"
)

const titlePrompt = `You name coding-agent sessions. From the user's first request, write a short title that says what the session is about.
Reply with the title only: 3 to 7 words, sentence case, no quotes, no trailing punctuation, no emoji. Use the same language as the request. Ignore slash commands such as /skill:name; describe the task itself.`

// GenerateTitle asks model for a short session title based on the first
// prompt. It uses no tools and no thinking.
func GenerateTitle(ctx context.Context, p llm.Provider, model, prompt string) (string, error) {
	if r := []rune(prompt); len(r) > 2000 {
		prompt = string(r[:2000])
	}
	req := &llm.Request{
		Model: model, System: titlePrompt, MaxTokens: 64, Class: llm.ClassAuxiliary,
		Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText,
			Text: "<request>\n" + prompt + "\n</request>\n\nTitle:"}}}},
	}
	msg, err := p.Stream(ctx, req, func(llm.Event) {})
	if err != nil {
		return "", err
	}
	t := cleanTitle(textOf(msg))
	if t == "" {
		return "", errors.New("empty title")
	}
	return t, nil
}

// cleanTitle keeps the first line, drops quotes, markdown and a trailing
// period, and caps the length.
func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	s, _, _ = strings.Cut(s, "\n")
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "Title:"))
	s = strings.Trim(s, "\"'`*#_ ")
	s = strings.TrimRight(s, ".。")
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 60 {
		s = strings.TrimSpace(string(r[:59])) + "…"
	}
	return s
}
