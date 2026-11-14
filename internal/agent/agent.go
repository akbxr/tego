// Package agent runs the model <-> tool loop, compaction and subagents.
package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // image.DecodeConfig in imageChars
	_ "image/jpeg"
	_ "image/png"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akbxr/tego/internal/audio"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/session"
	"github.com/akbxr/tego/internal/tools"
	"github.com/akbxr/tego/internal/video"
)

// UI receives streaming output. Implementations must be goroutine-safe:
// subagents report concurrently through OnInfo.
type UI interface {
	OnText(delta string)
	OnThinking(delta string)
	OnToolStart(name string)
	OnToolCall(name string, input json.RawMessage)
	OnToolResult(name string, input json.RawMessage, output string, isErr bool)
	OnTurnEnd(m *llm.Message)
	OnInfo(msg string)
}

type Agent struct {
	Provider     llm.Provider
	ProviderName string
	Model        string
	Thinking     string
	// MaxOutputTokens caps each response (llm.Request.MaxTokens); 0: the
	// provider's default for the model.
	MaxOutputTokens int
	// Shells is the session's background shells started since a time that
	// still run; with it, a model about to finish is reminded of them.
	Shells func(since time.Time) []tools.ShellInfo
	// Fast asks for fast mode on models that have it (llm.Request.Fast).
	// FastActive says whether the last response actually ran fast.
	Fast, FastActive bool
	System           string
	Tools            []tools.Tool
	Dynamic          func() []tools.Tool // extra tools that may change between requests (MCP); goroutine-safe
	Hooks            ToolHooks           // see every tool call before and after (extensions); nil: none
	// LazyThreshold: when there are more Dynamic tools than this, they are
	// deferred behind tool_search instead of sent with every request.
	// 0 = always defer, negative = never.
	LazyThreshold int
	Messages      []llm.Message
	Session       *session.Session
	SessionID     string
	LongCache     bool // ask the provider to keep the prompt cache for an hour
	// Asides keeps the last request a turn sent, so that a side question
	// (/btw, AsideRequest) asked while the turn runs reads its cache.
	Asides bool
	// Env returns the date and git branch (EnvNote), reported after a
	// prompt when they differ from what the conversation last saw.
	Env func() string
	// Notices reports events the model should know about before its next
	// request (a background shell ended); "" when there are none.
	Notices  func() string
	Subagent bool // a task subagent (labelled as such to the provider)
	// Plan, when set and true, is plan mode (plan.go): shared with the UI,
	// which toggles it between turns, and with subagents.
	Plan     *atomic.Bool
	planSaid bool // the last plan reminder said plan mode was on
	// Perms is the permission mode and the session's rules (permission.go),
	// shared with subagents; nil runs everything. Approve asks the user
	// about a call the mode makes wait; nil refuses such calls.
	Perms   *Permissions
	Approve Approver
	Name    string // a subagent's name, for the user asked about its calls
	// declined is set when the user declines a call without saying what to
	// do instead: the turn stops, its subagents included (they share it).
	declined *atomic.Bool
	dir      string // a subagent's worktree folder; "" for the session's
	// A subagent that runs on the session's model, and at its thinking
	// level: resumed after a /model, it follows the session.
	sessionModel, sessionThinking bool
	// Steer, when set, returns the messages the user sent while the agent
	// was working (each as the blocks of one user message). It is polled
	// after each tool round and when the model would stop, so a message
	// steers the next request without interrupting a running tool.
	Steer  func() [][]llm.Block
	wakeMu sync.Mutex
	wake   chan struct{} // closed by SteerArrived; tools that wait watch it
	// ToolStat, when set, is told about every tool call's outcome (for
	// usage statistics); subagents inherit it.
	ToolStat func(name string, isErr, subagent bool)
	// CacheWritePremium reports models whose cache writes cost more than
	// input (nil = none); see ElideAt.
	CacheWritePremium func(provider, model string) bool
	// CacheReadRatio is a cache read's price as a share of input's (nil or
	// 0 = 0.1); the keep-alive budget uses it (keepalive.go).
	CacheReadRatio func(provider, model string) float64
	// KeepAlive is how long idle the prompt cache is kept warm at most
	// (keepalive.go); 0 = never. KeepAliveAuto also stops once the
	// refreshes cost what letting the cache go cold would.
	KeepAlive     time.Duration
	KeepAliveAuto bool

	ContextWindow  int
	AutoCompact    bool
	CompactPercent int       // auto-compact above this % of the window; 0 = default (window - max(15%, 16k))
	KeepRecent     int       // tokens kept verbatim when compacting; 0 = 20k
	IdleCompactAt  int       // after a break the cache did not survive, compact above this many tokens (idle.go); 0 = never
	MaxTurns       int       // 0 = unlimited
	Watchdog       *Watchdog // a subagent's progress checks (watchdog.go); nil: none
	UI             UI

	mu      sync.Mutex
	Total   llm.Usage
	LastCtx int // context tokens of the latest request
	// elideFloor: after an elision pass, the next one waits until the
	// context has grown again (elideRegrow), so a context that hovers above
	// the soft threshold does not rebuild the cache every turn.
	elideFloor int
	// ctxRewrite is the context size right after an elision pass (measured
	// usage minus what was cut), for Messages[:ctxRewriteAt], until the next
	// response reports real usage. 0 = none.
	ctxRewrite, ctxRewriteAt int
	cleared                  int // tool results the provider reported cleared (server-side editing)
	clearUses                int // tool_uses trigger of server-side clearing, 0 = not yet (serveredit.go)
	// cutCall is the id of the tool call the output token limit cut off in
	// the batch being run ("" if none): it is answered without running.
	cutCall string
	// Repeated reads (readdedup.go): the state of each file a read call
	// returned, by call id, and the number of messages the provider may
	// have cleared tool results from.
	readMu     sync.Mutex
	readStamps map[string]fileStamp
	clearMark  int
	// lastReply is when the provider last answered (from the session on
	// resume): after the cache TTL the next request misses the cache.
	lastReply time.Time
	// sent is the last request that got a reply, which a compaction can
	// repeat to read its prompt cache.
	sent  sentRequest
	ka    keepAliveState
	clock func() time.Time // tests; nil = time.Now
	stuck stuckState
	stop  stopState

	cp            checkpointState
	pendingRewind *rewindReq // set by the rewind tool, applied after the tool batch

	todoMu  sync.Mutex
	todos   []Todo
	todoRev int // bumped by every todo call

	subMu sync.Mutex
	subs  []*SubagentStatus // subagents of the latest task call

	endedMu sync.Mutex
	ended   []*endedSub // ended subagents a task can resume (resume.go)

	videoMu      sync.Mutex
	videoFrames  map[[32]byte][]llm.Block // video blocks as frames, for models that can't watch (video.go)
	mediaDropped int                      // the oldest images requests leave out, as last told (media.go)
	asideMu      sync.Mutex
	aside        asideState // side questions (aside.go)

	audioMu    sync.Mutex
	audioTexts map[[32]byte]string // audio blocks as transcripts, for models that can't hear (audio.go)

	toolMu    sync.Mutex
	activated map[string]bool // deferred MCP tools loaded into the request
}

