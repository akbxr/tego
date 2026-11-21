package session

// Listing sessions shows of each file its title (or first prompt) and its
// message count, which only reading the file tells. What was read is kept
// while the file's size and modification time stay the same: the /resume
// picker lists again after every sort, tab, rename and delete, and reads
// only the files that changed. Find and Latest go by the file names (a
// session's id is its file's name), and read only the files they could
// return.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/akbxr/tego/internal/llm"
)

// summary is what a session list shows of a file that its directory entry
// does not tell.
type summary struct {
	label       string // the title, else the first prompt
	count       int    // messages on the active path
	cwd, forkOf string
}

type cachedSummary struct {
	size, mod int64 // the file's size and modification time (unix ns) it was read at
	sum       summary
}

func (c cachedSummary) fresh(f Info) bool {
	return c.size == f.Size && c.mod == f.Modified.UnixNano()
}

// summaries caches by folder, then file name. Listing a folder whole
// replaces its map, which forgets the files deleted since.
var summaries = struct {
	sync.Mutex
	dirs map[string]map[string]cachedSummary
}{dirs: map[string]map[string]cachedSummary{}}

// readHook, set by tests, hears of every file read for its summary.
var readHook func(path string)

// List returns sessions for cwd, newest first.
func List(base, cwd string) ([]Info, error) {
	dir := Dir(base, cwd)
	files, err := dirFiles(dir, "")
	if err != nil {
		return nil, err
	}
	out := withMessages(files, summarize(files, []string{dir}))
	sortNewest(out)
	return out, nil
}

// ListAll returns the sessions of every project, newest first.
func ListAll(base string) ([]Info, error) {
	dirs, err := projectDirs(base)
	if err != nil {
		return nil, err
	}
	var files []Info
	var whole []string
	for _, d := range dirs {
		list, err := dirFiles(d, "")
		if err != nil {
			continue // one unreadable project must not hide the rest
		}
		files = append(files, list...)
		whole = append(whole, d)
	}
	out := withMessages(files, summarize(files, whole))
	forgetProjectsGone(filepath.Join(base, "sessions"), dirs)
	sortNewest(out)
	return out, nil
}

// Find resolves an id prefix (or path) among cwd sessions, then among every
// project's. Like List it leaves out the sessions without messages, which
// takes reading the files whose names match, but no other.
func Find(base, cwd, ref string) (string, error) {
	if strings.HasSuffix(ref, ".jsonl") {
		return ref, nil
	}
	files, err := dirFiles(Dir(base, cwd), ref)
	if err != nil {
		return "", err
	}
	hits := withMessages(files, summarize(files, nil))
	if len(hits) == 0 { // ids are timestamps: one from another project is just as unique
		dirs, err := projectDirs(base)
		if err != nil {
			return "", err
		}
		files = nil
		for _, d := range dirs {
			list, _ := dirFiles(d, ref) // one unreadable project must not hide the rest
			files = append(files, list...)
		}
		hits = withMessages(files, summarize(files, nil))
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no session matching %q", ref)
	case 1:
		return hits[0].Path, nil
	}
	return "", fmt.Errorf("%q is ambiguous (%d sessions)", ref, len(hits))
}

// Latest is the path of the newest session of the project at cwd that has
// messages, List(base, cwd)[0], or "" when there is none. It reads the
// files newest first only until one has messages: a tego that has just
// started has a file of its own, newer and still empty.
func Latest(base, cwd string) (string, error) {
	files, err := dirFiles(Dir(base, cwd), "")
	if err != nil {
		return "", err
	}
	sortNewest(files)
	for _, f := range files {
		if summarize([]Info{f}, nil)[0].count > 0 {
			return f.Path, nil
		}
	}
	return "", nil
}

func sortNewest(list []Info) {
	sort.SliceStable(list, func(i, j int) bool { return list[i].Modified.After(list[j].Modified) })
}

// projectDirs are the session folders of every project.
func projectDirs(base string) ([]string, error) {
	root := filepath.Join(base, "sessions")
	ents, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var dirs []string
	for _, e := range ents {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	return dirs, nil
}

// dirFiles lists the session files in dir whose id starts with prefix, in
// directory order, from the directory alone. A missing dir has none.
func dirFiles(dir, prefix string) ([]Info, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Info
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || e.IsDir() || !strings.HasPrefix(id, prefix) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		fi, err := e.Info()
		if err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			fi, err = os.Stat(path) // the size and time of the file itself, which the cache goes by
		}
		if err != nil {
			continue
		}
		out = append(out, Info{ID: id, Path: path, Modified: fi.ModTime(), Size: fi.Size()})
	}
	return out, nil
}

