// Package extension writes tego extensions in Go. An extension is a
// program tego runs beside a session; this package speaks tego's protocol
// (JSON-RPC over stdin and stdout) for it:
//
//	package main
//
//	import (
//		"strings"
//
//		"github.com/akbxr/tego/extension"
//	)
//
//	func main() {
//		x := extension.New("guard")
//		x.On("tool_call", func(e extension.Event, ctx *extension.Ctx) extension.Result {
//			if e.Tool == "bash" && strings.Contains(e.String("command"), "rm -rf") && !ctx.Confirm("Run rm -rf?", e.String("command")) {
//				return extension.Block("the user said no")
//			}
//			return extension.Pass()
//		})
//		x.Run()
//	}
//
// Put the file (or a folder with main.go) in ~/.tego/extensions/ or a
// project's .tego/extensions/: tego builds it with go build, with this
// package as it ships in that tego, and builds it again when it changes.
// A folder with its own go.mod is built as it is, with its own
// dependencies.
//
// stdout belongs to the protocol: once Run starts, fmt.Println and the
// like write to stderr, which tego keeps in ~/.tego/logs/ext-<name>.log.
package extension

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// Events (see Extension.On).
const (
	SessionStart = "session_start" // Cwd, SessionID, Model
	ModelChange  = "model_change"  // Model: the session switched to it (/model)
	Input        = "input"         // Text; may ChangeText or Handled
	ToolCall     = "tool_call"     // Tool, Input, Agent; may Block or ChangeInput
	ToolResult   = "tool_result"   // Tool, Input, Output, IsError, Agent; may ChangeOutput
	TurnEnd      = "turn_end"      // Text: the final answer
	SessionEnd   = "session_end"
)

// Model is the session's model.
type Model struct {
	Provider string `json:"provider"`   // "anthropic"
	ID       string `json:"model"`      // "claude-opus-5-5"
	Name     string `json:"model_name"` // "Opus 5.5", as tego shows it
}

// Event is what an event handler gets; the fields set depend on the event.
type Event struct {
	Name      string
	Model     Model           // session_start, model_change
	Tool      string          // tool_call, tool_result
	Input     json.RawMessage // the tool's input
	Output    string          // tool_result
	IsError   bool            // tool_result
	Agent     string          // "" for the session's agent, else the subagent's name
	Text      string          // input: the prompt; turn_end: the answer
	Cwd       string          // session_start
	SessionID string          // session_start
}

// String is the string field key of the tool's input ("" if none).
func (e Event) String(key string) string {
	var m map[string]any
	if json.Unmarshal(e.Input, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// Decode decodes the tool's input into v.
func (e Event) Decode(v any) error { return json.Unmarshal(e.Input, v) }

// Result is what an event handler returns: Pass, or a change.
type Result struct {
	block   bool
	reason  string
	input   any
	output  *string
	text    *string
	handled bool
}

// Pass changes nothing.
func Pass() Result { return Result{} }

// Block stops a tool call (tool_call); the model sees the reason.
func Block(reason string) Result { return Result{block: true, reason: reason} }

// ChangeInput gives a tool call another input (tool_call); v is marshalled
// to JSON.
func ChangeInput(v any) Result { return Result{input: v} }

// ChangeOutput gives a tool result another output (tool_result).
func ChangeOutput(s string) Result { return Result{output: &s} }

// ChangeText changes the prompt (input).
func ChangeText(s string) Result { return Result{text: &s} }

// Handled takes the prompt: nothing is sent to the model (input).
func Handled() Result { return Result{handled: true} }

// Ctx is how handlers, tools and commands reach tego and the user.
type Ctx struct {
	Cwd       string
	SessionID string
	Model     Model // the session's, as of this call
	x         *Extension
}

// Notify shows a message ("info"; see Warn and Error).
func (c *Ctx) Notify(message string) {
	c.x.notify("ui/notify", map[string]any{"message": message, "level": "info"})
}

// Warn shows a warning.
func (c *Ctx) Warn(message string) {
	c.x.notify("ui/notify", map[string]any{"message": message, "level": "warning"})
}

// Error shows an error.
func (c *Ctx) Error(message string) {
	c.x.notify("ui/notify", map[string]any{"message": message, "level": "error"})
}

// Confirm asks yes or no; false without a terminal (tego -p).
func (c *Ctx) Confirm(title, message string) bool {
	var r struct {
		OK bool `json:"ok"`
	}
	if c.x.call("ui/confirm", map[string]any{"title": title, "message": message}, &r) != nil {
		return false
	}
	return r.OK
}

// Select asks to choose one of options; ok is false when none was chosen.
func (c *Ctx) Select(title string, options []string) (choice string, ok bool) {
	var r struct {
		Index int `json:"index"`
	}
	if c.x.call("ui/select", map[string]any{"title": title, "options": options}, &r) != nil || r.Index < 0 || r.Index >= len(options) {
		return "", false
	}
	return options[r.Index], true
}

// Input asks for text; ok is false when cancelled.
func (c *Ctx) Input(title, placeholder string) (text string, ok bool) {
	var r struct {
		Text      string `json:"text"`
		Cancelled bool   `json:"cancelled"`
	}
	if c.x.call("ui/input", map[string]any{"title": title, "placeholder": placeholder}, &r) != nil || r.Cancelled {
		return "", false
	}
	return r.Text, true
}

// SendMessage sends a prompt, after the running turn if there is one.
func (c *Ctx) SendMessage(text string) {
	c.x.notify("session/sendMessage", map[string]any{"text": text})
}

type handler func(Event, *Ctx) Result

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	run         func(json.RawMessage, *Ctx) (string, error)
}

type command struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	run         func(string, *Ctx) error
}

