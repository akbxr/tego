// Package images turns image files, clipboard contents and paths pasted into
// a prompt into llm image blocks sized for the providers' limits.
package images

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"runtime/debug"

	"github.com/akbxr/tego/internal/llm"
)

const (
	// MaxLongEdge is the long edge Anthropic recommends; larger images are
	// downscaled server-side anyway, costing latency and tokens.
	MaxLongEdge = 1568
	softBytes   = 15 << 18   // 3.75 MiB: re-encode above this
	hardBytes   = 5 << 20    // provider limit per image (decoded)
	hardEdge    = 8000       // provider limit per edge
	maxInput    = 64 << 20   // refuse to read larger files
	maxPixels   = 50_000_000 // refuse to decode larger images (~200 MB RGBA)
	jpegQuality = 85
)

// Info describes an image as it will be sent. Bytes is the size of the
// (possibly re-encoded) image data before base64.
type Info struct {
	Source        string
	Width, Height int
	Bytes         int
	MediaType     string
}

// MediaType detects an image type from its leading bytes (at least 12 are
// needed for WebP) and returns "" for anything else.
func MediaType(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(head, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return "image/gif"
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// Load reads an image file and returns a normalized image block.
func Load(path string) (llm.Block, Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return llm.Block{}, Info{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return llm.Block{}, Info{}, err
	}
	if st.Size() > maxInput {
		return llm.Block{}, Info{}, fmt.Errorf("%s: image file too large (%d MB)", path, st.Size()>>20)
	}
	data := make([]byte, st.Size())
	if _, err := io.ReadFull(f, data); err != nil {
		return llm.Block{}, Info{}, err
	}
	return FromBytes(data, path)
}

// FromBytes normalizes encoded image data (PNG, JPEG, GIF or WebP, detected
// by magic bytes). Images within the limits pass through unchanged; larger
// ones are downscaled to MaxLongEdge and re-encoded as PNG, or JPEG when the
// PNG is still too big. source labels the image in Info and errors.
func FromBytes(data []byte, source string) (llm.Block, Info, error) {
	mt := MediaType(data)
	if mt == "" {
		return llm.Block{}, Info{}, fmt.Errorf("%s: not a PNG, JPEG, GIF or WebP image", source)
	}
	w, h, err := dimensions(data, mt)
	if err != nil {
		return llm.Block{}, Info{}, fmt.Errorf("%s: %v", source, err)
	}
	info := Info{Source: source, Width: w, Height: h, Bytes: len(data), MediaType: mt}
	if mt == "image/webp" {
		// The standard library cannot decode WebP; send it as is if allowed.
		if len(data) > hardBytes || w > hardEdge || h > hardEdge {
			return llm.Block{}, Info{}, fmt.Errorf("%s: WebP image (%dx%d, %d KB) exceeds the 5 MB / %d px limit and cannot be resized; convert it to PNG or JPEG",
				source, w, h, len(data)>>10, hardEdge)
		}
		return block(data, mt), info, nil
	}
	if max(w, h) <= MaxLongEdge && len(data) <= softBytes {
		return block(data, mt), info, nil
	}
	if w*h > maxPixels {
		return llm.Block{}, Info{}, fmt.Errorf("%s: image too large to decode (%dx%d)", source, w, h)
	}
	blk, info, err := reencode(data, mt, source, info)
	// Decoding/resizing a large screenshot briefly needs tens of MB; hand it
	// back to the OS now instead of keeping the heap inflated.
	debug.FreeOSMemory()
	return blk, info, err
}

// reencode decodes, downscales to MaxLongEdge if needed and re-encodes.
func reencode(data []byte, mt, source string, info Info) (llm.Block, Info, error) {
	w, h := info.Width, info.Height
	img, err := decode(data, mt)
	if err != nil {
		return llm.Block{}, Info{}, fmt.Errorf("%s: %v", source, err)
	}
	if max(w, h) > MaxLongEdge {
		img = resize(img, MaxLongEdge)
	}
	b := img.Bounds()
	info.Width, info.Height = b.Dx(), b.Dy()
	var buf bytes.Buffer
	if mt != "image/jpeg" { // a photo re-encoded as PNG only grows
		if err := png.Encode(&buf, img); err != nil {
			return llm.Block{}, Info{}, fmt.Errorf("%s: encode png: %v", source, err)
		}
		if buf.Len() <= softBytes {
			return finish(buf.Bytes(), "image/png", info)
		}
		buf.Reset()
	}
	if err := jpeg.Encode(&buf, flatten(img), &jpeg.Options{Quality: jpegQuality}); err != nil {
		return llm.Block{}, Info{}, fmt.Errorf("%s: encode jpeg: %v", source, err)
	}
	if buf.Len() > hardBytes {
		return llm.Block{}, Info{}, fmt.Errorf("%s: image still %d KB after re-encoding", source, buf.Len()>>10)
	}
	return finish(buf.Bytes(), "image/jpeg", info)
}

func finish(data []byte, mt string, info Info) (llm.Block, Info, error) {
	info.Bytes, info.MediaType = len(data), mt
	return block(data, mt), info, nil
}

func block(data []byte, mt string) llm.Block {
	return llm.Block{Type: llm.BlockImage, MediaType: mt, Data: base64.StdEncoding.EncodeToString(data)}
}

func dimensions(data []byte, mt string) (int, int, error) {
	var cfg image.Config
	var err error
	switch mt {
	case "image/png":
		cfg, err = png.DecodeConfig(bytes.NewReader(data))
	case "image/jpeg":
		cfg, err = jpeg.DecodeConfig(bytes.NewReader(data))
	case "image/gif":
		cfg, err = gif.DecodeConfig(bytes.NewReader(data))
	case "image/webp":
		return webpSize(data)
	}
	return cfg.Width, cfg.Height, err
}

// decode returns the image (the first frame of an animated GIF).
func decode(data []byte, mt string) (image.Image, error) {
	r := bytes.NewReader(data)
	switch mt {
	case "image/png":
		return png.Decode(r)
	case "image/jpeg":
		return jpeg.Decode(r)
	case "image/gif":
		return gif.Decode(r)
	}
	return nil, fmt.Errorf("cannot decode %s", mt)
}

var errWebP = errors.New("malformed WebP header")

// webpSize reads the canvas size from a VP8, VP8L or VP8X WebP header.
func webpSize(d []byte) (int, int, error) {
	if len(d) < 30 {
		return 0, 0, errWebP
	}
	switch string(d[12:16]) {
	case "VP8X":
		w := int(d[24]) | int(d[25])<<8 | int(d[26])<<16
		h := int(d[27]) | int(d[28])<<8 | int(d[29])<<16
		return w + 1, h + 1, nil
	case "VP8 ":
		if d[23] != 0x9d || d[24] != 0x01 || d[25] != 0x2a {
			return 0, 0, errWebP
		}
		w := binary.LittleEndian.Uint16(d[26:]) & 0x3fff
		h := binary.LittleEndian.Uint16(d[28:]) & 0x3fff
		return int(w), int(h), nil
	case "VP8L":
		if d[20] != 0x2f {
			return 0, 0, errWebP
		}
		bits := binary.LittleEndian.Uint32(d[21:])
		return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, nil
	}
	return 0, 0, errWebP
}
