package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/akbxr/tego/internal/images"
	"github.com/akbxr/tego/internal/llm"
)

// mediaBudget returns msgs without their oldest images when one request
// would carry more images, or more image data, than the provider takes
// (llm.MaxMedia): Anthropic refused a subagent that had watched videos as
// frames ("624 images > 600"), and refused it again on every resume. What
// is left out follows from the history alone and moves in steps of a
// quarter of the limit, so the requests between two steps share their
// prefix and the prompt cache holds, and a resumed session leaves out the
// same. The history itself keeps the images. notify reports a step.
func (a *Agent) mediaBudget(msgs []llm.Message, notify bool) []llm.Message {
	msgs = a.fitImages(msgs)
	if a.Provider == nil {
		return msgs
	}
	maxN, maxB := llm.MaxMedia(a.Provider, a.Model)
	if maxN <= 0 && maxB <= 0 {
		return msgs
	}
	type ref struct{ msg, block, img int } // img -1: the block is the image
	var refs []ref
	var sizes []int
	for i, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		for j, b := range m.Blocks {
			switch {
			case b.Type == llm.BlockImage && b.Data != "":
				refs, sizes = append(refs, ref{i, j, -1}), append(sizes, len(b.Data))
			case b.Type == llm.BlockToolResult:
				for k, img := range b.Images {
					if img.Type == llm.BlockImage && img.Data != "" {
						refs, sizes = append(refs, ref{i, j, k}), append(sizes, len(img.Data))
					}
				}
			}
		}
	}
	k := mediaDrop(sizes, maxN, maxB)
	if k <= 0 {
		return msgs
	}
	if notify && k > a.mediaDropped {
		a.mediaDropped = k
		a.UI.OnInfo(fmt.Sprintf("%d images in the context, more than %s takes in one request: the oldest %d are left out of the requests (read them again to see them)",
			len(sizes), a.Model, k))
	}

	drop := map[[2]int][]int{} // message, block → images left out (-1: the block)
	touched := map[int]bool{}
	for _, r := range refs[:k] {
		key := [2]int{r.msg, r.block}
		drop[key] = append(drop[key], r.img)
		touched[r.msg] = true
	}
	paths := toolPaths(msgs)
	out := slices.Clone(msgs)
	for i := range out {
		if !touched[i] {
			continue
		}
		src := mediaSources(out[i].Blocks)
		blocks := make([]llm.Block, 0, len(out[i].Blocks))
		for j := 0; j < len(out[i].Blocks); j++ {
			b := out[i].Blocks[j]
			gone, ok := drop[[2]int{i, j}]
			switch {
			case !ok:
				blocks = append(blocks, b)
			case b.Type == llm.BlockToolResult:
				left := make([]llm.Block, 0, len(b.Images)-len(gone))
				for n, img := range b.Images {
					if !slices.Contains(gone, n) {
						left = append(left, img)
					}
				}
				b.Images = left
				b.Content = strings.TrimSpace(b.Content + "\n" + mediaStub("image", len(gone), paths[b.ToolUseID]))
				blocks = append(blocks, b)
			default:
				// A run of left-out images from one file becomes one stub.
				n := 1
				for j+n < len(out[i].Blocks) && src[j+n] == src[j] && src[j] != "" {
					if _, next := drop[[2]int{i, j + n}]; !next {
						break
					}
					n++
				}
				blocks = append(blocks, llm.Block{Type: llm.BlockText, Text: mediaStub("image", n, src[j])})
				j += n - 1
			}
		}
		out[i].Blocks = blocks
	}
	return out
}

// fitImages returns msgs with every image whose long edge is over
// images.ManyEdge downscaled: Anthropic refuses a request with more than 20
// images if one is larger, and an image an MCP server sent before tego
// downscaled those stayed in the history, failing every request after
// the 20th image, resumes included. Only headers are read; a downscaled
// image is kept, so every request sends the same one and the prompt
// cache holds. The history keeps the original.
func (a *Agent) fitImages(msgs []llm.Message) []llm.Message {
	large := func(b llm.Block) bool {
		if b.Type != llm.BlockImage || b.Data == "" {
			return false
		}
		w, h, err := images.Size(b)
		return err == nil && max(w, h) > images.ManyEdge
	}
	var out []llm.Message
	for i, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		var blocks []llm.Block
		for j, b := range m.Blocks {
			switch {
			case large(b):
				b = a.fitImage(b)
			case b.Type == llm.BlockToolResult && slices.ContainsFunc(b.Images, large):
				imgs := slices.Clone(b.Images)
				for k, img := range imgs {
					if large(img) {
						imgs[k] = a.fitImage(img)
					}
				}
				b.Images = imgs
			default:
				if blocks != nil {
					blocks = append(blocks, b)
				}
				continue
			}
			if blocks == nil {
				blocks = append(make([]llm.Block, 0, len(m.Blocks)), m.Blocks[:j]...)
			}
			blocks = append(blocks, b)
		}
		if blocks == nil {
			continue
		}
		if out == nil {
			out = slices.Clone(msgs)
		}
		out[i].Blocks = blocks
	}
	if out == nil {
		return msgs
	}
	return out
}

// fitImage downscales one image, once; one it can't (WebP) becomes a line
// saying so.
func (a *Agent) fitImage(b llm.Block) llm.Block {
	key := sha256.Sum256([]byte(b.Data))
	a.videoMu.Lock()
	fit, ok := a.imageFits[key]
	a.videoMu.Unlock()
	if ok {
		return fit
	}
	fit, info, err := images.FromBlock(b, "image")
	if err == nil && max(info.Width, info.Height) > images.ManyEdge {
		err = fmt.Errorf("%dx%d is still too large", info.Width, info.Height)
	}
	if err != nil {
		fit = llm.Block{Type: llm.BlockText, Text: fmt.Sprintf("[an image left out: larger than %d px on a side, and it could not be downscaled: %v]", images.ManyEdge, err)}
	}
	a.videoMu.Lock()
	if a.imageFits == nil {
		a.imageFits = map[[32]byte]llm.Block{}
	}
	a.imageFits[key] = fit
	a.videoMu.Unlock()
	return fit
}

// mediaDrop is how many of the oldest images (of the sizes given, oldest
// first) to leave out so that at most maxN images and maxB bytes remain,
// in steps of a quarter of each limit. The newest image always stays.
func mediaDrop(sizes []int, maxN, maxB int) int {
	n, k := len(sizes), 0
	if n == 0 {
		return 0
	}
	if maxN > 0 && n > maxN {
		step := max(1, maxN/4)
		k = (n - maxN + step - 1) / step * step
	}
	if maxB > 0 {
		total := 0
		for _, s := range sizes {
			total += s
		}
		if total > maxB {
			step := max(1, maxB/4)
			target := (total - maxB + step - 1) / step * step
			cut, sum := 0, 0
			for cut < n && sum < target {
				sum += sizes[cut]
				cut++
			}
			k = max(k, cut)
		}
	}
	return min(k, n-1)
}

// toolPaths maps tool call ids to the file the call read, when its input
// names one.
func toolPaths(msgs []llm.Message) map[string]string {
	paths := map[string]string{}
	for _, m := range msgs {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, b := range m.Blocks {
			if b.Type != llm.BlockToolUse || len(b.Input) == 0 {
				continue
			}
			var in struct {
				Path string `json:"path"`
			}
			if json.Unmarshal(b.Input, &in) == nil && in.Path != "" {
				paths[b.ID] = in.Path
			}
		}
	}
	return paths
}
