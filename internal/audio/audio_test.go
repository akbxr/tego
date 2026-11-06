package audio

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/video"
)

func TestSniff(t *testing.T) {
	theora := "OggS\x00\x02" + strings.Repeat("\x00", 20) + "\x01\x2a\x80theora\x03\x02"
	for name, c := range map[string]struct {
		head string
		want bool
	}{
		"id3":       {"ID3\x04\x00\x00\x00\x00\x00\x00", true},
		"mp3 frame": {"\xff\xfb\x90\x64\x00\x00", true},
		"adts":      {"\xff\xf1\x50\x80\x02\x1f", true},
		"utf16 bom": {"\xff\xfeh\x00i\x00", false},
		"wav":       {"RIFF\x00\x00\x00\x00WAVEfmt ", true},
		"aiff":      {"FORM\x00\x00\x00\x00AIFFCOMM", true},
		"flac":      {"fLaC\x00\x00\x00\x22", true},
		"ogg":       {"OggS\x00\x02" + strings.Repeat("\x00", 20) + "\x01\x1e\x01vorbis", true},
		"ogg video": {theora, false},
		"m4a":       {"\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00", true},
		"mp4":       {"\x00\x00\x00\x20ftypisom\x00\x00\x02\x00", false},
		"webm":      {"\x1a\x45\xdf\xa3\x9f\x42\x86\x81\x01\x42\xf7\x81", false},
		"avi":       {"RIFF\x00\x00\x00\x00AVI LIST", false},
		"png":       {"\x89PNG\r\n\x1a\n\x00\x00\x00\x0d", false},
		"jpeg":      {"\xff\xd8\xff\xe0\x00\x10JFIF", false},
		"text":      {"package main\n", false},
	} {
		if got := Sniff([]byte(c.head)); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	if !HasExt("/a/Voice Memo.M4A") || !HasExt("x.opus") || HasExt("a.mp4") || HasExt("main.go") {
		t.Fatal("HasExt")
	}
}

func ffmpeg(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("ffmpeg", append([]string{"-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
}

// A small file in a format Gemini reads goes as is; any other, or a part,
// is encoded small and mono; past the limit it does not go at all.
func TestEncode(t *testing.T) {
	if !Available() {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	wav, m4a := filepath.Join(dir, "a.wav"), filepath.Join(dir, "a.m4a")
	ffmpeg(t, "-f", "lavfi", "-i", "sine=frequency=440:duration=6", "-ac", "2", wav)
	ffmpeg(t, "-i", wav, m4a)
	c, b, err := Encode(context.Background(), wav, "a.wav", Options{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(wav)
	if b.Type != llm.BlockAudio || b.MediaType != "audio/wav" || b.Data != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("original: %s %s, %d bytes", b.Type, b.MediaType, len(b.Data))
	}
	if got := c.DescribeNative("a.wav"); got != "Audio a.wav (6.0 s): attached as audio" {
		t.Fatalf("describe %q", got)
	}
	if m := ReadMeta(b); m.Name != "a.wav" || m.End != 6 || Seconds(b) != 6 {
		t.Fatalf("meta %+v", m)
	}

	_, b, err = Encode(context.Background(), m4a, "a.m4a", Options{})
	if err != nil {
		t.Fatal(err)
	}
	enc := filepath.Join(dir, "enc")
	data, _ := base64.StdEncoding.DecodeString(b.Data)
	_ = os.WriteFile(enc, data, 0o644)
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,channels,sample_rate", "-of", "csv=p=0", enc).Output()
	if err != nil || b.MediaType != "audio/mp3" || strings.TrimSpace(string(out)) != "mp3,16000,1" || len(data) > 6*4000 {
		t.Fatalf("m4a encoded: %v %s %q, %d bytes", err, b.MediaType, out, len(data))
	}

	c, b, err = Encode(context.Background(), wav, "a.wav", Options{Start: 2, End: 5})
	if err != nil || b.MediaType != "audio/mp3" {
		t.Fatalf("part: %v %s", err, b.MediaType)
	}
	if got := c.DescribeNative("a.wav"); !strings.HasSuffix(got, "attached as audio, the part 0:02.0 to 0:05.0; its own timestamps start at 0:00, which is 0:02.0 in the whole file") {
		t.Fatalf("part: %q", got)
	}
	if _, _, err := Encode(context.Background(), wav, "a", Options{Start: 7}); err == nil || !strings.Contains(err.Error(), "past the end") {
		t.Fatalf("past the end: %v", err)
	}

	long := filepath.Join(dir, "long.flac")
	ffmpeg(t, "-f", "lavfi", "-i", "anullsrc=r=8000:cl=mono", "-t", "3700", long)
	if _, _, err := Encode(context.Background(), long, "long", Options{}); !errors.Is(err, ErrTooLong) {
		t.Fatalf("an hour and more: %v", err)
	}

	clip := filepath.Join(dir, "clip.mp4")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=5:duration=2", "-pix_fmt", "yuv420p", clip)
	if _, _, err := Encode(context.Background(), clip, "clip", Options{}); err == nil || !strings.Contains(err.Error(), "no audio stream") {
		t.Fatalf("silent video: %v", err)
	}
	// Sound in a video container sniffs as video, and is told apart there.
	weba := filepath.Join(dir, "a.webm")
	ffmpeg(t, "-i", wav, "-c:a", "libopus", weba)
	if _, err := video.Read(context.Background(), weba, video.Options{}); !errors.Is(err, video.ErrNoVideo) {
		t.Fatalf("webm audio as video: %v", err)
	}
}

type fakeWhisper struct{ calls *int }

func (fakeWhisper) Name() string { return "fake (tiny)" }
func (f fakeWhisper) Transcribe(_ context.Context, wav string) ([]Segment, error) {
	*f.calls++
	if st, err := os.Stat(wav); err != nil || st.Size() < 1000 {
		return nil, errors.New("no wav")
	}
	return []Segment{{Start: 0, End: 1.5, Text: " Hello there."}, {Start: 1.5, End: 2.9, Text: " General Kenobi."}}, nil
}

func useWhisper(t *testing.T, tr Transcriber) {
	t.Helper()
	find, cache := Find, CacheDir
	dir := t.TempDir()
	Find, CacheDir, Enabled = func() Transcriber { return tr }, func() string { return dir }, true
	t.Cleanup(func() { Find, CacheDir, Enabled = find, cache, false })
}

// A model that can't hear gets a transcript timed in the whole file, done
// once per content; without a whisper, a note saying so and how to get one.
func TestTranscribe(t *testing.T) {
	if !Available() {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	wav := filepath.Join(dir, "a.wav")
	ffmpeg(t, "-f", "lavfi", "-i", "sine=duration=6", wav)
	calls := 0
	useWhisper(t, fakeWhisper{&calls})

	tr, err := Transcribe(context.Background(), wav, "a.wav", Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := "Audio a.wav (6.0 s): transcript by local whisper, fake (tiny), timed in the whole file. You get the words only, not the sound (tone, music, noises).\n[0:00.0-0:01.5] Hello there.\n[0:01.5-0:02.9] General Kenobi."
	if got := tr.Text(); got != want {
		t.Fatalf("text:\n%s", got)
	}
	if _, err := Transcribe(context.Background(), wav, "a.wav", Options{}); err != nil || calls != 1 {
		t.Fatalf("not cached: %v, %d calls", err, calls)
	}
	// A part comes from the whole file's transcript: no second whisper run.
	tr, _ = Transcribe(context.Background(), wav, "a.wav", Options{Start: 2, End: 5})
	if got := tr.Text(); calls != 1 || !strings.Contains(got, "transcript of the part 0:02.0 to 0:05.0 by") ||
		!strings.Contains(got, "\n[0:01.5-0:02.9] General Kenobi.") || strings.Contains(got, "Hello") {
		t.Fatalf("part (%d calls):\n%s", calls, got)
	}

	_, blk, err := Encode(context.Background(), wav, "a.wav", Options{Start: 1, End: 4})
	if err != nil {
		t.Fatal(err)
	}
	tr, err = TranscribeBlock(context.Background(), blk, ReadMeta(blk), false)
	if err != nil || !strings.Contains(tr.Text(), "\n[0:01.0-0:02.5] Hello there.") || calls != 2 {
		t.Fatalf("block: %v\n%s", err, tr.Text())
	}

	Find = func() Transcriber { return nil }
	other := filepath.Join(dir, "b.wav")
	ffmpeg(t, "-f", "lavfi", "-i", "sine=frequency=300:duration=6", other) // not cached
	tr, _ = Transcribe(context.Background(), other, "b.wav", Options{End: 1})
	if got := tr.Text(); !strings.HasPrefix(got, "Audio b.wav (6.0 s): you can't hear it, and no local whisper is installed") || !strings.Contains(got, "pip install mlx-whisper") ||
		!strings.Contains(tr.Summary(), "no whisper installed") {
		t.Fatalf("no whisper:\n%s", got)
	}

	// Off (the default): no whisper runs; a cached transcript still serves.
	Find, Enabled = func() Transcriber { return fakeWhisper{&calls} }, false
	tr, _ = Transcribe(context.Background(), other, "b.wav", Options{End: 1})
	if got := tr.Text(); calls != 2 || !strings.HasPrefix(got, "Audio b.wav (6.0 s): you can't hear it, and transcription is off") ||
		!strings.Contains(got, `"transcribe": true`) || tr.Summary() != "0:06.0 · transcription off: the model can't hear it" {
		t.Fatalf("off (%d calls):\n%s\n%s", calls, got, tr.Summary())
	}
	if SoundTrack(context.Background(), other, "b.wav", 0, 1) != "" {
		t.Fatal("sound track transcribed while off")
	}
	if tr, _ = Transcribe(context.Background(), wav, "a.wav", Options{}); calls != 2 || len(tr.Segments) != 2 {
		t.Fatalf("cached transcript while off: %d calls, %+v", calls, tr)
	}
}

// whisper.cpp writes offsets in milliseconds.
func TestParseCPP(t *testing.T) {
	segs, err := parseCPP([]byte(`{"transcription":[{"timestamps":{"from":"00:00:00,000","to":"00:00:02,760"},"offsets":{"from":0,"to":2760},"text":" The quick brown fox."}]}`))
	if err != nil || len(segs) != 1 || segs[0].End != 2.76 || segs[0].Text != " The quick brown fox." {
		t.Fatalf("%v %+v", err, segs)
	}
}

func TestBestModel(t *testing.T) {
	if got := best([]string{"tiny", "medium.en", "base.en"}); got != "base.en" {
		t.Fatalf("pt: %s", got)
	}
	if got := best([]string{"mlx-community/whisper-tiny", "mlx-community/whisper-large-v3-turbo", "mlx-community/whisper-small-mlx"}); got != "mlx-community/whisper-large-v3-turbo" {
		t.Fatalf("mlx: %s", got)
	}
	if got := best([]string{"/m/ggml-base.en.bin", "/m/ggml-base.bin"}); got != "/m/ggml-base.bin" {
		t.Fatalf("cpp: %s", got)
	}
}

// A real whisper, when one is installed, hears what say(1) said.
func TestWhisper(t *testing.T) {
	tr := Find()
	if tr == nil || !Available() || testing.Short() {
		t.Skip("no whisper or ffmpeg")
	}
	if _, err := exec.LookPath("say"); err != nil {
		t.Skip("no say")
	}
	dir := t.TempDir()
	aiff := filepath.Join(dir, "s.aiff")
	if out, err := exec.Command("say", "-o", aiff, "The quick brown fox jumps over the lazy dog.").CombinedOutput(); err != nil {
		t.Skipf("say: %v %s", err, out)
	}
	cache := CacheDir
	CacheDir, Enabled = func() string { return dir }, true
	defer func() { CacheDir, Enabled = cache, false }()
	got, err := Transcribe(context.Background(), aiff, "s.aiff", Options{})
	if err != nil || got.Err != nil || !strings.Contains(strings.ToLower(got.Text()), "lazy dog") {
		t.Fatalf("%v %v\n%s", err, got.Err, got.Text())
	}
}

type slowWhisper struct {
	mu        *sync.Mutex
	now, most *int
}

func (slowWhisper) Name() string { return "slow" }
func (w slowWhisper) Transcribe(context.Context, string) ([]Segment, error) {
	w.mu.Lock()
	*w.now++
	*w.most = max(*w.most, *w.now)
	w.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	w.mu.Lock()
	*w.now--
	w.mu.Unlock()
	return []Segment{{Start: 0, End: 1, Text: "hi"}}, nil
}

// One whisper at a time (each loads a model of over a GB): within tego,
// and across tego processes through the lock file.
func TestOneWhisperAtATime(t *testing.T) {
	if !Available() {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	var mu sync.Mutex
	now, most := 0, 0
	useWhisper(t, slowWhisper{&mu, &now, &most})
	var wg sync.WaitGroup
	for i := range 4 {
		f := filepath.Join(dir, fmt.Sprintf("%d.wav", i))
		ffmpeg(t, "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:duration=2", 300+100*i), f)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tr, err := Transcribe(context.Background(), f, "x", Options{}); err != nil || len(tr.Segments) != 1 {
				t.Errorf("%v %+v", err, tr)
			}
		}()
	}
	wg.Wait()
	if most != 1 {
		t.Fatalf("%d whispers at once", most)
	}

	// Another tego holds the lock: this one waits, up to its context.
	other, err := os.OpenFile(filepath.Join(CacheDir(), ".whisper.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := lockWhisper(ctx); err == nil {
		t.Fatal("locked while another process holds it")
	}
	_ = syscall.Flock(int(other.Fd()), syscall.LOCK_UN)
	unlock, err := lockWhisper(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
