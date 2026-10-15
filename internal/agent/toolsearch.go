package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

func (a *Agent) lazy(n int) bool {
	return a.LazyThreshold >= 0 && n > a.LazyThreshold
}

func (a *Agent) activate(names ...string) {
	a.toolMu.Lock()
	if a.activated == nil {
		a.activated = map[string]bool{}
	}
	for _, n := range names {
		a.activated[n] = true
	}
	a.toolMu.Unlock()
}

// activateFromHistory keeps MCP tools the conversation already used loaded
// (resumed or branched sessions).
func (a *Agent) activateFromHistory(msgs []llm.Message) {
	a.toolMu.Lock()
	a.activated = nil
	a.toolMu.Unlock()
	var names []string
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolUse && strings.HasPrefix(b.Name, "mcp__") {
				names = append(names, b.Name)
			}
		}
	}
	if len(names) > 0 {
		a.activate(names...)
	}
}

// mcpServer extracts <server> from "mcp__<server>__<tool>".
func mcpServer(name string) string {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return ""
	}
	server, _, _ := strings.Cut(rest, "__")
	return server
}

// toolSearch is the stand-in for deferred MCP tools: it lists the
// servers, finds tools by keyword and loads the matches.
type toolSearch struct{ a *Agent }

// ReadOnly: loading tool definitions is idempotent and lock-protected.
func (t *toolSearch) ReadOnly() bool { return true }

func (t *toolSearch) Def() llm.Tool {
	counts := map[string]int{}
	var servers []string
	for _, tl := range t.a.Dynamic() {
		s := mcpServer(tl.Def().Name)
		if counts[s] == 0 {
			servers = append(servers, s)
		}
		counts[s]++
	}
	var list []string
	for _, s := range servers {
		list = append(list, fmt.Sprintf("%s (%d tools)", s, counts[s]))
	}
	return llm.Tool{
		Name: "tool_search",
		Description: "Find and load tools from connected MCP servers; they are not loaded up front to save context. Servers: " + strings.Join(list, ", ") + ". " +
			"Search with keywords for what you need (e.g. \"screenshot\", \"run script\"); matching tools are loaded and can be called by name right after. " +
			"Query a server name to list all its tool names.",
		Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"keywords, or a server name to list its tools"},"limit":{"type":"integer","description":"max tools to load (default 5)"}},"required":["query"]}`),
	}
}

// refTool is a tool whose result loads deferred tools: ToolRefs names
// them, and providers that defer tools send references instead of the text.
type refTool interface {
	RunRefs(ctx context.Context, input json.RawMessage) (string, []string, error)
}

func (t *toolSearch) Run(ctx context.Context, input json.RawMessage) (string, error) {
	out, _, err := t.RunRefs(ctx, input)
	return out, err
}

// RunRefs searches the catalog. The matches are activated (sent from now
// on to providers without deferred tools) and returned as references.
func (t *toolSearch) RunRefs(_ context.Context, input json.RawMessage) (string, []string, error) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", nil, err
	}
	if in.Limit <= 0 {
		in.Limit = 5
	}
	catalog := t.a.Dynamic()
	q := strings.ToLower(strings.TrimSpace(in.Query))

	// A server name lists its tools without loading them all.
	var listed []string
	for _, tl := range catalog {
		d := tl.Def()
		if mcpServer(d.Name) == q {
			listed = append(listed, fmt.Sprintf("- %s: %s", d.Name, firstSentence(d.Description, 120)))
		}
	}
	if len(listed) > 0 {
		how := "search for the ones you need, or call one by exact name to load it"
		if t.a.defersTools() {
			how = "search for the ones you need by keyword or exact name to load them"
		}
		return fmt.Sprintf("Server %s has %d tools (not loaded; %s):\n%s", q, len(listed), how, strings.Join(listed, "\n")), nil, nil
	}

	var hits []tools.Tool
	if sel, ok := strings.CutPrefix(q, "select:"); ok {
		// Claude Code's syntax for loading tools by exact name, which
		// Claude models use unprompted: "select:a,b".
		want := map[string]bool{}
		for _, n := range strings.Split(sel, ",") {
			want[strings.TrimSpace(n)] = true
		}
		for _, tl := range catalog {
			if want[strings.ToLower(tl.Def().Name)] {
				hits = append(hits, tl)
			}
		}
	} else {
		hits = rankTools(catalog, q)
	}
	if len(hits) == 0 {
		return "No MCP tool matches " + strconvQuote(in.Query) + ". Try other keywords or a server name.", nil, nil
	}
	if len(hits) > in.Limit {
		hits = hits[:in.Limit]
	}
	var b strings.Builder
	names := make([]string, 0, len(hits))
	fmt.Fprintf(&b, "Loaded %d tool(s); call them by name:\n", len(hits))
	for _, tl := range hits {
		d := tl.Def()
		names = append(names, d.Name)
		fmt.Fprintf(&b, "\n## %s\n%s\n%s\n", d.Name, firstSentence(d.Description, 400), paramSummary(d.Schema))
	}
	t.a.activate(names...)
	return b.String(), names, nil
}

// rankTools scores tools by keyword hits: name hits count triple.
func rankTools(catalog []tools.Tool, q string) []tools.Tool {
	words := strings.FieldsFunc(q, func(r rune) bool { return r == ' ' || r == '_' || r == '-' || r == ',' })
	type scored struct {
		t     tools.Tool
		score int
	}
	var out []scored
	for _, tl := range catalog {
		d := tl.Def()
		name := strings.ToLower(strings.NewReplacer("_", " ", "-", " ").Replace(d.Name))
		desc := strings.ToLower(d.Description)
		s := 0
		for _, w := range words {
			if strings.Contains(name, w) {
				s += 3
			}
			if strings.Contains(desc, w) {
				s++
			}
		}
		if s > 0 {
			out = append(out, scored{tl, s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	res := make([]tools.Tool, len(out))
	for i, s := range out {
		res[i] = s.t
	}
	return res
}

func firstSentence(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// paramSummary renders "Parameters: a (string, required), b (int)" from a
// JSON schema; the full schema arrives with the loaded tool definition.
func paramSummary(schema json.RawMessage) string {
	var s struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if json.Unmarshal(schema, &s) != nil || len(s.Properties) == 0 {
		return "Parameters: none"
	}
	req := map[string]bool{}
	for _, r := range s.Required {
		req[r] = true
	}
	names := make([]string, 0, len(s.Properties))
	for n := range s.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		p := fmt.Sprintf("%s (%v", n, s.Properties[n].Type)
		if req[n] {
			p += ", required"
		}
		parts[i] = p + ")"
	}
	return "Parameters: " + strings.Join(parts, ", ")
}

func strconvQuote(s string) string { return fmt.Sprintf("%q", s) }