// summarize returns the summaries of files: the cached ones of the files
// unchanged since, the others read now, several at a time. A file that
// cannot be read has a zero summary. whole are the folders files lists in
// full: their cached summaries are replaced by these, which forgets the
// files gone since.
func summarize(files []Info, whole []string) []summary {
	sums := make([]summary, len(files))
	read := make([]bool, len(files))
	var todo []int
	summaries.Lock()
	for i, f := range files {
		if c, ok := summaries.dirs[filepath.Dir(f.Path)][filepath.Base(f.Path)]; ok && c.fresh(f) {
			sums[i], read[i] = c.sum, true
		} else {
			todo = append(todo, i)
		}
	}
	summaries.Unlock()
	parallel(len(todo), func(k int) {
		i := todo[k]
		sum, err := readSummary(files[i].Path)
		// A line too long to read stays so while the file is the same;
		// other errors may pass, and are not cached.
		sums[i], read[i] = sum, err == nil || errors.Is(err, bufio.ErrTooLong)
	})
	summaries.Lock()
	defer summaries.Unlock()
	for _, d := range whole {
		delete(summaries.dirs, d)
	}
	for i, f := range files {
		if !read[i] {
			continue
		}
		dir := filepath.Dir(f.Path)
		m := summaries.dirs[dir]
		if m == nil {
			m = map[string]cachedSummary{}
			summaries.dirs[dir] = m
		}
		m[filepath.Base(f.Path)] = cachedSummary{size: f.Size, mod: f.Modified.UnixNano(), sum: sums[i]}
	}
	return sums
}

// forgetProjectsGone drops the cached summaries of the project folders in
// root other than dirs.
func forgetProjectsGone(root string, dirs []string) {
	keep := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		keep[d] = true
	}
	summaries.Lock()
	defer summaries.Unlock()
	for d := range summaries.dirs {
		if filepath.Dir(d) == root && !keep[d] {
			delete(summaries.dirs, d)
		}
	}
}

// withMessages is files with their summaries, those without messages left
// out.
func withMessages(files []Info, sums []summary) []Info {
	var out []Info
	for i, f := range files {
		s := sums[i]
		if s.count == 0 {
			continue
		}
		f.Preview, f.Count, f.Cwd, f.ForkOf = s.label, s.count, s.cwd, s.forkOf
		out = append(out, f)
	}
	return out
}

// parallel runs do(i) for every i < n on up to min(GOMAXPROCS, 8)
// goroutines.
func parallel(n int, do func(i int)) {
	workers := min(runtime.GOMAXPROCS(0), 8, n)
	if workers <= 1 {
		for i := range n {
			do(i)
		}
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := int(next.Add(1) - 1); i < n; i = int(next.Add(1) - 1) {
				do(i)
			}
		}()
	}
	wg.Wait()
}

// sumNode is what readSummary keeps of a message or compaction node.
type sumNode struct {
	id, parent string
	compaction bool
	prompt     string // a user message's first text
	off        int64
	size       int
}

var subagentPrefix = []byte(`{"type":"subagent",`)

