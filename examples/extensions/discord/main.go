// Command discord is a tego extension that shows what tego is doing as your
// Discord status ("Working on tego · Editing main.go").
//
// Install: copy or link this folder to ~/.tego/extensions/discord, and put
// the id of a Discord application in ~/.tego/discord.json (see README.md).
// tego builds it the first time it starts.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/akbxr/tego/extension"
)

// config is ~/.tego/discord.json.
type config struct {
	ClientID    string `json:"client_id"`    // the Discord application's id (also TEGO_DISCORD_CLIENT_ID)
	ShowProject *bool  `json:"show_project"` // the project's folder name (default true)
	ShowModel   *bool  `json:"show_model"`   // the session's model (default true)
	ShowDetails bool   `json:"show_details"` // file names and commands (default false)
	LargeImage  string `json:"large_image"`  // an art asset's key or an https URL; "": the application's icon
	LargeText   string `json:"large_text"`   // its tooltip (default "tego")
}

func loadConfig() config {
	home := os.Getenv("TEGO_HOME")
	if home == "" {
		h, _ := os.UserHomeDir()
		home = filepath.Join(h, ".tego")
	}
	var c config
	if data, err := os.ReadFile(filepath.Join(home, "discord.json")); err == nil {
		_ = json.Unmarshal(data, &c)
	}
	if id := os.Getenv("TEGO_DISCORD_CLIENT_ID"); id != "" {
		c.ClientID = id
	}
	return c
}

func (c config) showProject() bool { return c.ShowProject == nil || *c.ShowProject }
func (c config) showModel() bool   { return c.ShowModel == nil || *c.ShowModel }

type activity struct {
	Details    string      `json:"details,omitempty"`
	State      string      `json:"state,omitempty"`
	Timestamps *timestamps `json:"timestamps,omitempty"`
	Assets     *assets     `json:"assets,omitempty"`
}

type timestamps struct {
	Start int64 `json:"start"`
}

type assets struct {
	LargeImage string `json:"large_image,omitempty"`
	LargeText  string `json:"large_text,omitempty"`
}

// field fits a status line: Discord wants 2 to 128 characters.
func field(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 128 {
		s = string([]rune(s)[:127]) + "…"
	}
	if utf8.RuneCountInString(s) < 2 {
		s += "  "
	}
	return s
}

// describe says what a tool call is doing, with names and commands only
// when details are on.
func describe(tool string, input json.RawMessage, details bool) string {
	var in struct {
		Path, Command, Pattern, Query string
	}
	_ = json.Unmarshal(input, &in)
	file := filepath.Base(in.Path)
	if in.Path == "" {
		file = ""
	}
	pick := func(general, specific string, ok bool) string {
		if details && ok {
			return specific
		}
		return general
	}
	switch {
	case tool == "read":
		return pick("Reading code", "Reading "+file, file != "")
	case tool == "edit" || tool == "write":
		return pick("Editing code", "Editing "+file, file != "")
	case tool == "grep" || tool == "glob":
		return pick("Searching the code", "Searching for "+in.Pattern, in.Pattern != "")
	case tool == "bash" || tool == "bash_output":
		cmd, _, _ := strings.Cut(strings.TrimSpace(in.Command), "\n")
		return pick("Running a command", "Running "+cmd, cmd != "")
	case tool == "web_search":
		return pick("Searching the web", "Searching the web for "+in.Query, in.Query != "")
	case tool == "task":
		return "Running subagents"
	case tool == "todo":
		return "Planning"
	case strings.HasPrefix(tool, "mcp__"):
		server, _, _ := strings.Cut(strings.TrimPrefix(tool, "mcp__"), "__")
		return "Using " + server
	}
	return "Using " + tool
}

// minGap spaces the updates: Discord takes about five per 20 seconds.
var minGap = 4 * time.Second

// retryEvery is how often a missing Discord app is looked for again.
var retryEvery = 30 * time.Second

// presence keeps the status to show and sends it to Discord, latest
// first, no more often than minGap, connecting when it can.
type presence struct {
	cfg     config
	mu      sync.Mutex
	project string
	model   string // as tego shows it: "Opus 5.5"
	start   int64
	state   string
	wake    chan struct{}
	quit    chan struct{} // closed by stop
	once    sync.Once
	stopped chan struct{}
	conn    *ipc // set by loop only, under mu
	lastErr string
}

func newPresence(cfg config) *presence {
	p := &presence{cfg: cfg, start: time.Now().Unix(), state: "Starting", wake: make(chan struct{}, 1),
		quit: make(chan struct{}), stopped: make(chan struct{})}
	go p.loop()
	return p
}

