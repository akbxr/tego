package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
)

// media is base64-looking data of n bytes, distinct per tag.
func media(tag string, n int) string {
	return tag + strings.Repeat("Q", n-len(tag))
}

// mediaHistory has every kind of media: attached images (one twice), a
// video with its meta, an audio block (a type this package doesn't know),
// a tool result's images, and data too small for a blob.
func mediaHistory() (before []llm.Message, repl []llm.Message, after []llm.Message) {
	shot, clip, sound, tool := media("shot", 5000), media("clip", 9000), media("snd", 3000), media("tool", 4000)
	small := media("tiny", 200)
	before = []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockText, Text: "look [Image #1: /tmp/a.png] [Video #2: /tmp/v.mp4]"},
			{Type: llm.BlockImage, MediaType: "image/png", Data: shot},
			{Type: llm.BlockText, Text: "[Video #2] a clip"},
			{Type: llm.BlockVideo, MediaType: "video/mp4", Data: clip, Input: json.RawMessage(`{"name":"/tmp/v.mp4","end":3}`)},
			{Type: "audio", MediaType: "audio/mpeg", Data: sound},
			{Type: llm.BlockImage, MediaType: "image/gif", Data: small},
		}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "r1", Name: "read", Input: json.RawMessage(`{"path":"b.png"}`)},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolUseID: "r1", Content: "Image b.png",
				Images: []llm.Block{{Type: llm.BlockImage, MediaType: "image/png", Data: tool}, {Type: llm.BlockImage, MediaType: "image/png", Data: shot}}},
		}},
	}
	// A compaction keeps the recent messages, media and all.
	repl = append([]llm.Message{msg(llm.RoleUser, "summary")}, before...)
	after = []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "again"}, {Type: llm.BlockImage, MediaType: "image/png", Data: shot}}},
		msg(llm.RoleAssistant, "ok"),
	}
	return before, repl, after
}

