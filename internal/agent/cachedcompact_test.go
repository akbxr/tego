package agent

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

// cacheProvider plays a session whose requests read the prompt cache, and
// answers compactions: one that repeats the conversation (CachePrefix set)
// with cached, a transcript with plain.
type cacheProvider struct {
	replies []llm.Message
	reqs    []llm.Request
	cached  llm.Message // the reply to a cached compaction
	noCache bool        // usage reports no cache at all
	read    int         // cache read per request; 0 = 40k
}

func (p *cacheProvider) Name() string { return "fake" }
func (p *cacheProvider) Stream(_ context.Context, req *llm.Request, _ func(llm.Event)) (*llm.Message, error) {
	r := *req
	r.Messages = slices.Clone(req.Messages)
	p.reqs = append(p.reqs, r)
	read := p.read
	if read == 0 {
		read = 40_000
	}
	u := &llm.Usage{Input: 500, CacheRead: read, Output: 100}
	if p.noCache {
		u = &llm.Usage{Input: 500 + read, Output: 100}
	}
	switch {
	case req.Class == llm.ClassCompaction && req.CachePrefix > 0:
		m := p.cached
		m.Usage = u
		return &m, nil
	case req.Class == llm.ClassCompaction:
		return &llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{text("TRANSCRIPT SUMMARY")}, Usage: &llm.Usage{Input: 9000}}, nil
	}
	m := p.replies[0]
	p.replies = p.replies[1:]
	m.Usage = u
	return &m, nil
}

// cachedSession is a prompt worked through in six tool rounds with long
// answers, then a short final one.
func cachedSession(t *testing.T, prov *cacheProvider) *Agent {
	t.Helper()
	for i := range 6 {
		prov.replies = append(prov.replies, assistant(text(fmt.Sprintf("step %d: ", i)+strings.Repeat("reasoning about the code ", 300)),
			call(fmt.Sprintf("c%d", i), "read", `{"path":"a.go"}`)))
	}
	prov.replies = append(prov.replies, assistant(text("All done: the bug is fixed.")))
	if prov.cached.Blocks == nil {
		prov.cached = assistant(text("CACHED SUMMARY"))
	}
	ui := &infoUI{}
	a := &Agent{Provider: prov, UI: ui, Model: "m", System: "sys", ContextWindow: 200_000, KeepRecent: 3000,
		Tools: []tools.Tool{namedTool{name: "read"}}, Thinking: "off"}
	if err := a.Run(context.Background(), "fix the bug"); err != nil {
		t.Fatal(err)
	}
	return a
}

func lastInfo(a *Agent) string {
	infos := a.UI.(*infoUI).infos
	return infos[len(infos)-1]
}

// /compact right after a turn repeats the turn's last request, byte for
// byte, with one instruction after it, and quotes where the kept part
// begins.
func TestCompactReadsTheConversationsCache(t *testing.T) {
	prov := &cacheProvider{}
	a := cachedSession(t, prov)
	last := prov.reqs[len(prov.reqs)-1]
	if err := a.Compact(context.Background(), "the bug"); err != nil {
		t.Fatal(err)
	}
	got := prov.reqs[len(prov.reqs)-1]
	n := len(last.Messages)
	if got.Class != llm.ClassCompaction || got.CachePrefix != n {
		t.Fatalf("class %q, prefix %d (want %d)", got.Class, got.CachePrefix, n)
	}
	if got.System != last.System || !reflect.DeepEqual(got.Tools, last.Tools) || got.Model != last.Model || got.Thinking != last.Thinking {
		t.Fatal("the request's fields differ from the last one's")
	}
	if !reflect.DeepEqual(got.Messages[:n], last.Messages) {
		t.Fatal("the messages do not repeat the last request's")
	}
	if len(got.Messages) != n+2 || got.Messages[n].Role != llm.RoleAssistant {
		t.Fatalf("want the reply and the instruction after the prefix, got %d messages", len(got.Messages)-n)
	}
	instr := got.Messages[n+1].Blocks[0].Text
	for _, want := range []string{"<compaction-request>", "Do not call any tools", "Summarise the conversation before your message that ", "## Next steps", "Focus especially on: the bug"} {
		if !strings.Contains(instr, want) {
			t.Fatalf("instruction lacks %q:\n%s", want, instr)
		}
	}
	if !strings.Contains(a.Messages[0].Blocks[0].Text, "CACHED SUMMARY") {
		t.Fatalf("summary: %s", a.Messages[0].Blocks[0].Text)
	}
	if info := lastInfo(a); !strings.Contains(info, "summary from the conversation, 98% of ~40k tokens read from the prompt cache") {
		t.Fatalf("info %q", info)
	}
	// The kept part begins with the message the instruction points at.
	if w := pointAt(a.Messages[1]); !strings.Contains(instr, w) {
		t.Fatalf("instruction points at something else than %q", w)
	}
}

