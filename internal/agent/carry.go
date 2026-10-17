package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/akbxr/tego/internal/llm"
)

// What a compaction carries over word for word rather than leaving to the
// summary: the user's own messages (as Claude Code's and Codex's compaction
// do), and the skills in use, whose bodies are summarised away with the
// message that loaded them.

const (
	// promptsHeading lists the summarised user messages; each is a line
	// "- " then continuation lines indented by two spaces, so no line of a
	// message can be mistaken for a heading.
	promptsHeading = "\n\nThe user's messages so far, verbatim (oldest first):\n"
	// skillsHeading lists the skills loaded so far, as "name (path)".
	skillsHeading = "\n\nSkills used so far (read a skill's file again before following it): "

	maxPromptChars  = 2_000  // one message, beyond which its middle is cut
	maxPromptsChars = 16_000 // all of them (~4k tokens): the first and the latest are kept
)

var (
	reminderRE = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	fileRE     = regexp.MustCompile(`(?s)<file path="([^"]*)">.*?</file>`)
	skillRE    = regexp.MustCompile(`<skill(?:-attachment)? name="([^"]*)" path="([^"]*)">`)
	omittedRE  = regexp.MustCompile(`^…\((\d+) earlier messages? omitted\)$`)
)

// typedText is what the user typed in a prompt message: reminders, earlier
// summaries, attached files and skill bodies are left out ("" for none).
func typedText(m llm.Message) string {
	if !isPrompt(m) {
		return ""
	}
	var parts []string
	for _, b := range m.Blocks {
		if b.Type != llm.BlockText {
			continue
		}
		t := strings.TrimSpace(b.Text)
		switch {
		case strings.HasPrefix(t, contextSummaryTag), strings.HasPrefix(t, `<file path="`),
			strings.HasPrefix(t, `<skill-attachment `):
			continue
		case strings.HasPrefix(t, `<skill name="`): // a /skill:<name> prompt embeds the skill
			name := ""
			if sm := skillRE.FindStringSubmatch(t); sm != nil {
				name = sm[1]
			}
			_, req, _ := strings.Cut(t, "</skill>")
			if req = strings.TrimSpace(req); req == "Apply this skill to the current task." {
				req = ""
			}
			t = "/skill:" + name + " " + req
		}
		t = reminderRE.ReplaceAllString(t, "")
		t = fileRE.ReplaceAllString(t, "@$1") // files an editor embedded in the text
		if t = strings.TrimSpace(t); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// promptLog is the list of user messages carried over: omitted ones sit
// between the first (the original task) and the rest.
type promptLog struct {
	msgs    []string
	omitted int
}

// summarisedPrompts collects the user messages in head, after those an
// earlier summary in it carried over, and fits them in the budget.
func summarisedPrompts(head []llm.Message) promptLog {
	var log promptLog
	for _, m := range head {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Blocks {
			if b.Type == llm.BlockText && strings.HasPrefix(b.Text, contextSummaryTag) {
				old := parsePrompts(b.Text)
				if len(log.msgs) == 0 {
					log.omitted += old.omitted
				} else {
					log.omitted += len(old.msgs) + old.omitted // only one earlier summary is expected
					old.msgs = nil
				}
				log.msgs = append(log.msgs, old.msgs...)
			}
		}
		if t := typedText(m); t != "" {
			log.msgs = append(log.msgs, clipPrompt(t))
		}
	}
	log.fit()
	return log
}

// fit keeps the first message and as many of the latest as the budget
// allows.
func (l *promptLog) fit() {
	if len(l.msgs) < 2 {
		return
	}
	total, keep := len(l.msgs[0]), len(l.msgs)
	for keep > 1 && total+len(l.msgs[keep-1]) <= maxPromptsChars {
		total += len(l.msgs[keep-1])
		keep--
	}
	// l.msgs[keep:] fit; l.msgs[1:keep] do not.
	if dropped := keep - 1; dropped > 0 {
		l.omitted += dropped
		l.msgs = append(l.msgs[:1], l.msgs[keep:]...)
	}
}

// clipPrompt cuts the middle out of a long message.
func clipPrompt(s string) string {
	if len(s) <= maxPromptChars {
		return s
	}
	head, tail := maxPromptChars*3/4, maxPromptChars/4
	cut := len(s) - head - tail
	return strings.ToValidUTF8(s[:head], "") + fmt.Sprintf(" …[%d chars cut]… ", cut) + strings.ToValidUTF8(s[len(s)-tail:], "")
}

func (l promptLog) render() string {
	if len(l.msgs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(promptsHeading)
	for i, m := range l.msgs {
		if i == 1 && l.omitted > 0 {
			fmt.Fprintf(&b, "…(%d earlier %s omitted)\n", l.omitted, map[bool]string{true: "message", false: "messages"}[l.omitted == 1])
		}
		for j, line := range strings.Split(m, "\n") {
			if j == 0 {
				b.WriteString("- ")
			} else {
				b.WriteString("  ")
			}
			b.WriteString(strings.TrimRight(line, " \t\r"))
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// parsePrompts reads the list back from a summary.
func parsePrompts(summary string) promptLog {
	var l promptLog
	i := strings.Index(summary, promptsHeading)
	if i < 0 {
		return l
	}
	for _, line := range strings.Split(summary[i+len(promptsHeading):], "\n") {
		switch {
		case strings.HasPrefix(line, "- "):
			l.msgs = append(l.msgs, line[2:])
		case strings.HasPrefix(line, "  ") && len(l.msgs) > 0:
			l.msgs[len(l.msgs)-1] += "\n" + line[2:]
		case omittedRE.MatchString(line):
			n, _ := strconv.Atoi(omittedRE.FindStringSubmatch(line)[1])
			l.omitted += n
		default:
			return l
		}
	}
	return l
}

// skillRef is a skill the conversation loaded.
type skillRef struct{ name, path string }

// usedSkills lists the skills loaded in msgs (by /skill:, or a read of their
// SKILL.md), after those of an earlier summary's list: one entry a name,
// with the path it was last loaded from.
func usedSkills(msgs []llm.Message) []skillRef {
	var out []skillRef
	at := map[string]int{}
	add := func(name, path string) {
		if path == "" {
			return
		}
		if i, ok := at[name]; ok {
			out[i].path = path
			return
		}
		at[name] = len(out)
		out = append(out, skillRef{name, path})
	}
	failed := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolResult && b.IsError {
				failed[b.ToolUseID] = true
			}
		}
	}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch {
			case b.Type == llm.BlockText && m.Role == llm.RoleUser && strings.HasPrefix(b.Text, contextSummaryTag):
				for _, s := range parseSkills(b.Text) {
					add(s.name, s.path)
				}
			case b.Type == llm.BlockText && m.Role == llm.RoleUser:
				if sm := skillRE.FindStringSubmatch(strings.TrimSpace(b.Text)); sm != nil && strings.HasPrefix(strings.TrimSpace(b.Text), sm[0]) {
					add(sm[1], sm[2])
				}
			case b.Type == llm.BlockToolUse && b.Name == "read" && !failed[b.ID]:
				var in struct {
					Path string `json:"path"`
				}
				if json.Unmarshal(b.Input, &in) == nil && filepath.Base(in.Path) == "SKILL.md" {
					add(filepath.Base(filepath.Dir(in.Path)), in.Path)
				}
			}
		}
	}
	return out
}

var skillItemRE = regexp.MustCompile(`([^,()]+?) \(([^()]+)\)`)

func renderSkills(skills []skillRef) string {
	if len(skills) == 0 {
		return ""
	}
	items := make([]string, len(skills))
	for i, s := range skills {
		items[i] = s.name + " (" + s.path + ")"
	}
	return skillsHeading + strings.Join(items, ", ")
}

func parseSkills(summary string) []skillRef {
	i := strings.Index(summary, skillsHeading)
	if i < 0 {
		return nil
	}
	line, _, _ := strings.Cut(summary[i+len(skillsHeading):], "\n")
	var out []skillRef
	for _, m := range skillItemRE.FindAllStringSubmatch(line, -1) {
		out = append(out, skillRef{strings.TrimSpace(m[1]), m[2]})
	}
	return out
}
