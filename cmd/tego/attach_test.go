package main

import (
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