// When the request can't be shown to repeat the last one, or the cache may
// be gone, or the model calls a tool, the summary comes from a transcript.
func TestCompactFallsBackToATranscript(t *testing.T) {
	for _, c := range []struct {
		name, why string
		prep      func(a *Agent, p *cacheProvider)
	}{
		{"history changed", "the history changed since the last request", func(a *Agent, _ *cacheProvider) {
			a.Messages[0].Blocks[0].Text = "edited"
		}},
		{"cache expired", "the prompt cache has expired", func(a *Agent, _ *cacheProvider) {
			at := a.sent.at
			a.clock = func() time.Time { return at.Add(5 * time.Minute) }
		}},
		{"model changed", "the model changed", func(a *Agent, _ *cacheProvider) { a.Model = "other" }},
		{"no cache", "the provider reported no prompt cache", func(_ *Agent, p *cacheProvider) {}},
		{"a tool call", "the model called a tool instead", func(_ *Agent, p *cacheProvider) {
			p.cached = assistant(call("x", "read", `{}`))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			prov := &cacheProvider{noCache: c.name == "no cache"}
			if c.name == "a tool call" {
				prov.cached = assistant(call("x", "read", `{}`))
			}
			a := cachedSession(t, prov)
			c.prep(a, prov)
			if err := a.Compact(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			if got := prov.reqs[len(prov.reqs)-1]; got.CachePrefix != 0 || got.System != summaryPrompt {
				t.Fatalf("last request: prefix %d, system %.40q", got.CachePrefix, got.System)
			}
			if !strings.Contains(a.Messages[0].Blocks[0].Text, "TRANSCRIPT SUMMARY") {
				t.Fatal("not the transcript's summary")
			}
			if info := lastInfo(a); !strings.Contains(info, "summary from a transcript ("+c.why+")") {
				t.Fatalf("info %q", info)
			}
		})
	}
}

// A short conversation with a large cached prompt (big tool definitions,
// say): its transcript costs much less than reading it all.
func TestCompactSmallConversationUsesTheTranscript(t *testing.T) {
	prov := &cacheProvider{replies: []llm.Message{assistant(text("hi"))}, read: 150_000}
	prov.cached = assistant(text("CACHED SUMMARY"))
	a := &Agent{Provider: prov, UI: &infoUI{}, Model: "m", ContextWindow: 200_000}
	if err := a.Run(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if info := lastInfo(a); !strings.Contains(info, "summary from a transcript (cheaper here)") {
		t.Fatalf("info %q", info)
	}
}

// The instruction tells apart a kept message from earlier ones that begin
// the same, and points at a message of only tool calls by its first call.
func TestPointAt(t *testing.T) {
	same := assistant(text("Running the tests."), call("a", "bash", `{"command": "go test ./..."}`))
	msgs := []llm.Message{
		{Role: llm.RoleUser, Blocks: []llm.Block{text("<system-reminder>env</system-reminder>"), text("fix   the\nbug")}},
		same, {Role: llm.RoleUser, Blocks: []llm.Block{result("a", "FAIL")}},
		same, {Role: llm.RoleUser, Blocks: []llm.Block{result("a", "ok")}},
	}
	instr := cachedInstruction(msgs, msgs[3:], "")
	if !strings.Contains(instr, "before your message that begins: «Running the tests.» (occurrence 2 of such a message, not an earlier one). That message and the 1 after it") {
		t.Fatalf("instruction:\n%s", instr)
	}
	if got := pointAt(msgs[0]); got != "the user message that begins: «fix the bug»" {
		t.Fatalf("prompt: %q", got)
	}
	if got := pointAt(assistant(call("a", "read", `{"path": "a.go"}`))); got != `your message that calls the read tool with «{"path": "a.go"}»` {
		t.Fatalf("calls only: %q", got)
	}
	if got := pointAt(assistant(llm.Block{Type: llm.BlockThinking, Text: "hmm"})); got != "" {
		t.Fatalf("thinking only: %q", got)
	}
	if instr := cachedInstruction(msgs, nil, ""); !strings.Contains(instr, "Summarise the whole conversation above.") {
		t.Fatalf("no tail:\n%s", instr)
	}
}
