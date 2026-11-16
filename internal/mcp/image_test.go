package mcp

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"testing"

	"github.com/akbxr/tego/internal/images"
)

// onePixel is a 1x1 PNG.
const onePixel = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// A 4K render a server sends is downscaled as a read image is: as sent,
// it failed every request with more than 20 images.
func TestRenderSizesImages(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 2160, 3840)), nil); err != nil {
		t.Fatal(err)
	}
	r := callResult{Content: []content{{Type: "image", MimeType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(buf.Bytes())}}}
	text, imgs := r.render()
	if text != "" || len(imgs) != 1 {
		t.Fatalf("%q %d images", text, len(imgs))
	}
	w, h, err := images.Size(imgs[0])
	if err != nil || w != 882 || h != images.MaxLongEdge {
		t.Fatalf("%dx%d %v", w, h, err)
	}
}
