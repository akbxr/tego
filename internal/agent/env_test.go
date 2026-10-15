package agent

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/session"
)

func envNotes(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if strings.HasPrefix(b.Text, envPrefix) {
				out = append(out, b.Text)
			}
		}
	}
	return out
}

func TestSystemPromptModes(t *testing.T) {
	dir := t.TempDir()
	for mode, want := range map[PromptMode][2]bool{ // {headless note, subagent note}
		Interactive:  {false, false},
		Headless:     {true, false},
		SubagentMode: {false, true},
	} {
		p := SystemPrompt(dir, nil, nil, mode)
		if got := [2]bool{strings.Contains(p, "# Non-interactive run"), strings.Contains(p, "# Subagent")}; got != want {
			t.Errorf("mode %d: notes %v, want %v", mode, got, want)
		}
	}
}

func TestSystemPromptIsStable(t *testing.T) {
	p := SystemPrompt(t.TempDir(), nil, nil, Interactive)
	if strings.Contains(p, "Date:") || strings.Contains(p, "branch") {
		t.Fatalf("volatile facts in the system prompt:\n%s", p)
	}
}

func TestEnvReportedWhenItChanges(t *testing.T) {
	env := "today is 2026-10-14 (Wednesday); git branch: main."
	a := &Agent{Env: func() string { return env }}
	runWith(t, a, assistant(text("one")))
	runWith(t, a, assistant(text("two"))) // unchanged: not repeated
	if n := envNotes(a.Messages); len(n) != 1 {
		t.Fatalf("notes %q", n)
	}
	if session.FirstPrompt(a.Messages, 50) != "do it" {
		t.Fatalf("first prompt %q", session.FirstPrompt(a.Messages, 50))
	}
	env = "today is 2026-10-15 (Thursday); git branch: feature."
	runWith(t, a, assistant(text("three")))
	if n := envNotes(a.Messages); len(n) != 2 || !strings.Contains(n[1], "feature") {
		t.Fatalf("notes after a change %q", n)
	}
	// A resumed session knows what it last reported.
	b := &Agent{Env: func() string { return env }}
	b.SetHistory(a.Messages)
	runWith(t, b, assistant(text("four")))
	if n := envNotes(b.Messages); len(n) != 2 {
		t.Fatalf("notes after resume %q", n)
	}
}

func TestGitBranchWithoutCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q", "-b", "feature-x").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if b := gitBranch(dir); b != "feature-x" {
		t.Fatalf("branch %q", b)
	}
	if n := EnvNote(dir); !strings.Contains(n, "git branch: feature-x") {
		t.Fatalf("note %q", n)
	}
	if gitBranch(t.TempDir()) != "" {
		t.Fatal("branch outside a repository")
	}
}
