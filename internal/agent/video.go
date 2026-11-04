package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/video"
)

// videoAsFrames returns msgs for a model that can't take video: each video
// block (attached or returned by a tool for a model that could, before a
// switch) becomes its frames with a line saying what they are. The
// history keeps the video. Conversions are kept, so every request sends
// the same frames and the prompt cache holds.
func (a *Agent) videoAsFrames(ctx context.Context, msgs []llm.Message) []llm.Message {
	if llm.TakesVideo(a.Provider, a.Model) || !hasVideo(msgs) {
		return msgs
	}
	out := slices.Clone(msgs)
	for i, m := range out {
		if !hasVideo(m.Blocks) {
			continue
		}
		blocks := make([]llm.Block, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			switch {
			case b.Type == llm.BlockVideo:
				blocks = append(blocks, a.frames(ctx, b)...)
			case b.Type == llm.BlockToolResult && hasVideo(b.Images):
				imgs := make([]llm.Block, 0, len(b.Images))
				for _, img := range b.Images {
					if img.Type != llm.BlockVideo {
						imgs = append(imgs, img)
						continue
					}
					for _, f := range a.frames(ctx, img) {
						if f.Type == llm.BlockText {
							b.Content = strings.TrimSpace(b.Content + "\n" + f.Text)
						} else {
							imgs = append(imgs, f)
						}
					}
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

// frames converts one video block, once.
func (a *Agent) frames(ctx context.Context, b llm.Block) []llm.Block {
	key := sha256.Sum256([]byte(b.Data))
	a.videoMu.Lock()
	defer a.videoMu.Unlock()
	if f, ok := a.videoFrames[key]; ok {
		return f
	}
	f, err := video.ToFrames(ctx, b)
	if err != nil {
		if ctx.Err() != nil { // not kept: the next request tries again
			return []llm.Block{{Type: llm.BlockText, Text: "[video omitted: " + err.Error() + "]"}}
		}
		f = []llm.Block{{Type: llm.BlockText, Text: fmt.Sprintf("[video %s omitted: this model can't take video, and turning it into frames failed: %v]", video.ReadMeta(b).Name, err)}}
	}
	if a.videoFrames == nil {
		a.videoFrames = map[[32]byte][]llm.Block{}
	}
	a.videoFrames[key] = f
	return f
}

// hasVideo reports whether msgs (messages or blocks, tool results' images
// included) hold a video block.
func hasVideo[T llm.Message | llm.Block](items []T) bool {
	for _, it := range items {
		switch v := any(it).(type) {
		case llm.Message:
			if hasVideo(v.Blocks) {
				return true
			}
		case llm.Block:
			if v.Type == llm.BlockVideo || hasVideo(v.Images) {
				return true
			}
		}
	}
	return false
}

// videoChars is a video block's share of the chars/4 estimate: about 100
// tokens a second watched, at least what its frames would cost.
func videoChars(b llm.Block) int {
	return max(int(video.Seconds(b)*100), 2500) * 4
}
