package extension

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeTego drives an extension over pipes, as tego does.
type fakeTego struct {
	t    *testing.T
	in   *io.PipeWriter // to the extension
	out  *bufio.Reader  // from it
	next int
	done chan error
}

func start(t *testing.T, x *Extension) *fakeTego {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	f := &fakeTego{t: t, in: inW, out: bufio.NewReader(outR), done: make(chan error, 1)}
	go func() { f.done <- x.serve(inR, outW); outW.Close() }()
	return f
}

func (f *fakeTego) send(m map[string]any) {
	m["jsonrpc"] = "2.0"
	data, _ := json.Marshal(m)
	if _, err := f.in.Write(append(data, '\n')); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeTego) read() map[string]any {
	f.t.Helper()
	line, err := f.out.ReadBytes('\n')
	if err != nil {
		f.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		f.t.Fatalf("%s: %v", line, err)
	}
	return m
}

// request sends a request and returns its answer, answering what the
// extension asks meanwhile with answer.
func (f *fakeTego) request(method string, params any, answer func(method string, params map[string]any) any) map[string]any {
	f.t.Helper()
	f.next++
	id := f.next
	f.send(map[string]any{"id": id, "method": method, "params": params})
	for {
		m := f.read()
		if meth, ok := m["method"].(string); ok {
			p, _ := m["params"].(map[string]any)
			if m["id"] != nil {
				f.send(map[string]any{"id": m["id"], "result": answer(meth, p)})
			} else {
				answer(meth, p)
			}
			continue
		}
		if int(m["id"].(float64)) != id {
			f.t.Fatalf("answer to %v, want %d", m["id"], id)
		}
		if e, ok := m["error"].(map[string]any); ok {
			return map[string]any{"error": e["message"]}
		}
		r, _ := m["result"].(map[string]any)
		return r
	}
}

