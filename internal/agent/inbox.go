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
}

// messageNote heads a message to a subagent, so it knows who wrote it.
const messageNote = "Message from the agent that started you, sent while you work: "

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

// send queues text; false when its run doesn't take messages (it ended).
func (in *subInbox) send(text string) bool {
	in.mu.Lock()
	if !in.open {
		in.mu.Unlock()
		return false
	}
	in.msgs = append(in.msgs, text)
	wake := in.wake
	in.mu.Unlock()
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
		out = append(out, []llm.Block{{Type: llm.BlockText, Text: messageNote + m}})
	}
	in.msgs = nil
	return out
}