// toolList is what the next request advertises: the static tools plus MCP
// tools. With many MCP tools (see LazyThreshold) a tool_search tool stands
// in for them and they stay out of the context until the model searches.
// Providers that support deferred tools get every MCP tool marked deferred
// (from index deferFrom on) and load the matches through references in the
// search result, so the tool list, and with it the prompt cache, never
// changes. Other providers get the MCP tools activated so far. Recomputed
// per request, so servers that connect late become available.
func (a *Agent) toolList() (list []tools.Tool, deferFrom int) {
	if a.Dynamic == nil {
		return a.Tools, len(a.Tools)
	}
	dyn := a.Dynamic()
	if len(dyn) == 0 {
		return a.Tools, len(a.Tools)
	}
	out := a.Tools[:len(a.Tools):len(a.Tools)]
	if !a.lazy(len(dyn)) {
		out = append(out, dyn...)
		return out, len(out)
	}
	out = append(out, &toolSearch{a: a})
	if a.defersTools() {
		return append(out, dyn...), len(out)
	}
	a.toolMu.Lock()
	for _, t := range dyn {
		if a.activated[t.Def().Name] {
			out = append(out, t)
		}
	}
	a.toolMu.Unlock()
	return out, len(out)
}

func (a *Agent) allTools() []tools.Tool {
	list, _ := a.toolList()
	return list
}

