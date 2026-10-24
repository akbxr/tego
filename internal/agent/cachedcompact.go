package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// Cached compaction (as Claude Code's, and pi-cached-compaction): the
// summary is asked for as the next turn of the conversation itself, the
// last request repeated byte for byte with one instruction after it. The
// provider reads nearly all of it from the prompt cache at about a tenth of
// the input price, and the model writing the summary sees everything,
// whole tool output included, where a transcript cuts each result to 2,000
// characters and drops the middle of a long session. When the request
// can't be shown to repeat the last one, the cache may have expired, or the
// transcript costs less, the transcript way runs instead.

// sentRequest is a request as sent, to repeat it.
type sentRequest struct {
	req      llm.Request // its fields but Messages
	provider llm.Provider
	n        int      // how many messages it sent
	sum      [32]byte // of those messages
	at       time.Time
	usage    llm.Usage // the reply's
}

func (a *Agent) noteSent(req *llm.Request) sentRequest {
	s := sentRequest{req: *req, provider: a.Provider, n: len(req.Messages), sum: hashMessages(req.Messages)}
	s.req.Messages = nil
	return s
}

func (s sentRequest) answered(at time.Time, u *llm.Usage) sentRequest {
	s.at = at
	if u != nil {
		s.usage = *u
	}
	return s
}

// hashMessages covers what of the messages goes to the provider.
func hashMessages(msgs []llm.Message) [32]byte {
	h := sha256.New()
	enc := json.NewEncoder(h)
	for i := range msgs {
		m := &msgs[i]
		_ = enc.Encode(struct {
			R llm.Role
			P string
			B []llm.Block
		}{m.Role, m.Provider, m.Blocks})
	}
	var sum [32]byte
	h.Sum(sum[:0])
	return sum
}

// sameProvider: providers are pointers; anything else never compares equal.
func sameProvider(a, b llm.Provider) bool {
	if a == nil || b == nil {
		return false
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	return va.Kind() == reflect.Pointer && vb.Kind() == reflect.Pointer && va.Type() == vb.Type() && va.Pointer() == vb.Pointer()
}

const (
	// implicitCacheWarm is how long a cache without a known lifetime
	// (OpenAI, Codex, Gemini) is taken to be warm: OpenAI keeps one 5 to
	// 10 minutes idle.
	implicitCacheWarm = 4 * time.Minute
	// summaryRoom is the room the summary needs after the conversation.
	summaryRoom = 12_000
	// thinkingCost is a guess at the thinking the summary turn does, in
	// input-token terms (output costs about 5x input): it must run at the
	// conversation's thinking level, as changing that misses the cache.
	thinkingCost = 10_000
)

// cachedSummary asks for the summary as the conversation's next turn. It
// returns "" and why not when the transcript way should run; err only when
// ctx ended.
func (a *Agent) cachedSummary(ctx context.Context, tail []llm.Message, focus, transcript string) (summary, how, why string, err error) {
	s := a.sent
	switch {
	case s.n == 0 || s.at.IsZero():
		return "", "", "", nil // nothing sent yet this session: nothing cached to read
	case s.n >= len(a.Messages) || a.Messages[s.n].Role != llm.RoleAssistant:
		return "", "", "the history changed since the last request", nil
	case s.req.Model != a.Model || !sameProvider(s.provider, a.Provider):
		return "", "", "the model changed", nil
	case s.usage.CacheRead+s.usage.CacheWrite == 0:
		return "", "", "the provider reported no prompt cache", nil
	}
	idle := a.now().Sub(s.at)
	warm := implicitCacheWarm
	if llm.KnownCacheTTL(a.Provider, a.Model) {
		warm = 5 * time.Minute
		if s.req.LongCache {
			warm = time.Hour
		}
		warm -= warm / 10 // a margin: a miss rewrites the whole context
	}
	if idle > warm {
		return "", "", "the prompt cache has expired", nil
	}
	prompt := s.usage.Input + s.usage.CacheRead + s.usage.CacheWrite
	instr := cachedInstruction(a.Messages, tail, focus)
	if instr == "" {
		return "", "", "the first kept message has no text to point at", nil
	}
	fresh := estimateTokens(a.Messages[s.n:]) + len(instr)/4
	if a.ContextWindow > 0 && prompt+fresh+summaryRoom > a.ContextWindow {
		return "", "", "the window has no room for it", nil
	}
	// Reading everything at a tenth of the price against the cut-down
	// transcript at full price. Seeing the whole conversation is worth up
	// to half again as much, or 10k tokens more: a session of big file
	// reads has a small transcript, as it cuts each read to 2,000 chars.
	cost := prompt/10 + fresh
	if s.req.Thinking != "" && s.req.Thinking != "off" {
		cost += thinkingCost
	}
	if plain := len(transcript)/4 + len(summaryPrompt)/4; cost > max(plain+plain/2, plain+10_000) {
		return "", "", "cheaper here", nil
	}
	if hashMessages(a.Messages[:s.n]) != s.sum {
		return "", "", "the history changed since the last request", nil
	}

	req := s.req
	req.Messages = append(slices.Clip(a.Messages), llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: instr}}})
	req.Class = llm.ClassCompaction
	req.CachePrefix = s.n
	msg, err := a.Provider.Stream(ctx, &req, func(llm.Event) {})
	if err != nil {
		if ctx.Err() != nil {
			return "", "", "", ctx.Err()
		}
		return "", "", "the cached request failed: " + err.Error(), nil
	}
	a.addUsage(msg.Usage)
	for _, b := range msg.Blocks {
		if b.Type == llm.BlockToolUse {
			return "", "", "the model called a tool instead", nil
		}
	}
	summary = strings.TrimSpace(textOf(msg))
	if summary == "" {
		return "", "", "the model wrote no summary", nil
	}
	how = "from the conversation"
	if u := msg.Usage; u != nil {
		if total := u.Input + u.CacheRead + u.CacheWrite; total > 0 {
			how += fmt.Sprintf(", %d%% of ~%.0fk tokens read from the prompt cache", u.CacheRead*100/total, float64(total)/1000)
		}
	}
	return summary, how, "", nil
}

