package images

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// ErrNoClipboardImage is returned by FromClipboard when the clipboard holds
// no image.
var ErrNoClipboardImage = errors.New("clipboard does not contain an image")

const clipboardTimeout = 10 * time.Second

// FromClipboard reads a PNG image from the system clipboard: osascript (or
// pngpaste) on macOS, wl-paste or xclip on Linux.
func FromClipboard() (llm.Block, Info, error) {
	data, err := clipboardPNG()
	if err != nil {
		return llm.Block{}, Info{}, err
	}
	return FromBytes(data, "clipboard")
}

func clipboardPNG() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), clipboardTimeout)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		out, err := run(ctx, "osascript", "-e", "the clipboard as «class PNGf»")
		if err == nil {
			if data, err := parseAppleScriptData(out); err == nil {
				return data, nil
			}
		}
		if _, lerr := exec.LookPath("pngpaste"); lerr == nil {
			if out, err := run(ctx, "pngpaste", "-"); err == nil && MediaType(out) != "" {
				return out, nil
			}
		}
		return nil, ErrNoClipboardImage
	case "linux", "freebsd", "openbsd", "netbsd":
		tools := [][]string{
			{"wl-paste", "--no-newline", "--type", "image/png"},
			{"xclip", "-selection", "clipboard", "-t", "image/png", "-o"},
		}
		found := false
		for _, t := range tools {
			if _, err := exec.LookPath(t[0]); err != nil {
				continue
			}
			found = true
			if out, err := run(ctx, t[0], t[1:]...); err == nil && MediaType(out) != "" {
				return out, nil
			}
		}
		if !found {
			return nil, errors.New("reading clipboard images needs wl-paste (wl-clipboard) or xclip")
		}
		return nil, ErrNoClipboardImage
	}
	return nil, fmt.Errorf("clipboard images are not supported on %s", runtime.GOOS)
}

func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// parseAppleScriptData decodes osascript's «data PNGf89504E47…» rendering
// of clipboard data. The hex is decoded in place.
func parseAppleScriptData(out []byte) ([]byte, error) {
	i := bytes.Index(out, []byte("«data PNGf"))
	if i < 0 {
		return nil, ErrNoClipboardImage
	}
	h := out[i+len("«data PNGf"):]
	n := 0
	for n < len(h) && isHex(h[n]) {
		n++
	}
	h = h[:n&^1]
	// hex.Decode reads src[2i:2i+2] before writing dst[i], so decoding into
	// the front of the same buffer is safe.
	m, err := hex.Decode(h, h)
	if err != nil {
		return nil, err
	}
	if MediaType(h[:m]) == "" {
		return nil, ErrNoClipboardImage
	}
	return h[:m], nil
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}