func (a *Agent) defersTools() bool {
	return a.Provider != nil && llm.DefersTools(a.Provider, a.Model)
}

func (a *Agent) toolDefs() []llm.Tool {
	all, deferFrom := a.toolList()
	defs := make([]llm.Tool, len(all))
	for i, t := range all {
		defs[i] = t.Def()
		defs[i].Deferred = i >= deferFrom
	}
	return defs
}

func (a *Agent) tool(name string) tools.Tool {
	for _, t := range a.allTools() {
		if t.Def().Name == name {
			return t
		}
	}
	// A deferred MCP tool called by name (seen in an earlier search, or a
	// resumed session): resolve it from the catalog and keep it loaded.
	if a.Dynamic != nil {
		for _, t := range a.Dynamic() {
			if t.Def().Name == name {
				a.activate(name)
				return t
			}
		}
	}
	return nil
}

func (a *Agent) append(m llm.Message) {
	a.Messages = append(a.Messages, m)
	if err := a.Session.Append(m); err != nil {
		a.UI.OnInfo("session write failed: " + err.Error())
	}
}

// Usage returns the token totals; safe while subagents are still adding.
func (a *Agent) Usage() llm.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Total
}

func (a *Agent) addUsage(u *llm.Usage) {
	if u == nil {
		return
	}
	a.mu.Lock()
	a.Total.Input += u.Input
	a.Total.Output += u.Output
	a.Total.CacheRead += u.CacheRead
	a.Total.CacheWrite += u.CacheWrite
	a.mu.Unlock()
}

func (a *Agent) onEvent(ev llm.Event) {
	switch ev.Type {
	case llm.EvText:
		a.UI.OnText(ev.Text)
	case llm.EvThinking:
		a.UI.OnThinking(ev.Text)
	case llm.EvToolCall:
		a.UI.OnToolStart(ev.ToolName)
	case llm.EvToolDelta:
		if u, ok := a.UI.(ToolDeltaUI); ok {
			u.OnToolDelta(ev.ToolID, ev.ToolName, ev.Text)
		}
	}
}

// ToolDeltaUI is implemented by UIs that show a tool call while the model is
// still writing its arguments (the file a long write or edit is producing).
// chunk is the next piece of the call's raw JSON arguments.
type ToolDeltaUI interface {
	OnToolDelta(id, name, chunk string)
}

// Run sends a user prompt (plus optional image blocks) and loops until the
// model stops calling tools.
func (a *Agent) Run(ctx context.Context, prompt string, images ...llm.Block) error {
	blocks := append([]llm.Block{{Type: llm.BlockText, Text: prompt}}, images...)
	a.append(llm.Message{Role: llm.RoleUser, Blocks: blocks})
	if a.Env != nil {
		// After the prompt, so the prompt stays the session's first text.
		if note := a.Env(); note != "" && note != lastEnvNote(a.Messages) {
			a.append(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: envPrefix + note + "</system-reminder>"}}})
		}
	}
	a.notePlan()
	return a.loop(ctx)
}

// Continue resumes the loop without a new prompt (e.g. after an error).
func (a *Agent) Continue(ctx context.Context) error {
	if len(a.Messages) == 0 {
		return errors.New("nothing to continue")
	}
	return a.loop(ctx)
}

// ErrNoModel is returned when no provider/model is configured yet.
var ErrNoModel = errors.New("no model configured: use /login to sign in or add a key, then /model")

// lastResponseNote goes before a subagent's last request.
const lastResponseNote = "This is your last response. Don't call tools; calls in this response will not run. " +
	"Write your report for the agent that started you, so it can go on without you: what is done, the files you changed, what is left and how you would go on, and anything that failed or is unverified."

// lastTurnNote: the last request at the turn limit.
const lastTurnNote = "You have reached your limit of requests. " + lastResponseNote

// wrapUpNote warns a subagent near its limit, so it can finish a step and
// report rather than be cut off mid-change.
const wrapUpNote = "You have made %d of your %d requests. Start wrapping up: finish or undo the change in progress, " +
	"leave nothing half-done, and end with your report; say what is left instead of starting something new."

