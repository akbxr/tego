// Package audio gets an audio file to a model. Models that take audio
// (Gemini) get the sound itself: the file as is when it is small and in a
// format Gemini reads, else re-encoded small. The rest get a transcript
// with timestamps from a local whisper (whisper.go), or a note saying why
// there is none. It needs ffmpeg and ffprobe on PATH.
package audio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/video"
)

// Gemini turns audio into 32 tokens a second and downsamples it to 16 kbps
// mono itself, so 24 kbps mono MP3 at 16 kHz loses nothing it would keep:
// 75 s of speech came to 228 KB (32 kbps: 304 KB; Opus 24k: 220 KB; FLAC:
// 1.4 MB). MP3 is on Gemini's list of formats; Opus in Ogg is not sure to be.
const (
	// MaxNative bounds the seconds sent as one clip (about 115k tokens);
	// longer spans get a transcript.
	MaxNative = 60 * 60
	// MaxOriginalBytes: a whole file up to this size, in a format Gemini
	// reads, goes as is. Every later request carries it again, and audio
	// shrinks a lot when encoded, so this is lower than video's.
	MaxOriginalBytes = 4 << 20
	// MaxNativeBytes bounds an encoded clip (an hour is about 11 MB).
	MaxNativeBytes = 16 << 20
	sampleRate     = 16000
)

// ErrTooLong and ErrTooBig say a span can't go as one clip.
var (
	ErrTooLong = fmt.Errorf("longer than %d minutes", MaxNative/60)
	ErrTooBig  = fmt.Errorf("over %d MB encoded", MaxNativeBytes>>20)
)

// ErrNoFFmpeg is returned when ffmpeg or ffprobe is not on PATH.
var ErrNoFFmpeg = errors.New("reading audio needs ffmpeg and ffprobe on PATH (macOS: brew install ffmpeg; Debian/Ubuntu: apt install ffmpeg)")

var exts = map[string]bool{
	"mp3": true, "mp2": true, "wav": true, "wave": true, "m4a": true, "m4b": true, "aac": true,
	"ogg": true, "oga": true, "opus": true, "flac": true, "aif": true, "aiff": true, "aifc": true,
	"caf": true, "wma": true, "amr": true, "weba": true, "mka": true,
}

// HasExt reports whether name ends in an audio extension.
func HasExt(name string) bool {
	return exts[strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))]
}

// HeadLen is how much of a file Sniff wants: Ogg needs its first packet to
// tell Theora video from sound.
const HeadLen = 64

// Sniff reports whether head (the file's first HeadLen bytes) starts like
// an audio file: MP3 (ID3 or a frame header), ADTS AAC, WAV, AIFF, FLAC,
// Ogg (not Theora), M4A, CAF or AMR. Audio in containers that also hold
// video (WebM, MP4, ASF) sniffs as video, and is found to be sound only
// when probed (video.ErrNoVideo).
func Sniff(head []byte) bool {
	switch {
	case bytes.HasPrefix(head, []byte("ID3")):
		return true
	case len(head) >= 3 && head[0] == 0xff && head[1]&0xf6 == 0xf0: // ADTS
		return true
	case len(head) >= 3 && head[0] == 0xff && head[1]&0xe0 == 0xe0:
		// MPEG layer II or III with a valid rate; layer I is left out, as
		// it would take a UTF-16 byte order mark (ff fe).
		layer, version, rate, freq := head[1]>>1&3, head[1]>>3&3, head[2]>>4, head[2]>>2&3
		return (layer == 1 || layer == 2) && version != 1 && rate != 15 && freq != 3
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		return true
	case len(head) >= 12 && string(head[:4]) == "FORM" && (string(head[8:12]) == "AIFF" || string(head[8:12]) == "AIFC"):
		return true
	case bytes.HasPrefix(head, []byte("fLaC")), bytes.HasPrefix(head, []byte("caff")), bytes.HasPrefix(head, []byte("#!AMR")):
		return true
	case bytes.HasPrefix(head, []byte("OggS")):
		return !bytes.Contains(head, []byte("\x80theora"))
	case len(head) >= 12 && string(head[4:8]) == "ftyp":
		brand := string(head[8:12])
		return strings.HasPrefix(brand, "M4A") || strings.HasPrefix(brand, "M4B") || strings.HasPrefix(brand, "M4P")
	}
	return false
}

