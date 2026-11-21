package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A subagent's prompt doesn't tell it to use tools it doesn't have (task,
// todo, checkpoint/rewind; web_search is the task tool's to tell, when it
// gives it), or to ask the user it can't talk to. The others' keep them.
func TestSubagentPromptOnlyNamesItsTools(t *testing.T) {
	sub := SystemPrompt(t.TempDir(), nil, nil, SubagentMode)
	for _, s := range []string{"task tool", "todo tool", "checkpoint", "rewind", "web_search", "Ask the user"} {
		if strings.Contains(sub, s) {
			t.Errorf("subagent prompt names %q", s)
		}
	}
	for _, s := range []string{"# Subagent", "Verify your work", "Destructive or irreversible", "Style:"} {
		if !strings.Contains(sub, s) {
			t.Errorf("subagent prompt lacks %q", s)
		}
	}
	for _, mode := range []PromptMode{Interactive, Headless} {
		p := SystemPrompt(t.TempDir(), nil, nil, mode)
		for _, s := range []string{"task tool", "todo tool", "checkpoint", "web_search", "Ask the user only when", "Destructive or irreversible"} {
			if !strings.Contains(p, s) {
				t.Errorf("mode %d: prompt lacks %q", mode, s)
			}
		}
		if strings.Contains(p, "# Subagent") {
			t.Errorf("mode %d: subagent note", mode)
		}
	}
}

func writeSkill(t *testing.T, dir, name, desc string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + desc + "\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(p, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSkillsFromAgentsDirAndParents(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "pkg", "deep")
	_ = os.MkdirAll(sub, 0o755)
	writeSkill(t, filepath.Join(root, ".agents", "skills"), "zz-repo-skill", "from repo root .agents")
	writeSkill(t, filepath.Join(sub, ".agents", "skills"), "zz-local-skill", "from cwd .agents")
	// Same name nearer and in a higher-precedence folder wins.
	writeSkill(t, filepath.Join(root, ".agents", "skills"), "zz-shared", "outer")
	writeSkill(t, filepath.Join(sub, ".tego", "skills"), "zz-shared", "inner tego")

	got := map[string]string{}
	for _, s := range LoadSkills(t.TempDir(), sub) {
		got[s.Name] = s.Description
	}
	for name, want := range map[string]string{
		"zz-repo-skill":  "from repo root .agents",
		"zz-local-skill": "from cwd .agents",
		"zz-shared":      "inner tego",
	} {
		if got[name] != want {
			t.Errorf("skill %s = %q, want %q", name, got[name], want)
		}
	}
}