// wrapUpTurn is the request a subagent is told to wrap up before: a tenth
// of its limit before it (at least 5); 0 without a limit, or one too low
// for a warning to help.
func wrapUpTurn(limit int) int {
	if limit < 20 {
		return 0
	}
	return limit - max(5, limit/10)
}

func (a *Agent) loop(ctx context.Context) error {
	a.stuck = stuckState{} // streaks count within one prompt (or /retry)
	_, rev := a.TodoState()
	a.stop = stopState{todoRev: rev, started: time.Now()}
	overflowRetried := false
	// A cache miss is told once per prompt.
	missNoted := false
	if !a.Subagent || a.declined == nil { // a subagent keeps its parent's
		a.declined = new(atomic.Bool)
	}
	if a.Provider == nil {
		return ErrNoModel
	}
	a.keepAliveReport()
	for turn := 1; ; turn++ {
		if a.MaxTurns > 0 && turn > a.MaxTurns {
			return fmt.Errorf("stopped after %d turns", a.MaxTurns)
		}
		// A subagent's last request writes its report instead of stopping
		// mid-work with whatever it said last: at its turn limit, or when
		// the watchdog found it stuck twice in a row.
		var final error
		if a.Subagent && turn == wrapUpTurn(a.MaxTurns) {
			a.remind(fmt.Sprintf(wrapUpNote, turn-1, a.MaxTurns))
		}
		if a.Subagent && a.MaxTurns > 0 && turn == a.MaxTurns {
			final = fmt.Errorf("stopped at its limit of %d turns; its report says what is left", a.MaxTurns)
			a.remind(lastTurnNote)
		} else if stop, why := a.watchdogTurn(ctx, turn); stop {
			final = fmt.Errorf("stopped by the watchdog, no progress: %s; its report says what is left", why)
			a.remind("A second check found no real progress: " + why + ". " + lastResponseNote)
		}
		if a.Notices != nil {
			if n := a.Notices(); n != "" {
				a.remind(n)
			}
		}
		// Checked before every request, including mid-turn between tool
		// rounds, using the latest usage plus what was added since.
		a.LastCtx = a.EstimateContext()
		// The cache expired: shrinking the history now is free.
		a.coldElide()
		// Still long after that: a summary costs less than caching it all again.
		a.idleCompact(ctx)
		// Stage 1: drop bulky old tool output (free); stage 2, only if the
		// context is still too large: LLM summarisation.
		if a.needsElide() {
			before := a.LastCtx
			if n, _ := a.elide(a.warmMinSave()); n > 0 {
				a.LastCtx = a.EstimateContext()
				a.UI.OnInfo(fmt.Sprintf("context at %d%%: elided %d old block(s) (~%.1fk → ~%.1fk tokens)",
					before*100/a.ContextWindow, n, float64(before)/1000, float64(a.LastCtx)/1000))
			}
		}
		if before := a.LastCtx; a.ContextWindow > 0 {
			if n, _ := a.elideBeforeCompact(); n > 0 {
				a.LastCtx = a.EstimateContext()
				a.UI.OnInfo(fmt.Sprintf("context at %d%%: elided %d old block(s) instead of compacting (~%.1fk → ~%.1fk tokens)",
					before*100/a.ContextWindow, n, float64(before)/1000, float64(a.LastCtx)/1000))
			}
		}
		if a.needsCompact() {
			a.UI.OnInfo(fmt.Sprintf("context at %d%% (compacts above %d%%), compacting…", a.LastCtx*100/a.ContextWindow, a.CompactAt()*100/a.ContextWindow))
			if err := a.Compact(ctx, ""); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				a.UI.OnInfo("auto-compaction failed: " + err.Error())
			}
		}
		class := llm.ClassMain
		if a.Subagent {
			class = llm.ClassSubagent
		}
		req := &llm.Request{
			Model: a.Model, System: a.System, Messages: a.Messages, Tools: a.toolDefs(),
			Thinking: a.Thinking, SessionID: a.SessionID, LongCache: a.LongCache, Class: class,
			ClearToolResults: a.clearPolicy(), Fast: a.Fast, MaxTokens: a.MaxOutputTokens,
		}
		// idle: since the cache was last read (a keep-alive counts)
		sent, prev, idle := a.noteSent(req), a.sent, a.now().Sub(a.lastReply)
		req.Messages = a.mediaBudget(a.audioAsText(ctx, a.videoAsFrames(ctx, req.Messages)), true)
		a.noteAside(req)
		msg, err := a.Provider.Stream(ctx, req, a.onEvent)
		if err == nil && req.ClearToolResults != nil {
			msg.ClearTrigger = req.ClearToolResults.TriggerToolUses
		}
		if err != nil {
			if ctx.Err() != nil {
				a.recordAborted(msg)
				return ctx.Err()
			}
			// The window filled up anyway (estimate too low, huge tool
			// output): compact once and retry the same request.
			if !overflowRetried && isContextOverflow(err) && len(a.Messages) >= 2 {
				overflowRetried = true
				a.UI.OnInfo("context window exceeded; compacting and retrying…")
				if cerr := a.Compact(ctx, ""); cerr == nil {
					continue
				} else if ctx.Err() != nil {
					return ctx.Err()
				}
			}
			return err
		}
		a.addUsage(msg.Usage)
		a.lastReply = a.now()
		a.sent = sent.answered(a.lastReply, msg.Usage)
		if n := msg.Usage.ContextTokens(); n > 0 {
			a.LastCtx = n
		}
		a.noteServerEdits(msg)
		if msg.Usage != nil {
			a.FastActive = msg.Usage.Fast
		}
		if msg.FastNote != "" {
			a.UI.OnInfo(msg.FastNote)
		}
		if note := cacheMissNote(msg.CacheMiss); note != "" && !missNoted && a.expectsCacheRead(prev, sent, idle) {
			missNoted = true
			a.UI.OnInfo(note)
		}
		calls := sanitizeCalls(msg)
		if final != nil {
			// Calls it made anyway don't run: the report is all that is left.
			msg.Blocks = slices.DeleteFunc(msg.Blocks, func(b llm.Block) bool { return b.Type == llm.BlockToolUse })
			if len(msg.Blocks) > 0 {
				a.append(*msg)
			}
			a.UI.OnTurnEnd(msg)
			return final
		}
		a.append(*msg)
		a.noteAsideReply(msg)
		a.UI.OnTurnEnd(msg)
		if len(calls) == 0 {
			// The user wrote while this answer streamed: answer that too.
			if a.takeSteering() {
				continue
			}
			if msg.StopReason == "max_tokens" {
				if a.resumeCut(msg) {
					continue
				}
				a.UI.OnInfo("response hit the output token limit again; say \"continue\" to resume")
				return nil
			}
			if msg.StopReason == llm.StopContextFull {
				// Not the response's own limit: the window has no room left.
				if !overflowRetried && len(a.Messages) >= 2 {
					overflowRetried = true
					a.UI.OnInfo("the context window filled up mid-response; compacting and continuing…")
					if cerr := a.Compact(ctx, ""); cerr == nil {
						a.remind("Your last response was cut off because the context window filled up; the history is now summarised. " +
							"Go on with the task from where it stopped.")
						continue
					} else if ctx.Err() != nil {
						return ctx.Err()
					}
				}
				a.UI.OnInfo("the context window is full; /compact to summarise the history, or /new")
				return nil
			}
			if a.stopNudge() {
				continue
			}
			return nil
		}
		if a.cutCall = cutOffCall(msg); a.cutCall != "" {
			if msg.StopReason == llm.StopContextFull {
				a.UI.OnInfo("the context window filled up while a tool call was written; that call was not run")
			} else {
				a.UI.OnInfo("response hit the output token limit while writing a tool call; that call was not run")
			}
		}
		results := a.runCalls(ctx, calls)
		a.cutCall = ""
		a.trackEdits(calls, results)
		stuckErr := a.checkStuck(calls, results)
		a.append(llm.Message{Role: llm.RoleUser, Blocks: results})
		if stuckErr != nil {
			return stuckErr
		}
		// A rewind replaces this whole batch (and everything since the
		// checkpoint) with the report, so it must run after the results land.
		a.applyRewind()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if a.turnDeclined() {
			if !a.Subagent {
				a.UI.OnInfo("stopped: you declined a call. Say how to go on.")
			}
			return nil
		}
		a.takeSteering()
	}
}

