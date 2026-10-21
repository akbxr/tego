package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/akbxr/tego/internal/llm"
)

// stopState tracks, within one prompt, what the model has left undone when
// it wants to finish: files changed with no command run since (nearly all
// failed runs in SWE-Marathon never validated their change locally) and an
// unfinished todo list (planning mostly changes where a run stops, Fan et
// al. 2026). Each gets at most one reminder per prompt.
type stopState struct {
	unverified     []string // code files edited since the last bash call, in edit order
	todoRev        int      // todo revision when the prompt started
	todoReminded   bool
	verifyReminded bool
	cutResumes     int // times the prompt went on after the output limit (resumeCut)
}

// docExts are files whose edits need no build or test run.
var docExts = map[string]bool{
	".md": true, ".markdown": true, ".mdx": true, ".txt": true, ".rst": true, ".adoc": true, ".org": true,
}

// trackEdits updates the unverified-edit list with one tool round, in call
// order: a successful edit/write of a code file adds it, any bash call (a
// build, a test, a run) clears the list.
func (a *Agent) trackEdits(calls, results []llm.Block) {
	s := &a.stop
	for i, c := range calls {
		switch c.Name {
		case "bash":
			s.unverified = nil
		case "edit", "write":
			if results[i].IsError {
				continue
			}
			var in struct {
				Path string `json:"path"`
			}
			if json.Unmarshal(c.Input, &in) != nil || in.Path == "" || docExts[strings.ToLower(filepath.Ext(in.Path))] {
				continue
			}
			s.unverified = appendUnique(s.unverified, in.Path)
		}
	}
}

func appendUnique(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

// stopNudge runs when the model answers without tool calls. It appends at
// most one reminder and reports whether it did (the loop then continues):
// first an open checkpoint, then unfinished todos, then unverified edits.
func (a *Agent) stopNudge() bool {
	if a.needsRewindNudge() {
		return true
	}
	if a.Subagent {
		// A subagent's last text is its report to the parent; a reminder
		// turn would replace it, and the parent verifies the combined work.
		return false
	}
	return a.needsTodoNudge() || a.needsVerifyNudge()
}

// needsTodoNudge only fires for a list this prompt worked on: a stale list
// from an earlier prompt must not nag about unrelated questions.
func (a *Agent) needsTodoNudge() bool {
	list, rev := a.TodoState()
	if a.stop.todoReminded || rev == a.stop.todoRev || a.planning() { // a plan's list is meant to stay open
		return false
	}
	var open []string
	for _, td := range list {
		if td.Status != "completed" {
			open = append(open, td.Content)
		}
	}
	if len(open) == 0 {
		return false
	}
	a.stop.todoReminded = true
	a.UI.OnInfo("todo list unfinished: reminding the model")
	a.remind(fmt.Sprintf("Your todo list still has %d unfinished item(s): %s. "+
		"Continue with them; if they are done or no longer needed, update the list with the todo tool, then finish. "+
		"If the user asked you to stop here or you are waiting for their answer, reply with one short line; "+
		"your earlier answer stands, do not repeat it.", len(open), quoteList(open, 5)))
	return true
}

func (a *Agent) needsVerifyNudge() bool {
	if a.stop.verifyReminded || len(a.stop.unverified) == 0 {
		return false
	}
	a.stop.verifyReminded = true
	a.UI.OnInfo("edits not verified: asking the model to check them")
	a.remind(fmt.Sprintf("You changed %s and ran no command since. "+
		"Before finishing, verify the change works: build, run the relevant tests, or execute the changed path. "+
		"If verification does not apply, is impossible, or the user asked you not to run anything, reply with one short line saying why; "+
		"your earlier answer stands, do not repeat it.",
		quoteList(a.stop.unverified, 5)))
	return true
}

// Notice tells the model about something the user did outside the
// conversation (e.g. restored files with /undo), before its next request.
func (a *Agent) Notice(text string) { a.remind(text) }

func (a *Agent) remind(text string) {
	a.append(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText,
		Text: "<system-reminder>" + text + "</system-reminder>"}}})
}

// quoteList renders up to max items as "a", "b" and N more.
func quoteList(items []string, max int) string {
	shown := items
	if len(shown) > max {
		shown = shown[:max]
	}
	q := make([]string, len(shown))
	for i, s := range shown {
		q[i] = fmt.Sprintf("%q", s)
	}
	out := strings.Join(q, ", ")
	if n := len(items) - len(shown); n > 0 {
		out += fmt.Sprintf(" and %d more", n)
	}
	return out
}
