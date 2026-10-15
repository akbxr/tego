package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/tools"
)

func readCall(id, path string, extra ...string) llm.Block {
	in := map[string]any{"path": path}
	for i := 0; i+1 < len(extra); i += 2 {
		in[extra[i]] = json.Number(extra[i+1])
	}
	b, _ := json.Marshal(in)
	return call(id, "read", string(b))
}

func readsAgent(t *testing.T) (*Agent, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte("package a\n\nfunc A() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Agent{Tools: tools.Builtins(&tools.Env{Cwd: dir})}, p
}

func TestRepeatedReadOfUnchangedFile(t *testing.T) {
	a, p := readsAgent(t)
	runWith(t, a,
		assistant(readCall("r1", p)),
		assistant(readCall("r2", p)),
		assistant(readCall("r3", p)), // a repeat of a repeat still finds the content
		assistant(readCall("r4", p, "offset", "2")),
		assistant(text("done")),
	)
	first, _ := resultFor(a.Messages, "r1")
	if !strings.Contains(first.Content, "func A() int") {
		t.Fatalf("first read: %q", first.Content)
	}
	for _, id := range []string{"r2", "r3"} {
		if res, _ := resultFor(a.Messages, id); res.IsError || !strings.HasPrefix(res.Content, unchangedNote) || strings.Contains(res.Content, "func A") {
			t.Fatalf("%s: %+v", id, res)
		}
	}
	if res, _ := resultFor(a.Messages, "r4"); !strings.Contains(res.Content, "func A() int") {
		t.Fatalf("another range must be read: %q", res.Content)
	}
}

// changeTool rewrites a file, as an edit or a shell command would.
type changeTool struct{ path string }

func (t changeTool) Def() llm.Tool { return llm.Tool{Name: "change"} }
func (t changeTool) Run(_ context.Context, _ json.RawMessage) (string, error) {
	later := time.Now().Add(2 * time.Second) // a new mtime even on coarse clocks
	if err := os.WriteFile(t.path, []byte("package a\n\nfunc A() int { return 2 }\n"), 0o644); err != nil {
		return "", err
	}
	return "changed", os.Chtimes(t.path, later, later)
}

func TestReadAfterChangeIsRead(t *testing.T) {
	a, p := readsAgent(t)
	a.Tools = append(a.Tools, changeTool{p})
	runWith(t, a,
		assistant(readCall("r1", p)),
		assistant(call("c1", "change", `{}`)),
		assistant(readCall("r2", p)),
		assistant(text("done")),
	)
	if res, _ := resultFor(a.Messages, "r2"); !strings.Contains(res.Content, "return 2") {
		t.Fatalf("changed file not read again: %q", res.Content)
	}
}

func TestRepeatedReadWhenEarlierResultIsGone(t *testing.T) {
	a, p := readsAgent(t)
	st, _ := stampFile(p)
	setup := func() {
		a.Messages = []llm.Message{
			{Role: llm.RoleUser, Blocks: []llm.Block{text("go")}},
			assistant(readCall("r1", p)),
			{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolUseID: "r1", Content: "1\tpackage a"}}},
		}
		a.readStamps = map[string]fileStamp{"r1": st}
		a.clearMark = 0
	}
	again := readCall("r2", p)

	setup()
	if a.unchangedRead(again) == "" {
		t.Fatal("intact earlier read not recognised")
	}
	setup()
	a.Messages[2].Blocks[0].Content = elidedMarker + "read output: 1 lines, 0.0 KB…]"
	if a.unchangedRead(again) != "" {
		t.Fatal("elided result treated as still in context")
	}
	setup()
	a.clearMark = 3 // the provider cleared old tool results since
	if a.unchangedRead(again) != "" {
		t.Fatal("possibly cleared result treated as still in context")
	}
	setup()
	a.readStamps = nil // a resumed session: the file state of that read is unknown
	if a.unchangedRead(again) != "" {
		t.Fatal("read without a stamp treated as unchanged")
	}
	setup()
	a.Messages[2].Blocks[0].IsError = true
	if a.unchangedRead(again) != "" {
		t.Fatal("failed read treated as content")
	}
	setup()
	if a.unchangedRead(readCall("u", "https://example.com")) != "" {
		t.Fatal("URL deduplicated")
	}
}

func TestIdenticalReadsInOneBatchBothRun(t *testing.T) {
	a, p := readsAgent(t)
	runWith(t, a, assistant(readCall("r1", p), readCall("r2", p)), assistant(text("done")))
	for _, id := range []string{"r1", "r2"} {
		if res, _ := resultFor(a.Messages, id); !strings.Contains(res.Content, "func A() int") {
			t.Fatalf("%s: %q", id, res.Content)
		}
	}
}
