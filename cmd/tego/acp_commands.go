package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/acp"
	"github.com/akbxr/tego/internal/agent"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/stats"
)

// Slash commands in an editor: those of the terminal that make sense
// there. Session handling (new, resume, fork, rewind) is the editor's, and
// login needs a terminal.

type acpCommand struct {
	name, help, hint string
	run              func(sess *acpSession, ctx context.Context, args string) slashResult
}

// slashResult is what a command does: text to show, a prompt to run in its
// place, or a job to run as the turn.
type slashResult struct {
	reply  string
	prompt string
	job    func(context.Context) error
}

var acpCommands = []acpCommand{
	{"help", "Show the commands", "", nil}, // in slash: it lists this table
	{"compact", "Summarise the conversation to free context", "what to keep (optional)",
		func(sess *acpSession, _ context.Context, args string) slashResult {
			return slashResult{job: func(ctx context.Context) error { return sess.ag.Compact(ctx, args) }}
		}},
	{"retry", "Run the model again on the conversation (after an error)", "",
		func(sess *acpSession, _ context.Context, _ string) slashResult {
			return slashResult{job: sess.ag.Continue}
		}},
	{"plan", "Plan mode on or off; with a prompt, plan it now", "prompt (optional)",
		func(sess *acpSession, _ context.Context, args string) slashResult {
			if args == "" {
				on := !sess.plan.Load()
				sess.plan.Store(on)
				sess.notifyMode()
				if on {
					return slashResult{reply: "Plan mode on: the agent investigates and proposes, and changes nothing."}
				}
				return slashResult{reply: "Plan mode off."}
			}
			sess.plan.Store(true)
			sess.notifyMode()
			return slashResult{prompt: args}
		}},
	{"permissions", "Which tool calls ask first: yolo, edits or ask", "yolo | edits | ask",
		func(sess *acpSession, _ context.Context, args string) slashResult {
			if args == "" {
				return slashResult{reply: "Permission mode: `" + sess.ag.Perms.Mode() + "` (yolo, edits or ask)."}
			}
			if !agent.ValidPermission(args) {
				return slashResult{reply: "Unknown mode `" + args + "`: use yolo, edits or ask."}
			}
			sess.plan.Store(false)
			sess.ag.Perms.SetMode(args)
			sess.notifyMode()
			return slashResult{reply: "Permission mode: `" + args + "`."}
		}},
	{"model", "Show or switch the model", "provider/model",
		func(sess *acpSession, _ context.Context, args string) slashResult {
			if args != "" {
				if err := sess.app.setModel(args); err != nil {
					return slashResult{reply: "Could not switch: " + err.Error()}
				}
				sess.notify(acp.ConfigOptionUpdate{SessionUpdate: "config_option_update", ConfigOptions: sess.configOptions()})
			}
			return slashResult{reply: "Model: `" + sess.ag.ProviderName + "/" + sess.ag.Model + "`."}
		}},
	{"thinking", "Show or set the thinking level", strings.Join(llm.ThinkingLevels, " | "),
		func(sess *acpSession, _ context.Context, args string) slashResult {
			if args != "" {
				if !slices.Contains(llm.ThinkingLevels, args) {
					return slashResult{reply: "Unknown level `" + args + "`: use " + strings.Join(llm.ThinkingLevels, ", ") + "."}
				}
				sess.ag.Thinking = args
				sess.notify(acp.ConfigOptionUpdate{SessionUpdate: "config_option_update", ConfigOptions: sess.configOptions()})
			}
			return slashResult{reply: "Thinking: `" + firstNonEmpty(sess.ag.Thinking, llm.ThinkingOff) + "`."}
		}},
	{"fast", "Fast mode on or off for this session (Claude Opus 4.8+, twice the price)", "on | off",
		func(sess *acpSession, _ context.Context, args string) slashResult {
			switch args {
			case "":
				sess.ag.Fast = !sess.ag.Fast
			case "on", "off":
				sess.ag.Fast = args == "on"
			default:
				return slashResult{reply: "Use `/fast`, `/fast on` or `/fast off`."}
			}
			state := "off"
			if sess.ag.Fast {
				state = "on: faster output at twice the price per token"
				if !llm.SupportsFast(sess.ag.Provider, sess.ag.Model) {
					state = "on, but " + sess.ag.Model + " has no fast mode; it applies on Claude Opus 4.8 and newer"
				}
			}
			return slashResult{reply: "Fast mode " + state + "."}
		}},
	{"session", "Session info and token usage", "", func(sess *acpSession, _ context.Context, _ string) slashResult {
		t := sess.ag.Usage()
		var b strings.Builder
		fmt.Fprintf(&b, "session:  %s\nfile:     %s\n", sess.id, sess.ag.Session.Path)
		fmt.Fprintf(&b, "model:    %s/%s · thinking %s\n", sess.ag.ProviderName, sess.ag.Model, sess.ag.Thinking)
		fmt.Fprintf(&b, "messages: %d · context ~%s / %s\n", len(sess.ag.Messages), fmtTokens(sess.ag.LastCtx), fmtTokens(sess.ag.ContextWindow))
		fmt.Fprintf(&b, "tokens:   %s in · %s cache read · %s cache write · %s out\n",
			fmtTokens(t.Input), fmtTokens(t.CacheRead), fmtTokens(t.CacheWrite), fmtTokens(t.Output))
		return slashResult{reply: fence(b.String())}
	}},
	{"usage", "Subscription usage and resets of every logged-in account", "",
		func(sess *acpSession, ctx context.Context, _ string) slashResult {
			start := time.Now()
			all := fetchUsage(ctx, sess.srv.cfg, sess.srv.store)
			var b bytes.Buffer
			renderUsage(&b, palette{}, all, time.Since(start), nil)
			return slashResult{reply: fence(b.String())}
		}},
	{"stats", "Requests, tokens and estimated cost", "24h | 7d | 30d | 90d | all",
		func(sess *acpSession, _ context.Context, args string) slashResult {
			recs, err := stats.Load(sess.srv.cfg.Dir)
			if err != nil {
				return slashResult{reply: "stats: " + err.Error()}
			}
			rep, err := stats.Build(recs, firstNonEmpty(args, "7d"), time.Now(), statsPrices(sess.srv.cfg))
			if err != nil {
				return slashResult{reply: err.Error()}
			}
			var b bytes.Buffer
			stats.Summary(&b, rep, false)
			return slashResult{reply: fence(b.String()) + "\n`tego stats serve` opens the dashboard."}
		}},
	{"mcp", "MCP server status and tool counts", "", func(sess *acpSession, _ context.Context, _ string) slashResult {
		st := sess.mcp.Status()
		if len(st) == 0 {
			return slashResult{reply: "No MCP servers: the editor sent none, and tego has none configured (~/.tego/mcp.json, ./.mcp.json)."}
		}
		var b strings.Builder
		for _, s := range st {
			fmt.Fprintf(&b, "%-20s %-10s %-6s", s.Name, s.State, s.Transport)
			if s.State == "connected" {
				fmt.Fprintf(&b, " %d tools", s.Tools)
			}
			if s.Err != "" {
				b.WriteString("  " + strings.ReplaceAll(s.Err, "\n", " "))
			}
			b.WriteByte('\n')
		}
		return slashResult{reply: fence(b.String())}
	}},
	{"skills", "List the skills", "", func(sess *acpSession, _ context.Context, _ string) slashResult {
		if len(sess.skills) == 0 {
			return slashResult{reply: "No skills found (.agents/skills, .claude/skills, .tego/skills)."}
		}
		var b strings.Builder
		for _, s := range sess.skills {
			fmt.Fprintf(&b, "- `/skill:%s` %s\n", s.Name, s.Description)
		}
		return slashResult{reply: b.String()}
	}},
}