func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestExtension(t *testing.T) {
	x := New("guard")
	x.On(ToolCall, func(e Event, ctx *Ctx) Result {
		if e.Tool == "bash" && strings.Contains(e.String("command"), "rm -rf") && !ctx.Confirm("Run it?", e.String("command")) {
			return Block("the user said no")
		}
		if e.Tool == "bash" {
			return ChangeInput(map[string]string{"command": e.String("command") + " # 1"})
		}
		return Pass()
	})
	x.On(ToolCall, func(e Event, ctx *Ctx) Result { // sees the first one's change
		if e.Tool == "bash" {
			return ChangeInput(map[string]string{"command": e.String("command") + " # 2"})
		}
		return Pass()
	})
	x.On(ToolResult, func(e Event, ctx *Ctx) Result { return ChangeOutput(strings.ToUpper(e.Output)) })
	x.On(Input, func(e Event, ctx *Ctx) Result {
		if e.Text == "ping" {
			ctx.Notify("pong")
			return Handled()
		}
		return ChangeText(e.Text + "!")
	})
	type colorIn struct {
		Why string `json:"why"`
	}
	x.Tool("pick_color", "Ask for a color", map[string]any{"type": "object", "properties": map[string]any{"why": map[string]string{"type": "string"}}},
		func(raw json.RawMessage, ctx *Ctx) (string, error) {
			var in colorIn
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			c, ok := ctx.Select("Color for "+in.Why, []string{"red", "green"})
			name, _ := ctx.Input("Name it", "")
			return fmt.Sprintf("%s %v %s in %s", c, ok, name, ctx.Cwd), nil
		})
	x.Tool("boom", "Fails", nil, func(json.RawMessage, *Ctx) (string, error) { return "", errors.New("it broke") })
	x.Tool("panics", "Panics", nil, func(json.RawMessage, *Ctx) (string, error) { panic("oops") })
	x.Command("hello", "Say hello", func(args string, ctx *Ctx) error {
		ctx.Warn("hello " + args)
		ctx.SendMessage("run the tests")
		return nil
	})

	f := start(t, x)
	var asked []string
	answer := func(method string, p map[string]any) any {
		asked = append(asked, method+" "+js(p))
		switch method {
		case "ui/confirm":
			return map[string]bool{"ok": false}
		case "ui/select":
			return map[string]int{"index": 1}
		case "ui/input":
			return map[string]any{"text": "moss", "cancelled": false}
		}
		return nil
	}

	init := f.request("initialize", map[string]any{"protocol_version": 1, "cwd": "/work", "session_id": "s1"}, answer)
	if js(init["events"]) != `["tool_call","tool_result","input"]` || init["name"] != "guard" ||
		!strings.Contains(js(init["tools"]), `"name":"pick_color"`) || !strings.Contains(js(init["tools"]), `"name":"boom","parameters":{"properties":{},"type":"object"}`) ||
		js(init["commands"]) != `[{"description":"Say hello","name":"hello"}]` {
		t.Fatalf("initialize %s", js(init))
	}
	event := func(name string, data map[string]any) map[string]any {
		return f.request("event", map[string]any{"event": name, "data": data}, answer)
	}
	if r := event(ToolCall, map[string]any{"tool": "bash", "input": map[string]string{"command": "rm -rf /"}}); js(r) != `{"block":true,"reason":"the user said no"}` {
		t.Errorf("rm -rf: %s", js(r))
	}
	if r := event(ToolCall, map[string]any{"tool": "bash", "input": map[string]string{"command": "ls"}}); js(r) != `{"input":{"command":"ls # 1 # 2"}}` {
		t.Errorf("ls: %s", js(r))
	}
	if r := event(ToolCall, map[string]any{"tool": "read", "input": map[string]string{"path": "a"}}); js(r) != `{}` {
		t.Errorf("read: %s", js(r))
	}
	if r := event(ToolResult, map[string]any{"tool": "read", "output": "abc"}); js(r) != `{"output":"ABC"}` {
		t.Errorf("result: %s", js(r))
	}
	if r := event(Input, map[string]any{"text": "fix it"}); js(r) != `{"text":"fix it!"}` {
		t.Errorf("input: %s", js(r))
	}
	if r := event(Input, map[string]any{"text": "ping"}); js(r) != `{"handled":true}` {
		t.Errorf("ping: %s", js(r))
	}
	tool := func(name string, input any) map[string]any {
		return f.request("tool/execute", map[string]any{"name": name, "input": input}, answer)
	}
	if r := tool("pick_color", map[string]string{"why": "the logo"}); js(r) != `{"content":"green true moss in /work"}` {
		t.Errorf("pick_color: %s", js(r))
	}
	if r := tool("boom", nil); js(r) != `{"content":"it broke","is_error":true}` {
		t.Errorf("boom: %s", js(r))
	}
	if r := tool("panics", nil); !strings.Contains(js(r), "panicked: oops") {
		t.Errorf("panics: %s", js(r))
	}
	if r := f.request("command/execute", map[string]any{"name": "hello", "args": "world"}, answer); js(r) != `{}` {
		t.Errorf("hello: %s", js(r))
	}
	// Notifications arrive in order before the command's answer.
	want := []string{
		`ui/confirm {"message":"rm -rf /","title":"Run it?"}`,
		`ui/notify {"level":"info","message":"pong"}`,
		`ui/select {"options":["red","green"],"title":"Color for the logo"}`,
		`ui/input {"placeholder":"","title":"Name it"}`,
		`ui/notify {"level":"warning","message":"hello world"}`,
		`session/sendMessage {"text":"run the tests"}`,
	}
	if strings.Join(asked, "\n") != strings.Join(want, "\n") {
		t.Errorf("asked:\n%s\nwant:\n%s", strings.Join(asked, "\n"), strings.Join(want, "\n"))
	}

	f.send(map[string]any{"method": "shutdown"})
	select {
	case err := <-f.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exit on shutdown")
	}
}

func TestRunOutsideTego(t *testing.T) {
	t.Setenv("TEGO_EXTENSION", "")
	if err := New("x").Run(); err == nil || !strings.Contains(err.Error(), "~/.tego/extensions") {
		t.Fatalf("err %v", err)
	}
}
