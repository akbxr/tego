package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
)

// limitedProvider refuses requests over its image limit, as Anthropic does.
type limitedProvider struct {
	countingProvider
	images, bytes int
	sent          []int // images per request
}

func (p *limitedProvider) MaxMedia(string) (int, int) { return p.images, p.bytes }

func (p *limitedProvider) Stream(ctx context.Context, req *llm.Request, ev func(llm.Event)) (*llm.Message, error) {
	n := len(requestImages(req.Messages))
	p.sent = append(p.sent, n)
	if n > p.images {
		return nil, &llm.APIError{Provider: "fake", Status: 400, Body: fmt.Sprintf("Too much media: 0 document pages + %d images > %d", n, p.images)}
	}
	return p.countingProvider.Stream(ctx, req, ev)
}

// requestImages lists the data of the images msgs send, oldest first.
func requestImages(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockImage && b.Data != "" {
				out = append(out, b.Data)
			}
			for _, img := range b.Images {
				if img.Type == llm.BlockImage && img.Data != "" {
					out = append(out, img.Data)
				}
			}
		}
	}
	return out
}

func frame(i int) llm.Block {
	return llm.Block{Type: llm.BlockImage, MediaType: "image/jpeg", Data: fmt.Sprintf("img%03d", i)}
}

// framesHistory: a prompt with two attached images, then reads of a video
// returning frames frames each.
func framesHistory(reads, frames int) []llm.Message {
	h := []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{
		{Type: llm.BlockText, Text: "compare [Image #1: /refs/a.png] and [Image #2: /refs/b.png]"}, frame(0), frame(1)}}}
	n := 2
	for r := range reads {
		id := fmt.Sprintf("r%d", r)
		h = append(h, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: id, Name: "read", Input: []byte(fmt.Sprintf(`{"path":"render%d.mp4"}`, r))}}})
		res := llm.Block{Type: llm.BlockToolResult, ToolUseID: id, Content: fmt.Sprintf("Video render%d.mp4: %d frames", r, frames)}
		for range frames {
			res.Images = append(res.Images, frame(n))
			n++
		}
		h = append(h, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{res}})
	}
	return h
}

func TestMediaDrop(t *testing.T) {
	sizes := func(n, each int) []int {
		s := make([]int, n)
		for i := range s {
			s[i] = each
		}
		return s
	}
	for _, c := range []struct {
		n, each, maxN, maxB, want int
	}{
		{600, 1, 600, 0, 0},
		{601, 1, 600, 0, 150}, // a quarter of the limit at a time
		{750, 1, 600, 0, 150},
		{751, 1, 600, 0, 300},
		{10, 100, 0, 1000, 0},
		{11, 100, 0, 1000, 3},        // 100 bytes over, a step is 250: 3 images make it
		{20, 100, 0, 1000, 10},       // 1000 over: 4 steps
		{3, 5000, 0, 1000, 2},        // the newest stays, however big
		{700, 100, 600, 50_000, 250}, // the bytes ask for more than the count

	} {
		if got := mediaDrop(sizes(c.n, c.each), c.maxN, c.maxB); got != c.want {
			t.Errorf("mediaDrop(%d×%d, %d, %d) = %d, want %d", c.n, c.each, c.maxN, c.maxB, got, c.want)
		}
	}
}

func TestMediaBudget(t *testing.T) {
	prov := &limitedProvider{images: 8}
	ui := &infoUI{}
	h := framesHistory(3, 3) // 2 + 9 = 11 images
	a := &Agent{Provider: prov, UI: ui, Model: "m", Messages: h}
	if err := a.Run(context.Background(), "what changed?"); err != nil {
		t.Fatalf("refused: %v (sent %v)", err, prov.sent)
	}
	sent := prov.last.Messages
	// 11 over 8 by 3, in steps of 2: the 4 oldest go.
	if imgs := requestImages(sent); len(imgs) != 7 || imgs[0] != "img004" {
		t.Fatalf("sent %d images from %v", len(imgs), imgs[:1])
	}
	if got := sent[0].Blocks[1].Text; got != "[image elided: /refs/a.png; read it again if needed]" || sent[0].Blocks[2].Text != "[image elided: /refs/b.png; read it again if needed]" {
		t.Errorf("attachment stubs %q %q", got, sent[0].Blocks[2].Text)
	}
	if res := sent[2].Blocks[0]; len(res.Images) != 1 || !strings.HasSuffix(res.Content, "\n[2 images elided: render0.mp4; read it again if needed]") {
		t.Errorf("tool result: %d images, %q", len(res.Images), res.Content)
	}
	if len(requestImages(a.Messages)) != 11 {
		t.Error("the history lost images")
	}
	if len(ui.infos) != 1 || !strings.Contains(ui.infos[0], "11 images in the context, more than m takes in one request: the oldest 4") {
		t.Fatalf("notices %q", ui.infos)
	}

	// One more image, still within the step: the same are left out, so the
	// request starts as the last one did and the cache holds.
	a.Messages = append(a.Messages, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "and this"}, frame(99)}})
	first := fmt.Sprint(sent[:3])
	if err := a.Run(context.Background(), "go on"); err != nil {
		t.Fatal(err)
	}
	if got := prov.last.Messages; fmt.Sprint(got[:3]) != first || len(requestImages(got)) != 8 {
		t.Errorf("the prefix moved: %d images", len(requestImages(got)))
	}
	if len(ui.infos) != 1 {
		t.Errorf("told again: %q", ui.infos)
	}

	// Without a limit nothing changes.
	free := &countingProvider{}
	b := &Agent{Provider: free, UI: nopUI{}, Messages: framesHistory(3, 3)}
	if err := b.Run(context.Background(), "x"); err != nil || len(requestImages(free.last.Messages)) != 11 {
		t.Fatalf("no limit: %v", err)
	}
}

// A history already over the limit, as the subagent's that Anthropic
// refused, goes through on the next request.
func TestMediaBudgetRevivesRefusedHistory(t *testing.T) {
	prov := &limitedProvider{images: 600}
	a := &Agent{Provider: prov, UI: nopUI{}, Model: "claude-opus-5-5", Messages: framesHistory(13, 48)} // 626 images
	if err := a.Run(context.Background(), "go on"); err != nil {
		t.Fatalf("refused: %v", err)
	}
	if prov.sent[0] != 476 {
		t.Errorf("sent %v images", prov.sent)
	}
}
