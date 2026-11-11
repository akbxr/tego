package agent

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/session"
)

// resumedNote starts the prompt of a resumed subagent.
const resumedNote = "<system-reminder>The agent that started you resumed you with your context. Any limit you reached is reset: " +
	"you have a full budget of requests again and your tools work. Earlier notes that told you to stop no longer apply.</system-reminder>\n\n"

// endedMax is how many ended subagents a session keeps to resume.
const endedMax = 8

// endedSub is a subagent that ended, kept with its context so a later task
// can resume it ("resume" on a task) instead of starting over: one stopped at
// its turn limit or by the watchdog, interrupted, or done and asked for more.
// Worktree subagents are not kept: their worktree is committed or removed
// when they end.
//
// Its history is also recorded in the session file, so that it can be
// resumed after tego restarts: restored from there, sub is nil until a
// task resumes it.
type endedSub struct {
	sub   *Agent
	name  string
	id    string // its background id ("t3"), if it ran in the background
	agent string // its agent definition's name, if any
	task  string // what it was asked, for its watchdog
	model string // the model it was asked to run on ("" = the session's)
	ranOn string // restored: the model it ran on last

	// Its records in the session file.
	sess  *session.Session
	key   string // the same in all of them
	rec   string // the latest
	saved int    // messages the latest holds
	fp    string // fingerprint of the last of them
}

// endedMeta is what a session file's record keeps of a subagent besides
// its history.
type endedMeta struct {
	Name  string `json:"name"`
	ID    string `json:"id,omitempty"`
	Agent string `json:"agent,omitempty"`
	Task  string `json:"task,omitempty"`
	Model string `json:"model,omitempty"`
	RanOn string `json:"ran_on,omitempty"`
}

// ref is how a task names it in "resume".
func (e *endedSub) ref() string {
	if e.id != "" {
		return e.id
	}
	return e.name
}

// fingerprint tells whether a message is still the one recorded.
func fingerprint(m llm.Message) string {
	data, _ := json.Marshal(m)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:12])
}

// save records e's history in sess: what it added since its latest record
// there, or all of it (the first time, or once its history was rewritten,
// by a compaction). It reports what went wrong; nil without a session.
func (e *endedSub) save(sess *session.Session) error {
	msgs := e.sub.Messages
	if sess == nil || len(msgs) == 0 {
		return nil
	}
	from, prev := 0, ""
	if e.sess == sess && e.rec != "" && e.saved > 0 && e.saved <= len(msgs) && fingerprint(msgs[e.saved-1]) == e.fp {
		from, prev = e.saved, e.rec
	}
	if e.sess != sess || e.key == "" {
		e.key = randomKey()
	}
	meta, err := json.Marshal(endedMeta{Name: e.name, ID: e.id, Agent: e.agent, Task: e.task, Model: e.model, RanOn: e.sub.Model})
	if err != nil {
		return err
	}
	rec, err := sess.SaveSubagent(e.key, prev, from, meta, msgs[from:])
	if err != nil || rec == "" {
		return err
	}
	e.sess, e.rec, e.saved, e.fp = sess, rec, len(msgs), fingerprint(msgs[len(msgs)-1])
	return nil
}

func randomKey() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// restoreEnded takes back the subagents the session file holds for the
// active path: after a restart, or on another branch. Their histories are
// read when one is resumed.
func (a *Agent) restoreEnded() {
	if a.Subagent || a.Session == nil {
		return
	}
	recs := a.Session.Subagents()
	if len(recs) > endedMax {
		recs = recs[len(recs)-endedMax:]
	}
	var ended []*endedSub
	for _, r := range recs {
		var m endedMeta
		if json.Unmarshal(r.Meta, &m) != nil || m.Name == "" {
			continue
		}
		ended = append(ended, &endedSub{name: m.Name, id: m.ID, agent: m.Agent, task: m.Task, model: m.Model, ranOn: m.RanOn,
			sess: a.Session, key: r.Key, rec: r.ID, saved: r.Count})
	}
	a.endedMu.Lock()
	a.ended = ended
	a.endedMu.Unlock()
}

