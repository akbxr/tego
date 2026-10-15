package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/lineedit"
	"github.com/akbxr/tego/internal/llm"
)

// skillRefRe finds "/skill:<name>" references anywhere in a prompt; the
// slash must start the text or follow whitespace.
var skillRefRe = regexp.MustCompile(`(?:^|\s)/skill:([A-Za-z0-9][A-Za-z0-9._-]*)`)

func skillRefs(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range skillRefRe.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// skillRecency orders skills by last use so the ones you reach for come
// first in the popup. It is seeded from the prompt history and
// shared between the main loop (touch) and the TUI goroutine (suggest).
type skillRecency struct {
	mu   sync.Mutex
	last map[string]int
	seq  int
}

func (r *skillRecency) touch(text string) {
	refs := skillRefs(text)
	if len(refs) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = map[string]int{}
	}
	for _, name := range refs {
		r.seq++
		r.last[name] = r.seq
	}
}

// sorted returns skills, most recently used first, then by name.
func (r *skillRecency) sorted(skills []agent.Skill) []agent.Skill {
	out := append([]agent.Skill(nil), skills...)
	r.mu.Lock()
	defer r.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := r.last[out[i].Name], r.last[out[j].Name]
		if li != lj {
			return li > lj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// loadSkillRecency replays the history file (one JSON string per line).
func loadSkillRecency(path string) *skillRecency {
	r := &skillRecency{}
	f, err := os.Open(path)
	if err != nil {
		return r
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var line string
		if json.Unmarshal(sc.Bytes(), &line) == nil && strings.Contains(line, "/skill:") {
			r.touch(line)
		}
	}
	return r
}

func (a *app) findSkill(name string) (agent.Skill, bool) {
	for _, s := range a.skills {
		if s.Name == name {
			return s, true
		}
	}
	return agent.Skill{}, false
}

// skillAttachPrefix marks a skill attached by a "/skill:<name>" reference
// inside a prompt (a prompt that starts with /skill: embeds it instead).
const skillAttachPrefix = `<skill-attachment name="`

// skillBlocks attaches the skills referenced in text, except skip (the
// skill the prompt already embeds).
func (a *app) skillBlocks(text, skip string) []llm.Block {
	var blocks []llm.Block
	for _, name := range skillRefs(text) {
		s, ok := a.findSkill(name)
		if !ok || name == skip {
			continue
		}
		body, err := os.ReadFile(s.Path)
		if err != nil {
			a.ui.Errorf("skill %s: %v", name, err)
			continue
		}
		blocks = append(blocks, llm.Block{Type: llm.BlockText,
			Text: fmt.Sprintf("%s%s\" path=%q>\n%s\n</skill-attachment>", skillAttachPrefix, s.Name, s.Path, strings.TrimSpace(string(body)))})
		a.ui.Dim("✧ skill:%s", s.Name)
	}
	return blocks
}

// skillSuggestions lists skills for the popup. before is the prompt text in
// front of a mid-line "/skill:" token ("" when the line starts with it);
// accepting such an item completes the token and keeps editing.
func (a *app) skillSuggestions(before string) []lineedit.Suggestion {
	skills := a.skills
	if a.skillRecent != nil {
		skills = a.skillRecent.sorted(skills)
	}
	out := make([]lineedit.Suggestion, len(skills))
	for i, s := range skills {
		desc := s.Description
		if desc == "" {
			desc = "(no description)"
		}
		out[i] = lineedit.Suggestion{Value: "/skill:" + s.Name, Label: "skill:" + s.Name, Description: desc, Icon: "✧", Final: true}
		if before != "" {
			out[i].Value, out[i].Final = before+"/skill:"+s.Name+" ", false
		}
	}
	return out
}

// midlineSkillToken reports a "/skill:" token (or "/sk", "/skill") being
// typed after other text, on any line: the text before the token and the
// partial name. line is the text before the cursor.
func midlineSkillToken(line string) (before, query string, ok bool) {
	i := strings.LastIndexAny(line, " \t\n")
	if i < 0 {
		return "", "", false // at the very start: the command popup handles it
	}
	tok := line[i+1:]
	if q, ok := strings.CutPrefix(tok, "/skill:"); ok {
		return line[:i+1], q, true
	}
	// "/sk", "/ski", "/skill" already offer the skills; shorter tokens
	// could be paths like /src and are left alone.
	if len(tok) >= 3 && strings.HasPrefix("/skill:", tok) {
		return line[:i+1], "", true
	}
	return "", "", false
}
