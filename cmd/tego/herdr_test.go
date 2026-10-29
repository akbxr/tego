package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHerdrReports(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$*\" >> "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_BIN_PATH", bin)
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(dir, "sock"))
	h := newHerdr()
	if h == nil {
		t.Fatal("no reporter inside herdr")
	}
	calls := func(n int) []string {
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if b, _ := os.ReadFile(log); strings.Count(string(b), "\n") >= n {
				return strings.Split(strings.TrimSpace(string(b)), "\n")
			}
		}
		b, _ := os.ReadFile(log)
		t.Fatalf("want %d calls, got %q", n, b)
		return nil
	}
	sess := herdrSession{id: "s1", path: "/x/s1.jsonl", resume: []string{"tego", "-r", "s1", "-m", "anthropic/claude-opus-5-5", "-t", "high"}}
	h.setSession(sess) // no state yet: nothing to send
	h.update("working", "")
	calls(1)
	h.update("blocked", "Allow bash: rm -rf build?")
	calls(2)
	h.update("idle", "")
	calls(3)
	h.update("idle", "") // unchanged: not sent again
	h.setSession(sess)
	h.close()
	got := calls(4)
	if len(got) != 4 {
		t.Fatalf("calls: %q", got)
	}
	want := []string{
		"pane report-agent w1:p1 --source tego --agent tego --state working --seq ",
		"pane report-agent w1:p1 --source tego --agent tego --state blocked --seq ",
		"pane report-agent w1:p1 --source tego --agent tego --state idle --seq ",
		"pane release-agent w1:p1 --source tego --agent tego --seq ",
	}
	var last int64
	for i, c := range got {
		if !strings.HasPrefix(c, want[i]) {
			t.Fatalf("call %d = %q", i, c)
		}
		seq, _ := strconv.ParseInt(strings.Fields(c[len(want[i]):])[0], 10, 64)
		if seq <= last {
			t.Fatalf("seq not increasing: %q", got)
		}
		last = seq
	}
	if strings.Contains(got[0], "--agent-session") {
		t.Fatalf("session sent while working: %q", got[0])
	}
	if !strings.HasSuffix(got[1], "--message Allow bash: rm -rf build?") {
		t.Fatalf("blocked report = %q", got[1])
	}
	if !strings.HasSuffix(got[2], "--agent-session-id s1 --agent-session-path /x/s1.jsonl -- tego -r s1 -m anthropic/claude-opus-5-5 -t high") {
		t.Fatalf("session report = %q", got[2])
	}

	t.Setenv("HERDR_ENV", "")
	if newHerdr() != nil {
		t.Fatal("reporter outside herdr")
	}
}
