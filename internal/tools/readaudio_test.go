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
	return []audio.Segment{{Start: 0.5, End: 1, Text: " hi"}}, nil
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
	audio.Find, audio.CacheDir = func() audio.Transcriber { return fakeWhisper{} }, func() string { return dir }
	defer func() { audio.Find, audio.CacheDir = find, cache }()

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
