package main

import (
	"strings"

	"github.com/akbxr/tego/internal/acp"
	"github.com/akbxr/tego/internal/llm"
)

// Steering over ACP, as Enter does in the terminal (steer.go): a prompt
// sent while one runs goes into it before its next model request, after
// the current tool round, or right away when the model would stop. Running
// tools go on; task_status and bash_output waits end for it. One sent too
// late for the turn runs as the next turn of the same run.
//
// ACP v1 answers a prompt when its turn ends, so a steering prompt is
// answered with the running prompt, with the same stop reason. ACP v2
// (draft) has agents take prompts during a turn and lets them steer it.
// Editors show what they send, so the message is not echoed back.
//
// A command (/compact) does not steer: it waits for the run, as the
// terminal's slash commands are follow-ups. Neither does a prompt sent after
// session/cancel (Zed's "send now", T3 Code's steering of ACP agents): the
// cancelled run ends, then it runs.

// acpSteer is a prompt sent during a run, for it.
type acpSteer struct {
	text   string
	images []llm.Block
}

// addSteerLocked queues a message for the running prompt.
func (sess *acpSession) addSteerLocked(text string, images []llm.Block) {
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		return
	}
	sess.steers = append(sess.steers, acpSteer{text: text, images: images})
}

// takeSteering is the agent's Steer hook: each waiting message becomes a
// user message.
func (sess *acpSession) takeSteering() [][]llm.Block {
	sess.mu.Lock()
	steers := sess.steers
	sess.steers = nil
	sess.mu.Unlock()
	out := make([][]llm.Block, 0, len(steers))
	for _, s := range steers {
		var blocks []llm.Block
		if s.text != "" {
			blocks = append(blocks, llm.Block{Type: llm.BlockText, Text: s.text})
		}
		out = append(out, append(blocks, s.images...))
	}
	return out
}

// steerMethod is claude-code-acp's steering extension, which it offers in
// the initialize response's _meta (steering.supported): a message for the
// running turn, answered at once rather than when the turn ends.
const steerMethod = "_session/steering"

type steerRequest struct {
	SessionID string             `json:"sessionId"`
	Prompt    []acp.ContentBlock `json:"prompt"`
	Meta      *struct {
		Steering *struct {
			IdleBehavior string `json:"idleBehavior"` // "promptRequired": no turn is started when idle
		} `json:"steering"`
	} `json:"_meta"`
}

type steerResponse struct {
	Outcome string `json:"outcome"` // injected, startedNewTurn or promptRequired
	Reason  string `json:"reason,omitempty"`
}

// steerExt serves steerMethod. With no turn to steer the message starts
// one, as claude-code-acp does, unless the editor would rather send it as a
// prompt itself.
func (sess *acpSession) steerExt(req steerRequest) steerResponse {
	text, images := promptContent(req.Prompt)
	sess.mu.Lock()
	if cur := sess.run; cur != nil && cur.ctx.Err() == nil && !isSlashCommand(text) {
		sess.addSteerLocked(text, images)
		sess.mu.Unlock()
		sess.ag.SteerArrived()
		return steerResponse{Outcome: "injected"}
	}
	sess.mu.Unlock()
	if m := req.Meta; m != nil && m.Steering != nil && m.Steering.IdleBehavior == "promptRequired" {
		return steerResponse{Outcome: "promptRequired", Reason: "noRunningTurn"}
	}
	go func() { _, _ = sess.prompt(sess.life, req.Prompt) }()
	return steerResponse{Outcome: "startedNewTurn"}
}
