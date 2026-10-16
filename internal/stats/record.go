// Package stats records model requests and tool calls and summarises them
// (tego stats): totals, cost estimates, breakdowns and a dashboard.
package stats

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// Record is one line of the stats log: a model request or a tool call.
type Record struct {
	Kind     string `json:"kind"` // "request" or "tool"
	TS       int64  `json:"ts"`   // unix milliseconds
	Folder   string `json:"folder,omitempty"`
	Session  string `json:"session,omitempty"`
	Class    string `json:"class,omitempty"` // main, subagent, compaction, auxiliary
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// request
	Input      int      `json:"input,omitempty"`
	Output     int      `json:"output,omitempty"`
	CacheRead  int      `json:"cache_read,omitempty"`
	CacheWrite int      `json:"cache_write,omitempty"`
	LongCache  bool     `json:"long_cache,omitempty"`
	Fast       bool     `json:"fast,omitempty"` // ran in fast mode: every token at twice the price
	DurationMs int64    `json:"duration_ms,omitempty"`
	TTFTMs     int64    `json:"ttft_ms,omitempty"`
	GenStartMs int64    `json:"gen_start_ms,omitempty"` // when generation began (older records: none)
	GenOutput  int      `json:"gen_output,omitempty"`   // output generated after that (0 = all of Output)
	Stop       string   `json:"stop,omitempty"`
	Error      string   `json:"error,omitempty"`
	Tools      []string `json:"tools,omitempty"`
	// tool
	Tool      string `json:"tool,omitempty"`
	ToolError bool   `json:"tool_error,omitempty"`
}

// Log appends records to <dir>/stats.jsonl. Each record is one O_APPEND
// write, so several tego processes can share the file.
type Log struct {
	path string
	mu   sync.Mutex
}

func OpenLog(dir string) *Log { return &Log{path: filepath.Join(dir, "stats.jsonl")} }

func (l *Log) Add(r Record) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	f.Close()
}

// RequestRecord converts a finished request.
func RequestRecord(st llm.RequestStat, folder string) Record {
	class := st.Class
	if class == "" {
		class = llm.ClassMain
	}
	return Record{Kind: "request", TS: st.Start.UnixMilli(), Folder: folder, Session: st.SessionID, Class: class,
		Provider: st.Provider, Model: st.Model,
		Input: st.Usage.Input, Output: st.Usage.Output, CacheRead: st.Usage.CacheRead, CacheWrite: st.Usage.CacheWrite,
		LongCache: st.LongCache, Fast: st.Usage.Fast, DurationMs: st.Duration.Milliseconds(), TTFTMs: st.TTFT.Milliseconds(),
		GenStartMs: st.GenStart.Milliseconds(), GenOutput: st.GenOutput,
		Stop: st.StopReason, Error: st.Err, Tools: st.Tools}
}

// Load reads the stats log plus the history imported from session files
// that predate it. The import runs once, on the first Load: it stores the
// session-file records in <dir>/stats-history.jsonl, so later loads read
// two small files instead of every session.
func Load(dir string) ([]Record, error) {
	recs, err := readLog(filepath.Join(dir, "stats.jsonl"))
	if err != nil {
		return nil, err
	}
	histPath := filepath.Join(dir, "stats-history.jsonl")
	history, err := readLog(histPath)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(histPath); os.IsNotExist(statErr) {
		cutoff := time.Now().UnixMilli()
		for _, r := range recs {
			cutoff = min(cutoff, r.TS)
		}
		history = readSessions(filepath.Join(dir, "sessions"), cutoff)
		writeHistory(histPath, history)
	}
	recs = append(recs, history...)
	sort.Slice(recs, func(i, j int) bool { return recs[i].TS < recs[j].TS })
	return recs, nil
}

// writeHistory stores the imported records, empty or not: the file's
// existence marks the import as done. tmp+rename keeps a concurrent Load
// from reading half a file.
func writeHistory(path string, recs []Record) {
	var b []byte
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			continue
		}
		b = append(append(b, line...), '\n')
	}
	tmp := path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

func readLog(path string) ([]Record, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Kind != "" {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

type sessionEntry struct {
	Type    string       `json:"type"`
	Time    int64        `json:"ts"`
	Cwd     string       `json:"cwd"`
	Created time.Time    `json:"created"`
	Message *llm.Message `json:"message"`
}

// readSessions turns session messages older than cutoff (unix ms) into
// records. Forks and clones copy their history, so a request is counted
// once however many files hold it.
func readSessions(root string, cutoff int64) []Record {
	files, _ := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	seenReq, seenTool := map[string]bool{}, map[string]bool{}
	var out []Record
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		folder := ""
		var last int64               // old sessions lack per-message ts: use the latest known time
		names := map[string]string{} // tool_use id → tool name
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
		for sc.Scan() {
			var e sessionEntry
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			if e.Type == "header" {
				folder = e.Cwd
				if !e.Created.IsZero() {
					last = e.Created.UnixMilli()
				}
				continue
			}
			m := e.Message
			ts := e.Time * 1000
			if ts == 0 {
				ts = last
			}
			last = ts
			if ts == 0 {
				continue
			}
			if e.Type != "message" || m == nil || ts >= cutoff {
				continue
			}
			switch m.Role {
			case llm.RoleAssistant:
				if m.Usage == nil {
					continue
				}
				r := Record{Kind: "request", TS: ts, Folder: folder, Session: sid, Class: llm.ClassMain,
					Provider: m.Provider, Model: m.Model, Input: m.Usage.Input, Output: m.Usage.Output,
					CacheRead: m.Usage.CacheRead, CacheWrite: m.Usage.CacheWrite, Stop: m.StopReason}
				key := m.Provider + "|" + m.Model + "|" + strconv.Itoa(r.Input) + "|" + strconv.Itoa(r.Output) + "|" +
					strconv.Itoa(r.CacheRead) + "|" + strconv.Itoa(r.CacheWrite)
				for _, b := range m.Blocks {
					if b.Type == llm.BlockToolUse {
						r.Tools = append(r.Tools, b.Name)
						names[b.ID] = b.Name
						key += "|" + b.ID
					}
				}
				if r.Stop == "error" {
					r.Error = "error"
				}
				if !seenReq[key] {
					seenReq[key] = true
					out = append(out, r)
				}
			case llm.RoleUser:
				for _, b := range m.Blocks {
					if b.Type != llm.BlockToolResult || seenTool[b.ToolUseID] {
						continue
					}
					seenTool[b.ToolUseID] = true
					if name := names[b.ToolUseID]; name != "" {
						out = append(out, Record{Kind: "tool", TS: ts, Folder: folder, Session: sid, Class: llm.ClassMain,
							Tool: name, ToolError: b.IsError})
					}
				}
			}
		}
		f.Close()
	}
	return out
}
