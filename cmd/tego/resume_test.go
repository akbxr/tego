package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/session"
)

func newSessionIn(t *testing.T, base, cwd, text string) *session.Session {
	t.Helper()
	s, err := session.New(base, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: text}}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	return s
}

func TestOpenSessionFromAnotherProjectRestartsThere(t *testing.T) {
	base, here, there := t.TempDir(), t.TempDir(), t.TempDir()
	s := newSessionIn(t, base, there, "work over there")
	a := &app{cwd: here, ag: &agent.Agent{}, ui: &termUI{out: io.Discard, diag: io.Discard, atBOL: true}}
	a.openSession(s.Path)
	if a.restart == nil || a.restart.dir != there || !reflect.DeepEqual(a.restart.args, []string{"-r", s.Path}) {
		t.Fatalf("restart %+v", a.restart)
	}
	if sessionDir(s.Path) != there || !sameDir(there, there+string(filepath.Separator)) || sameDir(here, there) {
		t.Fatal("sessionDir/sameDir")
	}
}

// A fork or clone says which session it came from: by title when that one
// is listed, else by id.
func TestSessionItemsShowForkOf(t *testing.T) {
	base, here := t.TempDir(), t.TempDir()
	orig := newSessionIn(t, base, here, "build the parser")
	hist := []llm.Message{{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "build the parser"}}}}
	clone, err := session.NewFrom(base, here, hist, orig.ID)
	if err != nil {
		t.Fatal(err)
	}
	clone.Close()
	orphan, _ := session.NewFrom(base, here, hist, "20260101-000000-gone00")
	orphan.Close()
	list, err := session.List(base, here)
	if err != nil || len(list) != 3 {
		t.Fatalf("list %v %v", list, err)
	}
	a := &app{cwd: here, ag: &agent.Agent{}}
	items, _ := a.sessionItems(list, false, "")
	for i, s := range list {
		d := items[i].Detail
		switch s.ID {
		case orig.ID:
			if strings.Contains(d, "fork of") {
				t.Fatalf("original: %s", d)
			}
		case clone.ID:
			if s.ForkOf != orig.ID || !strings.Contains(d, "fork of “build the parser”") {
				t.Fatalf("clone: %s", d)
			}
		case orphan.ID:
			if !strings.Contains(d, "fork of 20260101-000000-gone00") {
				t.Fatalf("orphan: %s", d)
			}
		}
	}
}

func TestSessionItemsShowProjectAndCurrent(t *testing.T) {
	base, here, there := t.TempDir(), t.TempDir(), t.TempDir()
	cur := newSessionIn(t, base, here, "this one")
	other := newSessionIn(t, base, there, "that one")
	open, _, err := session.Open(cur.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	a := &app{cwd: here, ag: &agent.Agent{Session: open}}
	list, err := session.ListAll(base)
	if err != nil || len(list) != 2 {
		t.Fatalf("list %v %v", list, err)
	}
	items, sel := a.sessionItems(list, true, other.Path)
	if list[sel].Path != other.Path {
		t.Fatalf("cursor on %d", sel)
	}
	for i, it := range items {
		if !strings.Contains(it.Detail, shortHome(list[i].Cwd)) || strings.Contains(it.Detail, "current") != (list[i].Path == cur.Path) {
			t.Fatalf("item %d %+v", i, it)
		}
	}
	if items, _ := a.sessionItems(list, false, ""); strings.Contains(items[0].Detail, shortHome(list[0].Cwd)) {
		t.Fatalf("folder scope shows the folder: %+v", items[0])
	}
	if neighbour(list, 0) != list[1].Path || neighbour(list, 1) != list[0].Path || neighbour(list[:1], 0) != "" {
		t.Fatal("neighbour")
	}
	if home, _ := os.UserHomeDir(); shortHome(filepath.Join(home, "x")) != "~/x" {
		t.Fatal("shortHome")
	}
}

func TestRateUsesTheGenerationWindow(t *testing.T) {
	u := &termUI{}
	ok := llm.RequestStat{GenStart: 740 * time.Millisecond, Duration: 3020 * time.Millisecond, Usage: llm.Usage{Output: 131}}
	burst := llm.RequestStat{GenStart: 8780 * time.Millisecond, Duration: 8820 * time.Millisecond, Usage: llm.Usage{Output: 834}}
	u.streamed(400)
	u.requestDone(burst) // buffered answer: not counted, and the in-flight estimate ends
	if u.rate() != 0 {
		t.Fatalf("burst counted: %.0f tok/s", u.rate())
	}
	u.requestDone(ok)
	if r := u.rate(); r < 56 || r > 59 { // 131 tokens over 2.28 s
		t.Fatalf("rate %.1f", r)
	}
	u.resetRate()
	if u.rate() != 0 {
		t.Fatal("reset")
	}
}

func TestSortSessions(t *testing.T) {
	now := time.Now()
	list := []session.Info{
		{ID: "a", Preview: "beta", Size: 10, Modified: now.Add(-time.Hour)},
		{ID: "b", Preview: "Alpha", Size: 300, Modified: now.Add(-2 * time.Hour)},
		{ID: "c", Preview: "gamma", Size: 20, Modified: now},
	}
	ids := func() string {
		s := ""
		for _, x := range list {
			s += x.ID
		}
		return s
	}
	for o, want := range map[sessionOrder]string{sortRecent: "cab", sortSize: "bca", sortName: "bac"} {
		sortSessions(list, o)
		if ids() != want {
			t.Errorf("%v: %s, want %s", o, ids(), want)
		}
	}
}
