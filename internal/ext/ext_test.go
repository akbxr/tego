package ext

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost records what extensions ask of tego and answers as told.
type fakeHost struct {
	mu       sync.Mutex
	notes    []string
	sent     []string
	confirm  bool
	selected int
}

func (h *fakeHost) Notify(ext, msg, level string) {
	h.mu.Lock()
	h.notes = append(h.notes, ext+":"+level+":"+msg)
	h.mu.Unlock()
}
func (h *fakeHost) Confirm(_ context.Context, ext, title, msg string) (bool, error) {
	return h.confirm, nil
}
func (h *fakeHost) Select(_ context.Context, ext, title string, opts []string) (int, error) {
	return h.selected, nil
}
func (h *fakeHost) Input(_ context.Context, ext, title, ph string) (string, bool, error) {
	return "typed", true, nil
}
func (h *fakeHost) SendMessage(ext, text string) {
	h.mu.Lock()
	h.sent = append(h.sent, ext+":"+text)
	h.mu.Unlock()
}
func (h *fakeHost) snapshot() ([]string, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.notes...), append([]string(nil), h.sent...)
}

func needJS(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bun"); err == nil {
		return
	}
	if _, err := exec.LookPath("node"); err == nil {
		return
	}
	t.Skip("needs bun or node")
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

const guardTS = `
type Call = { tool: string; input: any; agent: string };
export default function (tego: any) {
  tego.on("tool_call", async (e: Call, ctx: any) => {
    if (e.tool === "bash" && /rm -rf/.test(e.input.command)) {
      if (!(await ctx.ui.confirm("Dangerous", e.input.command))) return { block: true, reason: "rm -rf refused" };
    }
    if (e.tool === "bash") return { input: { ...e.input, command: e.input.command + " # checked" } };
  });
  tego.on("tool_result", (e: any) => e.tool === "read" ? { output: e.output.replace(/SECRET/g, "[redacted]") } : undefined);
  tego.on("input", (e: any) => e.text === "ping" ? { handled: true } : { text: e.text + " (please be brief)" });
  tego.on("turn_end", (e: any, ctx: any) => ctx.ui.notify("turn ended: " + e.text));
  tego.registerTool({
    name: "pick_color", description: "Ask the user for a color",
    parameters: { type: "object", properties: { why: { type: "string" } } },
    execute: async (input: any, ctx: any) => "color " + (await ctx.ui.select("Color for " + input.why, ["red", "green"])),
  });
  tego.registerTool({ name: "boom", description: "Fails", execute: async () => { throw new Error("it broke"); } });
  tego.registerCommand("hello", { description: "Say hello", handler: async (args: string, ctx: any) => {
    ctx.ui.notify("hello " + args + " from " + ctx.cwd);
    ctx.sendMessage("run the tests");
  }});
  console.log("this goes to the log, not the protocol");
}
`

