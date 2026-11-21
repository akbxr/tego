package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
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
	// A tool call whose results were never written (tego stopped while it
	// ran) continues from itself: the agent answers it as interrupted.
	_, _ = s.Checkout(path[0].ID)
	_ = s.Append(llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "c2", Name: "bash"}}})
	dangling := s.ActivePath()[1]
	if id, err := s.ContinueFrom(dangling); err != nil || id != dangling.ID {
		t.Fatalf("tool call without results: %q %v", id, err)
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

// A resumed session keeps its /goal; one cleared stays cleared, and a
// file from before goals has none.
func TestGoalPersists(t *testing.T) {
	s, err := New(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Append(msg(llm.RoleUser, "hi"))
	reopen := func() string {
		t.Helper()
		re, _, err := Open(s.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer re.Close()
		return re.Goal()
	}
	if g := reopen(); g != "" {
		t.Fatalf("goal before one was set: %q", g)
	}
	_ = s.SetGoal("all tests pass")
	_ = s.Append(msg(llm.RoleAssistant, "ok"))
	if g := reopen(); g != "all tests pass" {
		t.Fatalf("goal %q", g)
	}
	_ = s.SetGoal("")
	s.Close()
	if g := reopen(); g != "" {
		t.Fatalf("cleared goal %q", g)
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

// sessionWith makes a closed session of texts, user and assistant in turn.
func sessionWith(t *testing.T, base, cwd, id string, texts ...string) *Session {
	t.Helper()
	s, err := NewWithID(base, cwd, id)
	if err != nil {
		t.Fatal(err)
	}
	for i, x := range texts {
		role := llm.RoleUser
		if i%2 == 1 {
			role = llm.RoleAssistant
		}
		if err := s.Append(msg(role, x)); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	return s
}

// recordReads collects the files read for their summaries; the function
// it returns hands them over, sorted, and starts again.
func recordReads(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	readHook = func(p string) {
		mu.Lock()
		paths = append(paths, p)
		mu.Unlock()
	}
	t.Cleanup(func() { readHook = nil })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := paths
		paths = nil
		slices.Sort(out)
		return out
	}
}

// oldPreview is how session lists read a file before the summary cache:
// the whole session loaded, its active path's outlines counted.
func oldPreview(path string) summary {
	s, err := load(path)
	if err != nil {
		return summary{}
	}
	msgs := s.history(s.leaf, true)
	label := s.Title
	if label != "" {
		label = Oneline(label, 70)
	} else {
		label = FirstPrompt(msgs, 70)
	}
	return summary{label: label, count: len(msgs), cwd: s.Cwd, forkOf: s.ForkOf}
}

// oldFind is Find before it went by file names.
func oldFind(base, cwd, ref string) (string, error) {
	if strings.HasSuffix(ref, ".jsonl") {
		return ref, nil
	}
	match := func(list []Info) []string {
		var hits []string
		for _, s := range list {
			if strings.HasPrefix(s.ID, ref) {
				hits = append(hits, s.Path)
			}
		}
		return hits
	}
	list, err := List(base, cwd)
	if err != nil {
		return "", err
	}
	hits := match(list)
	if len(hits) == 0 {
		all, err := ListAll(base)
		if err != nil {
			return "", err
		}
		hits = match(all)
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no session matching %q", ref)
	case 1:
		return hits[0], nil
	}
	return "", fmt.Errorf("%q is ambiguous (%d sessions)", ref, len(hits))
}

// Find goes by file names: in this folder first, then in every project,
// reading only the files whose names match (to leave out the empty ones,
// as List does). Huge or broken files beside them are not read.
func TestFindGoesByFileNames(t *testing.T) {
	base, here, there := t.TempDir(), t.TempDir(), t.TempDir()
	mine := sessionWith(t, base, here, "20260301-101010-aaaaaa", "mine")
	theirs := sessionWith(t, base, there, "20260302-101010-bbbbbb", "theirs")
	sessionWith(t, base, here, "20260303-101010-cccc01", "one")
	sessionWith(t, base, here, "20260303-101010-cccc02", "two")
	sessionWith(t, base, here, "20260304-101010-dddd01") // no messages: List leaves it out
	full := sessionWith(t, base, here, "20260304-101010-dddd02", "kept")
	sessionWith(t, base, there, "20260305-101010-eeee01") // no messages anywhere
	// Files no prefix below matches: a corrupt one, and one too big to read.
	hereDir, thereDir := Dir(base, here), Dir(base, there)
	_ = os.WriteFile(filepath.Join(hereDir, "zz-corrupt.jsonl"), []byte("{not json\x00\x01"), 0o600)
	_ = os.WriteFile(filepath.Join(thereDir, "zz-corrupt.jsonl"), []byte(`{"type":"message","id":`), 0o600)
	huge, err := os.Create(filepath.Join(hereDir, "zz-huge.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := huge.Truncate(300 << 20); err != nil { // sparse: no line end for 300 MB
		t.Fatal(err)
	}
	huge.Close()

	reads := recordReads(t)
	cases := []struct {
		ref, want, err string
		read           []string
	}{
		{ref: mine.ID, want: mine.Path, read: []string{mine.Path}},
		{ref: "20260301", want: mine.Path, read: []string{}},                  // read just before: cached
		{ref: theirs.ID[:12], want: theirs.Path, read: []string{theirs.Path}}, // not here: every project
		{ref: "20260303-101010-cccc", err: `"20260303-101010-cccc" is ambiguous (2 sessions)`},
		{ref: "20260304-101010-dddd", want: full.Path},
		{ref: "20260305", err: `no session matching "20260305"`},
		{ref: "nope", err: `no session matching "nope"`, read: []string{}},
		{ref: "/some/where.jsonl", want: "/some/where.jsonl", read: []string{}},
	}
	for _, c := range cases {
		got, err := Find(base, here, c.ref)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Fatalf("Find(%q) = %q, %v; want error %s", c.ref, got, err, c.err)
			}
		} else if err != nil || got != c.want {
			t.Fatalf("Find(%q) = %q, %v; want %s", c.ref, got, err, c.want)
		}
		read := reads()
		if c.read != nil && !reflect.DeepEqual(read, c.read) && !(len(read) == 0 && len(c.read) == 0) {
			t.Fatalf("Find(%q) read %v, want %v", c.ref, read, c.read)
		}
		for _, p := range read {
			if strings.Contains(p, "zz-") {
				t.Fatalf("Find(%q) read %s", c.ref, p)
			}
		}
	}
	readHook = nil
	// Same answers as when Find listed every session.
	for _, c := range cases {
		got, err := Find(base, here, c.ref)
		want, werr := oldFind(base, here, c.ref)
		if got != want || fmt.Sprint(err) != fmt.Sprint(werr) {
			t.Fatalf("Find(%q) = %q, %v; listing all said %q, %v", c.ref, got, err, want, werr)
		}
	}
}

// Lists read a file again only when its size or modification time changed.
func TestListCachesSummaries(t *testing.T) {
	defer func(c string) { trashCommand = c }(trashCommand)
	trashCommand = ""
	base, cwd := t.TempDir(), t.TempDir()
	a := sessionWith(t, base, cwd, "", "first a", "answer")
	b := sessionWith(t, base, cwd, "", "first b")
	reads := recordReads(t)
	list, err := List(base, cwd)
	if err != nil || len(list) != 2 {
		t.Fatalf("list %+v %v", list, err)
	}
	if got := reads(); !reflect.DeepEqual(got, sortedPaths(a, b)) {
		t.Fatalf("first list read %v", got)
	}
	again, _ := List(base, cwd)
	all, _ := ListAll(base)
	if got := reads(); len(got) != 0 {
		t.Fatalf("unchanged files read again: %v", got)
	}
	if !reflect.DeepEqual(again, list) || !reflect.DeepEqual(all, list) {
		t.Fatalf("cached list differs:\n%+v\n%+v\n%+v", list, again, all)
	}
	info := Info{Path: b.Path}
	Describe(&info)
	if info.Preview != "first b" || info.Count != 1 || info.Cwd != cwd || len(reads()) != 0 {
		t.Fatalf("describe %+v", info)
	}

	// An append changes the size: only that file is read again.
	r, _, err := Open(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Append(msg(llm.RoleUser, "more"))
	r.Close()
	list, _ = List(base, cwd)
	if got := reads(); !reflect.DeepEqual(got, []string{a.Path}) {
		t.Fatalf("after an append read %v", got)
	}
	if c := countOf(list, a.ID); c != 3 {
		t.Fatalf("count %d after an append", c)
	}
	if err := Rename(b.Path, "Named"); err != nil {
		t.Fatal(err)
	}
	list, _ = List(base, cwd)
	if got := reads(); !reflect.DeepEqual(got, []string{b.Path}) || previewOf(list, b.ID) != "Named" {
		t.Fatalf("after rename read %v, list %+v", got, list)
	}

	// The same size and time is the same file to the cache; another time is not.
	fi, _ := os.Stat(b.Path)
	data, _ := os.ReadFile(b.Path)
	swapped := strings.Replace(string(data), `"title":"Named"`, `"title":"Other"`, 1)
	if err := os.WriteFile(b.Path, []byte(swapped), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(b.Path, fi.ModTime(), fi.ModTime())
	if list, _ = List(base, cwd); previewOf(list, b.ID) != "Named" || len(reads()) != 0 {
		t.Fatalf("same size and time read again: %+v", list)
	}
	later := fi.ModTime().Add(time.Second)
	_ = os.Chtimes(b.Path, later, later)
	if list, _ = List(base, cwd); previewOf(list, b.ID) != "Other" {
		t.Fatalf("new time not read: %+v", list)
	}
	if got := reads(); !reflect.DeepEqual(got, []string{b.Path}) {
		t.Fatalf("new time read %v", got)
	}

	// A deleted file leaves the cache with the next list.
	if err := Delete(base, a.Path); err != nil {
		t.Fatal(err)
	}
	if list, _ = List(base, cwd); len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("after delete %+v", list)
	}
	summaries.Lock()
	_, kept := summaries.dirs[Dir(base, cwd)][filepath.Base(a.Path)]
	summaries.Unlock()
	if kept {
		t.Fatal("deleted file still cached")
	}
}

func sortedPaths(ss ...*Session) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Path)
	}
	slices.Sort(out)
	return out
}

func countOf(list []Info, id string) int {
	for _, s := range list {
		if s.ID == id {
			return s.Count
		}
	}
	return -1
}

func previewOf(list []Info, id string) string {
	for _, s := range list {
		if s.ID == id {
			return s.Preview
		}
	}
	return "(not listed)"
}

// readSummary reads what loading the session and walking its history did,
// on every kind of file.
func TestSummaryMatchesLoadedSession(t *testing.T) {
	base, cwd := t.TempDir(), t.TempDir()
	var paths []string
	add := func(s *Session) {
		s.Close()
		paths = append(paths, s.Path)
	}
	raw := func(name, data string) {
		p := filepath.Join(t.TempDir(), name+".jsonl")
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}

	add(sessionWith(t, base, cwd, "", "plain", "answer"))
	add(sessionWith(t, base, cwd, "")) // header only
	s, _ := New(base, cwd)             // branches, a title set and cleared
	for _, x := range []string{"q1", "a1", "q2", "a2"} {
		_ = s.Append(msg(llm.RoleUser, x))
	}
	_, _ = s.Checkout(s.Tree()[1].Node.ID)
	_ = s.Append(msg(llm.RoleUser, "q2b"))
	_ = s.SetTitle("A title\nover two lines that goes on and on and on past seventy runes for sure")
	add(s)
	s, _ = New(base, cwd)
	_ = s.Append(msg(llm.RoleUser, "x"))
	_ = s.SetTitle("set")
	_ = s.SetTitle("") // cleared: the first prompt again
	add(s)
	s, _ = New(base, cwd) // media in blobs, compaction holding some, subagent records
	_ = s.Append(msg(llm.RoleUser, "look at the screenshots"))
	for i := range 3 {
		for _, m := range heavy(i) {
			_ = s.Append(m)
		}
	}
	if _, err := s.SaveSubagent("task-1", "", 0, json.RawMessage(`{"a":1}`), heavy(4)); err != nil {
		t.Fatal(err)
	}
	add(s)
	s, _ = New(base, cwd)
	_ = s.Append(msg(llm.RoleUser, "before"))
	_ = s.Compacted(append([]llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{
		{Type: llm.BlockImage, MediaType: "image/png", Data: strings.Repeat("B", 3000)},
		{Type: llm.BlockText, Text: "summary prompt"}}}}, heavy(5)...))
	_ = s.Append(msg(llm.RoleAssistant, "after"))
	add(s)
	s, _ = New(base, cwd) // an empty compaction ends the history
	_ = s.Append(msg(llm.RoleUser, "gone"))
	_ = s.Compacted(nil)
	add(s)
	s, _ = New(base, cwd) // tool results first, a blank prompt
	_ = s.Append(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "t", Content: "out"}}})
	_ = s.Append(msg(llm.RoleUser, "  \n "))
	_ = s.Append(msg(llm.RoleUser, "real"))
	add(s)
	s, _ = New(base, cwd)
	_ = s.Append(msg(llm.RoleUser, "start over"))
	_, _ = s.Checkout("")
	add(s)
	f, _ := NewFrom(base, cwd, []llm.Message{msg(llm.RoleUser, "forked")}, "20260101-000000-abcdef")
	add(f)

	raw("legacy", `{"type":"header","id":"old"}
{"type":"message","message":{"role":"user","content":[{"type":"text","text":"x"}]}}
{"type":"compaction","messages":[{"role":"user","content":[{"type":"text","text":"sum"}]}]}
{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"y"}]}}
`)
	compacted := `{"type":"header","id":"m","cwd":"/w","fork_of":"p"}
%s{"type":"compaction","id":"c1","ts":1,"messages":[{"role":"user","content":[{"type":"media_ref","name":"image","media_type":"image/png","data":"sha256:ab12"},{"type":"text","text":"after the image"}]}]}
{"type":"message","id":"m1","parent":"c1","ts":1,"message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}
`
	raw("missing-blob", fmt.Sprintf(compacted, ""))
	raw("with-blob", fmt.Sprintf(compacted, `{"type":"blob","sha":"ab12","data":"QUJD"}`+"\n"))
	raw("torn", `{"type":"header","id":"t","cwd":"/t"}
{"type":"message","id":"a","ts":1,"message":{"role":"user","content":[{"type":"text","text":"one"}]}}
{"type":"message","id":"b","parent":"a","ts":1,"mess
{"type":"message","id":"c","parent":"b","ts":1,"message":{"role":"user","content":[{"type":"text","text":"orphan"}]}}
{"type":"leaf","id":"nowhere"}
{"type":"title","title":5}
{"type":"blob","sha":"ff","data":"QUJD"
{"type":"message","id":"d","parent":"c","ts":1,"message":{"role":"assistant","content":[{"type":"text","text":"tail"}]}}
{"type":"message","id":"e","parent":"d","ts":1,"message":{"role":"assistant","content":[{"type":"te`)
	raw("garbage", "\x00\x01 not a session\n")
	raw("empty", "")

	for _, p := range paths {
		got, err := readSummary(p)
		if want := oldPreview(p); err != nil || got != want {
			t.Errorf("%s:\n got %+v (%v)\nwant %+v", filepath.Base(p), got, err, want)
		}
	}
}