// fence shows text as is, in a code block.
func fence(s string) string { return "```\n" + strings.TrimRight(s, "\n") + "\n```\n" }

// commandList is what the editor offers after "/": tego's commands, the
// project's custom commands, and one per skill.
func (sess *acpSession) commandList() []acp.Command {
	var cmds []acp.Command
	for _, c := range acpCommands {
		cmd := acp.Command{Name: c.name, Description: c.help}
		if c.hint != "" {
			cmd.Input = &acp.CommandInput{Hint: c.hint}
		}
		cmds = append(cmds, cmd)
	}
	names := make([]string, 0, len(sess.commands))
	for n := range sess.commands {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		c := sess.commands[n]
		cmds = append(cmds, acp.Command{Name: n, Description: firstNonEmpty(c.Description, "Custom command"),
			Input: &acp.CommandInput{Hint: "arguments"}})
	}
	for _, s := range sess.skills {
		cmds = append(cmds, acp.Command{Name: "skill:" + s.Name, Description: firstNonEmpty(s.Description, "Skill"),
			Input: &acp.CommandInput{Hint: "what to apply it to (optional)"}})
	}
	return cmds
}

// slash runs a prompt that starts with "/" as a command; ok is false when
// it names none (then it is an ordinary prompt, a path for instance).
func (sess *acpSession) slash(ctx context.Context, text string) (slashResult, bool) {
	name, args, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
	name, args = strings.TrimSpace(name), strings.TrimSpace(args)
	if name == "help" {
		var b strings.Builder
		for _, c := range sess.commandList() {
			fmt.Fprintf(&b, "- `/%s` %s\n", c.Name, c.Description)
		}
		return slashResult{reply: b.String()}, true
	}
	for _, c := range acpCommands {
		if c.name == name {
			return c.run(sess, ctx, args), true
		}
	}
	if c, ok := sess.commands[name]; ok {
		return slashResult{prompt: c.Expand(args)}, true
	}
	if sk, ok := strings.CutPrefix(name, "skill:"); ok {
		for _, s := range sess.skills {
			if s.Name != sk {
				continue
			}
			body, err := os.ReadFile(s.Path)
			if err != nil {
				return slashResult{reply: "skill " + sk + ": " + err.Error()}, true
			}
			req := firstNonEmpty(args, "Apply this skill to the current task.")
			return slashResult{prompt: fmt.Sprintf("<skill name=%q path=%q>\n%s\n</skill>\n\n%s", s.Name, s.Path, strings.TrimSpace(string(body)), req)}, true
		}
		return slashResult{reply: "Unknown skill `" + sk + "` (see /skills)."}, true
	}
	return slashResult{}, false
}

// notifyMode tells the editor the mode changed (by a command, not its picker).
func (sess *acpSession) notifyMode() {
	sess.notify(acp.ModeUpdate{SessionUpdate: "current_mode_update", CurrentModeID: sess.mode()})
}
