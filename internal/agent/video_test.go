package agent

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/video"
)

type videoProvider struct{ countingProvider }

func (p *videoProvider) TakesVideo(string) bool { return true }

// A video in the history reaches a model that can't watch as frames, the
// same ones every request; one that can gets the video. The history keeps
// the video either way.
func TestVideoAsFrames(t *testing.T) {
	if !video.Available() {
		t.Skip("no ffmpeg")
	}
	clip := filepath.Join(t.TempDir(), "c.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=10:duration=4", "-pix_fmt", "yuv420p", clip).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	_, vid, err := video.Encode(context.Background(), clip, "c.mp4", video.Options{})
	if err != nil {
		t.Fatal(err)
	}
	history := func() []llm.Message {
		return []llm.Message{
			{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "watch"}, vid}},
			{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "r1", Name: "read", Input: []byte(`{"path":"c.mp4"}`)}}},
			{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "r1", Content: "Video c.mp4: attached as a video", Images: []llm.Block{vid}}}},
		}
	}

	prov := &countingProvider{}
	a := &Agent{Provider: prov, UI: nopUI{}, Messages: history()}
	if err := a.Run(context.Background(), "and now?"); err != nil {
		t.Fatal(err)
	}
	sent := prov.last.Messages
	if hasVideo(sent) || len(sent[0].Blocks) != 8 || !strings.Contains(sent[0].Blocks[1].Text, "6 frames in order") {
		t.Fatalf("attached: %d blocks", len(sent[0].Blocks))
	}
	res := sent[2].Blocks[0]
	if len(res.Images) != 6 || !strings.HasPrefix(res.Content, "Video c.mp4: attached as a video\nVideo c.mp4 (4.0 s") {
		t.Fatalf("tool result: %d images, %q", len(res.Images), res.Content)
	}
	if a.Messages[0].Blocks[1].Type != llm.BlockVideo || a.Messages[2].Blocks[0].Images[0].Type != llm.BlockVideo {
		t.Fatal("history lost the video")
	}
	first := sent[0].Blocks[2].Data
	if err := a.Run(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if prov.last.Messages[0].Blocks[2].Data != first || len(a.videoFrames) != 1 {
		t.Fatal("frames not reused")
	}

	vp := &videoProvider{}
	b := &Agent{Provider: vp, UI: nopUI{}, Messages: history()}
	if err := b.Run(context.Background(), "and now?"); err != nil {
		t.Fatal(err)
	}
	if got := vp.last.Messages; got[0].Blocks[1].Type != llm.BlockVideo || got[2].Blocks[0].Images[0].Type != llm.BlockVideo {
		t.Fatal("a model that takes video got frames")
	}
}