// takeSteering adds the messages the user sent while the agent worked, as
// user turns before the next request, and reports whether there were any.
func (a *Agent) takeSteering() bool {
	if a.Steer == nil {
		return false
	}
	a.SteerTaken() // taken (or put back with Alt+↑): the next one wakes again
	msgs := a.Steer()
	for _, blocks := range msgs {
		a.append(llm.Message{Role: llm.RoleUser, Blocks: blocks})
	}
	return len(msgs) > 0
}

// sanitizeCalls repairs tool inputs that are not valid JSON so the stored
// history stays valid (the raw text is kept for the error the model gets),
// and returns the tool_use blocks.
func sanitizeCalls(m *llm.Message) []llm.Block {
	var calls []llm.Block
	for i := range m.Blocks {
		b := &m.Blocks[i]
		if b.Type != llm.BlockToolUse {
			continue
		}
		if !json.Valid(b.Input) {
			b.Input = llm.ToolInput(b.Input)
		}
		calls = append(calls, *b)
	}
	return calls
}

// cutOffCall is the id of the tool call that m's output token limit cut
// off: its last block, when that is a tool call. Its arguments may be cut
// short yet still parse (Gemini sends them as whole objects), so running
// it could, say, write half a file. Calls before it are complete.
func cutOffCall(m *llm.Message) string {
	if m.StopReason != "max_tokens" && m.StopReason != llm.StopContextFull || len(m.Blocks) == 0 {
		return ""
	}
	if last := m.Blocks[len(m.Blocks)-1]; last.Type == llm.BlockToolUse {
		return last.ID
	}
	return ""
}

