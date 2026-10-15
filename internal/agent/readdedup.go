package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// Repeated reads: a read of a file the model already read, with the same
// range, while the file is unchanged and that earlier result is still intact
// in the context, is answered with a short note instead of the content again.
// Re-reads of unchanged files waste 6-18% of the tool budget of coding
// agents (SWE-Marathon, 2026); in tego's own benchmarks Gemini re-read 3-6
// files per task, mostly right after updating its todo list.

// unchangedNote starts the reply to a repeated read, so a later repeat can
// tell it apart from real file content.
const unchangedNote = "Unchanged: "

// fileStamp is a file's state when the model read it.
type fileStamp struct {
	mod  time.Time
	size int64
}

type readArgs struct {
	path          string
	offset, limit int
}

// parseRead returns a read call's arguments, normalised so that equivalent
// calls compare equal. ok is false for reads this does not apply to (URLs,
// bad input).
func parseRead(in json.RawMessage) (readArgs, bool) {
	var r struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if json.Unmarshal(in, &r) != nil || r.Path == "" || strings.HasPrefix(r.Path, "http://") || strings.HasPrefix(r.Path, "https://") {
		return readArgs{}, false
	}
	return readArgs{path: filepath.Clean(r.Path), offset: max(r.Offset, 1), limit: max(r.Limit, 0)}, true
}

// stampFile is the state of the regular, non-image file at p.
func stampFile(p string) (fileStamp, bool) {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return fileStamp{}, false
	}
	return fileStamp{st.ModTime(), st.Size()}, true
}

// stampBeforeRead is the state of the file call is about to read. Taken
// before reading: a change during the read then shows as a change later.
func stampBeforeRead(call llm.Block) (fileStamp, bool) {
	args, ok := parseRead(call.Input)
	if !ok {
		return fileStamp{}, false
	}
	return stampFile(args.path)
}

// keepStamp records st for a read that returned the file's text.
func (a *Agent) keepStamp(call llm.Block, st fileStamp, res llm.Block) {
	if res.IsError || len(res.Images) > 0 {
		return
	}
	a.readMu.Lock()
	defer a.readMu.Unlock()
	if a.readStamps == nil {
		a.readStamps = map[string]fileStamp{}
	}
	a.readStamps[call.ID] = st
}

// unchangedRead is the note that answers call when it repeats an earlier
// read whose result is still in context, of a file unchanged since; "" if it
// must run. Safe while a batch runs: a.Messages only changes between batches.
func (a *Agent) unchangedRead(call llm.Block) string {
	args, ok := parseRead(call.Input)
	if !ok {
		return ""
	}
	now, ok := stampFile(args.path)
	if !ok {
		return ""
	}
	// The newest earlier read of the same range; its result follows it.
	for i := len(a.Messages) - 1; i >= 0; i-- {
		for _, b := range a.Messages[i].Blocks {
			if b.Type != llm.BlockToolUse || b.Name != "read" || b.ID == call.ID {
				continue
			}
			if prev, ok := parseRead(b.Input); !ok || prev != args {
				continue
			}
			res, at := a.resultOf(b.ID, i+1)
			switch {
			case at < 0:
				continue // in this same batch: no result yet
			case res.IsError:
				return ""
			case strings.HasPrefix(res.Content, unchangedNote):
				continue // a note, not content: look further back
			case at < a.clearMark, strings.HasPrefix(res.Content, elidedMarker), len(res.Images) > 0:
				return "" // gone from the context (or the provider may have cleared it)
			}
			a.readMu.Lock()
			then, ok := a.readStamps[b.ID]
			a.readMu.Unlock()
			if !ok || then != now {
				return "" // changed since, or read before this process (resumed session)
			}
			return fmt.Sprintf("%s%s has not changed since you read it with the same range, and that result is still in "+
				"your context above, so it is not repeated. To see it again anyway, read it with a different offset or limit.", unchangedNote, args.path)
		}
	}
	return ""
}

// resultOf finds the result of tool call id at or after message from, and
// the index of its message (-1 if none).
func (a *Agent) resultOf(id string, from int) (llm.Block, int) {
	for i := from; i < len(a.Messages); i++ {
		for _, b := range a.Messages[i].Blocks {
			if b.Type == llm.BlockToolResult && b.ToolUseID == id {
				return b, i
			}
		}
	}
	return llm.Block{}, -1
}
