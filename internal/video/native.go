package video

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// Models that take video (Gemini) get the clip itself, sound included:
// re-encoded small, since they look at about one frame a second at a low
// resolution anyway.
const (
	// MaxNative bounds the seconds sent as one video (about 100 tokens a
	// second); longer spans go as frames.
	MaxNative  = 20 * 60
	nativeEdge = 640
	nativeFPS  = 5
	// MaxNativeBytes bounds an encoded clip: the request carries it inline.
	MaxNativeBytes = 16 << 20
)

// ErrTooLong and ErrTooBig say a span can't go as one video.
var (
	ErrTooLong = fmt.Errorf("longer than %d minutes", MaxNative/60)
	ErrTooBig  = fmt.Errorf("over %d MB encoded", MaxNativeBytes>>20)
)

// Meta is what a video block holds besides the clip, in its Input: enough
// to describe it and to turn it into frames for a model that can't watch.
type Meta struct {
	Name     string  `json:"name"`
	Duration float64 `json:"duration,omitempty"` // of the whole video
	Start    float64 `json:"start,omitempty"`    // the part encoded, in the whole video
	End      float64 `json:"end,omitempty"`
	Width    int     `json:"width,omitempty"`
	Height   int     `json:"height,omitempty"`
	FPS      float64 `json:"fps,omitempty"`
	Audio    bool    `json:"audio,omitempty"`
}

// Encode probes the video at path and encodes Start..End of it (End 0: the
// end) as a small MP4 with its sound, for a model that takes video. name
// is how the description calls it.
func Encode(ctx context.Context, path, name string, o Options) (*Clip, llm.Block, error) {
	if !Available() {
		return nil, llm.Block{}, ErrNoFFmpeg
	}
	c, err := probe(ctx, path)
	if err != nil {
		return nil, llm.Block{}, err
	}
	start, end := max(o.Start, 0), o.End
	if c.Duration > 0 {
		if end <= 0 || end > c.Duration {
			end = c.Duration
		}
		if start >= c.Duration {
			return nil, llm.Block{}, fmt.Errorf("start %.1fs is past the end of the video (%.1fs)", start, c.Duration)
		}
	}
	if end > 0 && end <= start {
		return nil, llm.Block{}, fmt.Errorf("end %.1fs is not after start %.1fs", end, start)
	}
	if end-start > MaxNative || (c.Duration <= 0 && end <= 0) {
		return nil, llm.Block{}, ErrTooLong
	}
	c.Start, c.End = start, end
	data, err := encode(ctx, path, start, end, c.Audio)
	if err != nil {
		return nil, llm.Block{}, err
	}
	if len(data) > MaxNativeBytes {
		return nil, llm.Block{}, ErrTooBig
	}
	meta, _ := json.Marshal(Meta{Name: name, Duration: c.Duration, Start: start, End: end, Width: c.Width, Height: c.Height, FPS: c.FPS, Audio: c.Audio})
	return c, llm.Block{Type: llm.BlockVideo, MediaType: "video/mp4", Data: base64.StdEncoding.EncodeToString(data), Input: meta}, nil
}