// cutResumes is how many times one prompt goes on by itself after a
// response hit the output limit; then the user decides.
const cutResumes = 2

// resumeCut goes on after a response that hit the output token limit
// without a tool call, as the user would by saying "continue"; up to
// cutResumes times a prompt.
func (a *Agent) resumeCut(m *llm.Message) bool {
	if a.stop.cutResumes >= cutResumes {
		return false
	}
	a.stop.cutResumes++
	wrote := false
	for _, b := range m.Blocks {
		if b.Type == llm.BlockText && strings.TrimSpace(b.Text) != "" {
			wrote = true
		}
	}
	if wrote {
		a.UI.OnInfo("response hit the output token limit; continuing…")
		a.remind("Your last response hit the output token limit and was cut off. Go on directly from where it stopped: " +
			"no apology, no recap. Put long content in files with tools, in pieces, rather than in one answer.")
		return true
	}
	// The whole limit went on thinking: nothing was written or run.
	a.UI.OnInfo("response hit the output token limit while still thinking; continuing…")
	a.remind("Your last response spent the whole output token limit thinking, so nothing was written or run. " +
		"Don't plan everything in advance or draft code in your head: act now. " +
		"Take the first concrete step with a tool call (write the first file, run the first command), then go on step by step.")
	return true
}

// recordAborted keeps visible text from an interrupted response so the
// transcript reflects what the user saw. Unfinished tool calls are dropped.
func (a *Agent) recordAborted(m *llm.Message) {
	if m == nil {
		return
	}
	var kept []llm.Block
	for _, b := range m.Blocks {
		if b.Type == llm.BlockText && strings.TrimSpace(b.Text) != "" {
			kept = append(kept, b)
		}
	}
	if len(kept) == 0 {
		return
	}
	kept[len(kept)-1].Text += "\n\n[interrupted by user]"
	a.append(llm.Message{Role: llm.RoleAssistant, Blocks: kept, Provider: m.Provider, Model: m.Model, StopReason: "aborted"})
}

// imageTokenChars approximates one ≤1568px image (~1600 tokens) in the
// chars/4 estimate; counting its base64 length would overshoot ~10x.
const imageTokenChars = 1600 * 4

