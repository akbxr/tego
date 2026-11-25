package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/akbxr/tego/internal/acp"
	"github.com/akbxr/tego/internal/lineedit"
	"github.com/akbxr/tego/internal/mcp"
	"github.com/akbxr/tego/internal/tools"
)

// An MCP server may ask the user something in the middle of a call
// (elicitation): to confirm a paid or destructive action, fill in a short
// form, or finish a step in the browser. In the terminal each field gets a
// picker or a text field, and the answers are shown to send, change or
// decline; Esc cancels. An editor shows the form itself when it can
// (elicitation/create), else a permission request where one will do.

var errElicitCancel = errors.New("cancelled")

// elicit asks the user in the terminal.
func (a *app) elicit(ctx context.Context, e *mcp.Elicitation) mcp.ElicitResult {
	if a.tui == nil {
		return mcp.ElicitResult{Action: mcp.ElicitCancel}
	}
	defer a.blockStatus("question", "["+e.Server+"] "+e.Message)()
	a.notify("MCP server " + e.Server + " asks something")
	if e.Mode == "url" {
		return a.elicitURL(ctx, e)
	}
	return a.elicitForm(ctx, e)
}

// elicitURL asks to open a page: the whole address shown, its host named,
// and nothing opened without a yes.
func (a *app) elicitURL(ctx context.Context, e *mcp.Elicitation) mcp.ElicitResult {
	host, warn := urlHost(e.URL)
	a.ui.OnInfo(fmt.Sprintf("◇ MCP server %s asks you to open a page in your browser: %s\n  %s\n  host: %s%s", e.Server, e.Message, e.URL, host, warn))
	items := []lineedit.PickItem{{Label: "Open in browser", Detail: host}, {Label: "Decline"}}
	idx, err := a.pickWhenFree(ctx, func() (int, error) { return a.tui.Pick("["+e.Server+"] open "+host+"?", items, "", 0) })
	switch {
	case err != nil:
		return mcp.ElicitResult{Action: mcp.ElicitCancel}
	case idx != 0:
		return mcp.ElicitResult{Action: mcp.ElicitDecline}
	}
	openURL(e.URL)
	return mcp.ElicitResult{Action: mcp.ElicitAccept}
}

// urlHost is u's host, and a warning when it may not be what it looks like
// (punycode, letters outside ASCII).
func urlHost(u string) (string, string) {
	p, err := url.Parse(u)
	if err != nil {
		return u, " ⚠ not a valid address"
	}
	host := p.Hostname()
	odd := strings.Contains(host, "xn--")
	for _, r := range host {
		odd = odd || r > unicode.MaxASCII
	}
	if odd {
		return host, " ⚠ the host has letters that may look like others (punycode): check it"
	}
	return host, ""
}

// elicitForm asks for a form's fields one at a time, then shows the answers
// to send, change or decline.
func (a *app) elicitForm(ctx context.Context, e *mcp.Elicitation) mcp.ElicitResult {
	a.ui.OnInfo(fmt.Sprintf("◇ MCP server %s asks: %s", e.Server, e.Message))
	if len(e.Fields) == 0 {
		items := []lineedit.PickItem{{Label: "Accept"}, {Label: "Decline"}}
		idx, err := a.pickWhenFree(ctx, func() (int, error) { return a.tui.Pick("["+e.Server+"] "+oneLineCut(e.Message, 80), items, "", 0) })
		switch {
		case err != nil:
			return mcp.ElicitResult{Action: mcp.ElicitCancel}
		case idx != 0:
			return mcp.ElicitResult{Action: mcp.ElicitDecline}
		}
		return mcp.ElicitResult{Action: mcp.ElicitAccept}
	}
	content := map[string]any{}
	for _, f := range e.Fields {
		if f.Default != nil {
			if v, err := f.Parse(f.DefaultText()); err == nil && v != nil {
				content[f.Name] = v
			}
		}
	}
	for {
		for _, f := range e.Fields {
			v, err := a.askField(ctx, e.Server, f, content[f.Name])
			if err != nil {
				return mcp.ElicitResult{Action: mcp.ElicitCancel}
			}
			if v == nil {
				delete(content, f.Name)
			} else {
				content[f.Name] = v
			}
		}
		summary := mcp.Summary(e.Fields, content)
		a.ui.Dim("%s", "  "+strings.ReplaceAll(summary, "\n", "\n  "))
		items := []lineedit.PickItem{{Label: "Send", Detail: oneLineCut(summary, 80)}, {Label: "Change the answers"}, {Label: "Decline", Detail: "send nothing"}}
		idx, err := a.pickWhenFree(ctx, func() (int, error) { return a.tui.Pick("["+e.Server+"] send these answers?", items, "", 0) })
		switch {
		case err != nil:
			return mcp.ElicitResult{Action: mcp.ElicitCancel}
		case idx == 0:
			return mcp.ElicitResult{Action: mcp.ElicitAccept, Content: content}
		case idx == 2:
			return mcp.ElicitResult{Action: mcp.ElicitDecline}
		}
	}
}

