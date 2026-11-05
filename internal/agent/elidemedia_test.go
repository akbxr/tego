package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

func fakeData(tag string) string { return tag + strings.Repeat("Q", 3000) }

func img(tag string) llm.Block {
	return llm.Block{Type: llm.BlockImage, MediaType: "image/png", Data: fakeData(tag)}
}

// A prompt as userBlocks builds it: the text, @mentions, then attachments
// in token order (a video after its "[Video #n]" line).
func TestMediaSourcesNameTheFiles(t *testing.T) {
	blocks := []llm.Block{
		{Type: llm.BlockText, Text: "compare [Image #4: /tmp/c.png], [Image #1: /tmp/a.png], [Image #3] and [Video #2: /tmp/v.mp4] with @docs/x.png @clip.mov"},
		{Type: llm.BlockText, Text: `<file path="docs/x.png" type="image"></file>`},
		img("x"),
		{Type: llm.BlockText, Text: `<file path="clip.mov" type="video">Video clip.mov (3.0 s, 640x360, no audio): 2 frames in order, at 0:00, 0:02. Read it with start/end/frames for a closer look.</file>`},
		img("m1"), img("m2"),
		img("a"),
		{Type: llm.BlockText, Text: "[Video #2] Video /tmp/v.mp4 (4.0 s, 640x360, no audio): 3 frames in order, at 0:00, 0:01, 0:02."},
		img("v1"), img("v2"), img("v3"),
		img("clipboard"),
		img("c"),
	}
	want := []string{"", "", "docs/x.png", "", "clip.mov", "clip.mov", "/tmp/a.png", "", "/tmp/v.mp4", "/tmp/v.mp4", "/tmp/v.mp4", "", "/tmp/c.png"}
	if got := mediaSources(blocks); !reflect.DeepEqual(got, want) {
		t.Fatalf("sources\n got %q\nwant %q", got, want)
	}
	// A video sent as itself names its file in its meta.
	native := []llm.Block{
		{Type: llm.BlockText, Text: "see @demo.mp4"},
		{Type: llm.BlockVideo, MediaType: "video/mp4", Data: fakeData("vid"), Input: json.RawMessage(`{"name":"demo.mp4","end":5}`)},
	}
	if got := mediaSources(native); got[1] != "demo.mp4" {
		t.Fatalf("native video source %q", got[1])
	}

	out, n, saved := elideMedia(blocks)
	var stubs []string
	for _, b := range out {
		if isMedia(b) {
			t.Fatalf("media left: %+v", b.Type)
		}
		if mediaStubRE.MatchString(b.Text) {
			stubs = append(stubs, b.Text)
		}
	}
	wantStubs := []string{
		"[image elided: docs/x.png; read it again if needed]",
		"[2 images elided: clip.mov; read it again if needed]",
		"[image elided: /tmp/a.png; read it again if needed]",
		"[3 images elided: /tmp/v.mp4; read it again if needed]",
		"[image elided: old attachments are dropped to save context]",
		"[image elided: /tmp/c.png; read it again if needed]",
	}
	if n != 9 || saved < 9*1500 || !reflect.DeepEqual(stubs, wantStubs) {
		t.Fatalf("elided %d (saved %d):\n%s", n, saved, strings.Join(stubs, "\n"))
	}
	if !isMedia(blocks[2]) {
		t.Fatal("elideMedia changed its input")
	}
	if strings.Contains(typedText(llm.Message{Role: llm.RoleUser, Blocks: out}), "elided") {
		t.Fatal("stubs carried over as the user's text")
	}
}

// recordingCache keeps every request's messages (an exact-TTL cache).
type recordingCache struct {
	exactCache
	sent [][]llm.Message
}

func (*recordingCache) TakesVideo(string) bool { return true }

func (p *recordingCache) Stream(ctx context.Context, req *llm.Request, fn func(llm.Event)) (*llm.Message, error) {
	p.sent = append(p.sent, slices.Clone(req.Messages))
	return p.exactCache.Stream(ctx, req, fn)
}

const (
	oldShot  = "/tmp/old.png"
	oldVideo = "/tmp/old.mp4"
	newShot  = "/tmp/new.png"
)

// mediaAgent has ~60k tokens of old tool output after a prompt that
// attached an image and a minute of video, then a recent prompt with an
// image: on a 1M window, below the warm elision threshold.
func mediaAgent(t *testing.T) (*Agent, *recordingCache, func(time.Duration)) {
	t.Helper()
	prov := &recordingCache{}
	a := &Agent{Provider: prov, UI: &infoUI{}, ContextWindow: 1_000_000, AutoCompact: true, KeepRecent: 5000}
	h := []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{
		{Type: llm.BlockText, Text: "fix the layout in [Image #1: " + oldShot + "] as the [Video #2: " + oldVideo + "] shows"},
		img("old"),
		{Type: llm.BlockText, Text: "[Video #2] Video " + oldVideo + " (60.0 s, 1280x720, sound included): attached as a video"},
		{Type: llm.BlockVideo, MediaType: "video/mp4", Data: fakeData("video"), Input: json.RawMessage(`{"name":"` + oldVideo + `","duration":60,"end":60}`)},
	}}}
	h = append(h, toolHistory(30, 2000)[1:]...)
	h = append(h,
		llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "now this one [Image #3: " + newShot + "]"}, img("new")}},
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "ok"}}},
		llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "next"}}})
	stripUsage(h)
	a.SetHistory(h)
	t0 := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	a.lastReply = t0
	now := t0
	a.clock = func() time.Time { return now }
	return a, prov, func(d time.Duration) { now = now.Add(d) }
}

