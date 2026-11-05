package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	"github.com/akbxr/tego/internal/audio"
	"github.com/akbxr/tego/internal/llm"
)

// audioAsText returns msgs for a model that can't take audio: each audio
// block (attached or returned by a tool for a model that could, before a
// switch) becomes its transcript, or a note saying why there is none. The
// history keeps the audio. Conversions are kept, so every request sends
// the same text and the prompt cache holds.
func (a *Agent) audioAsText(ctx context.Context, msgs []llm.Message) []llm.Message {
	if llm.TakesAudio(a.Provider, a.Model) || !hasType(msgs, llm.BlockAudio) {
		return msgs
	}
	out := slices.Clone(msgs)
	for i, m := range out {
		if !hasType(m.Blocks, llm.BlockAudio) {
			continue
		}
		blocks := make([]llm.Block, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			switch {
			case b.Type == llm.BlockAudio:
				blocks = append(blocks, llm.Block{Type: llm.BlockText, Text: a.transcript(ctx, b)})
			case b.Type == llm.BlockToolResult && hasType(b.Images, llm.BlockAudio):
				imgs := make([]llm.Block, 0, len(b.Images))
				for _, img := range b.Images {
					if img.Type != llm.BlockAudio {
						imgs = append(imgs, img)
						continue
					}
					b.Content = strings.TrimSpace(b.Content + "\n" + a.transcript(ctx, img))
				}
				b.Images = imgs
				blocks = append(blocks, b)
			default:
				blocks = append(blocks, b)
			}
		}
		out[i].Blocks = blocks
	}
	return out
}

// transcript converts one audio block, once.
func (a *Agent) transcript(ctx context.Context, b llm.Block) string {
	key := sha256.Sum256([]byte(b.Data))
	a.audioMu.Lock()
	defer a.audioMu.Unlock()
	if s, ok := a.audioTexts[key]; ok {
		return s
	}
	m := audio.ReadMeta(b)
	tr, err := audio.TranscribeBlock(ctx, b, m, false)
	if ctx.Err() != nil { // not kept: the next request tries again
		return "[audio omitted: " + ctx.Err().Error() + "]"
	}
	var s string
	if err != nil {
		s = fmt.Sprintf("[audio %s omitted: this model can't take audio, and reading it failed: %v]", m.Name, err)
	} else {
		// Answers about it from before a model switch may cite what it sounds like.
		s = tr.Text() + "\nNote from tego: before a model switch, this conversation ran on a model that took this audio itself, sound included. What earlier replies say about its sound (voice, tone, pace, noises) was heard, not guessed; you get the text above only."
	}
	if a.audioTexts == nil {
		a.audioTexts = map[[32]byte]string{}
	}
	a.audioTexts[key] = s
	return s
}

// hasType reports whether msgs (messages or blocks, tool results' images
// included) hold a block of type t.
func hasType[T llm.Message | llm.Block](items []T, t llm.BlockType) bool {
	for _, it := range items {
		switch v := any(it).(type) {
		case llm.Message:
			if hasType(v.Blocks, t) {
				return true
			}
		case llm.Block:
			if v.Type == t || hasType(v.Images, t) {
				return true
			}
		}
	}
	return false
}

// audioChars is an audio block's share of the chars/4 estimate: Gemini
// counts 32 tokens a second.
func audioChars(b llm.Block) int {
	return max(int(audio.Seconds(b)*32), 100) * 4
}