// readSummary reads the summary of the file at path as load and History
// would see it, without keeping the file: blob and subagent lines are
// passed over undecoded, of a message only its first prompt is kept, and a
// compaction's history is read back for its length and first prompt, its
// media left in the file.
func readSummary(path string) (summary, error) {
	if readHook != nil {
		readHook(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return summary{}, err
	}
	defer f.Close()
	var (
		sum         summary
		title, leaf string
		legacy      int
		pos         int64
		nodes       = map[string]*sumNode{}
		blobs       = map[string]bool{}
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		off := pos
		pos += int64(len(line)) + 1 // tego writes \n line ends
		if sha, ok := blobLine(line); ok {
			blobs[sha] = true
			continue
		}
		if bytes.HasPrefix(line, subagentPrefix) {
			continue // nothing a list shows
		}
		var e lightEntry // what load reads, so that the same lines fail
		if json.Unmarshal(line, &e) != nil {
			continue // tolerate a torn line
		}
		switch e.Type {
		case "header":
			sum.cwd, sum.forkOf = e.Cwd, e.ForkOf
		case "message", "compaction":
			if e.ID == "" { // pre-tree files: a linear chain
				legacy++
				e.ID, e.Parent = fmt.Sprintf("legacy-%d", legacy), leaf
			}
			if nodes[e.Parent] == nil {
				e.Parent = ""
			}
			nodes[e.ID] = &sumNode{id: e.ID, parent: e.Parent, compaction: e.Type == "compaction",
				prompt: e.Message.prompt(), off: off, size: len(line)}
			leaf = e.ID
		case "blob":
			if e.SHA != "" {
				blobs[e.SHA] = true
			}
		case "title":
			title = e.Title
		case "leaf":
			if nodes[e.ID] != nil || e.ID == "" {
				leaf = e.ID
			}
		}
	}
	if err := sc.Err(); err != nil {
		return summary{}, err
	}
	// The active path back to its last compaction, newest first; a loop
	// (a file with ids used twice) ends it.
	var active []*sumNode
	var comp *sumNode
	for n := nodes[leaf]; n != nil && len(active) <= len(nodes); n = nodes[n.parent] {
		if n.compaction {
			comp = n
			break
		}
		active = append(active, n)
	}
	first := ""
	if comp != nil {
		msgs := compactedHistory(f, comp)
		sum.count = len(msgs)
		first = firstText(msgs, blobs)
	}
	sum.count += len(active)
	for i := len(active) - 1; i >= 0 && first == ""; i-- {
		first = active[i].prompt
	}
	if title != "" {
		sum.label = Oneline(title, 70)
	} else {
		sum.label = Oneline(first, 70) // FirstPrompt's
	}
	return sum, nil
}

// compactedHistory reads compaction n's history back from f, as
// fileReader.replacement does, its media left as references.
func compactedHistory(f *os.File, n *sumNode) []llm.Message {
	buf := make([]byte, n.size)
	if _, err := f.ReadAt(buf, n.off); err != nil {
		return nil
	}
	var e entry
	if json.Unmarshal(buf, &e) != nil || (e.ID != "" && e.ID != n.id) {
		return nil
	}
	return e.Messages
}

// firstText is the text FirstPrompt takes from msgs, before it is
// shortened. A media reference reads as History would have it: its block
// when blobs has its data, else a note that it is missing.
func firstText(msgs []llm.Message, blobs map[string]bool) string {
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Blocks {
			if b.Type == blockRef {
				if blobs[strings.TrimPrefix(b.Data, refPrefix)] {
					b.Type = llm.BlockType(b.Name)
				} else {
					b = llm.Block{Type: llm.BlockText, Text: fmt.Sprintf("[%s %s is missing from the session file]", b.Name, b.MediaType)}
				}
			}
			if b.Type == llm.BlockText && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}

// prompt is the text FirstPrompt takes from the message's outline: a user
// message's first text that is not empty.
func (o *outlineEntry) prompt() string {
	if o == nil || o.Role != llm.RoleUser {
		return ""
	}
	for _, b := range o.Blocks {
		typ := b.Type
		if typ == blockRef {
			typ = llm.BlockType(b.Name)
		}
		if typ == llm.BlockText && b.Text != "" {
			return b.Text
		}
	}
	return ""
}

var (
	blobPrefix = []byte(`{"type":"blob","sha":"`)
	blobData   = []byte(`","data":"`)
	blobEnd    = []byte(`"}`)
)

// blobLine reads a blob entry as tego writes them,
// {"type":"blob","sha":"<hex>","data":"<base64>"}, without decoding its
// data. The line is whole JSON when the data has no quote, backslash or
// control byte, which is quicker to check. Any other line is not one (ok
// false), for json.Unmarshal to read.
func blobLine(line []byte) (sha string, ok bool) {
	rest, ok := bytes.CutPrefix(line, blobPrefix)
	if !ok {
		return "", false
	}
	i := bytes.IndexByte(rest, '"')
	if i <= 0 {
		return "", false
	}
	for _, c := range rest[:i] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	data, ok := bytes.CutPrefix(rest[i:], blobData)
	if !ok {
		return "", false
	}
	if data, ok = bytes.CutSuffix(data, blobEnd); !ok {
		return "", false
	}
	if bytes.IndexByte(data, '"') >= 0 || bytes.IndexByte(data, '\\') >= 0 || hasControl(data) {
		return "", false
	}
	return string(rest[:i]), true
}

// hasControl reports whether b holds a byte below 0x20, which a JSON
// string cannot. It tests eight bytes at a time: x-0x20 borrows into the
// top bit of the lowest byte below 0x20, and of no byte when there is none.
func hasControl(b []byte) bool {
	for ; len(b) >= 8; b = b[8:] {
		x := binary.LittleEndian.Uint64(b)
		if (x-0x2020202020202020)&^x&0x8080808080808080 != 0 {
			return true
		}
	}
	for _, c := range b {
		if c < 0x20 {
			return true
		}
	}
	return false
}
