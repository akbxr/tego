package session

import (
	"os"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// Files lists the session files of the project at cwd ("" = every
// project's), newest first, from the directory alone: Describe fills in the
// rest of one. Cheaper than List with many sessions.
func Files(base, cwd string) ([]Info, error) {
	var dirs []string
	if cwd != "" {
		dirs = []string{Dir(base, cwd)}
	} else {
		var err error
		if dirs, err = projectDirs(base); err != nil {
			return nil, err
		}
	}
	var out []Info
	for _, dir := range dirs {
		list, _ := dirFiles(dir, "")
		out = append(out, list...)
	}
	sortNewest(out)
	return out, nil
}

// Describe fills in what a session list shows of info: its title or first
// prompt, message count, folder and the session it was forked from. The
// file is read again only when it changed since it was last.
func Describe(info *Info) {
	var s summary
	if fi, err := os.Stat(info.Path); err == nil {
		s = summarize([]Info{{Path: info.Path, Modified: fi.ModTime(), Size: fi.Size()}}, nil)[0]
	}
	info.Preview, info.Count, info.Cwd, info.ForkOf = s.label, s.count, s.cwd, s.forkOf
}

// Peek reads the session at path without opening it for writing: a view of
// a session another tego may have open.
func Peek(path string) (*Session, error) { return load(path) }

// Turn is one entry of a session's active path, for reading it back.
type Turn struct {
	Time    time.Time
	Message llm.Message // media stays a reference: see MediaRef
	// Compacted: the agent's history was summarised here; Message is empty
	// and Summary holds what replaced the history before it.
	Compacted bool
	Summary   []llm.Message
}

// Transcript is the active path from its start, compactions included, so
// what came before a compaction can still be read. Media data is not read.
func (s *Session) Transcript() []Turn {
	var rev []*Node
	for n := s.nodes[s.leaf]; n != nil; n = s.nodes[n.Parent] {
		rev = append(rev, n)
	}
	r := s.reader()
	r.noMedia = true
	defer r.close()
	out := make([]Turn, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		n := rev[i]
		if n.compaction {
			out = append(out, Turn{Time: n.Time, Compacted: true, Summary: r.replacement(n)})
			continue
		}
		out = append(out, Turn{Time: n.Time, Message: r.message(n)})
	}
	return out
}

// MediaRef reports a block whose media stayed in the session file: its
// kind (image, video, audio) and media type.
func MediaRef(b llm.Block) (kind, mediaType string, ok bool) {
	if b.Type != blockRef {
		return "", "", false
	}
	return b.Name, b.MediaType, true
}

// TitleNow is the session's title, safe to read while another goroutine
// sets it.
func (s *Session) TitleNow() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Title
}