// imageChars is an image's share of the chars/4 estimate: its pixels over
// 750 tokens (what Anthropic bills), up to a full-size image when its size
// can't be read from its header.
func imageChars(b llm.Block) int {
	switch b.Type {
	case llm.BlockVideo:
		return videoChars(b)
	case llm.BlockAudio:
		return audioChars(b)
	}
	head := b.Data[:min(len(b.Data), 4096)]
	raw, err := base64.StdEncoding.DecodeString(head[:len(head)/4*4])
	if err != nil && len(raw) == 0 {
		return imageTokenChars
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return imageTokenChars
	}
	return min(cfg.Width*cfg.Height/750+1, 1600) * 4
}

func estimateTokens(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		for _, b := range m.Blocks {
			n += len(b.Text) + len(b.Content) + len(b.Input) + 16
			if b.Type == llm.BlockImage || b.Type == llm.BlockVideo || b.Type == llm.BlockAudio {
				n += imageChars(b)
			}
			for _, img := range b.Images {
				n += imageChars(img)
			}
		}
	}
	return n / 4
}

// isPrompt reports a user message typed by the user (not tool results).
func isPrompt(m llm.Message) bool {
	if m.Role != llm.RoleUser {
		return false
	}
	for _, b := range m.Blocks {
		if b.Type == llm.BlockToolResult {
			return false
		}
	}
	return true
}

func textOf(m *llm.Message) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, bl := range m.Blocks {
		if bl.Type == llm.BlockText {
			b.WriteString(bl.Text)
		}
	}
	return b.String()
}

// LastText is the final assistant text in the history.
func (a *Agent) LastText() string {
	for i := len(a.Messages) - 1; i >= 0; i-- {
		if a.Messages[i].Role == llm.RoleAssistant {
			if t := textOf(&a.Messages[i]); strings.TrimSpace(t) != "" {
				return t
			}
		}
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n/2] + fmt.Sprintf("\n…[%d chars omitted]…\n", len(s)-n) + s[len(s)-n/2:]
}

func renderTranscript(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		for _, bl := range m.Blocks {
			switch bl.Type {
			case llm.BlockText:
				if m.Role == llm.RoleUser {
					b.WriteString("\n[user]\n")
				} else {
					b.WriteString("\n[assistant]\n")
				}
				b.WriteString(clip(bl.Text, 12000))
				b.WriteByte('\n')
			case llm.BlockToolUse:
				fmt.Fprintf(&b, "\n[tool call %s] %s\n", bl.Name, clip(string(bl.Input), 2000))
			case llm.BlockToolResult:
				tag := "tool result"
				if bl.IsError {
					tag = "tool error"
				}
				fmt.Fprintf(&b, "[%s] %s\n", tag, clip(bl.Content, 2000))
				if len(bl.Images) > 0 {
					fmt.Fprintf(&b, "[%d image(s) returned]\n", len(bl.Images))
				}
			case llm.BlockImage:
				b.WriteString("[user attached an image]\n")
			case llm.BlockVideo:
				fmt.Fprintf(&b, "[user attached a video: %s]\n", video.ReadMeta(bl).Name)
			case llm.BlockAudio:
				fmt.Fprintf(&b, "[user attached audio: %s]\n", audio.ReadMeta(bl).Name)
			}
		}
	}
	return b.String()
}

// SteerArrived says the user sent a message to steer the running turn. A
// tool call that is only waiting (task_status, bash_output) stops waiting,
// so the message goes in with the next request; other tools run on.
// Safe from any goroutine.
func (a *Agent) SteerArrived() {
	a.wakeMu.Lock()
	defer a.wakeMu.Unlock()
	ch := a.wakeChanLocked()
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// SteerTaken says the messages SteerArrived announced were taken, by the
// Steer hook or as a prompt of their own (a follow-up after the turn), so
// later waits wait again. Safe from any goroutine.
func (a *Agent) SteerTaken() {
	a.wakeMu.Lock()
	defer a.wakeMu.Unlock()
	if a.wake != nil && closed(a.wake) {
		a.wake = nil
	}
}

func (a *Agent) wakeChan() <-chan struct{} {
	a.wakeMu.Lock()
	defer a.wakeMu.Unlock()
	return a.wakeChanLocked()
}

func (a *Agent) wakeChanLocked() chan struct{} {
	if a.wake == nil {
		a.wake = make(chan struct{})
	}
	return a.wake
}

func closed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
