package session

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
)

func textMsg(role llm.Role, text string) llm.Message {
	return llm.Message{Role: role, Blocks: []llm.Block{{Type: llm.BlockText, Text: text}}}
}

// Subagent records: whole, then what was added, read back after the file
// is opened again; the latest per subagent on the active path counts.
func TestSubagentRecords(t *testing.T) {
	base := t.TempDir()
	s, err := New(base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(textMsg(llm.RoleUser, "start")); err != nil {
		t.Fatal(err)
	}
	early := s.Leaf()
	image := strings.Repeat("A", 4000)
	hist := []llm.Message{textMsg(llm.RoleUser, "work"), {Role: llm.RoleAssistant, Blocks: []llm.Block{
		{Type: llm.BlockText, Text: strings.Repeat("found it ", 2000)}, {Type: llm.BlockImage, MediaType: "image/png", Data: image}}}}
	r1, err := s.SaveSubagent("k1", "", 0, json.RawMessage(`{"name":"A"}`), hist)
	if err != nil || r1 == "" {
		t.Fatal(r1, err)
	}
	if err := s.Append(textMsg(llm.RoleAssistant, "A ran")); err != nil {
		t.Fatal(err)
	}
	afterA := s.Leaf()
	hist = append(hist, textMsg(llm.RoleUser, "go on"), textMsg(llm.RoleAssistant, "done"))
	r2, err := s.SaveSubagent("k1", r1, 2, json.RawMessage(`{"name":"A","task":"more"}`), hist[2:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSubagent("k2", "", 0, json.RawMessage(`{"name":"B"}`), hist[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSubagent("k3", "x", 0, nil, hist); err == nil {
		t.Fatal("a record that builds on another starts after message 0")
	}
	s.Close()

	// The image is a blob like the session's, and the text is gzipped.
	data, _ := os.ReadFile(s.Path)
	if n := strings.Count(string(data), image); n != 1 {
		t.Fatalf("image %d times in the file", n)
	}
	if strings.Contains(string(data), "found it found it") || len(data) > 6000 {
		t.Fatalf("not compressed: %d bytes", len(data))
	}

	s, _, err = Open(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recs := s.Subagents()
	if len(recs) != 2 || recs[0].ID != r2 || recs[0].Count != 4 || string(recs[0].Meta) != `{"name":"A","task":"more"}` || recs[1].Key != "k2" {
		t.Fatalf("records %+v", recs)
	}
	got, err := s.SubagentHistory(r2)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := json.Marshal(hist); fmt.Sprint(mustJSON(got)) != string(want) {
		t.Fatalf("history:\n%s\nwant\n%s", mustJSON(got), want)
	}

	// Back on the branch before A's second record: its first is the latest.
	if _, err := s.Checkout(afterA); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(textMsg(llm.RoleUser, "another way")); err != nil {
		t.Fatal(err)
	}
	if recs := s.Subagents(); len(recs) != 2 || recs[0].ID != r2 {
		t.Fatalf("a branch after both: %+v", recs)
	}
	if _, err := s.Checkout(early); err != nil {
		t.Fatal(err)
	}
	if recs := s.Subagents(); len(recs) != 1 || recs[0].ID != r1 || recs[0].Count != 2 {
		t.Fatalf("where A had run once and B not yet: %+v", recs)
	}
	if _, err := s.Checkout(""); err != nil {
		t.Fatal(err)
	}
	if recs := s.Subagents(); len(recs) != 0 {
		t.Fatalf("before any: %+v", recs)
	}
	if _, err := s.SubagentHistory("nope"); err == nil {
		t.Fatal("no such record")
	}
}

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}