// cachedInstruction is the turn that asks for the summary. With a kept
// tail, it quotes where the tail starts: the model sees the tail too, and
// must leave it out.
func cachedInstruction(msgs, tail []llm.Message, focus string) string {
	var b strings.Builder
	b.WriteString("<compaction-request>\nStop here: this conversation is being compacted to free context, and the work goes on afterwards from your summary. Do not call any tools and do not go on with the task; reply with the summary only, and do not mention this request in it.\n\n")
	if len(tail) == 0 {
		b.WriteString("Summarise the whole conversation above.")
	} else {
		which := pointAt(tail[0])
		if which == "" {
			return ""
		}
		nth := 1
		for _, m := range msgs[:len(msgs)-len(tail)] {
			if pointAt(m) == which {
				nth++
			}
		}
		b.WriteString("Summarise the conversation before " + which)
		if nth > 1 {
			fmt.Fprintf(&b, " (occurrence %d of such a message, not an earlier one)", nth)
		}
		fmt.Fprintf(&b, ". That message and the %d after it are kept word for word right after your summary, so leave out everything from that message on: what it and later messages read, found, said or did is not part of the summary, and that work is not done yet as far as the summary goes. Describe the current state as it was just before that message.", len(tail)-1)
	}
	b.WriteString(" Write a dense, factual handoff with exactly these sections (write \"none\" for an empty one):\n\n")
	b.WriteString(summarySections)
	if focus != "" {
		b.WriteString("\n\nFocus especially on: " + focus)
	}
	b.WriteString("\n</compaction-request>")
	return b.String()
}

// pointAt describes a message to point at it: how its text begins, or for
// a message of only tool calls, the first call. "" when it has neither.
func pointAt(m llm.Message) string {
	whose := "the user message"
	if m.Role == llm.RoleAssistant {
		whose = "your message"
	}
	text := ""
	for _, bl := range m.Blocks {
		switch bl.Type {
		case llm.BlockText:
			t := strings.Join(strings.Fields(bl.Text), " ")
			if t != "" && text == "" {
				text = t
			}
			// A prompt's own words rather than a reminder put before them.
			if t != "" && !strings.HasPrefix(t, "<system-reminder>") {
				return whose + " that begins: «" + begin(t, 160) + "»"
			}
		case llm.BlockToolUse:
			if m.Role == llm.RoleAssistant && text == "" {
				in := strings.Join(strings.Fields(string(bl.Input)), " ")
				return "your message that calls the " + bl.Name + " tool with «" + begin(in, 160) + "»"
			}
		}
	}
	if text != "" {
		return whose + " that begins: «" + begin(text, 160) + "»"
	}
	return ""
}

// begin is s's first n runes, and "…" when there is more.
func begin(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