// askField asks for one field, cur its value so far (nil: none); nil and
// no error is no value. Esc is errElicitCancel.
func (a *app) askField(ctx context.Context, server string, f mcp.Field, cur any) (any, error) {
	title := "[" + server + "] " + f.Label()
	if f.Description != "" {
		title += " — " + oneLineCut(f.Description, 100)
	}
	pick := func(items []lineedit.PickItem, sel int) (int, error) {
		return a.pickWhenFree(ctx, func() (int, error) { return a.tui.Pick(title, items, "", sel) })
	}
	switch {
	case f.Type == "boolean":
		items := []lineedit.PickItem{{Label: "Yes"}, {Label: "No"}}
		if !f.Required {
			items = append(items, lineedit.PickItem{Label: "No answer"})
		}
		sel := 0
		if b, ok := cur.(bool); ok && !b {
			sel = 1
		}
		idx, err := pick(items, sel)
		switch {
		case err != nil:
			return nil, errElicitCancel
		case idx == 2:
			return nil, nil
		}
		return idx == 0, nil
	case len(f.Options) > 0 && !f.Multi:
		items := make([]lineedit.PickItem, 0, len(f.Options)+1)
		sel := 0
		for i, o := range f.Options {
			it := lineedit.PickItem{Label: o.Title}
			if o.Title != o.Value {
				it.Detail = o.Value
			}
			if cur == o.Value {
				sel = i
			}
			items = append(items, it)
		}
		if !f.Required {
			items = append(items, lineedit.PickItem{Label: "No answer"})
		}
		idx, err := pick(items, sel)
		switch {
		case err != nil:
			return nil, errElicitCancel
		case idx >= len(f.Options):
			return nil, nil
		}
		return f.Options[idx].Value, nil
	}
	hint := fieldHint(f)
	initial := ""
	if cur != nil {
		initial = valueText(cur)
	}
	prompt := title + " (" + hint + ")"
	for {
		var s string
		_, err := a.pickWhenFree(ctx, func() (int, error) {
			var err error
			s, err = a.tui.Ask(prompt, initial)
			return 0, err
		})
		if err != nil {
			return nil, errElicitCancel
		}
		v, perr := f.Parse(s)
		if perr == nil {
			return v, nil
		}
		initial, prompt = s, title+" ("+hint+") ✗ "+perr.Error()
	}
}

// fieldHint says what a text field takes.
func fieldHint(f mcp.Field) string {
	var parts []string
	switch {
	case f.Multi:
		var vals []string
		for _, o := range f.Options {
			vals = append(vals, o.Value)
		}
		parts = append(parts, "comma-separated: "+strings.Join(vals, ", "))
	case f.Type == "integer":
		parts = append(parts, "a whole number")
	case f.Type == "number":
		parts = append(parts, "a number")
	case f.Format != "":
		parts = append(parts, f.Format)
	}
	if f.Min != nil {
		parts = append(parts, fmt.Sprintf("≥ %v", *f.Min))
	}
	if f.Max != nil {
		parts = append(parts, fmt.Sprintf("≤ %v", *f.Max))
	}
	if f.Required {
		parts = append(parts, "required")
	} else {
		parts = append(parts, "optional: Enter leaves it out")
	}
	return strings.Join(parts, ", ")
}

