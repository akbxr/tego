package session

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

func msg(role llm.Role, text string) llm.Message {
	return llm.Message{Role: role, Blocks: []llm.Block{{Type: llm.BlockText, Text: text}}}
}

func texts(msgs []llm.Message) []string {
	out := []string{}
	for _, m := range msgs {
		out = append(out, m.Blocks[0].Text)
	}
	return out
}

func TestBranchingAndReload(t *testing.T) {
	base, cwd := t.TempDir(), t.TempDir()
	s, err := New(base, cwd)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []llm.Message{msg(llm.RoleUser, "q1"), msg(llm.RoleAssistant, "a1"), msg(llm.RoleUser, "q2"), msg(llm.RoleAssistant, "a2")} {
		if err := s.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	a1 := s.Tree()[1].Node.ID
	hist, err := s.Checkout(a1)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(hist); !reflect.DeepEqual(got, []string{"q1", "a1"}) {
		t.Fatalf("checkout history %v", got)
	}
	_ = s.Append(msg(llm.RoleUser, "q2b"))
	_ = s.Append(msg(llm.RoleAssistant, "a2b"))
	if got := texts(s.History(s.Leaf())); !reflect.DeepEqual(got, []string{"q1", "a1", "q2b", "a2b"}) {
		t.Fatalf("branch history %v", got)
	}
	s.Close()

	r, hist, err := Open(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := texts(hist); !reflect.DeepEqual(got, []string{"q1", "a1", "q2b", "a2b"}) {
		t.Fatalf("reloaded active path %v", got)
	}
	rows := r.Tree()
	if len(rows) != 6 {
		t.Fatalf("tree rows %d", len(rows))
	}
	// q2/a2 and q2b/a2b are sibling branches one level below a1.
	var old, cur []string
	for _, row := range rows {
		txt := row.Node.Message.Blocks[0].Text
		switch txt {
		case "q1", "a1":
			if row.Depth != 0 || !row.OnPath {
				t.Fatalf("%s depth=%d onPath=%v", txt, row.Depth, row.OnPath)
			}
		case "q2", "a2":
			old = append(old, txt)
			if row.Depth != 1 || row.OnPath {
				t.Fatalf("%s depth=%d onPath=%v", txt, row.Depth, row.OnPath)
			}
		case "q2b", "a2b":
			cur = append(cur, txt)
			if row.Depth != 1 || !row.OnPath {
				t.Fatalf("%s depth=%d onPath=%v", txt, row.Depth, row.OnPath)
			}
		}
	}
	if len(old) != 2 || len(cur) != 2 {
		t.Fatalf("branches old=%v cur=%v", old, cur)
	}
	// Checkout to the old branch survives a reload.
	if _, err := r.Checkout(rows[3].Node.ID); err != nil { // a2
		t.Fatal(err)
	}
	r.Close()
	_, hist, _ = Open(s.Path)
	if got := texts(hist); !reflect.DeepEqual(got, []string{"q1", "a1", "q2", "a2"}) {
		t.Fatalf("leaf not persisted: %v", got)
	}
}

func TestCompactionNodeReplacesPrefix(t *testing.T) {
	s, _ := New(t.TempDir(), t.TempDir())
	defer s.Close()
	_ = s.Append(msg(llm.RoleUser, "old"))
	_ = s.Compacted([]llm.Message{msg(llm.RoleUser, "summary")})
	_ = s.Append(msg(llm.RoleAssistant, "next"))
	if got := texts(s.History(s.Leaf())); !reflect.DeepEqual(got, []string{"summary", "next"}) {
		t.Fatalf("history %v", got)
	}
}

func TestLegacyLinearFileLoads(t *testing.T) {
	base, cwd := t.TempDir(), t.TempDir()
	dir := Dir(base, cwd)
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "old.jsonl")
	data := `{"type":"header","id":"old"}
{"type":"message","message":{"role":"user","content":[{"type":"text","text":"x"}]}}
{"type":"compaction","messages":[{"role":"user","content":[{"type":"text","text":"sum"}]}]}
{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"y"}]}}
`
	_ = os.WriteFile(path, []byte(data), 0o600)
	s, hist, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := texts(hist); !reflect.DeepEqual(got, []string{"sum", "y"}) {
		t.Fatalf("legacy history %v", got)
	}
	_ = s.Append(msg(llm.RoleUser, "z"))
	if got := texts(s.History(s.Leaf())); !reflect.DeepEqual(got, []string{"sum", "y", "z"}) {
		t.Fatalf("append after legacy %v", got)
	}
}

func TestNewFromCopiesHistory(t *testing.T) {
	base, cwd := t.TempDir(), t.TempDir()
	src := []llm.Message{msg(llm.RoleUser, "a"), msg(llm.RoleAssistant, "b")}
	s, err := NewFrom(base, cwd, src, "parent-id")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	_, hist, _ := Open(s.Path)
	if got := texts(hist); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("copied history %v", got)
	}
}

