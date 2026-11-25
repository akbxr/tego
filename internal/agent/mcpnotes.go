package agent

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// An MCP server may say how its tools are best used (its instructions:
// "call get_scene_info first"). They reach the model once per conversation,
// with the first tool_search result that loads the server's tools (beside
// it where the result is tool references, which take no text), or else
// with the first result of one of its tools; never in the system prompt,
// whose cache a server connecting late would then break.

// serverInstructed is a tool whose server has instructions (mcp's tools).
type serverInstructed interface {
	ServerInstructions() (server, text string)
}

// notesRe finds the instructions a conversation already carries.
var notesRe = regexp.MustCompile(`<mcp_server_instructions server="([^"]+)">`)

// serverNotes is the instructions of the servers of ts the conversation
// has not had yet, marked as had; "" when there are none.
func (a *Agent) serverNotes(ts ...tools.Tool) string {
	var out []string
	a.toolMu.Lock()
	defer a.toolMu.Unlock()
	for _, t := range ts {
		si, ok := t.(serverInstructed)
		if !ok {
			continue
		}
		server, text := si.ServerInstructions()
		if text = strings.TrimSpace(text); text == "" || a.instructed[server] {
			continue
		}
		if a.instructed == nil {
			a.instructed = map[string]bool{}
		}
		a.instructed[server] = true
		out = append(out, fmt.Sprintf("<mcp_server_instructions server=%q>\nInstructions from the MCP server %s on using its tools (passed on by tego, not written by the user):\n%s\n</mcp_server_instructions>",
			server, server, text))
	}
	return strings.Join(out, "\n\n")
}

// holdNote keeps note for the next request, beside the tool results.
func (a *Agent) holdNote(note string) {
	a.toolMu.Lock()
	a.heldNotes = append(a.heldNotes, note)
	a.toolMu.Unlock()
}

// takeNotes is the notes held for the next request ("" none).
func (a *Agent) takeNotes() string {
	a.toolMu.Lock()
	defer a.toolMu.Unlock()
	out := strings.Join(a.heldNotes, "\n\n")
	a.heldNotes = nil
	return out
}

// instructedFromHistory marks the servers whose instructions msgs carry
// (a resumed or branched conversation).
func (a *Agent) instructedFromHistory(msgs []llm.Message) {
	seen := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			text := b.Content
			if b.Type == llm.BlockText {
				text = b.Text
			}
			if !strings.Contains(text, "<mcp_server_instructions") {
				continue
			}
			for _, sm := range notesRe.FindAllStringSubmatch(text, -1) {
				seen[sm[1]] = true
			}
		}
	}
	a.toolMu.Lock()
	a.instructed, a.heldNotes = seen, nil
	a.toolMu.Unlock()
}