func TestTypeScriptExtension(t *testing.T) {
	needJS(t)
	home, cwd := t.TempDir(), t.TempDir()
	write(t, filepath.Join(home, "extensions", "guard.ts"), guardTS)
	h := &fakeHost{selected: 1}
	m := Start(Discover(home, cwd), Options{Home: home, Cwd: cwd, SessionID: "s1", Version: "test", Host: h})
	defer m.Close()
	m.Wait(20 * time.Second)
	st := m.Status()
	if len(st) != 1 || st[0].State != StateRunning || st[0].Name != "guard" ||
		!slices.Equal(st[0].Tools, []string{"pick_color", "boom"}) || !slices.Equal(st[0].Commands, []string{"hello"}) {
		t.Fatalf("status %+v", st)
	}
	ctx := context.Background()

	// tool_call: blocked after the user says no; otherwise the input changes.
	in, why := m.BeforeTool(ctx, "", "bash", json.RawMessage(`{"command":"rm -rf /tmp/x"}`))
	if why != "guard: rm -rf refused" || in != nil {
		t.Fatalf("rm -rf: input %s, block %q", in, why)
	}
	h.confirm = true
	in, why = m.BeforeTool(ctx, "Sub", "bash", json.RawMessage(`{"command":"ls"}`))
	if why != "" || string(in) != `{"command":"ls # checked"}` {
		t.Fatalf("ls: input %s, block %q", in, why)
	}
	if in, why := m.BeforeTool(ctx, "", "read", json.RawMessage(`{"path":"a"}`)); in != nil || why != "" {
		t.Fatalf("read: %s %q", in, why)
	}
	// tool_result
	if out, ch := m.AfterTool(ctx, "", "read", nil, "key=SECRET", false); !ch || out != "key=[redacted]" {
		t.Fatalf("redact: %q %v", out, ch)
	}
	if out, ch := m.AfterTool(ctx, "", "grep", nil, "SECRET", false); ch || out != "SECRET" {
		t.Fatalf("grep result changed: %q", out)
	}
	// input
	if text, handled := m.Input(ctx, "fix it"); handled || text != "fix it (please be brief)" {
		t.Fatalf("input %q %v", text, handled)
	}
	if _, handled := m.Input(ctx, "ping"); !handled {
		t.Fatal("ping not handled")
	}
	// tools
	tl := m.Tools(func(n string) bool { return n == "read" })
	if len(tl) != 2 {
		t.Fatalf("tools %d", len(tl))
	}
	if d := tl[0].Def(); d.Name != "pick_color" || !strings.Contains(string(d.Schema), `"why"`) {
		t.Fatalf("def %+v", d)
	}
	if out, err := tl[0].Run(ctx, json.RawMessage(`{"why":"the logo"}`)); err != nil || out != "color green" {
		t.Fatalf("pick_color: %q %v", out, err)
	}
	if _, err := tl[1].Run(ctx, nil); err == nil || err.Error() != "it broke" {
		t.Fatalf("boom: %v", err)
	}
	// commands, notifications, sendMessage
	if err := m.RunCommand(ctx, "hello", "world"); err != nil {
		t.Fatal(err)
	}
	m.Emit(EventTurnEnd, map[string]any{"text": "done"})
	waitUntil(t, func() bool { n, s := h.snapshot(); return len(n) == 2 && len(s) == 1 })
	notes, sent := h.snapshot()
	if notes[0] != "guard:info:hello world from "+cwd || notes[1] != "guard:info:turn ended: done" || sent[0] != "guard:run the tests" {
		t.Fatalf("notes %q sent %q", notes, sent)
	}
	if err := m.RunCommand(ctx, "nope", ""); err == nil {
		t.Fatal("unknown command ran")
	}
	log, _ := os.ReadFile(filepath.Join(home, "logs", "ext-guard.log"))
	if !strings.Contains(string(log), "this goes to the log") {
		t.Fatalf("log %q", log)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out")
}

// A project's extension runs only once trusted, and again needs trust when
// it changes. One that fails to load says why.
func TestProjectExtensionsNeedTrust(t *testing.T) {
	needJS(t)
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	home := filepath.Join(userHome, ".tego")
	repo := filepath.Join(userHome, "code", "repo")
	write(t, filepath.Join(repo, ".tego", "extensions", "proj.js"), `export default (tego) => tego.registerCommand("p", () => {});`)
	write(t, filepath.Join(repo, ".tego", "extensions", "broken.mjs"), `throw new Error("bad extension");`)
	write(t, filepath.Join(repo, ".tego", "extensions", "types.d.ts"), `export {}`)
	write(t, filepath.Join(repo, ".tego", "extensions", "notes.md"), `not one`)

	cands := Discover(home, repo)
	if len(cands) != 2 || cands[0].Name() != "broken" || cands[1].Name() != "proj" || !cands[1].Project {
		t.Fatalf("candidates %+v", cands)
	}
	m := Start(cands, Options{Home: home, Cwd: repo, Host: &fakeHost{}})
	if st := m.Status(); st[0].State != StateUntrusted || st[1].State != StateUntrusted {
		t.Fatalf("before trust %+v", st)
	}
	if got := m.Trust(); !slices.Equal(got, []string{"broken", "proj"}) {
		t.Fatalf("trusted %v", got)
	}
	m.Wait(20 * time.Second)
	st := m.Status()
	if st[1].State != StateRunning || st[0].State != StateFailed || !strings.Contains(st[0].Err, "bad extension") {
		t.Fatalf("after trust %+v", st)
	}
	m.Close()

	// Trusted next time; changed, untrusted again.
	m = Start(Discover(home, repo), Options{Home: home, Cwd: repo, Host: &fakeHost{}})
	m.Wait(20 * time.Second)
	if st := m.Status(); st[1].State != StateRunning {
		t.Fatalf("second session %+v", st)
	}
	m.Close()
	write(t, filepath.Join(repo, ".tego", "extensions", "proj.js"), `export default (tego) => tego.registerCommand("q", () => {});`)
	m = Start(Discover(home, repo), Options{Home: home, Cwd: repo, Host: &fakeHost{}})
	defer m.Close()
	if st := m.Status(); st[1].State != StateUntrusted {
		t.Fatalf("changed file still trusted %+v", st)
	}
}

// codeBlock is the first ```lang block of the docs.
func codeBlock(t *testing.T, lang string) string {
	t.Helper()
	data, err := os.ReadFile("../../docs/extensions.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(data), "```"+lang+"\n")
	block, _, ok2 := strings.Cut(rest, "```")
	if !ok || !ok2 {
		t.Fatalf("no %s block in the docs", lang)
	}
	return block
}

// The examples in docs/extensions.md run as written.
func TestDocsExamples(t *testing.T) {
	needJS(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("needs python3")
	}
	home, cwd := t.TempDir(), t.TempDir()
	write(t, filepath.Join(home, "extensions", "guard.ts"), codeBlock(t, "ts"))
	write(t, filepath.Join(home, "extensions", "nogit.py"), codeBlock(t, "python"))
	h := &fakeHost{confirm: false}
	m := Start(Discover(home, cwd), Options{Home: home, Cwd: cwd, Host: h})
	defer m.Close()
	m.Wait(20 * time.Second)
	st := m.Status()
	if len(st) != 2 || st[0].State != StateRunning || st[1].State != StateRunning ||
		!slices.Equal(st[0].Tools, []string{"deploy_status"}) || !slices.Equal(st[0].Commands, []string{"review"}) {
		t.Fatalf("status %+v", st)
	}
	ctx := context.Background()
	if _, why := m.BeforeTool(ctx, "", "bash", json.RawMessage(`{"command":"rm -rf build"}`)); why != "guard: the user said no" {
		t.Errorf("rm -rf: %q", why)
	}
	if _, why := m.BeforeTool(ctx, "", "bash", json.RawMessage(`{"command":"git push origin main"}`)); why != "nogit: push yourself" {
		t.Errorf("git push: %q", why)
	}
	if _, why := m.BeforeTool(ctx, "", "bash", json.RawMessage(`{"command":"ls"}`)); why != "" {
		t.Errorf("ls blocked: %q", why)
	}
	if err := m.RunCommand(ctx, "review", "carefully"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { _, s := h.snapshot(); return len(s) == 1 })
	if _, s := h.snapshot(); s[0] != "guard:Review the staged changes (git diff --cached). carefully" {
		t.Errorf("sent %q", s)
	}
}

func needGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("needs go")
	}
}

