package session

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
)

// heavy is a turn whose bulk (an image, a tool's output, thinking, a tool
// call's input) the session's nodes don't keep.
func heavy(i int) []llm.Message {
	img := strings.Repeat("A", 4000) + string(rune('a'+i))
	return []llm.Message{
		{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockThinking, Text: "thinking it over", Signature: "sig"},
			{Type: llm.BlockText, Text: "looking"},
			{Type: llm.BlockToolUse, ID: "t1", Name: "read", Input: json.RawMessage(`{"path":"shot.png"}`)},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolUseID: "t1", Content: "Image shot.png",
				Images: []llm.Block{{Type: llm.BlockImage, MediaType: "image/png", Data: img}}},
		}},
	}
}

// light reports what of a node's bulk is still in memory.
func light(t *testing.T, s *Session) {
	t.Helper()
	for _, n := range s.nodes {
		if n.full != nil || n.repl != nil {
			t.Fatalf("node %s is kept whole", n.ID)
		}
		if n.Message == nil {
			continue
		}
		for _, b := range n.Message.Blocks {
			if b.Data != "" || b.Images != nil || b.Content != "" || b.Input != nil || b.Signature != "" || (b.Type != llm.BlockText && b.Text != "") {
				t.Fatalf("node %s keeps %+v", n.ID, b)
			}
		}
	}
}

// Nodes keep an outline; History reads the whole messages back from the
// file, in this process and after a reload.
func TestHistoryReadsBulkFromTheFile(t *testing.T) {
	s, _ := New(t.TempDir(), t.TempDir())
	var want []llm.Message
	add := func(ms ...llm.Message) {
		for _, m := range ms {
			if err := s.Append(m); err != nil {
				t.Fatal(err)
			}
			want = append(want, m)
		}
	}
	add(msg(llm.RoleUser, "look at the screenshots"))
	for i := range 3 {
		add(heavy(i)...)
	}
	light(t, s)
	if got := s.History(s.Leaf()); !reflect.DeepEqual(got, want) {
		t.Fatalf("history differs from what was appended:\n%+v", got)
	}
	if p := s.Tree()[0].Node.Message; p.Blocks[0].Text != "look at the screenshots" {
		t.Fatalf("outline lost the prompt: %+v", p)
	}
	s.Close()

	r, hist, err := Open(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	light(t, r)
	if !reflect.DeepEqual(hist, want) {
		t.Fatal("reloaded history differs")
	}
	// The open file is read: moving it away loses nothing.
	if err := os.Rename(r.Path, r.Path+".moved"); err != nil {
		t.Fatal(err)
	}
	if got := r.History(r.Leaf()); !reflect.DeepEqual(got, want) {
		t.Fatal("history lost once the file moved")
	}
}

// A compaction's history is read when asked for, not kept: History and
// Replacement return it, in this process and after a reload, with the
// path before it on its own branch.
func TestCompactionReadOnDemand(t *testing.T) {
	s, _ := New(t.TempDir(), t.TempDir())
	_ = s.Append(msg(llm.RoleUser, "old"))
	repl := append([]llm.Message{msg(llm.RoleUser, "summary")}, heavy(0)...)
	_ = s.Compacted(repl)
	comp := s.Leaf()
	_ = s.Append(msg(llm.RoleUser, "next"))
	light(t, s)
	check := func(s *Session) {
		t.Helper()
		var c *Node
		for _, n := range s.ActivePath() {
			if n.IsCompaction() {
				c = n
			}
		}
		if c == nil || c.ID != comp {
			t.Fatal("no compaction on the active path")
		}
		if !reflect.DeepEqual(s.Replacement(c), repl) {
			t.Fatal("replacement differs")
		}
		if got := s.History(s.Leaf()); len(got) != 4 || !reflect.DeepEqual(got[:3], repl) || got[3].Blocks[0].Text != "next" {
			t.Fatalf("history %v", texts(got))
		}
	}
	check(s)
	s.Close()
	r, _, err := Open(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	light(t, r)
	check(r)
	// An empty compaction still replaces what was before it.
	_ = r.Compacted(nil)
	_ = r.Append(msg(llm.RoleUser, "fresh"))
	if got := texts(r.History(r.Leaf())); !reflect.DeepEqual(got, []string{"fresh"}) {
		t.Fatalf("after an empty compaction %v", got)
	}
}

// Without an open file (closed session) a node keeps its message whole,
// and a closed session reads its file again.
func TestClosedSessionKeepsWhatItCannotWrite(t *testing.T) {
	s, _ := New(t.TempDir(), t.TempDir())
	_ = s.Append(heavy(0)[0])
	s.Close()
	late := heavy(1)[1]
	_ = s.Append(late)
	got := s.History(s.Leaf())
	if len(got) != 2 || !reflect.DeepEqual(got[0], heavy(0)[0]) || !reflect.DeepEqual(got[1], late) {
		t.Fatalf("history after close %+v", got)
	}
	_ = s.Compacted([]llm.Message{late})
	if got := s.History(s.Leaf()); !reflect.DeepEqual(got, []llm.Message{late}) {
		t.Fatalf("compaction after close %+v", got)
	}
}

// The session list counts and previews from outlines.
func TestPreviewFromOutlines(t *testing.T) {
	base, cwd := t.TempDir(), t.TempDir()
	s, _ := New(base, cwd)
	_ = s.Compacted([]llm.Message{msg(llm.RoleUser, "first prompt")})
	for i := range 2 {
		for _, m := range heavy(i) {
			_ = s.Append(m)
		}
	}
	s.Close()
	list, err := List(base, cwd)
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	if list[0].Count != 5 || list[0].Preview != "first prompt" {
		t.Fatalf("info %+v", list[0])
	}
}
