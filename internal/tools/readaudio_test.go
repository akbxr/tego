package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/audio"
	"github.com/akbxr/tego/internal/llm"
)

type fakeWhisper struct{}

func (fakeWhisper) Name() string { return "fake" }
func (fakeWhisper) Transcribe(context.Context, string) ([]audio.Segment, error) {
	return []audio.Segment{{Start: 1.5, End: 2, Text: " hi"}}, nil
}

// read gives a model that takes audio the sound, any other a transcript;
// sound in a WebM (sniffed as video) goes the same way.
func TestReadAudio(t *testing.T) {
	if !audio.Available() {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"-f", "lavfi", "-i", "sine=duration=3", filepath.Join(dir, "a.mp3")},
		{"-f", "lavfi", "-i", "sine=duration=3", "-c:a", "libopus", filepath.Join(dir, "a.webm")},
	} {
		if out, err := exec.Command("ffmpeg", append([]string{"-v", "error"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v %s", err, out)
		}
	}
	find, cache := audio.Find, audio.CacheDir
	audio.Find, audio.CacheDir, audio.Enabled = func() audio.Transcriber { return fakeWhisper{} }, func() string { return dir }, true
	defer func() { audio.Find, audio.CacheDir, audio.Enabled = find, cache, false }()

	read := &readTool{&Env{Cwd: dir}}
	for _, name := range []string{"a.mp3", "a.webm"} {
		in, _ := json.Marshal(map[string]any{"path": name, "start": 1})
		text, blocks, err := read.RunImages(WithAudio(context.Background()), in)
		if err != nil || len(blocks) != 1 || blocks[0].Type != llm.BlockAudio || blocks[0].MediaType != "audio/mp3" ||
			!strings.HasPrefix(text, "Audio "+name+" (3.0 s): attached as audio, the part 0:01.0 to 0:03.0") {
			t.Fatalf("%s for an audio model: %v %d blocks %q", name, err, len(blocks), text)
		}
		text, blocks, err = read.RunImages(context.Background(), in)
		if err != nil || len(blocks) != 0 || !strings.HasSuffix(text, "\n[0:01.5-0:02.0] hi") {
			t.Fatalf("%s transcribed: %v %d blocks %q", name, err, len(blocks), text)
		}
	}
}

// Frames of a video asked by a model that hears come with the part's sound
// itself, not a transcript (no whisper run).
func TestReadVideoFramesWithSound(t *testing.T) {
	if !audio.Available() {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=10:duration=4",
		"-f", "lavfi", "-i", "sine=duration=4", "-shortest", "-pix_fmt", "yuv420p", filepath.Join(dir, "v.mp4")).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	find := audio.Find
	audio.Find, audio.Enabled = func() audio.Transcriber { t.Fatal("whisper started"); return nil }, true
	defer func() { audio.Find, audio.Enabled = find, false }()
	in, _ := json.Marshal(map[string]any{"path": "v.mp4", "start": 1, "end": 3, "frames": 3})
	text, blocks, err := (&readTool{&Env{Cwd: dir}}).RunImages(WithAudio(WithVideo(context.Background())), in)
	if err != nil || len(blocks) != 4 || blocks[3].Type != llm.BlockAudio || !strings.Contains(text, "sound attached after the frames") {
		t.Fatalf("%v %d blocks %q", err, len(blocks), text)
	}
	if m := audio.ReadMeta(blocks[3]); m.Start != 1 || m.End != 3 {
		t.Fatalf("sound of %+v", m)
	}
}
