package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
)

func boxUI(w int) (*termUI, *bytes.Buffer) {
	var buf bytes.Buffer
	u := &termUI{out: &buf, diag: &buf, atBOL: true, width: func() int { return w }, cwd: "/p", home: "/h"}
	return u, &buf
}

func checkFrame(t *testing.T, out string, w int) []string {
	t.Helper()
	rows := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for _, r := range rows {
		if got := runewidth.StringWidth(r); got != w {
			t.Fatalf("row %q is %d cells, want %d\n%s", r, got, w, out)
		}
	}
	if !strings.HasPrefix(rows[0], "╭─") || !strings.HasPrefix(rows[len(rows)-1], "╰") {
		t.Fatalf("not framed:\n%s", out)
	}
	return rows
}

func TestEditBox(t *testing.T) {
	u, buf := boxUI(40)
	in, _ := json.Marshal(map[string]string{"path": "/p/internal/a.go"})
	out := "Edited internal/a.go: 1 replacement\n 9\tfunc A() {\n-10\t\treturn 1\n+10\t\treturn 2 + some very long expression that will not fit\n" +
		"\nSyntax check (gofmt) found problems; fix them before moving on:\ninternal/a.go:10:3: expected ';'"
	u.OnToolCall("edit", in)
	if buf.Len() != 0 {
		t.Fatalf("edit title printed before the result: %q", buf.String())
	}
	u.OnToolResult("edit", in, out, false)
	rows := checkFrame(t, buf.String(), 40)
	text := buf.String()
	for _, want := range []string{"Edit: internal/a.go", "1 replacement", " 9 │ func A() {", "-10 │   return 1", "+10 │   return 2", "…", "Syntax check (gofmt)"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if strings.Contains(rows[0], "✗") {
		t.Fatal("successful edit marked as failed")
	}
}

func TestFailedWriteAndBashBoxes(t *testing.T) {
	u, buf := boxUI(36)
	in, _ := json.Marshal(map[string]string{"path": "/h/notes.txt", "content": "x"})
	u.OnToolResult("write", in, "notes.txt was not read in this session, so nothing was changed.\n1\told", true)
	rows := checkFrame(t, buf.String(), 36)
	if !strings.Contains(rows[0], "✗ Write: ~/notes.txt") {
		t.Fatalf("title = %q", rows[0])
	}

	buf.Reset()
	cmd, _ := json.Marshal(map[string]string{"command": "go test ./..."})
	u.OnToolCall("bash", cmd)
	// The box waits for the result so a failure can colour all of it.
	if buf.Len() != 0 {
		t.Fatalf("printed while running: %q", buf.String())
	}
	var lines []string
	for i := range 15 {
		lines = append(lines, "line "+string(rune('a'+i)))
	}
	u.OnToolResult("bash", cmd, strings.Join(lines, "\n")+"\nexit status 1", true)
	rows = checkFrame(t, buf.String(), 36)
	text := buf.String()
	if !strings.Contains(text, "lines above") || !strings.Contains(text, "exit status 1") || strings.Contains(text, "line a") {
		t.Fatalf("bash should show the tail:\n%s", text)
	}
	if !strings.Contains(rows[len(rows)-1], "✗ failed") {
		t.Fatalf("bottom = %q", rows[len(rows)-1])
	}
	if !strings.Contains(rows[0], "Bash: go test ./...") {
		t.Fatalf("title = %q", rows[0])
	}
	// A command that ran but exited non-zero is marked with its code.
	buf.Reset()
	u.OnToolCall("bash", cmd)
	u.OnToolResult("bash", cmd, "FAIL\n[exit code 2]", false)
	if rows := checkFrame(t, buf.String(), 36); !strings.Contains(rows[len(rows)-1], "✗ exit 2") {
		t.Fatalf("bottom = %q", rows[len(rows)-1])
	}
}

func TestFailedBashFrameIsRedAllRound(t *testing.T) {
	u, buf := boxUI(36)
	u.dc = palette{red: "<R>", dim: "<D>"}
	cmd, _ := json.Marshal(map[string]string{"command": "python3 x.py"})
	for _, c := range []struct {
		out    string
		isErr  bool
		border string
	}{
		{"Traceback\nKeyError: 'output'\n[exit code 1]", false, "<R>"},
		{"ok", false, "<D>"},
		{"aborted by user", true, "<R>"},
	} {
		buf.Reset()
		u.OnToolCall("bash", cmd)
		u.OnToolResult("bash", cmd, c.out, c.isErr)
		rows := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
		for _, r := range rows {
			if !strings.HasPrefix(r, c.border) { // top, every side and the bottom
				t.Fatalf("%q: row %q does not start with %s", c.out, r, c.border)
			}
			if strings.Contains(r, map[string]string{"<R>": "<D>", "<D>": "<R>"}[c.border]+"│") {
				t.Fatalf("%q: row %q has a side in the other colour", c.out, r)
			}
		}
	}
}

func TestRunningBashRow(t *testing.T) {
	u, buf := boxUI(60)
	cmd, _ := json.Marshal(map[string]string{"command": "cd ~/Code/projects/tego && go build -o /tmp/tego-test ./cmd/tego && echo built\nmore"})
	u.OnToolCall("bash", cmd)
	got, since, _ := u.runningBash()
	if !strings.HasPrefix(got, "cd ~/Code/projects/tego && go build") || !strings.HasSuffix(got, " …") || since.IsZero() || buf.Len() != 0 {
		t.Fatalf("running %q since %v, printed %q", got, since, buf.String())
	}
	for _, w := range []int{120, 60, 30} {
		box := u.runningBox(got, 12*time.Second, nil, w)
		if len(box) != 3 || !strings.HasPrefix(box[0], " ╭─ Bash: cd ~/Code") || !strings.Contains(box[1], "running · 12s") || !strings.HasPrefix(box[2], " ╰") {
			t.Fatalf("width %d: box %q", w, box)
		}
		for _, l := range box {
			// Margin plus the content width: one cell short of the terminal, like the finished box.
			if rw := runewidth.StringWidth(l); rw != w-1 {
				t.Fatalf("width %d: line is %d cells: %q", w, rw, l)
			}
		}
	}
	// The label animates: the bright spot moves between frames.
	c := palette{dim: "<d>", bold: "<b>", reset: "<r>"}
	if shimmer(c, "running", 2) == shimmer(c, "running", 3) {
		t.Fatal("shimmer does not move")
	}
	u.OnToolResult("bash", cmd, "built", false)
	if got, _, _ := u.runningBash(); got != "" {
		t.Fatalf("still running after the result: %q", got)
	}
	u.OnToolCall("bash", cmd)
	u.Finish() // a turn that ended without a result
	if got, _, _ := u.runningBash(); got != "" {
		t.Fatalf("still running after the turn: %q", got)
	}
}

func TestLastLines(t *testing.T) {
	out := "building\n\x1b[32mok\x1b[0m  pkg/a\t0.1s\n\nDownloading 10%\rDownloading 55%\rDownloading 99%\n" +
		"\x1b]8;;http://x\x07link\x1b]8;;\x07\npartial line"
	got := lastLines([]byte(out), 4)
	want := []string{"ok  pkg/a 0.1s", "Downloading 99%", "link", "partial line"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestRunningBoxShowsOutputTail(t *testing.T) {
	u, _ := boxUI(60)
	cmd, _ := json.Marshal(map[string]string{"command": "go test ./..."})
	u.OnToolCall("bash", cmd)
	for i := 1; i <= 8; i++ {
		u.OnToolOutput("bash", []byte(fmt.Sprintf("ok  pkg/%d\n", i)))
	}
	u.OnToolOutput("read", []byte("not bash\n")) // other tools are ignored
	got, since, tail := u.runningBash()
	if len(tail) != runningTailMax || tail[0] != "ok  pkg/4" || tail[len(tail)-1] != "ok  pkg/8" {
		t.Fatalf("tail %q", tail)
	}
	box := u.runningBox(got, time.Since(since), tail, 60)
	if len(box) != runningTailMax+3 || !strings.Contains(box[1], "ok  pkg/4") || !strings.Contains(box[len(box)-2], "running") {
		t.Fatalf("box %q", box)
	}
	for _, l := range box {
		if runewidth.StringWidth(l) != 59 {
			t.Fatalf("line %q is %d cells", l, runewidth.StringWidth(l))
		}
	}
	u.OnToolResult("bash", cmd, "done", false)
	u.OnToolOutput("bash", []byte("late\n")) // after the result: dropped
	if _, _, tail := u.runningBash(); len(tail) != 0 {
		t.Fatalf("output kept after the command: %q", tail)
	}
}

func TestBashBoxKeepsItsFrameWithControlBytes(t *testing.T) {
	u, buf := boxUI(40)
	cmd, _ := json.Marshal(map[string]string{"command": "fetch"})
	u.OnToolCall("bash", cmd)
	u.OnToolResult("bash", cmd, "get 10%\rget 100%\n\x1b[31mred\x1b[0m\tx\x07", false)
	rows := checkFrame(t, buf.String(), 40)
	if !strings.Contains(rows[1], "│ get 100%") || !strings.Contains(rows[2], "│ red x") {
		t.Fatalf("rows %q", rows)
	}
}
