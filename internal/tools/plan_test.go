package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPlanBash(t *testing.T) {
	for cmd, allowed := range map[string]bool{
		// Reading and investigating.
		"ls -la internal":                            true,
		"cat go.mod && head -50 README.md":           true,
		"grep -rn 'func Run' internal | head -20":    true,
		"rg -n TODO --type go":                       true,
		"find . -name '*.go' -newer go.mod | wc -l":  true,
		"git status && git log --oneline -10":        true,
		"git diff HEAD~1 -- internal/agent":          true,
		"git -C repo show HEAD:README.md":            true,
		"git branch -a":                              true,
		"git config --get remote.origin.url":         true,
		"git stash list":                             true,
		"go test ./internal/... 2>&1 | tail -20":     true,
		"go vet ./... >/dev/null && echo ok":         true,
		"cd internal && go list ./...":               true,
		"sed -n '10,40p' main.go":                    true,
		"python3 -m pytest -q tests":                 true,
		"npm test":                                   true,
		"npm run test:unit":                          true,
		"curl -s localhost:8080/health | jq .status": true,
		"ps aux | grep tego":                         true,
		// Changing things.
		"rm -rf build":                             false,
		"echo hi > notes.md":                       false,
		"echo hi > \"notes.md\"":                   false,
		"go test ./... | tee out.txt":              false,
		"sed -i 's/a/b/' main.go":                  false,
		"mkdir newdir":                             false,
		"git commit -am wip":                       false,
		"git checkout main":                        false,
		"git branch -d old":                        false,
		"git stash":                                false,
		"git config user.name me":                  false,
		"go build -o bin/tego ./cmd/tego":          false,
		"go mod tidy":                              false,
		"npm install":                              false,
		"npm run build":                            false,
		"python3 script.py":                        false,
		"python3 -c 'import os; os.remove(\"x\")'": false,
		"find . -name '*.tmp' -delete":             false,
		"find . -exec rm {} +":                     false,
		"curl -o out.html https://example.com":     false,
		"curl -X POST localhost:8080/reset":        false,
		"ls | xargs rm":                            false,
		"bash -c 'rm x'":                           false,
		"echo \"$(rm -rf x)\"":                     false,
		"make":                                     false,
		"touch a.go":                               false,
		"env FOO=1 rm a":                           false,
	} {
		why := planBashBlocked(cmd)
		if (why == "") != allowed {
			t.Errorf("%q: allowed=%v, reason %q", cmd, why == "", why)
		}
	}
}

func TestPlanBlocked(t *testing.T) {
	in := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	if PlanBlocked("bash", in(map[string]any{"command": "ls"})) != "" {
		t.Fatal("ls blocked")
	}
	if PlanBlocked("bash", in(map[string]any{"command": "ls", "background": true})) == "" {
		t.Fatal("background allowed")
	}
	if PlanBlocked("bash_output", in(map[string]any{"id": "bg1", "kill": true})) != "" {
		t.Fatal("bash_output blocked")
	}
	if PlanBlocked("edit", in(map[string]any{"path": "a"})) == "" || PlanBlocked("write", nil) == "" {
		t.Fatal("edit/write allowed")
	}
}

func TestCommandPrefixes(t *testing.T) {
	for _, c := range []struct {
		cmd  string
		want string // "" = no rule may cover it
	}{
		{"go test ./...", "go test"},
		{"go test ./... && git status", "go test|git status"},
		{"npm run build 2>/dev/null | tail -5", "npm run|tail"},
		{"cd web && make build", "cd|make build"},
		{"git -C repo push origin", "git push"},
		{"touch x", "touch"},
		{"rm tmp.txt", "rm"},
		{"env FOO=1 go vet ./...", "go vet"},
		{"python3 -m pytest -q", "python3"},
		{"git log -p", "git log"},
		{"cp -r a b", "cp"},
		// What a rule must never cover.
		{"sudo rm -rf build", ""},
		{"bash -c 'rm -rf x'", ""},
		{"sh script.sh", ""},
		{"find . -name x | xargs rm", ""},
		{"echo $(rm -rf x)", ""},
		{"echo `rm x`", ""},
		{"go test > out.txt", ""},
		{`echo hi >"out.txt"`, ""},
		{"python3 -c 'import os'", ""},
		{"node -e 'x'", ""},
		{"awk '{print}' f", ""},
		{"eval rm x", ""},
	} {
		got, ok := CommandPrefixes(c.cmd)
		if s := strings.Join(got, "|"); (c.want == "") == ok || ok && s != c.want {
			t.Errorf("%q: %q ok=%v, want %q", c.cmd, s, ok, c.want)
		}
	}
}
