package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// AgentDef is a subagent defined in a markdown file, in Claude Code's
// format: agents/<name>.md with name, description, tools and model in its
// frontmatter and its instructions as the body. The task tool runs a task
// as one with "agent": "<name>".
type AgentDef struct {
	Name, Description, Path string
	Prompt                  string   // the body: what the agent is and does
	Tools                   []string // tool names it may use; nil: all of a subagent's
	Disallowed              []string // tool names it may not use (applied after Tools)
	Model                   string   // "", "inherit", an alias (sonnet, opus, haiku), "small", a subagent_models name, or provider/model
	Thinking                string   // from effort
	MaxTurns                int
	Worktree                bool // isolation: worktree
}

func agentDirs(home, cwd string) []string {
	userHome, _ := os.UserHomeDir()
	dirs := []string{
		filepath.Join(userHome, ".claude", "agents"),
		filepath.Join(userHome, ".agents", "agents"),
		filepath.Join(home, "agents"),
	}
	return append(dirs, projectDirs(cwd, "agents")...)
}

// LoadAgents finds the agent definitions: the user's (~/.claude/agents,
// ~/.agents/agents, ~/.tego/agents), then the project's from the outermost
// folder in to cwd. A later one of the same name wins. Files without a
// description are skipped, as in Claude Code.
func LoadAgents(home, cwd string) []AgentDef {
	byName := map[string]AgentDef{}
	for _, dir := range agentDirs(home, cwd) {
		var paths []string
		filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(path, ".md") {
				paths = append(paths, path)
			}
			return nil
		})
		sort.Strings(paths)
		for _, path := range paths {
			if d, ok := parseAgentDef(path); ok {
				byName[d.Name] = d
			}
		}
	}
	out := make([]AgentDef, 0, len(byName))
	for _, d := range byName {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func parseAgentDef(path string) (AgentDef, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AgentDef{}, false
	}
	fm, body := splitFrontmatter(string(data))
	d := AgentDef{
		Name:        fm["name"],
		Description: oneLine(fm["description"]),
		Path:        path,
		Prompt:      strings.TrimSpace(body),
		Tools:       fmList(fm["tools"]),
		Disallowed:  fmList(fm["disallowedTools"]),
		Model:       strings.TrimSpace(fm["model"]),
		Thinking:    strings.ToLower(strings.TrimSpace(fm["effort"])),
		Worktree:    strings.EqualFold(strings.TrimSpace(fm["isolation"]), "worktree"),
	}
	if d.Name == "" {
		d.Name = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	d.MaxTurns, _ = strconv.Atoi(strings.TrimSpace(fm["maxTurns"]))
	if !slices.Contains(llm.ThinkingLevels, d.Thinking) {
		d.Thinking = "" // a level tego doesn't know: the session's
	}
	if d.Description == "" || strings.ContainsAny(d.Name, ": \t") || strings.HasPrefix(d.Name, "-") {
		return AgentDef{}, false
	}
	if _, ok := fm["tools"]; ok && d.Tools == nil {
		d.Tools = []string{} // "tools:" with nothing: no tools
	}
	return d, true
}

// fmList is a frontmatter list: "Read, Grep", "[Read, Grep]" or a YAML
// list (which splitFrontmatter joins with commas); nil if empty.
func fmList(v string) []string {
	v = strings.TrimSpace(strings.Trim(strings.TrimSpace(v), "[]"))
	var out []string
	for _, s := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
		if s = strings.Trim(s, `"'`); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// claudeToolNames maps Claude Code's tool names to tego's. Tools tego's
// subagents don't have (WebFetch, TodoWrite, Task, ...) map to nothing.
var claudeToolNames = map[string][]string{
	"read": {"read"}, "write": {"write"}, "edit": {"edit"}, "multiedit": {"edit"},
	"bash": {"bash", "bash_output"}, "grep": {"grep"}, "glob": {"glob"}, "ls": {"glob"},
	"bash_output": {"bash_output"},
}

// toolMatches says whether a tool named name is one of patterns: a tool name
// in Claude Code's or tego's spelling, "mcp__server" or "mcp__server__*"
// for a server's tools, "mcp__*" for all of MCP, or "Bash(...)" (its
// arguments are not checked: the tool as a whole).
func toolMatches(name string, patterns []string) bool {
	for _, p := range patterns {
		if i := strings.IndexByte(p, '('); i > 0 {
			p = p[:i]
		}
		switch {
		case strings.HasPrefix(p, "mcp__"):
			if p == "mcp__*" && strings.HasPrefix(name, "mcp__") ||
				strings.HasSuffix(p, "*") && strings.HasPrefix(name, strings.TrimSuffix(p, "*")) ||
				name == p || strings.HasPrefix(name, p+"__") {
				return true
			}
		default:
			for _, n := range claudeToolNames[strings.ToLower(p)] {
				if n == name {
					return true
				}
			}
		}
	}
	return false
}

// allowsTool says whether d lets its agent use the tool named name.
func (d *AgentDef) allowsTool(name string) bool {
	if d.Tools != nil && !toolMatches(name, d.Tools) {
		return false
	}
	return !toolMatches(name, d.Disallowed)
}

func (d *AgentDef) filterTools(ts []tools.Tool) []tools.Tool {
	out := ts[:0:0]
	for _, t := range ts {
		if d.allowsTool(t.Def().Name) {
			out = append(out, t)
		}
	}
	return out
}

// apply makes sub one of d's: its instructions, tools, turn limit and
// thinking level. Its model is chosen with the task's (pickModels).
func (d *AgentDef) apply(sub *Agent) {
	sub.System = d.systemPrompt(sub.System)
	sub.Tools = d.filterTools(sub.Tools)
	if dyn := sub.Dynamic; dyn != nil && (d.Tools != nil || d.Disallowed != nil) {
		sub.Dynamic = func() []tools.Tool { return d.filterTools(dyn()) }
	}
	if d.MaxTurns > 0 && d.MaxTurns < sub.MaxTurns {
		sub.MaxTurns = d.MaxTurns
	}
	if d.Thinking != "" {
		sub.Thinking = d.Thinking
	}
}

// systemPrompt is a subagent's system prompt with d's instructions.
func (d *AgentDef) systemPrompt(base string) string {
	if d.Prompt == "" {
		return base
	}
	return base + "\n\n# Your role: " + d.Name + "\n" + d.Prompt
}