// encode writes start..end of path as H.264 (MPEG-4 Part 2 when ffmpeg has
// no x264) at most 640px and 5 fps, with mono AAC sound.
func encode(ctx context.Context, path string, start, end float64, audio bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tmp, err := os.CreateTemp("", "tego-video-*.mp4")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	scale := fmt.Sprintf("scale=w='min(%d,iw)':h='min(%d,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2,fps='min(%d,source_fps)'", nativeEdge, nativeEdge, nativeFPS)
	args := []string{"-nostdin", "-v", "error", "-y", "-ss", strconv.FormatFloat(start, 'f', 3, 64)}
	if end > 0 {
		args = append(args, "-to", strconv.FormatFloat(end, 'f', 3, 64))
	}
	args = append(args, "-i", path, "-map", "0:v:0", "-vf", scale, "-pix_fmt", "yuv420p")
	if audio {
		args = append(args, "-map", "0:a:0?", "-c:a", "aac", "-ac", "1", "-b:a", "48k")
	}
	tail := []string{"-movflags", "+faststart", "-f", "mp4", tmp.Name()}
	var last error
	for _, codec := range [][]string{{"-c:v", "libx264", "-preset", "veryfast", "-crf", "30"}, {"-c:v", "mpeg4", "-q:v", "8"}} {
		cmd := exec.CommandContext(ctx, "ffmpeg", append(append(append([]string{}, args...), codec...), tail...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("ffmpeg: %w", ctx.Err())
			}
			last = fmt.Errorf("ffmpeg: %s", firstLine(stderr.String()))
			continue
		}
		return os.ReadFile(tmp.Name())
	}
	return nil, last
}

// DescribeNative says what the attached video is: its length, size and
// rate, that its sound is included, and which part it is.
func (c *Clip) DescribeNative(name string) string {
	return Meta{Name: name, Duration: c.Duration, Start: c.Start, End: c.End, Width: c.Width, Height: c.Height, FPS: c.FPS, Audio: c.Audio}.describe()
}

func (m Meta) describe() string {
	var facts []string
	if m.Duration > 0 {
		facts = append(facts, fmt.Sprintf("%.1f s", m.Duration))
	}
	facts = append(facts, fmt.Sprintf("%dx%d", m.Width, m.Height))
	if m.FPS > 0 {
		facts = append(facts, strconv.FormatFloat(math.Round(m.FPS*100)/100, 'f', -1, 64)+" fps")
	}
	if m.Audio {
		facts = append(facts, "sound included")
	} else {
		facts = append(facts, "no sound")
	}
	s := fmt.Sprintf("Video %s (%s): attached as a video", m.Name, strings.Join(facts, ", "))
	if m.Duration > 0 && (m.Start > 0.05 || m.End < m.Duration-0.05) {
		s += fmt.Sprintf(", the part %s to %s; its own timestamps start at 0:00, which is %s in the whole video", Stamp(m.Start), Stamp(m.End), Stamp(m.Start))
	}
	return s
}

// ReadMeta is the Meta of a video block.
func ReadMeta(b llm.Block) Meta {
	var m Meta
	_ = json.Unmarshal(b.Input, &m)
	return m
}

// Seconds is how long the clip in a video block runs.
func Seconds(b llm.Block) float64 {
	m := ReadMeta(b)
	return m.End - m.Start
}

// ToFrames turns a video block into what a model that can't watch gets:
// a line saying what the frames are, then the frames, with timestamps in
// the whole video. The sound is lost.
func ToFrames(ctx context.Context, b llm.Block) ([]llm.Block, error) {
	if b.Type != llm.BlockVideo {
		return nil, errors.New("not a video block")
	}
	data, err := base64.StdEncoding.DecodeString(b.Data)
	if err != nil {
		return nil, fmt.Errorf("video block: %w", err)
	}
	tmp, err := os.CreateTemp("", "tego-video-*.mp4")
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
	m := ReadMeta(b)
	c, err := Read(ctx, tmp.Name(), Options{})
	if err != nil {
		return nil, err
	}
	for i := range c.Frames {
		c.Frames[i].At = math.Round((c.Frames[i].At+m.Start)*100) / 100
	}
	if m.Duration > 0 {
		c.Duration, c.Start, c.End = m.Duration, m.Start, m.End
	} else {
		c.Start, c.End = c.Start+m.Start, c.End+m.Start
	}
	if m.Width > 0 {
		c.Width, c.Height, c.FPS, c.Audio = m.Width, m.Height, m.FPS, m.Audio
	}
	name := m.Name
	if name == "" {
		name = "clip"
	}
	return append([]llm.Block{{Type: llm.BlockText, Text: c.Describe(name)}}, c.Blocks()...), nil
}
