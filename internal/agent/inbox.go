package agent

import (
	"sync"

	"github.com/akbxr/tego/internal/llm"
)

// subInbox holds the messages sent to a running subagent (task_status
// message): it reads them before its next request, as the session reads
// what the user types while it works.
type subInbox struct {
	mu   sync.Mutex
	msgs []string
	wake func() // ends a wait it is in (task_status, bash_output)
	open bool   // its run takes messages
	// shown, if set, shows a message where the subagent's work shows
	shown func(text string)
}

// messageNote heads a message to a subagent, so it knows who wrote it;
// userNote one the user sent it directly (the agents panel).
const (
	messageNote = "Message from the agent that started you, sent while you work: "
	userNote    = "Message from the user, sent to you directly while you work (the agent that started you is told too): "
)

// attach makes sub read the inbox, until detach.
func (in *subInbox) attach(sub *Agent) {
	in.mu.Lock()
	in.wake, in.open = sub.SteerArrived, true
	in.mu.Unlock()
	sub.Steer = in.take
}

func (in *subInbox) detach() {
	in.mu.Lock()
	in.open, in.wake = false, nil
	in.mu.Unlock()
}

// send queues text from the agent that started it; false when its run
// doesn't take messages (it ended).
func (in *subInbox) send(text string) bool { return in.post(messageNote+text, text) }

// tell queues text from the user.
func (in *subInbox) tell(text string) bool { return in.post(userNote+text, text) }

// post queues msg, shown as text where the subagent's work shows.
func (in *subInbox) post(msg, text string) bool {
	in.mu.Lock()
	if !in.open {
		in.mu.Unlock()
		return false
	}
	in.msgs = append(in.msgs, msg)
	wake, shown := in.wake, in.shown
	in.mu.Unlock()
	if shown != nil {
		shown(text)
	}
	if wake != nil {
		wake()
	}
	return true
}

func (in *subInbox) take() [][]llm.Block {
	in.mu.Lock()
	defer in.mu.Unlock()
	out := make([][]llm.Block, 0, len(in.msgs))
	for _, m := range in.msgs {
		out = append(out, []llm.Block{{Type: llm.BlockText, Text: m}})
	}
	in.msgs = nil
	return out
}
