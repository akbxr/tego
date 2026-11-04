package video

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
)

func TestSniff(t *testing.T) {
	for name, c := range map[string]struct {
		head string
		want bool
	}{
		"mp4":   {"\x00\x00\x00\x20ftypisom\x00\x00\x02\x00", true},
		"mov":   {"\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00", true},
		"m4a":   {"\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00", false},
		"heic":  {"\x00\x00\x00\x18ftypheic\x00\x00\x00\x00", false},
		"webm":  {"\x1a\x45\xdf\xa3\x9f\x42\x86\x81\x01\x42\xf7\x81", true},
		"avi":   {"RIFF\x00\x00\x00\x00AVI LIST", true},
		"wav":   {"RIFF\x00\x00\x00\x00WAVEfmt ", false},
		"png":   {"\x89PNG\r\n\x1a\n\x00\x00\x00\x0d", false},
		"text":  {"package main\n", false},
		"short": {"\x00\x00", false},
	} {
		if got := Sniff([]byte(c.head)); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	if !HasExt("/a/Screen Recording.MOV") || HasExt("main.ts") || HasExt("a.png") {
		t.Fatal("HasExt")
	}
}

func TestTimes(t *testing.T) {
	got := Times(0, 20, 0)
	if len(got) != 10 || got[0] != 1 || got[9] != 19 {
		t.Fatalf("20 s: %v", got)
	}
	if got := Times(0, 600, 0); len(got) != 16 {
		t.Fatalf("10 min: %d frames", len(got))
	}
	if got := Times(0, 2, 0); len(got) != 6 {
		t.Fatalf("2 s: %d frames", len(got))
	}
	if got := Times(5, 6, 200); len(got) != MaxFrames || got[0] < 5 || got[len(got)-1] > 6 {
		t.Fatalf("capped: %d frames, %v..%v", len(got), got[0], got[len(got)-1])
	}
	if got := Times(3, 3, 0); len(got) != 1 || got[0] != 3 {
		t.Fatalf("still: %v", got)
	}
}

func TestStamp(t *testing.T) {
	for sec, want := range map[float64]string{0: "0:00.0", 5.25: "0:05.3", 83.5: "1:23.5", 3725: "1:02:05.0"} {
		if got := Stamp(sec); got != want {
			t.Errorf("%v: %q, want %q", sec, got, want)
		}
	}
}

// Frames come out in order with their timestamps, sized down, rotated
// as shown; part of the clip can be looked at closer.
func TestRead(t *testing.T) {
	if !Available() {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	flat, clip := filepath.Join(dir, "flat.mp4"), filepath.Join(dir, "clip.mp4")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=8", "-f", "lavfi", "-i", "sine=duration=8",
		"-shortest", "-pix_fmt", "yuv420p", flat)
	ffmpeg(t, "-display_rotation", "90", "-i", flat, "-c", "copy", clip) // a phone held upright
	c, err := Read(context.Background(), clip, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Frames) != 6 || c.Width != 720 || c.Height != 1280 || !c.Audio || c.FPS != 30 {
		t.Fatalf("%d frames, %dx%d, audio %v, %v fps", len(c.Frames), c.Width, c.Height, c.Audio, c.FPS)
	}
	desc := c.Describe("clip.mp4")
	if desc != "Video clip.mp4 (8.0 s, 720x1280, 30 fps, has audio (not included)): 6 frames in order, at 0:00.7, 0:02.0, 0:03.3, 0:04.7, 0:06.0, 0:07.3" {
		t.Fatalf("describe %q", desc)
	}
	c, err = Read(context.Background(), clip, Options{Start: 2, End: 3, Frames: 2})
	if err != nil || len(c.Frames) != 2 || !strings.Contains(c.Describe("c"), "at 0:02.3, 0:02.8 (from the part 0:02.0 to 0:03.0)") {
		t.Fatalf("part: %v %q", err, c.Describe("c"))
	}
	if _, err := Read(context.Background(), clip, Options{Start: 30}); err == nil || !strings.Contains(err.Error(), "past the end") {
		t.Fatalf("past the end: %v", err)
	}
	audio := filepath.Join(dir, "a.m4a")
	ffmpeg(t, "-f", "lavfi", "-i", "sine=duration=1", audio)
	if _, err := Read(context.Background(), audio, Options{}); err == nil || !strings.Contains(err.Error(), "no video stream") {
		t.Fatalf("audio only: %v", err)
	}
}

func ffmpeg(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("ffmpeg", append([]string{"-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
}

// A model that takes video gets a small file in a format it reads as is,
// anything else re-encoded with its sound; a part of it says where it
// sits, and turns back into frames timed in the whole.
func TestEncode(t *testing.T) {
	if !Available() {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	clip, mkv := filepath.Join(dir, "clip.mp4"), filepath.Join(dir, "clip.mkv")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=30:duration=8", "-f", "lavfi", "-i", "sine=duration=8",
		"-shortest", "-pix_fmt", "yuv420p", clip)
	ffmpeg(t, "-i", clip, "-c", "copy", mkv)
	c, b, err := Encode(context.Background(), clip, "clip.mp4", Options{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(clip)
	if b.Type != llm.BlockVideo || b.MediaType != "video/mp4" || b.Data != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("original: %s %s, %d bytes", b.Type, b.MediaType, len(b.Data))
	}
	if got := c.DescribeNative("clip.mp4"); got != "Video clip.mp4 (8.0 s, 1920x1080, 30 fps, sound included): attached as a video" {
		t.Fatalf("describe %q", got)
	}
	m := ReadMeta(b)
	if m.Name != "clip.mp4" || m.End != 8 || !m.Audio || Seconds(b) != 8 {
		t.Fatalf("meta %+v", m)
	}

	_, b, err = Encode(context.Background(), mkv, "clip.mkv", Options{})
	if err != nil {
		t.Fatal(err)
	}
	enc := filepath.Join(dir, "enc.mp4")
	data, _ := base64.StdEncoding.DecodeString(b.Data)
	_ = os.WriteFile(enc, data, 0o644)
	e, err := probe(context.Background(), enc)
	if err != nil || b.MediaType != "video/mp4" || e.Width != 1280 || e.Height != 720 || !e.Audio || e.FPS > 2 {
		t.Fatalf("mkv encoded: %v %s %dx%d %v fps, audio %v", err, b.MediaType, e.Width, e.Height, e.FPS, e.Audio)
	}

	c, b, err = Encode(context.Background(), clip, "clip.mp4", Options{Start: 2, End: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.DescribeNative("clip.mp4"); !strings.HasSuffix(got, "attached as a video, the part 0:02.0 to 0:05.0; its own timestamps start at 0:00, which is 0:02.0 in the whole video") {
		t.Fatalf("part: %q", got)
	}
	frames, err := ToFrames(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 7 || frames[1].Type != llm.BlockImage ||
		frames[0].Text != "Video clip.mp4 (8.0 s, 1920x1080, 30 fps, has audio (not included)): 6 frames in order, at 0:02.2, 0:02.6, 0:03.0, 0:03.5, 0:03.9, 0:04.3 (from the part 0:02.0 to 0:05.0)" {
		t.Fatalf("%d blocks, %q", len(frames), frames[0].Text)
	}
	if _, _, err := Encode(context.Background(), clip, "c", Options{Start: 5, End: 5}); err == nil {
		t.Fatal("empty part encoded")
	}
}