func (p *presence) set(fn func(p *presence)) {
	p.mu.Lock()
	fn(p)
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *presence) activity() *activity {
	p.mu.Lock()
	defer p.mu.Unlock()
	model := ""
	if p.cfg.showModel() {
		model = p.model
	}
	var details string
	switch project := p.cfg.showProject() && p.project != ""; {
	case project && model != "":
		details = "Working on " + p.project + "(" + model + ")"
	case project:
		details = "Working on " + p.project
	case model != "":
		details = "Using " + model
	default:
		details = "Coding with tego"
	}
	a := &activity{Details: field(details), State: field(p.state), Timestamps: &timestamps{Start: p.start}}
	if p.cfg.LargeImage != "" {
		text := p.cfg.LargeText
		if text == "" {
			text = "tego"
		}
		a.Assets = &assets{LargeImage: p.cfg.LargeImage, LargeText: field(text)}
	}
	return a
}

func (p *presence) setConn(c *ipc, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conn = c
	if err != nil {
		p.lastErr = err.Error()
	}
}

// connected says whether Discord shows the status.
func (p *presence) connected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn != nil && p.conn.alive()
}

func (p *presence) loop() {
	defer close(p.stopped)
	defer func() {
		if p.conn != nil {
			_ = p.conn.setActivity(nil)
			p.conn.close()
		}
	}()
	var last time.Time
	var shown string // the activity Discord has, as JSON
	retry := time.NewTicker(retryEvery)
	defer retry.Stop()
	for {
		select {
		case <-p.quit:
			return
		case <-p.wake:
		case <-retry.C:
			if p.connected() {
				continue // shown already; nothing new
			}
		}
		if wait := minGap - time.Since(last); wait > 0 {
			select { // what arrives meanwhile is taken below
			case <-time.After(wait):
			case <-p.quit:
				return
			}
		}
		select { // changes made meanwhile are in what is sent now
		case <-p.wake:
		default:
		}
		if !p.connected() {
			c, err := connect(p.cfg.ClientID)
			p.setConn(c, err)
			if err != nil {
				continue // the ticker tries again
			}
			shown = "" // a new connection shows nothing yet
		}
		a := p.activity()
		js, _ := json.Marshal(a)
		if string(js) == shown {
			continue
		}
		if err := p.conn.setActivity(a); err != nil {
			p.setConn(nil, err)
			continue
		}
		shown, last = string(js), time.Now()
	}
}

// stop clears the status and disconnects; Discord would clear it anyway
// once this process ends.
func (p *presence) stop() {
	p.once.Do(func() { close(p.quit) })
	select {
	case <-p.stopped:
	case <-time.After(time.Second):
	}
}

func main() {
	cfg := loadConfig()
	x := extension.New("discord")
	if cfg.ClientID == "" {
		x.On(extension.SessionStart, func(e extension.Event, ctx *extension.Ctx) extension.Result {
			ctx.Warn("set client_id in ~/.tego/discord.json to the id of a Discord application to show tego as your status")
			return extension.Pass()
		})
		x.Run()
		return
	}
	p := newPresence(cfg)
	x.On(extension.SessionStart, func(e extension.Event, ctx *extension.Ctx) extension.Result {
		p.set(func(p *presence) {
			p.project, p.model, p.state = filepath.Base(e.Cwd), e.Model.Name, "Waiting for a prompt"
		})
		return extension.Pass()
	})
	x.On(extension.ModelChange, func(e extension.Event, ctx *extension.Ctx) extension.Result {
		p.set(func(p *presence) { p.model = e.Model.Name })
		return extension.Pass()
	})
	x.On(extension.Input, func(e extension.Event, ctx *extension.Ctx) extension.Result {
		p.set(func(p *presence) { p.state = "Thinking" })
		return extension.Pass()
	})
	x.On(extension.ToolCall, func(e extension.Event, ctx *extension.Ctx) extension.Result {
		s := describe(e.Tool, e.Input, cfg.ShowDetails)
		p.set(func(p *presence) { p.state = s })
		return extension.Pass()
	})
	x.On(extension.TurnEnd, func(e extension.Event, ctx *extension.Ctx) extension.Result {
		p.set(func(p *presence) { p.state = "Waiting for a prompt" })
		return extension.Pass()
	})
	x.On(extension.SessionEnd, func(e extension.Event, ctx *extension.Ctx) extension.Result {
		p.stop()
		return extension.Pass()
	})
	x.Command("discord", "Discord status: whether it shows", func(args string, ctx *extension.Ctx) error {
		p.mu.Lock()
		state, err := p.state, p.lastErr
		p.mu.Unlock()
		if p.connected() {
			ctx.Notify("showing on Discord: " + state)
		} else if err != "" {
			ctx.Warn("not connected to Discord: " + err)
		} else {
			ctx.Notify("connecting to Discord")
		}
		return nil
	})
	x.Run()
	p.stop()
}
