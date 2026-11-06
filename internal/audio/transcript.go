package audio

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// Transcript is what a model that can't hear gets for a span of sound.
type Transcript struct {
	Name       string
	Duration   float64 // of the whole file; 0 when unknown
	Start, End float64 // the span transcribed, in the whole file
	SoundTrack bool    // a video's sound
	Engine     string  // the whisper used; "" when none was found
	Segments   []Segment
	Err        error // why there are no segments: no whisper, or it failed
}

// ErrNoWhisper says no local whisper was found.
var ErrNoWhisper = errors.New("no local whisper installed")

// ErrOff says transcription is turned off in the config.
var ErrOff = errors.New("transcription is off")

// maxTranscript bounds the text a model gets (an hour of talk is ~60 KB).
const maxTranscript = 200 << 10

// CacheDir is where transcripts are kept, by content hash, so a file is
// transcribed once: $TEGO_HOME/cache/transcripts (~/.tego/...).
var CacheDir = func() string {
	if d := os.Getenv("TEGO_HOME"); d != "" {
		return filepath.Join(d, "cache", "transcripts")
	}
	return filepath.Join(home(), ".tego", "cache", "transcripts")
}

// Transcribe transcribes Start..End (End 0: the end) of the sound in the
// file at path (audio, or a video's sound track). Not having a whisper,
// or its failing, is in the Transcript, not an error: the model is told.
func Transcribe(ctx context.Context, path, name string, o Options) (*Transcript, error) {
	c, err := probe(ctx, path)
	if err != nil {
		return nil, err
	}
	start, end, err := c.span(o)
	if err != nil {
		return nil, err
	}
	t := &Transcript{Name: name, Duration: c.Duration, Start: start, End: end}
	if c.Duration > 0 && c.Duration <= wholeMax {
		// The whole file once, then any part from it: a model reading a
		// video window by window would otherwise start whisper (and load
		// its model, over a GB) for every window.
		w := &Transcript{Start: 0, End: c.Duration}
		key, err := fileKey(path, 0, c.Duration)
		if err != nil {
			return nil, err
		}
		w.run(ctx, path, key, 0, 0)
		t.Engine, t.Err = w.Engine, w.Err
		for _, sg := range w.Segments {
			if sg.End > start && sg.Start < end {
				t.Segments = append(t.Segments, Segment{Start: sg.Start - start, End: sg.End - start, Text: sg.Text})
			}
		}
		return t, nil
	}
	key, err := fileKey(path, start, end)
	if err != nil {
		return nil, err
	}
	t.run(ctx, path, key, start, end)
	return t, nil
}

// wholeMax is the longest file transcribed whole for a part of it.
const wholeMax = 30 * 60

// SoundTrack is a transcript of start..end of a video's sound, for a
// model that gets its frames, after a newline; "" without a local whisper
// (the frames' line already says the sound is not included).
func SoundTrack(ctx context.Context, path, name string, start, end float64) string {
	if !CanTranscribe() {
		return ""
	}
	tr, err := Transcribe(ctx, path, name, Options{Start: start, End: end})
	if err != nil {
		return ""
	}
	tr.SoundTrack = true
	return "\n" + tr.Text()
}

// TranscribeBlock transcribes the sound of an audio or video block, for a
// model that can't take it (after a model switch).
func TranscribeBlock(ctx context.Context, b llm.Block, m Meta, soundTrack bool) (*Transcript, error) {
	data, err := base64.StdEncoding.DecodeString(b.Data)
	if err != nil {
		return nil, fmt.Errorf("%s block: %w", b.Type, err)
	}
	t := &Transcript{Name: m.Name, Duration: m.Duration, Start: m.Start, End: m.End, SoundTrack: soundTrack}
	if t.Name == "" {
		t.Name = "clip"
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])
	if t.segmentsCached(key) {
		return t, nil
	}
	tmp, err := os.CreateTemp("", "tego-audio-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if t.End <= t.Start { // older or odd meta: time it from the clip
		if c, err := probe(ctx, tmp.Name()); err == nil {
			t.End = t.Start + c.Duration
		}
	}
	t.run(ctx, tmp.Name(), key, 0, 0)
	return t, nil
}

// fileKey is the cache key of start..end of the file at path: its
// content hash and the span.
func fileKey(path string, start, end float64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x-%.3f-%.3f", h.Sum(nil), start, end), nil
}

func (t *Transcript) segmentsCached(key string) bool {
	data, err := os.ReadFile(filepath.Join(CacheDir(), key+".json"))
	if err != nil {
		return false
	}
	var c struct {
		Engine   string    `json:"engine"`
		Segments []Segment `json:"segments"`
	}
	if json.Unmarshal(data, &c) != nil || c.Engine == "" {
		return false
	}
	t.Engine, t.Segments = c.Engine, c.Segments
	return true
}

