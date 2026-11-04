package agent

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"testing"

	"github.com/akbxr/tego/internal/llm"
)

// An image counts by its pixels (a video frame is far less than a
// full-size screenshot); one whose size can't be read counts as full size.
func TestImageCharsFollowSize(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 768, 432)), nil); err != nil {
		t.Fatal(err)
	}
	frame := llm.Block{Type: llm.BlockImage, MediaType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(buf.Bytes())}
	if got := imageChars(frame) / 4; got != 768*432/750+1 {
		t.Fatalf("frame: %d tokens", got)
	}
	if got := imageChars(llm.Block{Type: llm.BlockImage, Data: "not base64!"}); got != imageTokenChars {
		t.Fatalf("unreadable: %d", got)
	}
	msgs := []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, Images: []llm.Block{frame, frame}}}}}
	if got := estimateTokens(msgs); got > 1000 || got < 800 {
		t.Fatalf("two frames estimated at %d tokens", got)
	}
}