// A Go extension (the docs' example) is built once, runs, and is built
// again when it changes; a broken one says what go build said.
func TestGoExtension(t *testing.T) {
	needGo(t)
	home, cwd := t.TempDir(), t.TempDir()
	src := filepath.Join(home, "extensions", "guard.go")
	write(t, src, codeBlock(t, "go"))
	h := &fakeHost{}
	m := Start(Discover(home, cwd), Options{Home: home, Cwd: cwd, Host: h})
	m.Wait(3 * time.Minute)
	st := m.Status()
	if len(st) != 1 || st[0].State != StateRunning || !slices.Equal(st[0].Tools, []string{"now"}) || !slices.Equal(st[0].Commands, []string{"review"}) {
		t.Fatalf("status %+v", st)
	}
	ctx := context.Background()
	if _, why := m.BeforeTool(ctx, "", "bash", json.RawMessage(`{"command":"rm -rf build"}`)); why != "guard: the user said no" {
		t.Errorf("rm -rf: %q", why)
	}
	if out, err := m.Tools(nil)[0].Run(ctx, json.RawMessage(`{"zone":"UTC"}`)); err != nil || !strings.HasSuffix(out, "UTC") {
		t.Errorf("now: %q %v", out, err)
	}
	if _, err := m.Tools(nil)[0].Run(ctx, json.RawMessage(`{"zone":"Nowhere/City"}`)); err == nil {
		t.Error("bad zone: no error")
	}
	m.Close()
	bins, _ := filepath.Glob(filepath.Join(home, "cache", "ext-bin", "guard-*"))
	if len(bins) != 1 {
		t.Fatalf("binaries %v", bins)
	}

	// Unchanged: the same binary, no build.
	before, _ := os.Stat(bins[0])
	m = Start(Discover(home, cwd), Options{Home: home, Cwd: cwd, Host: h})
	m.Wait(time.Minute)
	m.Close()
	if after, err := os.Stat(bins[0]); err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("rebuilt without a change")
	}

	// Changed: built again, the old binary gone. Broken: go build's error.
	write(t, src, strings.Replace(codeBlock(t, "go"), `"guard"`, `"guard2"`, 1))
	m = Start(Discover(home, cwd), Options{Home: home, Cwd: cwd, Host: h})
	m.Wait(3 * time.Minute)
	m.Close()
	if bins2, _ := filepath.Glob(filepath.Join(home, "cache", "ext-bin", "guard-*")); len(bins2) != 1 || bins2[0] == bins[0] {
		t.Fatalf("after a change: %v", bins2)
	}
	write(t, src, "package main\n\nfunc main() { undefined() }\n")
	m = Start(Discover(home, cwd), Options{Home: home, Cwd: cwd, Host: h})
	m.Wait(3 * time.Minute)
	defer m.Close()
	if st := m.Status(); st[0].State != StateFailed || !strings.Contains(st[0].Err, "go build:") || !strings.Contains(st[0].Err, "undefined") {
		t.Fatalf("broken: %+v", st)
	}
}

