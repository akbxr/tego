package agent

import (
	"os"
	"path/filepath"
	"testing"
)

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
