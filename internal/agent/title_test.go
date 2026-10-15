package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/akbxr/tego/internal/llm"
)

type titleProvider struct {
	reply string
	req   *llm.Request
}

func (p *titleProvider) Name() string { return "fake" }
func (p *titleProvider) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	p.req = req
	return &llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: p.reply}}}, nil
}

func TestGenerateTitle(t *testing.T) {
	p := &titleProvider{reply: "**\"Redesign billing dashboard page.\"**\nextra line"}
	got, err := GenerateTitle(context.Background(), p, "m", "/skill:frontend-design redesign /dashboard/billing")
	if err != nil || got != "Redesign billing dashboard page" {
		t.Fatalf("title = %q, %v", got, err)
	}
	if p.req.Model != "m" || len(p.req.Tools) != 0 || p.req.Thinking != "" || !strings.Contains(p.req.Messages[0].Blocks[0].Text, "/dashboard/billing") {
		t.Fatalf("request = %+v", p.req)
	}
	if _, err := GenerateTitle(context.Background(), &titleProvider{reply: "  \n"}, "m", "x"); err == nil {
		t.Fatal("an empty reply must be an error")
	}
	long := cleanTitle(strings.Repeat("word ", 30))
	if r := []rune(long); len(r) > 60 || !strings.HasSuffix(long, "…") {
		t.Fatalf("long title = %q", long)
	}
}