// Extension is one extension: its handlers, tools and commands.
type Extension struct {
	name     string
	handlers map[string][]handler
	events   []string
	tools    []*tool
	commands []*command

	cwd, session string
	model        Model // guarded by mu
	w            io.Writer
	wmu          sync.Mutex
	mu           sync.Mutex
	next         int64
	pending      map[int64]chan *message
	done         bool // tego is gone: calls get no answer
}

// New makes an extension; name is how tego shows it.
func New(name string) *Extension {
	return &Extension{name: name, handlers: map[string][]handler{}, pending: map[int64]chan *message{}}
}

// On adds a handler for an event (the constants above). Handlers run in
// the order added; each sees what the ones before it changed.
func (x *Extension) On(event string, fn func(Event, *Ctx) Result) {
	if _, ok := x.handlers[event]; !ok {
		x.events = append(x.events, event)
	}
	x.handlers[event] = append(x.handlers[event], fn)
}

// Tool adds a tool the model can call. parameters is its JSON schema (a
// value marshalled to JSON, or json.RawMessage); nil means no parameters.
// The input comes as JSON: decode it with json.Unmarshal. An error is
// shown to the model as the tool's failure.
func (x *Extension) Tool(name, description string, parameters any, run func(input json.RawMessage, ctx *Ctx) (string, error)) {
	schema := json.RawMessage(`{"type":"object","properties":{}}`)
	if parameters != nil {
		if raw, ok := parameters.(json.RawMessage); ok {
			schema = raw
		} else if b, err := json.Marshal(parameters); err == nil {
			schema = b
		}
	}
	x.tools = append(x.tools, &tool{Name: name, Description: description, Parameters: schema, run: run})
}

// Command adds a slash command: /name args.
func (x *Extension) Command(name, description string, run func(args string, ctx *Ctx) error) {
	x.commands = append(x.commands, &command{Name: name, Description: description, run: run})
}

// Run serves tego until it says to stop or closes stdin. It returns at
// once with an error when not started by tego.
func (x *Extension) Run() error {
	if os.Getenv("TEGO_EXTENSION") == "" {
		return errors.New("this is a tego extension: put it in ~/.tego/extensions/ and tego runs it")
	}
	out := os.Stdout
	os.Stdout = os.Stderr // prints go to the log, not into the protocol
	return x.serve(os.Stdin, out)
}

type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (x *Extension) write(m *message) {
	m.JSONRPC = "2.0"
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	x.wmu.Lock()
	defer x.wmu.Unlock()
	_, _ = x.w.Write(append(data, '\n'))
}

func (x *Extension) notify(method string, params any) {
	raw, _ := json.Marshal(params)
	x.write(&message{Method: method, Params: raw})
}

func (x *Extension) call(method string, params, result any) error {
	raw, _ := json.Marshal(params)
	x.mu.Lock()
	if x.done {
		x.mu.Unlock()
		return errors.New("tego went away")
	}
	x.next++
	id := x.next
	ch := make(chan *message, 1)
	x.pending[id] = ch
	x.mu.Unlock()
	idRaw := json.RawMessage(fmt.Sprint(id))
	x.write(&message{ID: &idRaw, Method: method, Params: raw})
	m := <-ch
	if m == nil {
		return errors.New("tego went away")
	}
	if m.Error != nil {
		return errors.New(m.Error.Message)
	}
	return json.Unmarshal(m.Result, result)
}