func valueText(v any) string {
	switch x := v.(type) {
	case []any:
		var parts []string
		for _, p := range x {
			parts = append(parts, fmt.Sprint(p))
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprint(v)
}

// elicit asks the user in an editor: elicitation/create when the editor
// shows that kind; else a permission request, which can take a yes or no,
// a choice, or opening a page (here, on this machine). A form it can't take
// is cancelled with a word to the user.
func (sess *acpSession) elicit(ctx context.Context, e *mcp.Elicitation) mcp.ElicitResult {
	cancel := mcp.ElicitResult{Action: mcp.ElicitCancel}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	defer context.AfterFunc(sess.life, stop)()
	if caps := sess.srv.caps.Elicitation; caps != nil && ((e.Mode == "url" && caps.URL != nil) || (e.Mode == "form" && caps.Form != nil)) {
		req := acp.ElicitationRequest{SessionID: sess.id, Mode: e.Mode, Message: "[" + e.Server + "] " + e.Message}
		if id := tools.CallID(ctx); id != "" && sess.callSession(id) == sess.id {
			req.ToolCallID = id
		}
		if e.Mode == "url" {
			req.URL, req.ElicitationID = e.URL, e.ID
			if req.ElicitationID == "" {
				req.ElicitationID = fmt.Sprintf("tego-%d", sess.asks.Add(1))
			}
		} else {
			req.RequestedSchema = withoutSchemaKeyword(e.Schema)
		}
		var resp acp.ElicitationResponse
		if err := sess.srv.conn.Call(ctx, "elicitation/create", req, &resp); err != nil {
			return cancel
		}
		return mcp.ElicitResult{Action: resp.Action, Content: resp.Content}
	}
	title := "[" + e.Server + "] " + e.Message
	switch {
	case e.Mode == "url":
		host, warn := urlHost(e.URL)
		id := sess.ask(ctx, "["+e.Server+"] open "+host+" in your browser?", e.Message+"\n\n"+e.URL+warn, []acp.PermissionOption{
			{OptionID: "open", Name: "Open in browser", Kind: "allow_once"},
			{OptionID: "decline", Name: "Decline", Kind: "reject_once"},
		})
		switch id {
		case "open":
			openURL(e.URL)
			return mcp.ElicitResult{Action: mcp.ElicitAccept}
		case "decline":
			return mcp.ElicitResult{Action: mcp.ElicitDecline}
		}
		return cancel
	case len(e.Fields) == 0:
		id := sess.ask(ctx, title, "", []acp.PermissionOption{
			{OptionID: "accept", Name: "Accept", Kind: "allow_once"},
			{OptionID: "decline", Name: "Decline", Kind: "reject_once"},
		})
		return answerOf(id, nil)
	case len(e.Fields) == 1 && e.Fields[0].Type == "boolean":
		f := e.Fields[0]
		id := sess.ask(ctx, title, f.Label(), []acp.PermissionOption{
			{OptionID: "yes", Name: "Yes", Kind: "allow_once"},
			{OptionID: "no", Name: "No", Kind: "allow_once"},
			{OptionID: "decline", Name: "Decline", Kind: "reject_once"},
		})
		switch id {
		case "yes", "no":
			return mcp.ElicitResult{Action: mcp.ElicitAccept, Content: map[string]any{f.Name: id == "yes"}}
		}
		return answerOf(id, nil)
	case len(e.Fields) == 1 && len(e.Fields[0].Options) > 0 && !e.Fields[0].Multi:
		f := e.Fields[0]
		opts := make([]acp.PermissionOption, 0, len(f.Options)+1)
		for i, o := range f.Options {
			opts = append(opts, acp.PermissionOption{OptionID: fmt.Sprint(i), Name: o.Title, Kind: "allow_once"})
		}
		opts = append(opts, acp.PermissionOption{OptionID: "decline", Name: "Decline", Kind: "reject_once"})
		id := sess.ask(ctx, title, f.Label(), opts)
		for i, o := range f.Options {
			if id == fmt.Sprint(i) {
				return mcp.ElicitResult{Action: mcp.ElicitAccept, Content: map[string]any{f.Name: o.Value}}
			}
		}
		return answerOf(id, nil)
	}
	sess.OnInfo(fmt.Sprintf("MCP server %s asked for a form this editor can't show, so it was cancelled: %s", e.Server, e.Message))
	return cancel
}

// answerOf is a permission request's "accept"/"decline" as an answer.
func answerOf(id string, content map[string]any) mcp.ElicitResult {
	switch id {
	case "accept":
		return mcp.ElicitResult{Action: mcp.ElicitAccept, Content: content}
	case "decline":
		return mcp.ElicitResult{Action: mcp.ElicitDecline}
	}
	return mcp.ElicitResult{Action: mcp.ElicitCancel}
}

// withoutSchemaKeyword drops "$schema", which ACP's forms leave out.
func withoutSchemaKeyword(raw json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m["$schema"] == nil {
		return raw
	}
	delete(m, "$schema")
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}
