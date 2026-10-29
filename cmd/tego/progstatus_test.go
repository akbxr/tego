package main

import (
	"encoding/base64"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// reports splits written bytes into OSC 7501 bodies.
func reports(t *testing.T, s string) []string {
	t.Helper()
	var out []string
	for _, part := range strings.SplitAfter(s, "\x1b\\") {
		if part == "" {
			continue
		}
		body, ok := strings.CutPrefix(part, "\x1b]7501;")
		if !ok || !strings.HasSuffix(body, "\x1b\\") {
			t.Fatalf("not an OSC 7501 report: %q", part)
		}
		out = append(out, strings.TrimSuffix(body, "\x1b\\"))
	}
	return out
}

// field returns key's value in a report body ("" when absent), decoding
// msg and title.
func field(t *testing.T, body, key string) string {
	t.Helper()
	for _, pair := range strings.Split(body, ":") {
		k, v, _ := strings.Cut(pair, "=")
		if k != key {
			continue
		}
		if key == "msg" || key == "title" {
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				t.Fatalf("%s=%q: %v", key, v, err)
			}
			return string(b)
		}
		return v
	}
	return ""
}

var specPair = regexp.MustCompile(`^[a-z]+=[A-Za-z0-9_.,+/=-]*$`)

func TestStatusEncodeFollowsSpec(t *testing.T) {
	r := statusRecord{state: "blocked", kind: "permission", title: "x", msg: "Allow bash:\n\x1b[31mrm\x1b[0m -rf\t\u202ebuild\u0085?\xff"}
	seq := r.encode("agents/t1")
	if !strings.HasPrefix(seq, "\x1b]7501;") || !strings.HasSuffix(seq, "\x1b\\") || len(seq) > 4096 {
		t.Fatalf("framing: %q", seq)
	}
	body := reports(t, seq)[0]
	for _, pair := range strings.Split(body, ":") {
		if !specPair.MatchString(pair) {
			t.Fatalf("pair %q outside the grammar", pair)
		}
	}
	if got := field(t, body, "msg"); got != "Allow bash: rm -rf build ?" {
		t.Fatalf("msg = %q", got)
	}
	for k, want := range map[string]string{"state": "blocked", "kind": "permission", "id": "agents/t1", "app": "tego"} {
		if got := field(t, body, k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
	// kind only on blocked; clear carries nothing but the id.
	if b := reports(t, statusRecord{state: "working", kind: "permission"}.encode(""))[0]; b != "state=working:app=tego" {
		t.Fatalf("working = %q", b)
	}
	if b := reports(t, statusRecord{state: "clear", msg: "x"}.encode("agents/a"))[0]; b != "state=clear:id=agents/a" {
		t.Fatalf("clear = %q", b)
	}
}

func TestStatusTextLimits(t *testing.T) {
	long := strings.Repeat("é", 400) // 800 bytes
	got := statusText(long, statusMsgMax)
	if len(got) > statusMsgMax || !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, "é") {
		t.Fatalf("cut to %d bytes: %q", len(got), got)
	}
	if got := statusText("  a\r\n\tb  ", 10); got != "a b" {
		t.Fatalf("whitespace: %q", got)
	}
	for in, want := range map[string]string{"code-reviewer": "code-reviewer", "Review src/x.rs!": "Review-src-x.rs", "日本": "agent", strings.Repeat("a", 50): strings.Repeat("a", 28)} {
		if got := statusSegment(in); got != want {
			t.Fatalf("segment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProgStatusReports(t *testing.T) {
	t.Setenv("TEGO_PROGRAM_STATUS", "")
	var mu sync.Mutex
	var out strings.Builder
	p := newProgStatus(func(s string) { mu.Lock(); out.WriteString(s); mu.Unlock() })
	var roots []string
	p.onRoot = func(r statusRecord) { roots = append(roots, r.state) }
	take := func() []string {
		mu.Lock()
		defer mu.Unlock()
		s := out.String()
		out.Reset()
		return reports(t, s)
	}

	p.set(statusRecord{state: "idle"})
	p.set(statusRecord{state: "idle"}) // unchanged: not written again
	if got := take(); len(got) != 1 || got[0] != "state=idle:app=tego" {
		t.Fatalf("idle: %q", got)
	}
	p.set(statusRecord{state: "working", msg: "fix the bug"})
	p.children(map[string]statusRecord{"agents/explore": {state: "working", title: "explore", msg: "grep foo"}})
	got := take()
	if len(got) != 2 || field(t, got[0], "msg") != "fix the bug" || field(t, got[1], "id") != "agents/explore" || field(t, got[1], "title") != "explore" {
		t.Fatalf("working: %q", got)
	}

	// Two questions at once: the latest shows, then the older one again; the
	// loop's working comes back when both are answered, even if it set the
	// root meanwhile.
	u1 := p.block("permission", "Allow bash: make?")
	u2 := p.block("question", "Pick one")
	p.set(statusRecord{state: "working", msg: "fix the bug"})
	u2()
	u2() // twice: no effect
	u1()
	got = take()
	if len(got) != 4 || field(t, got[0], "kind") != "permission" || field(t, got[1], "kind") != "question" ||
		field(t, got[2], "kind") != "permission" || field(t, got[3], "state") != "working" || field(t, got[3], "msg") != "fix the bug" {
		t.Fatalf("blocked: %q", got)
	}

	// A subagent that ends is cleared before the root's next report.
	p.children(nil)
	p.set(statusRecord{state: "done", msg: "Fixed"})
	got = take()
	if len(got) != 2 || got[0] != "state=clear:id=agents/explore" || field(t, got[1], "state") != "done" {
		t.Fatalf("done: %q", got)
	}
	if want := "idle working blocked blocked blocked working done"; strings.Join(roots, " ") != want {
		t.Fatalf("roots = %q, want %q", roots, want)
	}

	p.close()
	p.set(statusRecord{state: "working"}) // after close: nothing
	if got := take(); len(got) != 1 || got[0] != "state=clear" {
		t.Fatalf("close: %q", got)
	}

	t.Setenv("TEGO_PROGRAM_STATUS", "0")
	off := newProgStatus(func(string) { t.Fatal("written while off") })
	var offRoots int
	off.onRoot = func(statusRecord) { offRoots++ }
	off.set(statusRecord{state: "working"})
	off.close()
	if offRoots != 1 {
		t.Fatal("herdr must still hear the state when OSC 7501 is off")
	}

	var nilp *progStatus // -p into a pipe
	nilp.set(statusRecord{state: "working"})
	nilp.block("permission", "x")()
	nilp.finish(statusRecord{state: "done"})
}

func TestAnswerLine(t *testing.T) {
	if got := answerLine("\n\n## **Fixed** the bug\nmore"); got != "Fixed the bug" {
		t.Fatalf("got %q", got)
	}
}
