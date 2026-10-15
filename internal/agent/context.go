package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

type ContextFile struct {
	Path    string
	Content string
}

type Skill struct {
	Name        string
	Description string
	Path        string
}

type Command struct {
	Name        string
	Description string
	Body        string
	Path        string
}

const maxContextFileBytes = 64 * 1024

// LoadContextFiles collects global and project instruction files, outermost
// first so nearer files take precedence when read top to bottom.
func LoadContextFiles(home, cwd string) []ContextFile {
	var out []ContextFile
	seen := map[string]bool{}
	add := func(path string) bool {
		if seen[path] {
			return false
		}
		data, err := os.ReadFile(path)
		if err != nil || len(strings.TrimSpace(string(data))) == 0 {
			return false
		}
		seen[path] = true
		if len(data) > maxContextFileBytes {
			data = append(data[:maxContextFileBytes], "\n…(truncated)"...)
		}
		out = append(out, ContextFile{Path: path, Content: string(data)})
		return true
	}
	add(filepath.Join(home, "AGENTS.md"))

	userHome, _ := os.UserHomeDir()
	var dirs []string
	for d := cwd; ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if d == userHome || filepath.Dir(d) == d {
			break
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if d == userHome && d != cwd {
			continue // home-level files are global config, not project context
		}
		if !add(filepath.Join(d, "AGENTS.md")) {
			add(filepath.Join(d, "CLAUDE.md"))
		}
		add(filepath.Join(d, ".tego", "AGENTS.md"))
	}
	return out
}

// projectConfigDirs are the per-project folders whose skills/ and commands/
// are loaded, lowest precedence first (.tego wins).
var projectConfigDirs = []string{".agents", ".claude", ".tego"}

// projectDirs lists <dir>/<cfg>/<sub> for cwd and its parents up to (not
// including) the home directory, outermost first so nearer folders win.
func projectDirs(cwd, sub string) []string {
	userHome, _ := os.UserHomeDir()
	var chain []string
	for d := cwd; d != userHome; d = filepath.Dir(d) {
		chain = append(chain, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	var out []string
	for i := len(chain) - 1; i >= 0; i-- {
		for _, c := range projectConfigDirs {
			out = append(out, filepath.Join(chain[i], c, sub))
		}
	}
	return out
}

func skillDirs(home, cwd string) []string {
	userHome, _ := os.UserHomeDir()
	dirs := []string{
		filepath.Join(userHome, ".agents", "skills"),
		filepath.Join(userHome, ".claude", "skills"),
		filepath.Join(home, "skills"),
	}
	return append(dirs, projectDirs(cwd, "skills")...)
}

// LoadSkills finds <dir>/<name>/SKILL.md; later dirs override earlier ones by name.
func LoadSkills(home, cwd string) []Skill {
	byName := map[string]Skill{}
	for _, dir := range skillDirs(home, cwd) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			path := filepath.Join(dir, e.Name(), "SKILL.md")
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			fm, _ := splitFrontmatter(string(data))
			name := fm["name"]
			if name == "" {
				name = e.Name()
			}
			desc := fm["description"]
			if r := []rune(desc); len(r) > 240 {
				desc = string(r[:240]) + "…"
			}
			byName[name] = Skill{Name: name, Description: desc, Path: path}
		}
	}
	out := make([]Skill, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LoadCommands finds markdown prompt templates usable as /name.
func LoadCommands(home, cwd string) map[string]Command {
	userHome, _ := os.UserHomeDir()
	dirs := []string{
		filepath.Join(userHome, ".claude", "commands"),
		filepath.Join(userHome, ".agents", "commands"),
		filepath.Join(home, "commands"),
	}
	dirs = append(dirs, projectDirs(cwd, "commands")...)
	out := map[string]Command{}
	for _, dir := range dirs {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			fm, body := splitFrontmatter(string(data))
			name := strings.TrimSuffix(e.Name(), ".md")
			desc := fm["description"]
			if desc == "" {
				desc = firstLine(body)
			}
			out[name] = Command{Name: name, Description: desc, Body: body, Path: path}
		}
	}
	return out
}

// Expand fills $ARGUMENTS / $1..$9, or appends args when no placeholder exists.
func (c Command) Expand(args string) string {
	body := c.Body
	fields := strings.Fields(args)
	used := strings.Contains(body, "$ARGUMENTS")
	body = strings.ReplaceAll(body, "$ARGUMENTS", args)
	for i := 9; i >= 1; i-- {
		ph := fmt.Sprintf("$%d", i)
		if strings.Contains(body, ph) {
			used = true
			v := ""
			if i <= len(fields) {
				v = fields[i-1]
			}
			body = strings.ReplaceAll(body, ph, v)
		}
	}
	if !used && strings.TrimSpace(args) != "" {
		body = strings.TrimRight(body, "\n") + "\n\n" + args
	}
	return body
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(l, "# "))
		if l != "" {
			if r := []rune(l); len(r) > 80 {
				l = string(r[:80]) + "…"
			}
			return l
		}
	}
	return ""
}

