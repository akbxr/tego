package llm

import (
	"bytes"
	"encoding/json"
	"strings"
)

// blockAccumulator buffers streamed deltas per message block so long
// responses are not rebuilt by repeated string concatenation.
type blockAccumulator struct {
	text     map[int]*strings.Builder // block index -> text/thinking so far
	args     map[int][]byte           // block index -> raw tool arguments so far
	complete map[int]bool             // tool blocks whose arguments finished
}

func newBlockAccumulator() *blockAccumulator {
	return &blockAccumulator{text: map[int]*strings.Builder{}, args: map[int][]byte{}, complete: map[int]bool{}}
}

func (a *blockAccumulator) addText(bi int, s string) {
	if s == "" {
		return
	}
	b := a.text[bi]
	if b == nil {
		b = &strings.Builder{}
		a.text[bi] = b
	}
	b.WriteString(s)
}

func (a *blockAccumulator) textLen(bi int) int {
	if b := a.text[bi]; b != nil {
		return b.Len()
	}
	return 0
}

func (a *blockAccumulator) addArgs(bi int, s string) {
	a.args[bi] = append(a.args[bi], s...)
}

// finishTool stores the accumulated (or given, if non-nil) arguments into
// the tool_use block ("{}" when empty; malformed JSON is wrapped by
// InvalidToolInput) and marks it complete.
func (a *blockAccumulator) finishTool(msg *Message, bi int, full []byte) {
	raw := full
	if raw == nil {
		raw = a.args[bi]
	}
	msg.Blocks[bi].Input = ToolInput(raw)
	delete(a.args, bi)
	a.complete[bi] = true
}

// invalidInputKey marks tool arguments that were not valid JSON. The raw
// text is kept (inside a valid object, so the history can be replayed) and
// the agent reports it back to the model instead of running the tool with
// made-up empty arguments.
const invalidInputKey = "_invalid_json"

// ToolInput normalises streamed tool arguments: empty becomes "{}", valid
// JSON is kept, anything else is wrapped by InvalidToolInput.
func ToolInput(raw []byte) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0:
		return json.RawMessage("{}")
	case json.Valid(raw):
		return json.RawMessage(bytes.Clone(raw))
	}
	return InvalidToolInput(raw)
}

// InvalidToolInput wraps malformed arguments in a valid JSON object.
func InvalidToolInput(raw []byte) json.RawMessage {
	b, _ := json.Marshal(map[string]string{invalidInputKey: string(raw)})
	return b
}

// InvalidInput returns the raw text of arguments wrapped by
// InvalidToolInput.
func InvalidInput(in json.RawMessage) (string, bool) {
	if !bytes.HasPrefix(in, []byte(`{"`+invalidInputKey+`":`)) {
		return "", false
	}
	var m map[string]string
	if json.Unmarshal(in, &m) != nil || len(m) != 1 {
		return "", false
	}
	raw, ok := m[invalidInputKey]
	return raw, ok
}

// finalize flushes buffered text into the blocks. When the turn failed or
// was aborted, tool_use blocks whose arguments never completed are dropped.
func (a *blockAccumulator) finalize(msg *Message, failed bool) {
	for bi, b := range a.text {
		msg.Blocks[bi].Text = b.String()
	}
	a.text = map[int]*strings.Builder{}
	if !failed {
		return
	}
	out := msg.Blocks[:0]
	for i, b := range msg.Blocks {
		if b.Type == BlockToolUse && !a.complete[i] {
			continue
		}
		out = append(out, b)
	}
	msg.Blocks = out
}
