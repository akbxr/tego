package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/mattn/go-runewidth"
)

// stream sends raw in small chunks, as a provider does.
func stream(u *termUI, id, name, raw string) {
	for len(raw) > 0 {
		n := min(7, len(raw))
		u.OnToolDelta(id, name, raw[:n])
		raw = raw[n:]
	}
}

// aged makes the call being written look older than the box's delay.
func aged(u *termUI) {
	u.runMu.Lock()
	u.writing.since = time.Now().Add(-3 * time.Second)
	u.runMu.Unlock()
}

func TestLiveWriteBox(t *testing.T) {
	u, buf := boxUI(80)
	content := strings.Repeat("filler line\n", 30) + "func main() {\n\tfmt.Println(\"hi\")\n"
	c, _ := json.Marshal(content)
	raw := `{"path":"/p/cmd/main.go","content":` + string(c[:len(c)-1]) // still being written: no closing quote
	stream(u, "w1", "write", raw)

	if box := u.writingBox(100); box != nil {
		t.Fatalf("box before the delay:\n%s", strings.Join(box, "\n"))
	}
	aged(u)
	box := u.writingBox(100)
	if box == nil {
		t.Fatal("no box after the delay")
	}
	all := strings.Join(box, "\n")
	for _, want := range []string{"Write: cmd/main.go", "func main() {", "fmt.Println", "writing", "32 lines"} {
		if !strings.Contains(all, want) {
			t.Fatalf("box lacks %q:\n%s", want, all)
		}
	}
	// No height known (no TUI): the minimum rows, the last of the content.
	if n := strings.Count(all, "filler"); n != liveWriteMinRows-2 {
		t.Fatalf("%d filler rows, want the last %d rows of the content:\n%s", n, liveWriteMinRows, all)
	}
	for _, l := range box { // the frame lines up with the finished box: margin + content width
		if w := runewidth.StringWidth(l); w != 99 {
			t.Fatalf("row is %d cells, want 99: %q", w, l)
		}
	}
	if buf.Len() != 0 {
		t.Fatalf("printed to the scrollback: %q", buf.String())
	}

	// The finished call replaces it.
	u.OnToolCall("write", json.RawMessage(`{"path":"/p/cmd/main.go","content":"x"}`))
	if u.writingBox(100) != nil {
		t.Fatal("box kept after the call completed")
	}
}

func TestLiveEditBoxAndEndings(t *testing.T) {
	u, _ := boxUI(80)
	stream(u, "e1", "edit", `{"path":"/p/a.go","edits":[{"old_string":"a","new_string":"one\ntwo"},{"old_string":"b","new_string":"three\nfou`)
	aged(u)
	all := strings.Join(u.writingBox(80), "\n")
	for _, want := range []string{"Edit: a.go", "three", "fou", "change 2"} {
		if !strings.Contains(all, want) {
			t.Fatalf("edit box lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "one") {
		t.Fatalf("shows the first change's text:\n%s", all)
	}

	// A response that ends without the call (cut off, error) drops the box.
	u.OnTurnEnd(&llm.Message{})
	if u.writingBox(80) != nil {
		t.Fatal("box kept after the response ended")
	}
	stream(u, "e2", "edit", `{"path":"/p/b.go","new_string":"x`)
	u.Finish()
	if u.writing != nil {
		t.Fatal("box kept after the turn ended")
	}
	// Other tools, and subagents' calls, never get one.
	stream(u, "r1", "read", `{"path":"/p/c.go"}`)
	if u.writing != nil {
		t.Fatal("a box for read")
	}
}

func TestLiveWriteRowsFollowTheScreen(t *testing.T) {
	for h, want := range map[int]int{0: 8, 10: 8, 30: 12, 45: 18, 100: 20} {
		if got := liveWriteRows(h); got != want {
			t.Errorf("height %d: %d rows, want %d", h, got, want)
		}
	}
	// A tall screen gets a tall box, sized once when the call starts.
	u, _ := boxUI(80)
	h := 50
	u.height = func() int { return h }
	c, _ := json.Marshal(strings.Repeat("row\n", 40))
	stream(u, "w1", "write", `{"path":"/p/a.txt","content":`+string(c[:len(c)-1]))
	aged(u)
	h = 10 // the box made the content area smaller: the size holds
	stream(u, "w1", "write", "")
	if n := strings.Count(strings.Join(u.writingBox(100), "\n"), "row"); n != 20 {
		t.Fatalf("%d rows shown, want 20", n)
	}
}
