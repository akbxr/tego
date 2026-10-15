package images

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindPaths(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, f := range []string{"a.png", "Screenshot 2026-10-12 at 07.52.02.png", "sub/B.JPG", "it's.gif", "c.webp"} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "h.jpeg"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	shot := filepath.Join(dir, "Screenshot 2026-10-12 at 07.52.02.png")
	escaped := strings.ReplaceAll(shot, " ", `\ `)

	tests := []struct {
		name, text string
		want       []string // matched substrings
		paths      []string // resolved paths
	}{
		{"bare", "look at " + dir + "/a.png please", []string{dir + "/a.png"}, []string{dir + "/a.png"}},
		{"relative", "see sub/B.JPG", []string{"sub/B.JPG"}, []string{dir + "/sub/B.JPG"}},
		{"home", "~/h.jpeg", []string{"~/h.jpeg"}, []string{home + "/h.jpeg"}},
		{"escaped", "what is " + escaped + "?", []string{escaped}, []string{shot}},
		{"single", "'" + shot + "' ok", []string{"'" + shot + "'"}, []string{shot}},
		{"double", `x "` + shot + `"`, []string{`"` + shot + `"`}, []string{shot}},
		{"double apostrophe", `"it's.gif"`, []string{`"it's.gif"`}, []string{dir + "/it's.gif"}},
		{"file url", "file://" + strings.ReplaceAll(shot, " ", "%20"), []string{"file://" + strings.ReplaceAll(shot, " ", "%20")}, []string{shot}},
		{"plain spaces", "is " + shot + " ok", []string{shot}, []string{shot}},
		{"punctuation", "(a.png), and c.webp.", []string{"a.png", "c.webp"}, []string{dir + "/a.png", dir + "/c.webp"}},
		{"adjacent", "a.png c.webp", []string{"a.png", "c.webp"}, []string{dir + "/a.png", dir + "/c.webp"}},
		{"missing", "nope.png " + dir + "/missing.png", nil, nil},
		{"not image", "notes.txt " + dir + "/notes.txt", nil, nil},
		{"unbalanced quote", `he said "hi a.png`, []string{"a.png"}, []string{dir + "/a.png"}},
	}
	for _, tt := range tests {
		ms := FindPaths(tt.text, dir)
		var got, paths []string
		last := 0
		for _, m := range ms {
			if m.Start < last {
				t.Errorf("%s: overlapping matches %+v", tt.name, ms)
			}
			last = m.End
			got = append(got, tt.text[m.Start:m.End])
			paths = append(paths, m.Path)
		}
		if strings.Join(got, "|") != strings.Join(tt.want, "|") || strings.Join(paths, "|") != strings.Join(tt.paths, "|") {
			t.Errorf("%s: got %q -> %q; want %q -> %q", tt.name, got, paths, tt.want, tt.paths)
		}
	}
}

func TestResize(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 3000, 2000))
	for y := range 2000 {
		for x := range 3000 {
			src.SetNRGBA(x, y, color.NRGBA{uint8(x), uint8(y), uint8(x ^ y), 255})
		}
	}
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	blk, info, err := FromBytes(buf.Bytes(), "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if info.Width != 1568 || info.Height != 1045 || info.Bytes > softBytes {
		t.Fatalf("info %+v", info)
	}
	data, err := base64.StdEncoding.DecodeString(blk.Data)
	if err != nil || len(data) != info.Bytes || MediaType(data) != info.MediaType {
		t.Fatalf("block data: %v, %d bytes, %s", err, len(data), info.MediaType)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 1568 || b.Dy() != 1045 {
		t.Fatalf("decoded %v", b)
	}

	// Area averaging: a uniform image stays uniform, alpha included.
	flat := image.NewNRGBA(image.Rect(0, 0, 1700, 300))
	for i := 0; i < len(flat.Pix); i += 4 {
		copy(flat.Pix[i:], []byte{200, 100, 50, 128})
	}
	out := resize(flat, MaxLongEdge)
	if b := out.Bounds(); b.Dx() != 1568 || b.Dy() != 277 {
		t.Fatalf("flat bounds %v", b)
	}
	for i := 0; i < len(out.Pix); i += 4 {
		if p := out.Pix[i : i+4]; p[0] != 200 || p[1] != 100 || p[2] != 50 || p[3] != 128 {
			t.Fatalf("pixel %d = %v", i/4, p)
		}
	}
}

func TestFromBytesPassThrough(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 40, 30))); err != nil {
		t.Fatal(err)
	}
	blk, info, err := FromBytes(buf.Bytes(), "small")
	if err != nil {
		t.Fatal(err)
	}
	if info.Width != 40 || info.Height != 30 || info.MediaType != "image/png" || blk.Data != base64.StdEncoding.EncodeToString(buf.Bytes()) {
		t.Fatalf("pass-through changed image: %+v", info)
	}
	if _, _, err := FromBytes([]byte("hello"), "text"); err == nil {
		t.Fatal("non-image accepted")
	}
}

func TestParseAppleScriptData(t *testing.T) {
	got, err := parseAppleScriptData([]byte("«data PNGf89504E470D0A1A0A0001»\n"))
	if err != nil || string(got) != "\x89PNG\r\n\x1a\n\x00\x01" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := parseAppleScriptData([]byte("hello")); err != ErrNoClipboardImage {
		t.Fatalf("text clipboard: %v", err)
	}
}