// attached lists what a request holds of the three attachments: "media"
// while sent, else the stub's text.
func attached(msgs []llm.Message) map[string]string {
	got := map[string]string{}
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Blocks {
			switch {
			case strings.HasPrefix(b.Data, "old"):
				got[oldShot] = "media"
			case strings.HasPrefix(b.Data, "video"):
				got[oldVideo] = "media"
			case strings.HasPrefix(b.Data, "new"):
				got[newShot] = "media"
			case mediaStubRE.MatchString(b.Text):
				for _, p := range []string{oldShot, oldVideo, newShot} {
					if strings.Contains(b.Text, p) {
						got[p] = b.Text
					}
				}
			}
		}
	}
	return got
}

func TestOldAttachmentsElidedOnlyOnceTheCacheIsCold(t *testing.T) {
	a, prov, wait := mediaAgent(t)
	// Warm cache: every request sends the attachments as they were.
	for i := range 3 {
		wait(2 * time.Minute)
		if err := a.loop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := attached(prov.sent[i]); got[oldShot] != "media" || got[oldVideo] != "media" || got[newShot] != "media" {
			t.Fatalf("warm request %d: %v", i, got)
		}
		a.Messages = append(a.Messages, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "more"}}})
	}
	if !hasVideo(a.Messages) {
		t.Fatal("setup: the video is gone")
	}
	// The cache expired: the old attachments become stubs naming their
	// files; the recent one stays.
	wait(10 * time.Minute)
	if err := a.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	cold := prov.sent[len(prov.sent)-1]
	got := attached(cold)
	if got[oldShot] != "[image elided: "+oldShot+"; read it again if needed]" ||
		got[oldVideo] != "[video elided: "+oldVideo+"; read it again if needed]" {
		t.Fatalf("old attachments: %q", got)
	}
	if got[newShot] != "media" {
		t.Fatalf("the recent image was elided: %q", got[newShot])
	}
	if hasVideo(cold) {
		t.Fatal("an elided video is still sent (and would be turned into frames)")
	}
	if ui := a.UI.(*infoUI); len(ui.infos) == 0 || !strings.Contains(ui.infos[len(ui.infos)-1], "prompt cache expired") {
		t.Fatalf("infos %q", ui.infos)
	}
	// Later requests extend the elided history: one stable prefix.
	for range 2 {
		a.Messages = append(a.Messages, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "go on"}}})
		wait(time.Minute)
		if err := a.loop(context.Background()); err != nil {
			t.Fatal(err)
		}
		next := prov.sent[len(prov.sent)-1]
		if len(next) <= len(cold) || hashMessages(next[:len(cold)]) != hashMessages(cold) {
			t.Fatal("the request after elision does not start with the one before")
		}
		cold = next
	}
}

// When the recent window alone is over budget, everything but the last
// round is elided, yet the media of the latest prompts stays.
func TestLatestPromptsKeepTheirMedia(t *testing.T) {
	a := &Agent{Provider: &countingProvider{}, UI: nopUI{}, ContextWindow: 100_000, AutoCompact: true, KeepRecent: 5000}
	video := llm.Block{Type: llm.BlockVideo, MediaType: "video/mp4", Data: fakeData("video"),
		Input: json.RawMessage(`{"name":"/tmp/long.mp4","end":600}`)} // ~60k tokens
	h := []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "old [Image #1: /tmp/a.png]"}, img("a")}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "seen"}}},
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "watch [Video #2: /tmp/long.mp4]"}, {Type: llm.BlockText, Text: "[Video #2] Video /tmp/long.mp4: attached as a video"}, video}},
	}
	h = append(h, toolHistory(3, 6000)[1:]...)
	stripUsage(h)
	a.SetHistory(h)
	if a.keepCut() >= 0 {
		t.Fatal("setup: the recent window should be over budget")
	}
	if n, _ := a.elide(0); n == 0 {
		t.Fatal("nothing elided")
	}
	if !isMedia(a.Messages[2].Blocks[2]) {
		t.Fatal("the video of the latest prompt was elided")
	}
	if !isMedia(a.Messages[0].Blocks[1]) {
		t.Fatal("within the last two prompts: kept")
	}
	// A third prompt makes the first one old.
	a.Messages = append(a.Messages, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "and?"}}})
	if n, _ := a.elide(0); n == 0 || isMedia(a.Messages[0].Blocks[1]) || !isMedia(a.Messages[2].Blocks[2]) {
		t.Fatalf("elided %d: first image %v, video %v", n, isMedia(a.Messages[0].Blocks[1]), isMedia(a.Messages[2].Blocks[2]))
	}
}

// An attached audio file (its own block type, from another change) is
// found and stubbed like images and video.
func TestElideMediaAudio(t *testing.T) {
	blocks := []llm.Block{
		{Type: llm.BlockText, Text: "listen to [Audio #1: /tmp/memo.wav]"},
		{Type: llm.BlockText, Text: "[Audio #1] Audio memo.wav (5.0 s): attached as audio"},
		{Type: llm.BlockAudio, MediaType: "audio/wav", Data: strings.Repeat("A", 4000)},
	}
	if src := mediaSources(blocks); src[2] != "/tmp/memo.wav" {
		t.Fatalf("sources %q", src)
	}
	out, changed, _ := elideMedia(blocks)
	if changed != 1 || len(out) != 3 || out[2].Type != llm.BlockText || !strings.Contains(out[2].Text, "audio elided: /tmp/memo.wav") {
		t.Fatalf("%d changed: %+v", changed, out)
	}
}