func TestContinueFromToolCallKeepsResults(t *testing.T) {
	s, _ := New(t.TempDir(), t.TempDir())
	defer s.Close()
	_ = s.Append(msg(llm.RoleUser, "q"))
	_ = s.Append(llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
		{Type: llm.BlockText, Text: "checking"}, {Type: llm.BlockToolUse, ID: "c1", Name: "bash"}}})
	_ = s.Append(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "c1", Content: "ok"}}})
	_ = s.Append(msg(llm.RoleAssistant, "done"))

	path := s.ActivePath()
	if len(path) != 4 {
		t.Fatalf("active path %d nodes", len(path))
	}
	id, err := s.ContinueFrom(path[1]) // the tool-calling answer
	if err != nil || id != path[2].ID {
		t.Fatalf("continue from tool call should land on its results, got %q %v", id, err)
	}
	if id, _ := s.ContinueFrom(path[3]); id != path[3].ID {
		t.Fatalf("plain answer should continue from itself, got %q", id)
	}
	// A tool call whose results were never written cannot be continued from.
	_, _ = s.Checkout(path[0].ID)
	_ = s.Append(llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "c2", Name: "bash"}}})
	dangling := s.ActivePath()[1]
	if _, err := s.ContinueFrom(dangling); err == nil {
		t.Fatal("expected an error for a tool call without results")
	}
}

func TestTitlePersistsAndNamesTheSession(t *testing.T) {
	base, cwd := t.TempDir(), t.TempDir()
	s, err := New(base, cwd)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Append(msg(llm.RoleUser, "please redesign the billing dashboard and also fix the navbar"))
	_ = s.Append(msg(llm.RoleAssistant, "ok"))
	if list, _ := List(base, cwd); len(list) != 1 || list[0].Preview != "please redesign the billing dashboard and also fix the navbar" {
		t.Fatalf("untitled preview = %+v", list)
	}
	_ = s.SetTitle("Old title")
	_ = s.SetTitle("Billing dashboard redesign") // the last one wins
	_ = s.Append(msg(llm.RoleUser, "next"))
	s.Close()
	re, hist, err := Open(s.Path)
	if err != nil || re.Title != "Billing dashboard redesign" || len(hist) != 3 {
		t.Fatalf("reopened: title %q, %d messages, %v", re.Title, len(hist), err)
	}
	re.Close()
	if list, _ := List(base, cwd); list[0].Preview != "Billing dashboard redesign" {
		t.Fatalf("titled preview = %q", list[0].Preview)
	}
}

func TestListAllFindAndDelete(t *testing.T) {
	defer func(c string) { trashCommand = c }(trashCommand)
	trashCommand = "" // never fill the real Trash from a test
	base, projA, projB := t.TempDir(), t.TempDir(), t.TempDir()
	mk := func(cwd, text string) *Session {
		s, err := New(base, cwd)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Append(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: text}}}); err != nil {
			t.Fatal(err)
		}
		s.Close()
		return s
	}
	a := mk(projA, "in A")
	b := mk(projB, "in B")
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(a.Path, old, old)

	all, err := ListAll(base)
	if err != nil || len(all) != 2 || all[0].ID != b.ID || all[1].ID != a.ID {
		t.Fatalf("ListAll %+v %v", all, err)
	}
	if all[0].Cwd != projB || all[1].Cwd != projA || all[0].Size == 0 {
		t.Fatalf("cwd/size %+v", all)
	}
	if here, _ := List(base, projA); len(here) != 1 || here[0].ID != a.ID {
		t.Fatalf("List %+v", here)
	}
	// An id from another project still resolves.
	if p, err := Find(base, projA, b.ID[:len(b.ID)-2]); err != nil || p != b.Path {
		t.Fatalf("Find across projects %q %v", p, err)
	}
	if s, _, err := Open(b.Path); err != nil || s.Cwd != projB {
		t.Fatalf("Open cwd %v %v", s, err)
	} else {
		s.Close()
	}

	outside := filepath.Join(t.TempDir(), "x.jsonl")
	_ = os.WriteFile(outside, []byte("{}"), 0o600)
	if err := Delete(base, outside); err == nil {
		t.Fatal("deleted a file outside the session store")
	}
	if err := Delete(base, b.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(b.Path)); !os.IsNotExist(err) {
		t.Fatal("empty project folder kept")
	}
	if all, _ := ListAll(base); len(all) != 1 || all[0].ID != a.ID {
		t.Fatalf("after delete %+v", all)
	}
}

func TestRenameWithoutOpening(t *testing.T) {
	base, cwd := t.TempDir(), t.TempDir()
	s, err := New(base, cwd)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Append(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "first prompt"}}})
	s.Close()
	if err := Rename(s.Path, "My session"); err != nil {
		t.Fatal(err)
	}
	if list, _ := List(base, cwd); len(list) != 1 || list[0].Preview != "My session" || list[0].Count != 1 {
		t.Fatalf("list %+v", list)
	}
}
