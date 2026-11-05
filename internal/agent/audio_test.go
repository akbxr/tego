package agent

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/audio"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/video"
)

type audioProvider struct{ countingProvider }

func (p *audioProvider) TakesAudio(string) bool { return true }

type fakeWhisper struct{ calls *int }

func (fakeWhisper) Name() string { return "fake" }
func (f fakeWhisper) Transcribe(context.Context, string) ([]audio.Segment, error) {
	*f.calls++
	return []audio.Segment{{Start: 0.2, End: 1.4, Text: " Testing, one two."}}, nil
}

// Audio in the history reaches a model that can't hear as its transcript,
// made once; one that can gets the audio. The history keeps the audio.
func TestAudioAsText(t *testing.T) {
	if !audio.Available() {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	clip := filepath.Join(dir, "a.mp3")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=duration=4", clip).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	_, aud, err := audio.Encode(context.Background(), clip, "a.mp3", audio.Options{Start: 1})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	find, cache := audio.Find, audio.CacheDir
	audio.Find, audio.CacheDir = func() audio.Transcriber { return fakeWhisper{&calls} }, func() string { return dir }
	defer func() { audio.Find, audio.CacheDir = find, cache }()
	history := func() []llm.Message {
		return []llm.Message{
			{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "listen"}, aud}},
			{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "r1", Name: "read", Input: []byte(`{"path":"a.mp3"}`)}}},
			{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "r1", Content: "Audio a.mp3: attached as audio", Images: []llm.Block{aud}}}},
		}
	}

	prov := &countingProvider{}
	a := &Agent{Provider: prov, UI: nopUI{}, Messages: history()}
	if err := a.Run(context.Background(), "and now?"); err != nil {
		t.Fatal(err)
	}
	sent := prov.last.Messages
	if hasType(sent, llm.BlockAudio) || len(sent[0].Blocks) != 2 || !strings.Contains(sent[0].Blocks[1].Text, "\n[0:01.2-0:02.4] Testing, one two.") ||
		!strings.Contains(sent[0].Blocks[1].Text, "the part 0:01.0 to 0:04.0") {
		t.Fatalf("attached: %d blocks %q", len(sent[0].Blocks), sent[0].Blocks[1].Text)
	}
	if res := sent[2].Blocks[0]; len(res.Images) != 0 || !strings.HasPrefix(res.Content, "Audio a.mp3: attached as audio\nAudio a.mp3 (4.0 s): transcript of the part") {
		t.Fatalf("tool result: %d images, %q", len(res.Images), res.Content)
	}
	if a.Messages[0].Blocks[1].Type != llm.BlockAudio || a.Messages[2].Blocks[0].Images[0].Type != llm.BlockAudio {
		t.Fatal("history lost the audio")
	}
	if err := a.Run(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if prov.last.Messages[0].Blocks[1].Text != sent[0].Blocks[1].Text || calls != 1 {
		t.Fatalf("transcript not reused: %d calls", calls)
	}
	if n := estimateTokens(history()[:1]); n < 3*32 || n > 3*32+50 {
		t.Fatalf("estimate %d tokens for 3 s", n)
	}

	ap := &audioProvider{}
	b := &Agent{Provider: ap, UI: nopUI{}, Messages: history()}
	if err := b.Run(context.Background(), "and now?"); err != nil {
		t.Fatal(err)
	}
	if got := ap.last.Messages; got[0].Blocks[1].Type != llm.BlockAudio || got[2].Blocks[0].Images[0].Type != llm.BlockAudio {
		t.Fatal("a model that takes audio got a transcript")
	}
}

// With a whisper, a model that gets a video's frames gets its sound
// transcribed too.
func TestVideoSoundTrack(t *testing.T) {
	if !audio.Available() {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	clip := filepath.Join(dir, "c.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=10:duration=3", "-f", "lavfi", "-i", "sine=duration=3",
		"-shortest", "-pix_fmt", "yuv420p", clip).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	_, vid, err := video.Encode(context.Background(), clip, "c.mp4", video.Options{})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	find, cache := audio.Find, audio.CacheDir
	audio.Find, audio.CacheDir = func() audio.Transcriber { return fakeWhisper{&calls} }, func() string { return dir }
	defer func() { audio.Find, audio.CacheDir = find, cache }()
	prov := &countingProvider{}
	a := &Agent{Provider: prov, UI: nopUI{}, Messages: []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "watch"}, vid}}}}
	if err := a.Run(context.Background(), "and now?"); err != nil {
		t.Fatal(err)
	}
	got := prov.last.Messages[0].Blocks
	if last := got[len(got)-1]; last.Type != llm.BlockText || !strings.HasPrefix(last.Text, "The sound track of c.mp4 (3.0 s): transcript by local whisper") ||
		!strings.HasSuffix(last.Text, "[0:00.2-0:01.4] Testing, one two.") || got[2].Type != llm.BlockImage {
		t.Fatalf("%d blocks, last %+v", len(got), last)
	}
}