// revive gives e, restored from the session file, its subagent: its
// history read from there, on the model it was asked for (the session's
// when that is gone), as the agent it ran as if that is still defined.
func (t *TaskTool) revive(e *endedSub) error {
	msgs, err := e.sess.SubagentHistory(e.rec)
	if err == nil && len(msgs) == 0 {
		err = fmt.Errorf("its history is empty")
	}
	if err != nil {
		return fmt.Errorf("subagent %q can't be resumed: %v; start a new one", e.ref(), err)
	}
	var def *AgentDef
	if e.agent != "" {
		for _, d := range t.agents() {
			if d.Name == e.agent {
				d.Worktree = false // it ran in the session's folder
				def = &d
				break
			}
		}
	}
	model := t.pickModels([]string{e.model})[0]
	if model.note != "" {
		t.Parent.UI.OnInfo(fmt.Sprintf("  [%s] %s; it runs on %s", e.name, model.note, t.Parent.Model))
	}
	sub, _, _ := t.newSub(taskSpec{Name: e.name, spec: e.model}, def, model, func(_, status string) (string, string) { return status, "" })
	e.saved, e.fp = len(msgs), fingerprint(msgs[len(msgs)-1])
	sub.SetHistory(msgs)
	e.sub = sub
	return nil
}

// endedID reports whether a kept subagent has background id id: one
// restored from the session file may, and a new task must not.
func (a *Agent) endedID(id string) bool {
	a.endedMu.Lock()
	defer a.endedMu.Unlock()
	return slices.ContainsFunc(a.ended, func(e *endedSub) bool { return e.id == id })
}

// followSession puts a subagent that ran on the session's model on the
// one p has now, after a /model since it ended: one resumed would
// otherwise stay on the old model for as long as it is resumed. It
// reports whether the model changed. A subagent with a model of its own
// (small, a named one, an agent file's) keeps it.
func (a *Agent) followSession(p *Agent) bool {
	if !a.sessionModel {
		return false
	}
	changed := a.Model != p.Model || a.ProviderName != p.ProviderName
	a.Provider, a.ProviderName, a.Model, a.ContextWindow = p.Provider, p.ProviderName, p.Model, p.ContextWindow
	a.Fast, a.MaxOutputTokens, a.CacheWritePremium = p.Fast, p.MaxOutputTokens, p.CacheWritePremium
	if a.sessionThinking {
		a.Thinking = p.Thinking
	}
	return changed
}

// keepEnded keeps e, forgetting the oldest past endedMax.
func (a *Agent) keepEnded(e *endedSub) {
	a.endedMu.Lock()
	defer a.endedMu.Unlock()
	a.ended = append(a.ended, e)
	if len(a.ended) > endedMax {
		a.ended = slices.Delete(a.ended, 0, len(a.ended)-endedMax)
	}
}

// takeEnded takes out the ended subagent ref names, a background id or a
// name (the latest of that name), so that no other task resumes it too.
func (a *Agent) takeEnded(ref string) (*endedSub, error) {
	a.endedMu.Lock()
	defer a.endedMu.Unlock()
	ref = strings.TrimSpace(ref)
	idx := slices.IndexFunc(a.ended, func(e *endedSub) bool { return e.id != "" && e.id == ref })
	if idx < 0 {
		for i := len(a.ended) - 1; i >= 0; i-- {
			if strings.EqualFold(a.ended[i].name, ref) {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		if len(a.ended) == 0 {
			return nil, fmt.Errorf("no subagent %q to resume: none can be in this session (worktree subagents can't be); start a new one", ref)
		}
		refs := make([]string, len(a.ended))
		for i, e := range a.ended {
			refs[i] = e.ref()
		}
		return nil, fmt.Errorf("no subagent %q to resume; these can be: %s", ref, strings.Join(refs, ", "))
	}
	e := a.ended[idx]
	a.ended = slices.Delete(a.ended, idx, idx+1)
	return e, nil
}

// forgetEnded drops the kept subagents: another session or branch.
func (a *Agent) forgetEnded() {
	a.endedMu.Lock()
	a.ended = nil
	a.endedMu.Unlock()
}
