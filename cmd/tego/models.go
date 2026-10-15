package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/akbxr/tego/internal/config"
	"github.com/akbxr/tego/internal/lineedit"
	"github.com/akbxr/tego/internal/llm"
)

const modelCacheTTL = 6 * time.Hour

type modelCacheEntry struct {
	Fetched int64    `json:"fetched"`
	Models  []string `json:"models"`
}

// listModels returns provider/model specs for every provider with
// credentials. Lists are cached in ~/.tego/models.json; refresh forces a
// refetch. Providers that fail without cached data are reported separately.
func (a *app) listModels(refresh bool) (specs, unreachable []string) {
	path := filepath.Join(a.cfg.Dir, "models.json")
	cache := map[string]modelCacheEntry{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &cache)
	}
	var names []string
	for _, p := range a.cfg.ProviderNames() {
		if a.cfg.Available(p, a.store) {
			names = append(names, p)
		}
	}
	type result struct {
		ids     []string
		fetched bool
		err     error
	}
	results := make([]result, len(names))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i, name := range names {
		if c, ok := cache[name]; ok && !refresh && time.Since(time.Unix(c.Fetched, 0)) < modelCacheTTL {
			results[i].ids = c.Models
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := a.cfg.NewProvider(name, a.store)
			if err != nil {
				results[i].err = err
				return
			}
			lister, ok := p.(llm.ModelLister)
			if !ok {
				results[i].err = errors.New("provider cannot list models")
				return
			}
			ids, err := lister.ListModels(ctx)
			results[i] = result{ids: ids, fetched: err == nil, err: err}
		}()
	}
	if len(names) > 0 {
		a.ui.Dim("loading models…")
	}
	wg.Wait()
	changed := false
	for i, name := range names {
		r := results[i]
		switch {
		case r.fetched:
			cache[name] = modelCacheEntry{Fetched: time.Now().Unix(), Models: r.ids}
			changed = true
		case r.err != nil:
			if c, ok := cache[name]; ok {
				r.ids = c.Models
			} else {
				unreachable = append(unreachable, name)
				if d := config.DefaultModelFor(name); d != "" {
					r.ids = []string{d}
				}
			}
		}
		for _, id := range r.ids {
			specs = append(specs, name+"/"+id)
		}
	}
	if changed {
		if data, err := json.Marshal(cache); err == nil {
			_ = os.WriteFile(path, data, 0o600)
		}
	}
	return specs, unreachable
}

// pickModel resolves /model [query][:level]: an exact or unique match
// switches directly, otherwise the picker opens with query prefilled. The
// thinking level follows: ":level" sets it, else its picker opens.
func (a *app) pickModel(query string) {
	refresh := false
	if f := strings.Fields(query); len(f) > 0 && (f[0] == "-r" || f[0] == "--refresh") {
		refresh, query = true, strings.TrimSpace(strings.Join(f[1:], " "))
	}
	query, level := splitThinking(query)
	current := a.ag.ProviderName + "/" + a.ag.Model
	specs, unreachable := a.listModels(refresh)
	if !contains(specs, current) {
		specs = append([]string{current}, specs...)
	}
	items := make([]lineedit.PickItem, len(specs))
	sel := 0
	for i, s := range specs {
		items[i].Label = s
		if s == current {
			items[i].Detail, sel = "current", i
		}
	}
	if query != "" {
		if spec, ok := exactModel(specs, query); ok {
			a.chooseModel(spec, level)
			return
		}
		switch hits := lineedit.Filter(items, query); {
		case len(hits) == 1:
			a.chooseModel(specs[hits[0]], level)
			return
		case len(hits) == 0 && strings.Contains(query, "/"):
			// An explicit provider/model that no listing knows about is still allowed.
			a.chooseModel(query, level)
			return
		}
	}
	title := "Select model · then thinking level"
	if len(unreachable) > 0 {
		title += "  (unreachable: " + strings.Join(unreachable, ", ") + ")"
	}
	idx, err := a.tui.Pick(title, items, query, sel)
	if err != nil {
		if !errors.Is(err, lineedit.ErrCanceled) {
			a.ui.Errorf("%v", err)
		}
		return
	}
	a.chooseModel(specs[idx], level)
}

// exactModel matches query as a full provider/model spec or a bare model id
// that only one provider offers.
func exactModel(specs []string, query string) (string, bool) {
	var byID []string
	for _, s := range specs {
		if s == query {
			return s, true
		}
		if _, id, _ := strings.Cut(s, "/"); id == query {
			byID = append(byID, s)
		}
	}
	if len(byID) == 1 {
		return byID[0], true
	}
	return "", false
}

