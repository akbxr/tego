package peer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/session"
)

type fakeHandler struct {
	mu   sync.Mutex
	open []Open
	got  []Message
}

func (f *fakeHandler) Open() []Open { return f.open }
func (f *fakeHandler) Deliver(m Message) Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, m)
	return Outcome{Status: "steered"}
}

// A listening process is listed and gets messages; one that is gone is
// removed from the registry.
func TestRegistry(t *testing.T) {
	base := t.TempDir()
	h := &fakeHandler{open: []Open{{ID: "20261107-100000-aaaaaa", Title: "video", Busy: true}}}
	hub, err := Listen(base, h)
	if err != nil {
		t.Fatal(err)
	}
	// A process that died left its record.
	dead := filepath.Join(liveDir(base), "999999.json")
	data, _ := json.Marshal(record{PID: 999999, Socket: filepath.Join(base, "x.sock")})
	if err := os.WriteFile(dead, data, 0o600); err != nil {
		t.Fatal(err)
	}
	open := List(context.Background(), base)
	if len(open) != 1 || open[0].ID != "20261107-100000-aaaaaa" || !open[0].Busy || open[0].PID != os.Getpid() {
		t.Fatalf("open %+v", open)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatal("the dead process's record is still there")
	}
	out, err := Send(context.Background(), base, Message{To: "20261107-100000-aaaaaa", From: "me", Text: "hi", Hops: 1})
	if err != nil || out.Status != "steered" || len(h.got) != 1 || h.got[0].Text != "hi" {
		t.Fatalf("send %+v %v %+v", out, err, h.got)
	}
	if _, err := Send(context.Background(), base, Message{To: "nobody", Text: "hi"}); err != ErrNotOpen {
		t.Fatalf("to nobody: %v", err)
	}
	hub.Close()
	if len(List(context.Background(), base)) != 0 {
		t.Fatal("listed after Close")
	}
}

