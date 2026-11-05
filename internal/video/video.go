// Package video gets a video file to a model. Models that take video
// (Gemini) get the clip itself, re-encoded small, sound included (native.go);
// the rest get frames: images taken at even intervals with their
// timestamps. It needs ffmpeg and ffprobe on PATH.
package video

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akbxr/tego/internal/images"
	"github.com/akbxr/tego/internal/llm"
)

const (
	// MaxFrames bounds frames per call: each is an image in the request.
	MaxFrames = 48
	edge      = 768  // long edge of a frame
	closeEdge = 1024 // ... when only a few are taken (6 or less)
	workers   = 4
)

var exts = map[string]bool{
	"mp4": true, "m4v": true, "mov": true, "qt": true, "webm": true, "mkv": true, "avi": true,
	"mpeg": true, "mpg": true, "3gp": true, "flv": true, "wmv": true, "ogv": true,
}

// HasExt reports whether name ends in a video extension.
func HasExt(name string) bool {
	return exts[strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))]
}

// Sniff reports whether head (the file's first 16 bytes or more) starts
// like a video container: MP4/MOV/3GP, Matroska/WebM, AVI, MPEG, FLV, ASF
// or Ogg. Audio-only files in the same containers are told apart later.
func Sniff(head []byte) bool {
	switch {
	case len(head) >= 12 && string(head[4:8]) == "ftyp":
		brand := string(head[8:12])
		return !strings.HasPrefix(brand, "M4A") && !strings.HasPrefix(brand, "M4B") && !strings.HasPrefix(brand, "heic") && !strings.HasPrefix(brand, "avif") && !strings.HasPrefix(brand, "mif1")
	case bytes.HasPrefix(head, []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return true
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "AVI ":
		return true
	case bytes.HasPrefix(head, []byte{0, 0, 1, 0xba}), bytes.HasPrefix(head, []byte{0, 0, 1, 0xb3}):
		return true
	case bytes.HasPrefix(head, []byte("FLV")):
		return true
	case bytes.HasPrefix(head, []byte{0x30, 0x26, 0xb2, 0x75, 0x8e, 0x66, 0xcf, 0x11}):
		return true
	}
	return false
}

// Options choose the frames: Frames of them (0: by length) spread over
// Start..End seconds (End 0: the end).
type Options struct {
	Start, End float64
	Frames     int
}

// Frame is one image of the clip, At seconds in.
type Frame struct {
	At    float64
	Block llm.Block
}

// Clip is what was read of a video.
type Clip struct {
	Path          string
	Duration      float64 // seconds; 0 when the file doesn't say
	Width, Height int     // as shown (rotation applied)
	FPS           float64
	Audio         bool
	Start, End    float64 // the span the frames cover
	Frames        []Frame
	format, codec string // ffprobe's container and video codec names
	size          int64  // bytes
}

// ErrNoFFmpeg is returned when ffmpeg or ffprobe is not on PATH.
var ErrNoFFmpeg = errors.New("reading video needs ffmpeg and ffprobe on PATH (macOS: brew install ffmpeg; Debian/Ubuntu: apt install ffmpeg)")

// ErrNoVideo is returned for a file with no video stream: audio in a
// container Sniff can't tell from video (WebM, MP4).
var ErrNoVideo = errors.New("no video stream")

// Available reports whether ffmpeg and ffprobe are on PATH.
func Available() bool {
	_, e1 := exec.LookPath("ffmpeg")
	_, e2 := exec.LookPath("ffprobe")
	return e1 == nil && e2 == nil
}

// Read probes the video at path and takes its frames.
func Read(ctx context.Context, path string, o Options) (*Clip, error) {
	if !Available() {
		return nil, ErrNoFFmpeg
	}
	c, err := probe(ctx, path)
	if err != nil {
		return nil, err
	}
	start, end := max(o.Start, 0), o.End
	if c.Duration > 0 {
		if end <= 0 || end > c.Duration {
			end = c.Duration
		}
		if start >= c.Duration {
			return nil, fmt.Errorf("start %.1fs is past the end of the video (%.1fs)", start, c.Duration)
		}
	} else if end <= start {
		end = start // length unknown: one frame at start
	}
	if end < start {
		return nil, fmt.Errorf("end %.1fs is before start %.1fs", end, start)
	}
	c.Start, c.End = start, end
	times := Times(start, end, o.Frames)
	size := edge
	if len(times) <= 6 {
		size = closeEdge
	}
	frames := make([]*Frame, len(times))
	errs := make([]error, len(times))
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for i, t := range times {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			data, err := frameAt(ctx, path, t, size)
			if err != nil {
				errs[i] = err
				return
			}
			blk, _, err := images.FromBytes(data, path)
			if err != nil {
				errs[i] = err
				return
			}
			frames[i] = &Frame{At: t, Block: blk}
		}()
	}
	wg.Wait()
	for _, f := range frames {
		if f != nil {
			c.Frames = append(c.Frames, *f)
		}
	}
	if len(c.Frames) == 0 {
		if err := errors.Join(errs...); err != nil {
			return nil, err
		}
		return nil, errors.New("no frames could be read")
	}
	return c, nil
}

// DefaultFrames is how many frames a span of span seconds gets when the
// caller does not say: one per 2 seconds, 6 to 16 (a 768px frame is about
// 450 tokens).
func DefaultFrames(span float64) int {
	return min(max(int(math.Ceil(span/2)), 6), 16)
}