// A folder with its own go.mod is built as it is.
func TestGoModuleExtension(t *testing.T) {
	needGo(t)
	home, cwd := t.TempDir(), t.TempDir()
	dir := filepath.Join(home, "extensions", "raw")
	write(t, filepath.Join(dir, "go.mod"), "module example.com/raw\n\ngo 1.22\n")
	write(t, filepath.Join(dir, "main.go"), `package main

import (
	"bufio"
	"encoding/json"
	"os"
)

// The protocol by hand: only initialize and shutdown.
func main() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var m map[string]any
		json.Unmarshal(sc.Bytes(), &m)
		if m["method"] == "initialize" {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{
				"name": "raw", "commands": []map[string]string{{"name": "raw", "description": "by hand"}}}})
			os.Stdout.Write(append(b, '\n'))
		}
		if m["method"] == "shutdown" {
			return
		}
	}
}
`)
	m := Start(Discover(home, cwd), Options{Home: home, Cwd: cwd, Host: &fakeHost{}})
	defer m.Close()
	m.Wait(3 * time.Minute)
	if st := m.Status(); len(st) != 1 || st[0].State != StateRunning || !slices.Equal(st[0].Commands, []string{"raw"}) {
		t.Fatalf("status %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.sum")); err == nil {
		t.Error("the build wrote into the extension's folder")
	}
}

// An extension that dies stops answering, and its hooks are skipped.
func TestExtensionExits(t *testing.T) {
	needJS(t)
	home, cwd := t.TempDir(), t.TempDir()
	write(t, filepath.Join(home, "extensions", "quitter.js"), `export default (tego) => {
  tego.on("tool_call", () => { process.exit(3); });
  tego.registerTool({ name: "t", execute: () => "x" });
}`)
	m := Start(Discover(home, cwd), Options{Home: home, Cwd: cwd, Host: &fakeHost{}})
	defer m.Close()
	m.Wait(20 * time.Second)
	tl := m.Tools(nil)
	if in, why := m.BeforeTool(context.Background(), "", "bash", json.RawMessage(`{}`)); in != nil || why != "" {
		t.Fatalf("dead extension blocked: %s %q", in, why)
	}
	waitUntil(t, func() bool { return m.Status()[0].State == StateFailed })
	if _, err := tl[0].Run(context.Background(), nil); err == nil {
		t.Fatal("dead extension's tool ran")
	}
	if len(m.Tools(nil)) != 0 {
		t.Fatal("dead extension's tools still offered")
	}
}
