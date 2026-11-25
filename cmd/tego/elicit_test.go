package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/acp"
	"github.com/akbxr/tego/internal/mcp"
)

func confirmForm(t *testing.T, schema string) *mcp.Elicitation {
	t.Helper()
	fields, err := mcp.ParseSchema(json.RawMessage(schema))
	if err != nil {
		t.Fatal(err)
	}
	return &mcp.Elicitation{Server: "db", Mode: "form", Message: "Drop the table?", Fields: fields, Schema: json.RawMessage(schema)}
}

// An editor that shows forms gets the server's (without $schema), named
// after the server; one that doesn't gets a permission request for a yes or
// no, and a word that a larger form was cancelled.
func TestACPElicitation(t *testing.T) {
	const schema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"ok":{"type":"boolean","title":"Drop it"}},"required":["ok"]}`
	caps := acp.ClientCapabilities{Elicitation: &acp.ElicitationCapabilities{Form: &struct{}{}, URL: &struct{}{}}}
	ed := startACPWith(t, t.TempDir(), &scripted{}, caps)
	var setup acp.SessionSetup
	if err := ed.call("session/new", acp.NewSessionRequest{Cwd: t.TempDir(), MCPServers: []acp.MCPServer{}}, &setup); err != nil {
		t.Fatal(err)
	}
	sess := ed.srv.session(setup.SessionID)
	r := sess.elicit(context.Background(), confirmForm(t, schema))
	if r.Action != mcp.ElicitAccept || r.Content["ok"] != true {
		t.Fatalf("answer: %+v", r)
	}
	ed.mu.Lock()
	if len(ed.elicits) != 1 {
		t.Fatalf("asked %d times", len(ed.elicits))
	}
	req := ed.elicits[0]
	ed.mu.Unlock()
	if req.SessionID != setup.SessionID || req.Mode != "form" || req.Message != "[db] Drop the table?" || strings.Contains(string(req.RequestedSchema), "$schema") {
		t.Fatalf("request: %+v %s", req, req.RequestedSchema)
	}

	plain := startACPWith(t, t.TempDir(), &scripted{}, acp.ClientCapabilities{})
	if err := plain.call("session/new", acp.NewSessionRequest{Cwd: t.TempDir(), MCPServers: []acp.MCPServer{}}, &setup); err != nil {
		t.Fatal(err)
	}
	sess = plain.srv.session(setup.SessionID)
	plain.answer = "no"
	if r := sess.elicit(context.Background(), confirmForm(t, schema)); r.Action != mcp.ElicitAccept || r.Content["ok"] != false {
		t.Fatalf("yes/no as a permission: %+v", r)
	}
	plain.mu.Lock()
	if len(plain.asked) != 1 || plain.asked[0].ToolCall.Title != "[db] Drop the table?" || len(plain.asked[0].Options) != 3 {
		t.Fatalf("permission: %+v", plain.asked)
	}
	plain.mu.Unlock()
	big := confirmForm(t, `{"type":"object","properties":{"name":{"type":"string"},"ok":{"type":"boolean"}}}`)
	if r := sess.elicit(context.Background(), big); r.Action != mcp.ElicitCancel {
		t.Fatalf("a form the editor can't show: %+v", r)
	}
	waitFor(t, 5*time.Second, "a word that the form was cancelled", func() bool {
		for _, u := range plain.seen("agent_thought_chunk") {
			if strings.Contains(u["content"].(map[string]any)["text"].(string), "asked for a form this editor can't show") {
				return true
			}
		}
		return false
	})
}

func TestURLHost(t *testing.T) {
	if h, w := urlHost("https://github.com/login/device?x=1"); h != "github.com" || w != "" {
		t.Fatalf("%q %q", h, w)
	}
	if _, w := urlHost("https://xn--gthub-n4a.com/"); w == "" {
		t.Fatal("punycode not flagged")
	}
}