// blobLine passes over blob entries as tego writes them, and leaves every
// other line to json.Unmarshal.
func TestBlobLine(t *testing.T) {
	line, _ := json.Marshal(entry{Type: "blob", SHA: "0a1b2c", Data: "iVBORw0KGgo+/=="})
	if sha, ok := blobLine(line); !ok || sha != "0a1b2c" {
		t.Fatalf("tego's blob line: %q %v", sha, ok)
	}
	rejected := []string{
		`{"type":"blob","sha":"0a1b2c","data":"iVBORw0KGgo`,                            // torn
		`{"type":"blob","sha":"0a1b2c","data":"iVBO{"type":"message","id":"a"}`,        // torn, then joined
		`{"type":"blob","sha":"0a1b2c","data":"iVBO"}{"type":"blob"}`,                  // joined
		"{\"type\":\"blob\",\"sha\":\"0a1b\",\"data\":\"iVBO\x00\x00\x00\x00\x00RK\"}", // zeros a crash left
		`{"type":"blob","sha":"0A1B","data":"iVBO"}`,                                   // not tego's sha
		`{"type":"blob","sha":"","data":"iVBO"}`,
		`{"type":"blob","sha":"0a1b","data":"a\"b"}`,
		`{"type":"blob","sha":"0a1b","data":"a\\b"}`,
		`{"type":"blob","sha":"0a1b","data":"iVBO","x":1}`,
		`{"type":"blob","sha":"0a1b"}`,
		`{"sha":"0a1b","type":"blob","data":"iVBO"}`,
		`{"type":"message","id":"a"}`,
	}
	for _, l := range rejected {
		if sha, ok := blobLine([]byte(l)); ok {
			t.Errorf("%s read as blob %q", l, sha)
		}
	}
	// Whatever it takes for a blob, json.Unmarshal reads as that blob.
	lines := append(rejected, string(line), `{"type":"blob","sha":"ff","data":"héllo <&> wörld"}`, `{"type":"blob","sha":"ff","data":""}`)
	for i := range 300 {
		data := []byte(strings.Repeat("QUJD", i%7))
		data = append(data, byte(i%128), byte(255-i%128))
		lines = append(lines, `{"type":"blob","sha":"ab","data":"`+string(data)+`"}`)
	}
	for _, l := range lines {
		sha, ok := blobLine([]byte(l))
		var e lightEntry
		err := json.Unmarshal([]byte(l), &e)
		if ok && (err != nil || e.Type != "blob" || e.SHA != sha) {
			t.Errorf("%q: blob %q, but json reads %+v %v", l, sha, e, err)
		}
	}
	// hasControl, eight bytes at a time, against a byte at a time.
	for n := range 20 {
		for at := range n {
			for _, c := range []byte{0, 1, 0x1f, 0x20, 0x21, 0x7f, 0x80, 0x9f, 0xa0, 0xff} {
				b := []byte(strings.Repeat("\xa0", n)) // every byte above 0x20, high bit set
				b[at] = c
				if got, want := hasControl(b), c < 0x20; got != want {
					t.Fatalf("hasControl(%q) = %v", b, got)
				}
				b = []byte(strings.Repeat(" ", n))
				b[at] = c
				if got, want := hasControl(b), c < 0x20; got != want {
					t.Fatalf("hasControl(%q) = %v", b, got)
				}
			}
		}
	}
}