func TestSendLimits(t *testing.T) {
	base := t.TempDir()
	hub, err := Listen(base, &fakeHandler{open: []Open{{ID: "s2"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	tool := &SendTool{Base: base, Self: func() Self { return Self{ID: "s1", Hops: 2} }}
	for i := 0; i < sendsPerMinute; i++ {
		if _, err := tool.Run(context.Background(), []byte(`{"id":"s2","text":"x"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tool.Run(context.Background(), []byte(`{"id":"s2","text":"x"}`)); err == nil || !strings.Contains(err.Error(), "a minute") {
		t.Fatalf("over the rate: %v", err)
	}
	if _, err := Send(context.Background(), base, Message{To: "s2", Text: strings.Repeat("x", MaxText+1)}); err == nil {
		t.Fatal("an oversized message went")
	}
}

func TestPrompt(t *testing.T) {
	p := Message{From: "a1", FromTitle: "render", Text: "done", CanReply: true}.Prompt()
	if !strings.HasPrefix(p, `<session-message from="a1" title="render">`+"\ndone\n</session-message>") || !strings.Contains(p, "not the user") ||
		!strings.Contains(p, `session_send to id "a1"`) {
		t.Fatalf("%s", p)
	}
	if p := (Message{From: "a1", Text: "x"}).Prompt(); !strings.Contains(p, "can't take an answer") {
		t.Fatalf("%s", p)
	}
}

// list, search and read over real session files: newest first, media
// left out, what came before a compaction still readable.
func TestSessionsTool(t *testing.T) {
	base, cwd, other := t.TempDir(), t.TempDir(), t.TempDir()
	user := func(text string, extra ...llm.Block) llm.Message {
		return llm.Message{Role: llm.RoleUser, Blocks: append([]llm.Block{{Type: llm.BlockText, Text: text}}, extra...)}
	}
	say := func(text string) llm.Message {
		return llm.Message{Role: llm.RoleAssistant, Model: "m1", Blocks: []llm.Block{{Type: llm.BlockText, Text: text}}}
	}
	img := llm.Block{Type: llm.BlockImage, MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(make([]byte, 4096))}

	old, err := session.New(base, other)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []llm.Message{user("plan the blender intro"), say("we chose a low-poly forest for the intro")} {
		if err := old.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	cur, err := session.New(base, cwd)
	if err != nil {
		t.Fatal(err)
	}
	calls := llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"ffmpeg -i a.mp4"}`)}}}
	results := llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "c1", Content: "encoded 3 frames"}}}
	for _, m := range []llm.Message{user("render the clip", img), calls, results, say("rendered it")} {
		if err := cur.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := cur.Compacted([]llm.Message{user("summary: the clip was rendered")}); err != nil {
		t.Fatal(err)
	}
	if err := cur.Append(user("now add music")); err != nil {
		t.Fatal(err)
	}
	cur.SetTitle("Clip render")
	defer cur.Close()

	tool := &SessionsTool{Base: base, Cwd: cwd, Self: func() Self { return Self{ID: cur.ID} }}
	run := func(in string) string {
		t.Helper()
		out, err := tool.Run(context.Background(), json.RawMessage(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		return out
	}
	if out := run(`{"action":"list"}`); !strings.Contains(out, cur.ID) || !strings.Contains(out, "this session") || strings.Contains(out, old.ID) {
		t.Fatalf("list this folder:\n%s", out)
	}
	if out := run(`{"action":"list","folder":"all"}`); strings.Index(out, cur.ID) > strings.Index(out, old.ID) || !strings.Contains(out, "plan the blender intro") {
		t.Fatalf("list all, newest first:\n%s", out)
	}
	if out := run(`{"action":"search","query":"low-?poly"}`); !strings.Contains(out, old.ID) || !strings.Contains(out, "assistant: we chose a low-poly forest") {
		t.Fatalf("search:\n%s", out)
	}
	if out := run(`{"action":"search","query":"ffmpeg"}`); !strings.HasPrefix(out, "No match") {
		t.Fatalf("searched its own session:\n%s", out)
	}
	elsewhere := &SessionsTool{Base: base, Cwd: t.TempDir()}
	if out, _ := elsewhere.Run(context.Background(), json.RawMessage(`{"action":"search","query":"ffmpeg"}`)); !strings.Contains(out, "call bash") {
		t.Fatalf("search calls:\n%s", out)
	}
	if out := run(`{"action":"search","query":"iVBOR|AAAAAAAA"}`); !strings.HasPrefix(out, "No match") {
		t.Fatalf("media searched:\n%s", out)
	}
	out := run(`{"action":"read","id":"` + cur.ID[:17] + `"}`)
	for _, want := range []string{"Clip render", "(this is the session you are in)", "render the clip", "[image image/png]", "→ bash ffmpeg -i a.mp4",
		"← encoded 3 frames", "rendered it", "compaction: the history above was summarised", "summary: the clip was rendered", "now add music"} {
		if !strings.Contains(out, want) {
			t.Fatalf("read lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "AAAAAAAA") {
		t.Fatal("read shows media data")
	}
	if out := run(`{"action":"read","id":"` + cur.ID + `","offset":2,"limit":1}`); !strings.Contains(out, "Messages 2 to 2 of 6; earlier: offset 1; later: offset 3") || strings.Contains(out, "render the clip") {
		t.Fatalf("paged:\n%s", out)
	}
	// This folder's sessions first; from elsewhere a prefix of both is ambiguous.
	if _, err := elsewhere.Run(context.Background(), json.RawMessage(`{"action":"read","id":"2"}`)); err == nil || !strings.Contains(err.Error(), "matches 2 sessions") {
		t.Fatalf("ambiguous: %v", err)
	}
}

func TestLiterals(t *testing.T) {
	for _, c := range []struct {
		query string
		words []string
	}{
		{"low-poly", []string{"low-poly"}},
		{"Blender|Remotion", []string{"blender", "remotion"}},
		{"low-?poly", nil}, {"a<b", nil}, {"ünlü", nil}, {"a||b", nil},
	} {
		var got []string
		for _, w := range literals(c.query) {
			got = append(got, string(w))
		}
		if strings.Join(got, ",") != strings.Join(c.words, ",") {
			t.Fatalf("%q: %q", c.query, got)
		}
	}
	for _, c := range []struct {
		hay, needle string
		want        bool
	}{{"the Low-Poly forest", "low-poly", true}, {"LOW", "low", true}, {"lo", "low", false}, {"xLxLow", "low", true}, {"", "a", false}} {
		if containsFold([]byte(c.hay), []byte(c.needle)) != c.want {
			t.Fatalf("%q in %q", c.needle, c.hay)
		}
	}
}