// writeOld writes the history as tego did before blobs: data inline.
func writeOld(t *testing.T, path string, before, repl, after []llm.Message) {
	t.Helper()
	var lines []entry
	lines = append(lines, entry{Type: "header", ID: "old", Cwd: "/x"})
	parent := ""
	add := func(e entry) {
		e.ID, e.Parent, e.Time = fmt.Sprintf("n%d", len(lines)), parent, 1
		parent = e.ID
		lines = append(lines, e)
	}
	for _, m := range before {
		add(entry{Type: "message", Message: &m})
	}
	add(entry{Type: "compaction", Messages: repl})
	for _, m := range after {
		add(entry{Type: "message", Message: &m})
	}
	var b strings.Builder
	for _, e := range lines {
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeNew(t *testing.T, base string, before, repl, after []llm.Message) *Session {
	t.Helper()
	s, err := New(base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range before {
		if err := s.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Compacted(repl); err != nil {
		t.Fatal(err)
	}
	for _, m := range after {
		if err := s.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func compactionOf(s *Session) *Node {
	for _, n := range s.ActivePath() {
		if n.IsCompaction() {
			return n
		}
	}
	return nil
}

func wire(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A file written with blobs reads back exactly what one with the data
// inline does: the model gets the same bytes.
func TestBlobFileReadsLikeAnInlineOne(t *testing.T) {
	before, repl, after := mediaHistory()
	want := append(append([]llm.Message{}, repl...), after...)

	oldPath := filepath.Join(t.TempDir(), "old.jsonl")
	writeOld(t, oldPath, before, repl, after)
	ns := writeNew(t, t.TempDir(), before, repl, after)
	newHist := ns.History(ns.Leaf())
	ns.Close()

	o, oldHist, err := Open(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	n, reopened, err := Open(ns.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	light(t, n)
	for name, got := range map[string][]llm.Message{"old file": oldHist, "new file": reopened, "new session": newHist} {
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: history differs from what was written", name)
		}
	}
	if wire(t, oldHist) != wire(t, reopened) {
		t.Fatal("old and new files give different bytes")
	}
	if !reflect.DeepEqual(n.Replacement(compactionOf(n)), o.Replacement(compactionOf(o))) {
		t.Fatal("replacements differ")
	}
	// Outlines show the media's own types, not the reference's.
	var types []llm.BlockType
	for _, b := range n.Tree()[0].Node.Message.Blocks {
		types = append(types, b.Type)
	}
	if want := []llm.BlockType{llm.BlockText, llm.BlockImage, llm.BlockText, llm.BlockVideo, "audio", llm.BlockImage}; !reflect.DeepEqual(types, want) {
		t.Fatalf("outline block types %v", types)
	}

	// Each piece of data is in the new file once; small data stays inline.
	oldData, _ := os.ReadFile(oldPath)
	newData, _ := os.ReadFile(ns.Path)
	for _, d := range []string{media("shot", 5000), media("clip", 9000), media("snd", 3000), media("tool", 4000)} {
		if c := strings.Count(string(newData), d); c != 1 {
			t.Fatalf("data %.4s… is in the new file %d times", d, c)
		}
	}
	if c := strings.Count(string(newData), media("tiny", 200)); c != 2 {
		t.Fatalf("small data is in the new file %d times, want inline in both copies", c)
	}
	if c := strings.Count(string(oldData), media("shot", 5000)); c < 4 {
		t.Fatalf("setup: the old file has the image %d times", c)
	}
	if len(newData)*2 > len(oldData) {
		t.Fatalf("new file %d bytes, old %d", len(newData), len(oldData))
	}
}

// A fork writes a new file: it gets its own blobs.
func TestForkWritesItsBlobs(t *testing.T) {
	before, repl, after := mediaHistory()
	base := t.TempDir()
	s := writeNew(t, base, before, repl, after)
	hist := s.History(s.Leaf())
	s.Close()
	f, err := NewFrom(base, t.TempDir(), hist, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	_ = os.Remove(s.Path) // the fork must not need its parent's file
	_, got, err := Open(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, hist) {
		t.Fatal("fork history differs")
	}
	data, _ := os.ReadFile(f.Path)
	if c := strings.Count(string(data), media("shot", 5000)); c != 1 {
		t.Fatalf("fork holds the image %d times", c)
	}
}

// Appending to a file written before blobs adds them from then on.
func TestOldFileGainsBlobsFromThenOn(t *testing.T) {
	before, repl, after := mediaHistory()
	path := filepath.Join(t.TempDir(), "old.jsonl")
	writeOld(t, path, before, repl, after)
	s, hist, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Compacted(hist); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(after[0]); err != nil {
		t.Fatal(err)
	}
	s.Close()
	_, got, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, append(hist, after[0])) {
		t.Fatal("history differs after appending to an old file")
	}
}

// What an older tego makes of a file with blobs: its decoder sees reference
// blocks of a type no provider sends, so it leaves the media out rather
// than send blocks without data, and no entry fails to parse.
func TestOlderReaderSkipsReferences(t *testing.T) {
	before, repl, after := mediaHistory()
	s := writeNew(t, t.TempDir(), before, repl, after)
	s.Close()
	f, _ := os.Open(s.Path)
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	refs := 0
	var check func(bs []llm.Block)
	check = func(bs []llm.Block) {
		for _, b := range bs {
			check(b.Images)
			switch b.Type {
			case llm.BlockImage, llm.BlockVideo, "audio":
				if b.Data == "" || strings.HasPrefix(b.Data, refPrefix) {
					t.Fatalf("a %s block an older tego would send without data", b.Type)
				}
			case blockRef:
				refs++
				if b.Name == "" || !strings.HasPrefix(b.Data, refPrefix) || b.MediaType == "" {
					t.Fatalf("reference %+v", b)
				}
			}
		}
	}
	for sc.Scan() {
		var e entry // the old decoder: llm.Message as is
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e.Message != nil {
			check(e.Message.Blocks)
		}
		for _, m := range e.Messages {
			check(m.Blocks)
		}
	}
	if refs < 8 {
		t.Fatalf("%d references", refs)
	}
}

// A reference whose blob is not in the file (a fork an older tego copied)
// reads as a note, not a block without data.
func TestMissingBlobLeavesANote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.jsonl")
	data := `{"type":"header","id":"f"}
{"type":"message","id":"a","ts":1,"message":{"role":"user","content":[{"type":"text","text":"x"},{"type":"media_ref","name":"image","media_type":"image/png","data":"sha256:00ff"}]}}
`
	_ = os.WriteFile(path, []byte(data), 0o600)
	s, hist, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if b := hist[0].Blocks[1]; b.Type != llm.BlockText || !strings.Contains(b.Text, "image image/png is missing") {
		t.Fatalf("block %+v", b)
	}
}