// Latest is List's first session, read from the newest file on only until
// one has messages.
func TestLatestSkipsEmptyAndPicksNewest(t *testing.T) {
	base, cwd := t.TempDir(), t.TempDir()
	if p, err := Latest(base, cwd); p != "" || err != nil {
		t.Fatalf("no sessions: %q %v", p, err)
	}
	now := time.Now()
	var ss []*Session
	for i, x := range []string{"oldest", "middle", "newest"} {
		s := sessionWith(t, base, cwd, "", x)
		when := now.Add(time.Duration(i-5) * time.Minute)
		_ = os.Chtimes(s.Path, when, when)
		ss = append(ss, s)
	}
	fresh := sessionWith(t, base, cwd, "") // the one a tego just started has
	_ = os.Chtimes(fresh.Path, now, now)
	reads := recordReads(t)
	p, err := Latest(base, cwd)
	if err != nil || p != ss[2].Path {
		t.Fatalf("Latest = %q %v, want %s", p, err, ss[2].Path)
	}
	if got := reads(); !reflect.DeepEqual(got, sortedPaths(ss[2], fresh)) {
		t.Fatalf("Latest read %v", got)
	}
	if list, _ := List(base, cwd); list[0].Path != p {
		t.Fatalf("List's first is %s", list[0].Path)
	}
}