// Times spreads n frames (0: DefaultFrames) over start..end, each in the
// middle of its share of the span: no black first frame, no seek past the
// end.
func Times(start, end float64, n int) []float64 {
	span := end - start
	if span < 0.05 {
		return []float64{start}
	}
	if n <= 0 {
		n = DefaultFrames(span)
	}
	n = min(n, MaxFrames)
	step := span / float64(n)
	out := make([]float64, n)
	for i := range out {
		out[i] = math.Round((start+step*(float64(i)+0.5))*100) / 100
	}
	return out
}

func probe(ctx context.Context, path string) (*Clip, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("ffprobe: %s", strings.TrimSpace(firstLine(string(ee.Stderr))))
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
			Type         string            `json:"codec_type"`
			Codec        string            `json:"codec_name"`
			Width        int               `json:"width"`
			Height       int               `json:"height"`
			AvgFrameRate string            `json:"avg_frame_rate"`
			Duration     string            `json:"duration"`
			Tags         map[string]string `json:"tags"`
			Disposition  map[string]int    `json:"disposition"`
			SideData     []struct {
				Rotation float64 `json:"rotation"`
			} `json:"side_data_list"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	c := &Clip{Path: path, format: p.Format.Name}
	c.Duration, _ = strconv.ParseFloat(p.Format.Duration, 64)
	c.size, _ = strconv.ParseInt(p.Format.Size, 10, 64)
	video := false
	for _, s := range p.Streams {
		switch s.Type {
		case "audio":
			c.Audio = true
		case "video":
			if video || s.Disposition["attached_pic"] == 1 { // cover art is not video
				continue
			}
			video = true
			c.Width, c.Height = s.Width, s.Height
			c.FPS = rate(s.AvgFrameRate)
			c.codec = s.Codec
			rot, _ := strconv.ParseFloat(s.Tags["rotate"], 64)
			for _, sd := range s.SideData {
				if sd.Rotation != 0 {
					rot = sd.Rotation
				}
			}
			if r := int(math.Abs(rot)) % 180; r == 90 {
				c.Width, c.Height = c.Height, c.Width
			}
			if c.Duration <= 0 {
				c.Duration, _ = strconv.ParseFloat(s.Duration, 64)
			}
		}
	}
	if !video {
		return nil, fmt.Errorf("%s has %w", filepath.Base(path), ErrNoVideo)
	}
	return c, nil
}

func rate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	n, _ := strconv.ParseFloat(num, 64)
	if !ok {
		return n
	}
	d, _ := strconv.ParseFloat(den, 64)
	if d == 0 {
		return 0
	}
	return n / d
}

// frameAt decodes the frame at t seconds as a JPEG whose long edge is at
// most size. Seeking before the input is fast and, since ffmpeg 2.1, exact.
func frameAt(ctx context.Context, path string, t float64, size int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	scale := fmt.Sprintf("scale=w='min(%d,iw)':h='min(%d,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2,format=yuvj420p", size, size)
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-ss", strconv.FormatFloat(t, 'f', 3, 64), "-i", path,
		"-frames:v", "1", "-vf", scale, "-f", "image2pipe", "-c:v", "mjpeg", "-q:v", "4", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := firstLine(stderr.String()); msg != "" {
			return nil, fmt.Errorf("ffmpeg at %s: %s", Stamp(t), msg)
		}
		return nil, fmt.Errorf("ffmpeg at %s: %w", Stamp(t), err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("ffmpeg at %s: no frame", Stamp(t))
	}
	return out, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// Stamp formats seconds as m:ss.s (h:mm:ss.s from an hour).
func Stamp(sec float64) string {
	tenths := int(math.Round(sec * 10))
	h, m, s, d := tenths/36000, tenths/600%60, tenths/10%60, tenths%10
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d.%d", h, m, s, d)
	}
	return fmt.Sprintf("%d:%02d.%d", m, s, d)
}

// Describe says what the frames are, for the text before them: the
// video's length, size and rate, whether it has sound (not included), and
// each frame's timestamp in order.
func (c *Clip) Describe(name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Video %s", name)
	var facts []string
	if c.Duration > 0 {
		facts = append(facts, fmt.Sprintf("%.1f s", c.Duration))
	}
	facts = append(facts, fmt.Sprintf("%dx%d", c.Width, c.Height))
	if c.FPS > 0 {
		facts = append(facts, strconv.FormatFloat(math.Round(c.FPS*100)/100, 'f', -1, 64)+" fps")
	}
	if c.Audio {
		facts = append(facts, "has audio (not included)")
	} else {
		facts = append(facts, "no audio")
	}
	fmt.Fprintf(&b, " (%s): ", strings.Join(facts, ", "))
	stamps := make([]string, len(c.Frames))
	for i, f := range c.Frames {
		stamps[i] = Stamp(f.At)
	}
	if len(c.Frames) == 1 {
		fmt.Fprintf(&b, "1 frame, at %s", stamps[0])
	} else {
		fmt.Fprintf(&b, "%d frames in order, at %s", len(c.Frames), strings.Join(stamps, ", "))
	}
	if c.Duration > 0 && (c.Start > 0 || c.End < c.Duration) {
		fmt.Fprintf(&b, " (from the part %s to %s)", Stamp(c.Start), Stamp(c.End))
	}
	return b.String()
}

// Blocks are the frames' image blocks, in order.
func (c *Clip) Blocks() []llm.Block {
	out := make([]llm.Block, len(c.Frames))
	for i, f := range c.Frames {
		out[i] = f.Block
	}
	return out
}