// chooseModel switches to spec and then sets the thinking level: level
// when given (from "/model spec:level"), otherwise from the thinking picker
// (Esc keeps the current level).
func (a *app) chooseModel(spec, level string) {
	if !a.switchModel(spec) {
		return
	}
	a.refreshStatus() // show the new model while the thinking picker is open
	if level == "" {
		var ok bool
		if level, ok = a.pickThinking("Thinking level · " + modelLabel(a.ag.Model)); !ok {
			return
		}
	}
	a.setThinking(level)
}

func (a *app) switchModel(spec string) bool {
	if err := a.setModel(spec); err != nil {
		a.ui.Errorf("%v", err)
		return false
	}
	a.ui.Dim("model → %s/%s", a.ag.ProviderName, a.ag.Model)
	a.cfg.Model = a.ag.ProviderName + "/" + a.ag.Model
	if err := a.cfg.Save(); err != nil {
		a.ui.Errorf("saving default model: %v", err)
	}
	return true
}

var thinkingLevels = []string{llm.ThinkingOff, llm.ThinkingLow, llm.ThinkingMedium, llm.ThinkingHigh}

// pickThinking opens the thinking-level picker on the current level.
func (a *app) pickThinking(title string) (string, bool) {
	items := make([]lineedit.PickItem, len(thinkingLevels))
	sel := 0
	for i, l := range thinkingLevels {
		items[i].Label = l
		if l == a.ag.Thinking {
			items[i].Detail, sel = "current", i
		}
	}
	idx, err := a.tui.Pick(title, items, "", sel)
	if err != nil {
		return "", false
	}
	return thinkingLevels[idx], true
}

func (a *app) setThinking(level string) {
	a.ag.Thinking = level
	a.cfg.Thinking = level
	if err := a.cfg.Save(); err != nil {
		a.ui.Errorf("saving thinking level: %v", err)
	}
	a.ui.Dim("thinking → %s", level)
}

// splitThinking strips a ":level" suffix ("opus:high").
func splitThinking(query string) (string, string) {
	if i := strings.LastIndexByte(query, ':'); i >= 0 && validThinking(query[i+1:]) {
		return query[:i], query[i+1:]
	}
	return query, ""
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func (a *app) resumeAndShow(path string) {
	if err := a.resume(path); err != nil {
		a.ui.Errorf("%v", err)
		return
	}
	a.replay()
}

// replay re-renders the whole resumed conversation through the same UI
// paths as a live turn: prompts, thinking, markdown answers, tool calls and
// their result previews. Nothing is clipped.
func (a *app) replay() {
	calls := map[string]llm.Block{} // tool_use id -> call, to label results
	for _, m := range a.ag.Messages {
		for _, b := range m.Blocks {
			switch {
			case m.Role == llm.RoleUser && b.Type == llm.BlockText:
				a.ui.Finish()
				switch text := strings.TrimSpace(b.Text); {
				case strings.HasPrefix(text, "<system-reminder>"):
				case strings.HasPrefix(text, mentionPrefix):
					ref, _, _ := strings.Cut(strings.TrimPrefix(text, mentionPrefix), `"`)
					a.ui.Dim("  📎 @%s", ref)
				case strings.HasPrefix(text, skillAttachPrefix):
					name, _, _ := strings.Cut(strings.TrimPrefix(text, skillAttachPrefix), `"`)
					a.ui.Dim("  ✧ skill:%s", name)
				case strings.HasPrefix(text, "<context-summary>"):
					a.ui.Dim("── earlier conversation was compacted; summary ──")
					summary := strings.TrimSuffix(strings.TrimPrefix(text, "<context-summary>"), "</context-summary>")
					for _, l := range strings.Split(strings.TrimSpace(summary), "\n") {
						a.ui.Dim("%s", l)
					}
				default:
					a.echoPrompt(displayPrompt(text))
				}
			case b.Type == llm.BlockImage:
				a.ui.Dim("  [image]")
			case b.Type == llm.BlockThinking && b.Text != "":
				a.ui.OnThinking(b.Text)
			case b.Type == llm.BlockText:
				a.ui.OnText(b.Text)
			case b.Type == llm.BlockToolUse:
				calls[b.ID] = b
				a.ui.OnToolCall(b.Name, b.Input)
			case b.Type == llm.BlockToolResult:
				c := calls[b.ToolUseID]
				a.ui.OnToolResult(c.Name, c.Input, b.Content, b.IsError)
			}
		}
	}
	a.ui.Finish()
	fmt.Fprintln(a.out)
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Format("Jan 02 2006")
}