func (x *Extension) serve(r io.Reader, w io.Writer) error {
	x.w = w
	br := bufio.NewReader(r)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer func() { // what is still waiting for tego will get no answer
		x.mu.Lock()
		x.done = true
		for id, ch := range x.pending {
			close(ch)
			delete(x.pending, id)
		}
		x.mu.Unlock()
	}()
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var m message
			if json.Unmarshal(line, &m) == nil {
				if m.Method == "shutdown" {
					return nil
				}
				if m.Method == "" && m.ID != nil { // an answer to a call
					var id int64
					if json.Unmarshal(*m.ID, &id) == nil {
						x.mu.Lock()
						ch := x.pending[id]
						delete(x.pending, id)
						x.mu.Unlock()
						if ch != nil {
							ch <- &m
						}
					}
				} else if m.Method != "" {
					wg.Add(1)
					go func() {
						defer wg.Done()
						x.handle(&m)
					}()
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// handle answers one request (or takes a notification) from tego.
func (x *Extension) handle(m *message) {
	res, err := x.dispatch(m.Method, m.Params)
	if m.ID == nil {
		return
	}
	out := &message{ID: m.ID}
	if err != nil {
		out.Error = &rpcError{Code: -32000, Message: err.Error()}
	} else {
		out.Result, _ = json.Marshal(res)
	}
	x.write(out)
}

func (x *Extension) ctx() *Ctx {
	x.mu.Lock()
	defer x.mu.Unlock()
	return &Ctx{Cwd: x.cwd, SessionID: x.session, Model: x.model, x: x}
}

func (x *Extension) dispatch(method string, params json.RawMessage) (res any, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%s panicked: %v", method, p)
		}
	}()
	switch method {
	case "initialize":
		var p struct {
			Cwd     string `json:"cwd"`
			Session string `json:"session_id"`
			Model   Model  `json:"model"`
		}
		_ = json.Unmarshal(params, &p)
		x.mu.Lock()
		x.cwd, x.session, x.model = p.Cwd, p.Session, p.Model
		x.mu.Unlock()
		events := x.events
		if events == nil {
			events = []string{}
		}
		return map[string]any{"name": x.name, "tools": x.tools, "commands": x.commands, "events": events}, nil
	case "event":
		var p struct {
			Event string `json:"event"`
			Data  struct {
				Tool      string          `json:"tool"`
				Input     json.RawMessage `json:"input"`
				Output    string          `json:"output"`
				IsError   bool            `json:"is_error"`
				Agent     string          `json:"agent"`
				Text      string          `json:"text"`
				Cwd       string          `json:"cwd"`
				SessionID string          `json:"session_id"`
				Model
			} `json:"data"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		d := p.Data
		if p.Event == SessionStart || p.Event == ModelChange {
			x.mu.Lock()
			x.model = d.Model
			x.mu.Unlock()
		}
		return x.runEvent(Event{Name: p.Event, Tool: d.Tool, Input: d.Input, Output: d.Output, IsError: d.IsError,
			Agent: d.Agent, Text: d.Text, Cwd: d.Cwd, SessionID: d.SessionID, Model: d.Model})
	case "tool/execute":
		var p struct {
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		_ = json.Unmarshal(params, &p)
		for _, t := range x.tools {
			if t.Name == p.Name {
				out, err := t.run(p.Input, x.ctx())
				if err != nil {
					return map[string]any{"content": err.Error(), "is_error": true}, nil
				}
				return map[string]any{"content": out}, nil
			}
		}
		return nil, fmt.Errorf("no tool %s", p.Name)
	case "command/execute":
		var p struct {
			Name string `json:"name"`
			Args string `json:"args"`
		}
		_ = json.Unmarshal(params, &p)
		for _, c := range x.commands {
			if c.Name == p.Name {
				return struct{}{}, c.run(p.Args, x.ctx())
			}
		}
		return nil, fmt.Errorf("no command /%s", p.Name)
	}
	return nil, fmt.Errorf("method not found: %s", method)
}

// runEvent runs an event's handlers in order, as tego's JavaScript host
// does: a change is what the next handler sees, and a block or a handled
// prompt ends the chain.
func (x *Extension) runEvent(e Event) (map[string]any, error) {
	out := map[string]any{}
	for _, h := range x.handlers[e.Name] {
		r := h(e, x.ctx())
		switch {
		case r.block:
			return map[string]any{"block": true, "reason": r.reason}, nil
		case r.handled:
			out["handled"] = true
			if r.text != nil {
				out["text"] = *r.text
			}
			return out, nil
		}
		if r.input != nil {
			raw, err := json.Marshal(r.input)
			if err != nil {
				return nil, err
			}
			e.Input, out["input"] = raw, json.RawMessage(raw)
		}
		if r.output != nil {
			e.Output, out["output"] = *r.output, *r.output
		}
		if r.text != nil {
			e.Text, out["text"] = *r.text, *r.text
		}
	}
	return out, nil
}