// A crash in the middle of a write leaves part of a line at the end of the
// file. Open cuts it off, so the lines appended next stay whole and
// chained.
func TestOpenMendsTornLastLine(t *testing.T) {
	base, cwd := t.TempDir(), t.TempDir()
	s := sessionWith(t, base, cwd, "", "1", "2")
	clean, _ := os.Stat(s.Path)
	f, _ := os.OpenFile(s.Path, os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.WriteString(`{"type":"message","id":"deadbeef","parent":"x","ts":1,"message":{"role":"user","content":[{"ty`)
	f.Close()

	r, hist, err := Open(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(hist); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Fatalf("history with a torn line %v", got)
	}
	if fi, _ := os.Stat(s.Path); fi.Size() != clean.Size() {
		t.Fatalf("torn line kept: %d bytes, %d before it", fi.Size(), clean.Size())
	}
	_ = r.Append(msg(llm.RoleUser, "3"))
	_ = r.Append(msg(llm.RoleAssistant, "4"))
	r.Close()
	r, hist, err = Open(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if got := texts(hist); !reflect.DeepEqual(got, []string{"1", "2", "3", "4"}) {
		t.Fatalf("history after the mend %v", got)
	}
	if list, _ := List(base, cwd); len(list) != 1 || list[0].Count != 4 {
		t.Fatalf("list %+v", list)
	}
}

// A last line that is whole but lacks its newline (written by hand) is
// kept, and ended; a file that is all one torn line is emptied.
func TestOpenKeepsWholeLastLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hand.jsonl")
	_ = os.WriteFile(path, []byte(`{"type":"header","id":"hand","cwd":"/h"}
{"type":"message","id":"a","ts":1,"message":{"role":"user","content":[{"type":"text","text":"1"}]}}
{"type":"title","title":"kept"}`), 0o600)
	r, hist, err := Open(path)
	if err != nil || len(hist) != 1 || r.Title != "kept" {
		t.Fatalf("open: %v %v %q", hist, err, r.Title)
	}
	_ = r.Append(msg(llm.RoleAssistant, "2"))
	r.Close()
	r, hist, _ = Open(path)
	r.Close()
	if got := texts(hist); !reflect.DeepEqual(got, []string{"1", "2"}) || r.Title != "kept" || r.Cwd != "/h" {
		t.Fatalf("reopened: %v title %q cwd %q", got, r.Title, r.Cwd)
	}

	torn := filepath.Join(dir, "torn.jsonl")
	_ = os.WriteFile(torn, []byte(`{"type":"header","id":"to`), 0o600)
	r, hist, err = Open(torn)
	if err != nil || len(hist) != 0 {
		t.Fatalf("open torn: %v %v", hist, err)
	}
	_ = r.Append(msg(llm.RoleUser, "fresh"))
	r.Close()
	if _, hist, _ = Open(torn); !reflect.DeepEqual(texts(hist), []string{"fresh"}) {
		t.Fatalf("after a torn header %v", texts(hist))
	}
}

// A write that failed part way is cut off; when that fails too, the next
// write mends the file first. Rename, which writes without Open, checks
// the end of the file as well.
func TestFailedWriteLeavesNoTornLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.jsonl")
	_ = os.WriteFile(path, []byte("{\"type\":\"header\"}\n"), 0o600)
	f, _ := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o600)
	n, _ := f.WriteString(`{"type":"message","id":`)
	if err := cutWrite(f, n); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "{\"type\":\"header\"}\n" {
		t.Fatalf("after the cut %q", data)
	}
	// Another process wrote after it: nothing is cut.
	n, _ = f.WriteString(`{"type":"mess`)
	g, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = g.WriteString("{}\n")
	g.Close()
	if err := cutWrite(f, n); err == nil {
		t.Fatal("cut what another process wrote after")
	}
	f.Close()

	base, cwd := t.TempDir(), t.TempDir()
	s := sessionWith(t, base, cwd, "", "1")
	r, _, err := Open(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = r.f.WriteString(`{"type":"message","id":"x","message":{"ro`) // a write cut short, not taken back
	r.torn = true
	if err := r.Append(msg(llm.RoleAssistant, "2")); err != nil {
		t.Fatal(err)
	}
	if err := r.Append(msg(llm.RoleUser, "3")); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if _, hist, _ := Open(s.Path); !reflect.DeepEqual(texts(hist), []string{"1", "2", "3"}) {
		t.Fatalf("after a failed write %v", texts(hist))
	}

	a, _ := os.OpenFile(s.Path, os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = a.WriteString(`{"type":"title","ti`)
	a.Close()
	if err := Rename(s.Path, "Renamed"); err != nil {
		t.Fatal(err)
	}
	if list, _ := List(base, cwd); len(list) != 1 || list[0].Preview != "Renamed" || list[0].Count != 3 {
		t.Fatalf("rename after a torn line %+v", list)
	}
}

// Lists, Find and Latest from several goroutines while a session is
// written (run with -race).
func TestListingConcurrently(t *testing.T) {
	base, cwd := t.TempDir(), t.TempDir()
	for i := range 12 {
		sessionWith(t, base, cwd, "", fmt.Sprint("session ", i), "answer")
	}
	live, err := New(base, cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	var wg sync.WaitGroup
	for g := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				switch g % 3 {
				case 0:
					if list, err := List(base, cwd); err != nil || len(list) < 12 {
						t.Errorf("list %d %v", len(list), err)
					}
				case 1:
					if _, err := ListAll(base); err != nil {
						t.Error(err)
					}
				default:
					if _, err := Latest(base, cwd); err != nil {
						t.Error(err)
					}
					_, _ = Find(base, cwd, live.ID)
				}
			}
		}()
	}
	for i := range 20 {
		_ = live.Append(msg(llm.RoleUser, fmt.Sprint("live ", i)))
	}
	wg.Wait()
	if p, err := Find(base, cwd, live.ID); err != nil || p != live.Path {
		t.Fatalf("Find live %q %v", p, err)
	}
	if list, _ := List(base, cwd); countOf(list, live.ID) != 20 {
		t.Fatalf("live count %d", countOf(list, live.ID))
	}
}
