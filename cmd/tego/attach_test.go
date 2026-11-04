package main

import (
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/video"
)

func TestImagePathsBecomeTokens(t *testing.T) {
	dir := t.TempDir()
	shot := filepath.Join(dir, "TemporaryItems", "Screenshot 2026-10-12 at 19.43.38.png")
	_ = os.MkdirAll(filepath.Dir(shot), 0o755)
	f, _ := os.Create(shot)
	_ = png.Encode(f, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	f.Close()
	a := &app{cwd: dir, icons: iconPresets["unicode"], ui: &termUI{out: io.Discard, diag: io.Discard, atBOL: true}}
	// A dragged file is pasted as a backslash-escaped path.
	pasted := a.pasteText(strings.ReplaceAll(shot, " ", `\ `))
	if pasted != "🖼 #1" {
		t.Fatalf("pasted %q", pasted)
	}
	line := a.tokenizeImages("see " + pasted + " and #12 as well")
	prompt, blocks := a.collectImages(line)
	if len(blocks) != 1 || prompt != "see [Image #1: "+shot+"] and #12 as well" {
		t.Fatalf("prompt %q, %d blocks", prompt, len(blocks))
	}
	// The transcript shows the token again, never the long path.
	if got := displayPrompt(prompt); got != "see 🖼 #1 and #12 as well" {
		t.Fatalf("display %q", got)
	}
	// A deleted token drops its image; "#1" does not match inside "#12".
	a.pasteText(shot)
	if p, b := a.collectImages("only 🖼 #12 here"); len(b) != 0 || p != "only 🖼 #12 here" {
		t.Fatalf("stale token: %q %d", p, len(b))
	}
}

// A dragged video becomes a token; sending the prompt takes its frames,
// after a line saying what they are.
func TestVideoPathsBecomeTokens(t *testing.T) {
	if !video.Available() {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	clip := filepath.Join(dir, "Screen Recording 1.mov")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=10:duration=6", "-pix_fmt", "yuv420p", clip).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	a := &app{cwd: dir, icons: iconPresets["unicode"], ui: &termUI{out: io.Discard, diag: io.Discard, atBOL: true}}
	pasted := a.pasteText(strings.ReplaceAll(clip, " ", `\ `))
	if pasted != "🎞 #1" {
		t.Fatalf("pasted %q", pasted)
	}
	prompt, blocks := a.collectImages("what happens in " + pasted + "?")
	if prompt != "what happens in [Video #1: "+clip+"]?" {
		t.Fatalf("prompt %q", prompt)
	}
	if len(blocks) != 7 || blocks[0].Type != llm.BlockText || !strings.Contains(blocks[0].Text, "6 frames in order, at 0:00.5, 0:01.5, 0:02.5, 0:03.5, 0:04.5, 0:05.5") {
		t.Fatalf("%d blocks, first %+v", len(blocks), blocks[0])
	}
	for _, b := range blocks[1:] {
		if b.Type != llm.BlockImage || b.MediaType != "image/jpeg" {
			t.Fatalf("frame %+v", b.Type)
		}
	}
	if got := displayPrompt(prompt); got != "what happens in 🎞 #1?" {
		t.Fatalf("display %q", got)
	}
}
