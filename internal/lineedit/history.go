package lineedit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// historyCap is the maximum number of entries kept in memory and on disk.
const historyCap = 1000

// history stores submitted lines. On disk each entry is one JSON string per
// line so multi-line entries round-trip.
type history struct {
	path    string
	entries []string
	loaded  bool
}

// load reads the history file once. Files grown past historyCap (appends
// never trim) are rewritten with only the newest entries.
func (h *history) load() {
	if h.loaded {
		return
	}
	h.loaded = true
	if h.path == "" {
		return
	}
	f, err := os.Open(h.path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	total := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		var s string
		if json.Unmarshal([]byte(line), &s) != nil {
			s = line // tolerate plain-text lines
		}
		total++
		h.push(s)
	}
	if total > historyCap {
		h.rewrite()
	}
}

// push appends s in memory, skipping blanks and consecutive duplicates.
func (h *history) push(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	if n := len(h.entries); n > 0 && h.entries[n-1] == s {
		return false
	}
	h.entries = append(h.entries, s)
	if over := len(h.entries) - historyCap; over > 0 {
		h.entries = append(h.entries[:0], h.entries[over:]...)
	}
	return true
}

func (h *history) add(s string) {
	h.load()
	if !h.push(s) || h.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(h.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	line, _ := json.Marshal(s)
	_, _ = f.Write(append(line, '\n'))
}

// rewrite replaces the file with the in-memory entries via a temp file.
func (h *history) rewrite() {
	var sb strings.Builder
	for _, e := range h.entries {
		line, _ := json.Marshal(e)
		sb.Write(line)
		sb.WriteByte('\n')
	}
	tmp := h.path + ".tmp"
	if os.WriteFile(tmp, []byte(sb.String()), 0o600) != nil {
		return
	}
	if os.Rename(tmp, h.path) != nil {
		_ = os.Remove(tmp)
	}
}