func saveSegments(key, engine string, segs []Segment) {
	dir := CacheDir()
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	data, _ := json.Marshal(map[string]any{"engine": engine, "segments": segs})
	tmp := filepath.Join(dir, key+".json.tmp")
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, filepath.Join(dir, key+".json"))
	}
}

// run fills in the segments of start..end of src (relative to start),
// from the cache or a whisper. Failures are not cached: they may pass.
func (t *Transcript) run(ctx context.Context, src, key string, start, end float64) {
	if t.segmentsCached(key) {
		return
	}
	tr, err := whisper()
	if err != nil {
		t.Err = err
		return
	}
	t.Engine = tr.Name()
	unlock, err := lockWhisper(ctx)
	if err != nil {
		t.Err = err
		return
	}
	defer unlock()
	if t.segmentsCached(key) { // done by whoever held the lock
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout(t.End-t.Start))
	defer cancel()
	dir, err := os.MkdirTemp("", "tego-audio-*")
	if err != nil {
		t.Err = err
		return
	}
	defer os.RemoveAll(dir)
	wav := filepath.Join(dir, "audio.wav")
	args := append(cutArgs(src, start, end), "-ac", "1", "-ar", strconv.Itoa(sampleRate), "-c:a", "pcm_s16le", wav)
	if out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		t.Err = fmt.Errorf("ffmpeg: %s", firstLine(string(out)))
		if ctx.Err() != nil {
			t.Err = timeoutErr(ctx)
		}
		return
	}
	segs, err := tr.Transcribe(ctx, wav)
	if err != nil {
		if ctx.Err() != nil {
			err = timeoutErr(ctx)
		}
		t.Err = err
		return
	}
	t.Segments = segs
	saveSegments(key, t.Engine, segs)
}

// timeout bounds a transcription of span seconds: whisper on a CPU can
// take half the length of the audio.
func timeout(span float64) time.Duration {
	return min(max(time.Duration(span*float64(time.Second))/2, 2*time.Minute), 15*time.Minute)
}

func timeoutErr(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("timed out")
	}
	return ctx.Err()
}

// Text is what the model gets: what the sound is, then the transcript
// with timestamps in the whole file, or why there is none.
func (t *Transcript) Text() string {
	var b strings.Builder
	what := "Audio " + t.Name
	if t.SoundTrack {
		what = "The sound track of " + t.Name
	}
	b.WriteString(what)
	if t.Duration > 0 {
		fmt.Fprintf(&b, " (%.1f s)", t.Duration)
	}
	part := ""
	if m := (Meta{Duration: t.Duration, Start: t.Start, End: t.End}); m.partial() {
		part = fmt.Sprintf(" of the part %s to %s", Stamp(t.Start), Stamp(t.End))
	}
	switch {
	case errors.Is(t.Err, ErrOff):
		b.WriteString(": you can't hear it, and transcription is off, so what it says is unknown. Tell the user: a Gemini model hears audio itself, " +
			"and \"transcribe\": true in ~/.tego/config.json lets tego transcribe it with a local whisper.")
		return b.String()
	case errors.Is(t.Err, ErrNoWhisper):
		fmt.Fprintf(&b, ": you can't hear it, and no local whisper is installed to transcribe it, so what it says is unknown. "+
			"Tell the user that installing one gives you a transcript: %s. A Gemini model hears audio itself.", InstallHint)
		return b.String()
	case t.Err != nil:
		fmt.Fprintf(&b, ": you can't hear it, and transcribing it%s with %s failed: %v.", part, t.Engine, t.Err)
		return b.String()
	}
	fmt.Fprintf(&b, ": transcript%s by local whisper, %s, timed in the whole file. You get the words only, not the sound (tone, music, noises).", part, t.Engine)
	n := 0
	for _, s := range t.Segments {
		text := strings.TrimSpace(s.Text)
		if text == "" {
			continue
		}
		if b.Len() > maxTranscript {
			b.WriteString("\n[transcript truncated; read a later part with start/end]")
			break
		}
		fmt.Fprintf(&b, "\n[%s-%s] %s", Stamp(t.Start+s.Start), Stamp(t.Start+s.End), text)
		n++
	}
	if n == 0 {
		b.WriteString("\n(no speech recognised)")
	}
	return b.String()
}

// Summary is a short line for the user: what the model got.
func (t *Transcript) Summary() string {
	d := ""
	if t.Duration > 0 {
		d = Stamp(t.Duration) + " · "
	}
	switch {
	case errors.Is(t.Err, ErrOff):
		return d + "transcription off: the model can't hear it"
	case errors.Is(t.Err, ErrNoWhisper):
		return d + "no whisper installed: the model can't hear it"
	case t.Err != nil:
		return d + "transcript failed: " + t.Err.Error()
	}
	return fmt.Sprintf("%stranscript by %s, %d segments", d, t.Engine, len(t.Segments))
}