// Available reports whether ffmpeg and ffprobe are on PATH.
func Available() bool { return video.Available() }

// Options choose the span: Start..End seconds (End 0: the end).
type Options struct {
	Start, End float64
	Encode     bool // never send the file as is (a video's sound)
}

// Clip is what was read of an audio file.
type Clip struct {
	Path          string
	Duration      float64 // seconds; 0 when the file doesn't say
	Start, End    float64 // the span sent
	format, codec string  // ffprobe's container and audio codec names
	size          int64
}

func probe(ctx context.Context, path string) (*Clip, error) {
	if !Available() {
		return nil, ErrNoFFmpeg
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("ffprobe: %s", firstLine(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	var p struct {
		Format struct {
			Duration string `json:"duration"`
			Name     string `json:"format_name"`
			Size     string `json:"size"`
		} `json:"format"`
		Streams []struct {
			Type     string `json:"codec_type"`
			Codec    string `json:"codec_name"`
			Duration string `json:"duration"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	c := &Clip{Path: path, format: p.Format.Name}
	c.Duration, _ = strconv.ParseFloat(p.Format.Duration, 64)
	c.size, _ = strconv.ParseInt(p.Format.Size, 10, 64)
	for _, s := range p.Streams {
		if s.Type == "audio" {
			c.codec = s.Codec
			if c.Duration <= 0 {
				c.Duration, _ = strconv.ParseFloat(s.Duration, 64)
			}
			return c, nil
		}
	}
	return nil, fmt.Errorf("%s has no audio stream", filepath.Base(path))
}

// span clamps o to the clip; end stays 0 when the length is unknown.
func (c *Clip) span(o Options) (start, end float64, err error) {
	start, end = max(o.Start, 0), o.End
	if c.Duration > 0 {
		if end <= 0 || end > c.Duration {
			end = c.Duration
		}
		if start >= c.Duration {
			return 0, 0, fmt.Errorf("start %.1fs is past the end of the audio (%.1fs)", start, c.Duration)
		}
	}
	if end > 0 && end <= start {
		return 0, 0, fmt.Errorf("end %.1fs is not after start %.1fs", end, start)
	}
	return start, end, nil
}

func (c *Clip) whole() bool {
	return c.Start == 0 && (c.Duration <= 0 || c.End >= c.Duration)
}

// Meta is what an audio block holds besides the sound, in its Input:
// enough to describe it and to transcribe it for a model that can't hear.
type Meta struct {
	Name     string  `json:"name"`
	Duration float64 `json:"duration,omitempty"` // of the whole file
	Start    float64 `json:"start,omitempty"`    // the part sent, in the whole file
	End      float64 `json:"end,omitempty"`
}

// ReadMeta is the Meta of an audio block.
func ReadMeta(b llm.Block) Meta {
	var m Meta
	_ = json.Unmarshal(b.Input, &m)
	return m
}

// Seconds is how long the clip in an audio block runs.
func Seconds(b llm.Block) float64 {
	m := ReadMeta(b)
	return m.End - m.Start
}

// Encode probes the audio at path and returns Start..End of it (End 0: the
// end) as a block for a model that takes audio. name is how the
// description calls it.
func Encode(ctx context.Context, path, name string, o Options) (*Clip, llm.Block, error) {
	c, err := probe(ctx, path)
	if err != nil {
		return nil, llm.Block{}, err
	}
	c.Start, c.End, err = c.span(o)
	if err != nil {
		return nil, llm.Block{}, err
	}
	if c.End-c.Start > MaxNative || c.End <= 0 {
		return nil, llm.Block{}, ErrTooLong
	}
	meta, _ := json.Marshal(Meta{Name: name, Duration: c.Duration, Start: c.Start, End: c.End})
	if mt := c.originalType(); mt != "" && c.whole() && !o.Encode {
		if data, err := os.ReadFile(path); err == nil && len(data) <= MaxOriginalBytes {
			return c, llm.Block{Type: llm.BlockAudio, MediaType: mt, Data: base64.StdEncoding.EncodeToString(data), Input: meta}, nil
		}
	}
	data, mt, err := encode(ctx, path, c.Start, c.End)
	if err != nil {
		return nil, llm.Block{}, err
	}
	if len(data) > MaxNativeBytes {
		return nil, llm.Block{}, ErrTooBig
	}
	return c, llm.Block{Type: llm.BlockAudio, MediaType: mt, Data: base64.StdEncoding.EncodeToString(data), Input: meta}, nil
}

// originalType is the MIME type to send the file as is with, or "" when it
// needs encoding: too big, or not one of Gemini's formats (WAV, MP3, AIFF,
// AAC, Ogg, FLAC).
func (c *Clip) originalType() string {
	if c.size <= 0 || c.size > MaxOriginalBytes {
		return ""
	}
	pcm := strings.HasPrefix(c.codec, "pcm_")
	switch {
	case c.format == "mp3" && c.codec == "mp3":
		return "audio/mp3"
	case c.format == "wav" && pcm:
		return "audio/wav"
	case c.format == "aiff" && pcm:
		return "audio/aiff"
	case c.format == "aac" && c.codec == "aac": // ADTS
		return "audio/aac"
	case c.format == "ogg" && (c.codec == "vorbis" || c.codec == "opus" || c.codec == "flac"):
		return "audio/ogg"
	case c.format == "flac" && c.codec == "flac":
		return "audio/flac"
	}
	return ""
}

// encode writes start..end of path as mono 16 kHz MP3 at 24 kbps, or ADTS
// AAC when ffmpeg has no LAME.
func encode(ctx context.Context, path string, start, end float64) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	args := append(cutArgs(path, start, end), "-ac", "1", "-ar", strconv.Itoa(sampleRate))
	var last error
	for _, f := range []struct {
		mt   string
		args []string
	}{
		{"audio/mp3", []string{"-c:a", "libmp3lame", "-b:a", "24k", "-f", "mp3", "-"}},
		{"audio/aac", []string{"-c:a", "aac", "-b:a", "32k", "-f", "adts", "-"}},
	} {
		cmd := exec.CommandContext(ctx, "ffmpeg", append(append([]string{}, args...), f.args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err == nil && len(out) > 0 {
			return out, f.mt, nil
		}
		if ctx.Err() != nil {
			return nil, "", fmt.Errorf("ffmpeg: %w", ctx.Err())
		}
		last = fmt.Errorf("ffmpeg: %s", firstLine(stderr.String()))
	}
	return nil, "", last
}

// cutArgs are ffmpeg's arguments to read the first audio stream of
// start..end (end 0: the end) of path, without any video.
func cutArgs(path string, start, end float64) []string {
	args := []string{"-nostdin", "-v", "error", "-y"}
	if start > 0 {
		args = append(args, "-ss", strconv.FormatFloat(start, 'f', 3, 64))
	}
	if end > 0 {
		args = append(args, "-to", strconv.FormatFloat(end, 'f', 3, 64))
	}
	return append(args, "-i", path, "-vn", "-map", "0:a:0")
}

// DescribeNative says what the attached clip is: its length and which part
// it is.
func (c *Clip) DescribeNative(name string) string {
	return Meta{Name: name, Duration: c.Duration, Start: c.Start, End: c.End}.describe()
}

func (m Meta) describe() string {
	s := fmt.Sprintf("Audio %s", m.Name)
	if m.Duration > 0 {
		s += fmt.Sprintf(" (%.1f s)", m.Duration)
	}
	s += ": attached as audio"
	if m.partial() {
		s += fmt.Sprintf(", the part %s to %s; its own timestamps start at 0:00, which is %s in the whole file", Stamp(m.Start), Stamp(m.End), Stamp(m.Start))
	}
	return s
}

func (m Meta) partial() bool {
	return m.Duration > 0 && (m.Start > 0.05 || m.End < m.Duration-0.05)
}

// Stamp formats seconds as m:ss.s (h:mm:ss.s from an hour).
func Stamp(sec float64) string { return video.Stamp(sec) }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
