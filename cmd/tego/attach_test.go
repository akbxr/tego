package main

import (
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/audio"
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

type watchingProvider struct{}

func (watchingProvider) Name() string { return "fake" }
func (watchingProvider) Stream(context.Context, *llm.Request, func(llm.Event)) (*llm.Message, error) {
	return nil, errors.New("unused")
}
func (watchingProvider) TakesVideo(string) bool { return true }

// For a model that takes video, an attached video goes whole, with sound.
func TestVideoAttachedWhole(t *testing.T) {
	if !video.Available() {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	clip := filepath.Join(dir, "clip.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=10:duration=3", "-f", "lavfi", "-i", "sine=duration=3",
		"-shortest", "-pix_fmt", "yuv420p", clip).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	a := &app{cwd: dir, icons: iconPresets["unicode"], ui: &termUI{out: io.Discard, diag: io.Discard, atBOL: true},
		ag: &agent.Agent{Provider: watchingProvider{}, Model: "gemini-3.8-flash"}}
	prompt, blocks := a.collectImages("what happens in " + a.pasteText(clip) + "?")
	if prompt != "what happens in [Video #1: "+clip+"]?" || len(blocks) != 2 || blocks[1].Type != llm.BlockVideo ||
		!strings.Contains(blocks[0].Text, "sound included): attached as a video. Read it with start/end") {
		t.Fatalf("prompt %q, %d blocks, %+v", prompt, len(blocks), blocks[0].Text)
	}
}

type hearingProvider struct{ watchingProvider }

func (hearingProvider) TakesAudio(string) bool { return true }

type fakeWhisper struct{}

func (fakeWhisper) Name() string { return "fake" }
func (fakeWhisper) Transcribe(context.Context, string) ([]audio.Segment, error) {
	return []audio.Segment{{Start: 0, End: 1, Text: " Hello."}}, nil
}

// A dragged audio file becomes a token: a model that hears gets the
// sound, any other a transcript. @mentions go the same way, sound in a
// WebM included.
func TestAudioPathsBecomeTokens(t *testing.T) {
	if !audio.Available() {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	memo, weba := filepath.Join(dir, "Voice Memo 1.m4a"), filepath.Join(dir, "a.webm")
	for _, args := range [][]string{{"-f", "lavfi", "-i", "sine=duration=2", memo}, {"-f", "lavfi", "-i", "sine=duration=2", "-c:a", "libopus", weba}} {
		if out, err := exec.Command("ffmpeg", append([]string{"-v", "error"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v %s", err, out)
		}
	}
	find, cache := audio.Find, audio.CacheDir
	audio.Find, audio.CacheDir, audio.Enabled = func() audio.Transcriber { return fakeWhisper{} }, func() string { return dir }, true
	defer func() { audio.Find, audio.CacheDir, audio.Enabled = find, cache, false }()

	var diag strings.Builder
	a := &app{cwd: dir, icons: iconPresets["unicode"], ui: &termUI{out: io.Discard, diag: &diag, atBOL: true}}
	pasted := a.pasteText(strings.ReplaceAll(memo, " ", `\ `))
	if pasted != "🔊 #1" {
		t.Fatalf("pasted %q", pasted)
	}
	prompt, blocks := a.collectImages("what is said in " + pasted + "?")
	if prompt != "what is said in [Audio #1: "+memo+"]?" || len(blocks) != 1 ||
		!strings.HasSuffix(blocks[0].Text, "You get the words only, not the sound (tone, music, noises).\n[0:00.0-0:01.0] Hello.") {
		t.Fatalf("prompt %q, %d blocks, %+v", prompt, len(blocks), blocks)
	}
	if got := displayPrompt(prompt); got != "what is said in 🔊 #1?" {
		t.Fatalf("display %q", got)
	}
	if strings.Contains(diag.String(), "error") || !strings.Contains(diag.String(), "📎 🔊 #1 Voice Memo 1.m4a · 0:02.0 · transcript by fake, 1 segments") {
		t.Fatalf("shown: %q", diag.String())
	}

	a.ag = &agent.Agent{Provider: hearingProvider{}, Model: "gemini-3.8-flash"}
	prompt, blocks = a.collectImages("and " + a.tokenizeImages(weba)) // .webm: a video token, sound only
	if prompt != "and [Audio #2: "+weba+"]" || len(blocks) != 2 || blocks[1].Type != llm.BlockAudio || blocks[1].MediaType != "audio/mp3" ||
		!strings.HasPrefix(blocks[0].Text, "[Audio #2] Audio "+weba+" (2.0 s): attached as audio") {
		t.Fatalf("webm: prompt %q, %d blocks, %+v", prompt, len(blocks), blocks[0].Text)
	}

	blocks = a.collectMentions("hear @a.webm and @\"Voice Memo 1.m4a\"")
	if len(blocks) != 4 || blocks[1].Type != llm.BlockAudio || blocks[3].Type != llm.BlockAudio ||
		!strings.HasPrefix(blocks[2].Text, `<file path="Voice Memo 1.m4a" type="audio">Audio Voice Memo 1.m4a (2.0 s): attached as audio`) {
		t.Fatalf("mentions: %d blocks, %+v", len(blocks), blocks)
	}
	a.ag = &agent.Agent{Provider: watchingProvider{}, Model: "claude"}
	if blocks = a.collectMentions("hear @a.webm"); len(blocks) != 1 || !strings.Contains(blocks[0].Text, `type="audio">Audio a.webm (2.0 s): transcript by local whisper`) {
		t.Fatalf("mention transcribed: %+v", blocks)
	}
}
