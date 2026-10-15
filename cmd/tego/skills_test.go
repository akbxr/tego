package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/agent"
)

func TestSkillRefs(t *testing.T) {
	got := skillRefs("/skill:caveman fix it with /skill:frontend-design, not path/skill:x or /skill:caveman again")
	if strings.Join(got, ",") != "caveman,frontend-design" {
		t.Fatalf("skillRefs = %v", got)
	}
}

func TestSkillSuggestions(t *testing.T) {
	dir := t.TempDir()
	hist := filepath.Join(dir, "history")
	// History is one JSON string per line; the latest use ranks first.
	_ = os.WriteFile(hist, []byte(`"/skill:caveman hi"`+"\n"+`"make it nice /skill:frontend-design"`+"\n"), 0o600)
	a := &app{
		skills: []agent.Skill{
			{Name: "agentcash", Description: "pay"},
			{Name: "caveman"},
			{Name: "frontend-design", Description: "visual design"},
		},
		skillRecent: loadSkillRecency(hist),
	}
	labels := func(line string) string {
		var out []string
		for _, s := range a.suggest(line) {
			out = append(out, s.Label+"="+s.Value)
		}
		return strings.Join(out, " ")
	}
	if got := labels("/skill:"); got != "skill:frontend-design=/skill:frontend-design skill:caveman=/skill:caveman skill:agentcash=/skill:agentcash" {
		t.Fatalf("/skill: → %s", got)
	}
	// A bare name after "/" finds the skill too.
	if got := labels("/front"); got != "skill:frontend-design=/skill:frontend-design" {
		t.Fatalf("/front → %s", got)
	}
	// Mid-prompt: completes the token and keeps the text before it.
	if got := labels("redo the page /skill:cav"); got != "skill:caveman=redo the page /skill:caveman " {
		t.Fatalf("mid-line → %s", got)
	}
	if s := a.suggest("redo the page /skill:cav"); s[0].Final {
		t.Fatal("a mid-line completion must not submit")
	}
	// On a later line, and with just "/sk" typed.
	if got := labels("redo it\nthen /sk"); got != "skill:frontend-design=redo it\nthen /skill:frontend-design  skill:caveman=redo it\nthen /skill:caveman  skill:agentcash=redo it\nthen /skill:agentcash " {
		t.Fatalf("later line → %q", got)
	}
	// Paths are not mistaken for skills.
	if got := labels("open /src"); got != "" {
		t.Fatalf("/src → %q", got)
	}
	// Using a skill moves it to the front.
	a.skillRecent.touch("/skill:agentcash")
	if got := a.suggest("/skill:")[0].Label; got != "skill:agentcash" {
		t.Fatalf("after use, first = %s", got)
	}
}