// splitFrontmatter parses a leading `---` YAML block of simple key: value lines.
func splitFrontmatter(s string) (map[string]string, string) {
	fm := map[string]string{}
	s = strings.TrimPrefix(s, "\ufeff")
	if !strings.HasPrefix(s, "---") {
		return fm, s
	}
	rest := s[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, s
	}
	lines := strings.Split(rest[:end], "\n")
	body := strings.TrimLeft(rest[end+4:], "-\r\n")
	for i := 0; i < len(lines); i++ {
		k, v, ok := strings.Cut(lines[i], ":")
		if !ok || strings.HasPrefix(lines[i], " ") {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if v == ">" || v == "|" || v == ">-" || v == "|-" {
			var parts []string
			for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || strings.TrimSpace(lines[i+1]) == "") {
				i++
				parts = append(parts, strings.TrimSpace(lines[i]))
			}
			v = strings.TrimSpace(strings.Join(parts, " "))
		}
		fm[k] = strings.Trim(v, `"'`)
	}
	return fm, body
}

const basePrompt = `You are tego, an expert coding agent working in the user's terminal. You complete software engineering tasks by searching, reading and editing files and running commands in the user's workspace.

# How to work
- Investigate before changing: read the relevant code, follow existing conventions, reuse existing patterns. Never guess file contents.
- Edit with the edit tool (exact old_string -> new_string, include enough context to be unique). Use write only for new files or full rewrites.
- Prefer grep/glob/read over shell equivalents (grep, find, cat, ls).
- Verify your work: build, run the relevant tests, or execute the changed path. Report only what you actually observed; never fabricate output or results.
- Keep going until the task is fully done. Do not stop at a plan unless asked. Ask the user only when a decision genuinely needs them.
- Destructive or irreversible actions (deleting files you did not create, git reset --hard, force-push, dropping data) need explicit user approval.
- For independent, parallelizable work (separate areas of a codebase, broad research), delegate with the task tool; give each subagent a complete, self-contained prompt.
- Multi-step work (4+ distinct steps, or several items from the user): track it with the todo tool. Update the list in the same response as your next tool call, never as a turn of its own. Long explorations with many intermediate reads: wrap them in checkpoint … rewind so only the report stays in context.
- Facts that may have changed since your training (versions, APIs, news): use web_search, then read the relevant URLs.
- Style: concise, direct, technical. Reference code as path:line. No filler.`

const subagentNote = `

# Subagent
You are a subagent executing one delegated task; you cannot talk to the user. Work autonomously to completion. Your final message is returned verbatim to the parent agent as your report: state concrete findings, file paths, what you changed, and how you verified it.`

// headlessNote is for one-shot runs (tego -p): a question ends the run with
// nothing done. In a VS Code benchmark, Sonnet stopped this way on 2 of 3
// refactors at a naming choice it had already picked a recommendation for,
// where oh-my-pi's runs went on with that choice.
const headlessNote = `

# Non-interactive run
This is a one-shot run: nobody will read a question or answer it before the run ends. When a choice is needed, pick the option you would recommend, carry it out, and state the assumption in your final answer. The rule on destructive or irreversible actions still holds: skip those and say what you would have done.`

// PromptMode is who the agent is talking to.
type PromptMode int

const (
	Interactive  PromptMode = iota // a user at the terminal
	Headless                       // tego -p: nobody to answer questions
	SubagentMode                   // a task subagent reporting to its parent
)

// SystemPrompt assembles the full system prompt.
func SystemPrompt(cwd string, files []ContextFile, skills []Skill, mode PromptMode) string {
	var b strings.Builder
	b.WriteString(basePrompt)
	switch mode {
	case SubagentMode:
		b.WriteString(subagentNote)
	case Headless:
		b.WriteString(headlessNote)
	}
	b.WriteString("\n\n# Environment\n")
	fmt.Fprintf(&b, "- Working directory: %s\n", cwd)
	fmt.Fprintf(&b, "- Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	// The date and git branch change during a session's life (a resume on
	// another day, a checkout). Here they would change the cached prefix and
	// invalidate every thinking block after it, so they are reported in
	// the conversation instead (EnvNote).
	if len(files) > 0 {
		b.WriteString("\n# Project instructions\nThe user's instruction files. Follow them; nearer (later) files take precedence.\n")
		for _, f := range files {
			fmt.Fprintf(&b, "\n<file path=%q>\n%s\n</file>\n", f.Path, strings.TrimSpace(f.Content))
		}
	}
	if len(skills) > 0 {
		b.WriteString("\n# Skills\nSkills are instruction files for specialised tasks. When a task matches a skill, read its SKILL.md with the read tool before acting.\n")
		for _, s := range skills {
			fmt.Fprintf(&b, "- %s: %s (%s)\n", s.Name, s.Description, s.Path)
		}
	}
	return b.String()
}

// envPrefix starts the reminder that reports the date and git branch.
const envPrefix = "<system-reminder>Environment: "

// EnvNote is the date and git branch of cwd, for Agent.Env.
func EnvNote(cwd string) string {
	note := "today is " + time.Now().Format("2006-01-02 (Monday)")
	if b := gitBranch(cwd); b != "" {
		note += "; git branch: " + b
	}
	return note + "."
}

// lastEnvNote is the environment last reported in msgs ("" if none).
func lastEnvNote(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != llm.RoleUser {
			continue
		}
		for _, b := range msgs[i].Blocks {
			if note, ok := strings.CutPrefix(b.Text, envPrefix); ok && b.Type == llm.BlockText {
				return strings.TrimSuffix(note, "</system-reminder>")
			}
		}
	}
	return ""
}

// gitBranch is cwd's current branch: symbolic-ref also works in a repository
// without commits yet; a detached HEAD is named by its commit.
func gitBranch(cwd string) string {
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	if b := run("symbolic-ref", "--short", "-q", "HEAD"); b != "" {
		return b
	}
	if c := run("rev-parse", "--short", "HEAD"); c != "" {
		return "detached at " + c
	}
	return ""
}
