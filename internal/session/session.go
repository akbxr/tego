// Package session persists conversations as append-only JSONL files under
// ~/.tego/sessions/<cwd-slug>/.
//
// A session is a tree: every message entry records its parent,
// so jumping back to an earlier point (/tree) and continuing creates a new
// branch while the old path stays in the file. The active path ends at the
// "leaf": the last appended node, or the target of the latest leaf entry.
//
// Media (any block with Data: images, video, audio, a tool result's images)
// is stored once per file. The first write of a given Data appends a blob
// entry, and blocks refer to it (see refBlock):
//
//	{"type":"blob","sha":"<sha256 hex of data>","data":"<base64>"}
//	{"type":"media_ref","name":"image","media_type":"image/png","data":"sha256:<hex>"}
//
// Files without blobs hold the data inline and read as before.
package session

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

type entry struct {
	Type    string `json:"type"` // header | message | compaction | title | leaf | blob
	ID      string `json:"id,omitempty"`
	Parent  string `json:"parent,omitempty"`
	Time    int64  `json:"ts,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Created string `json:"created,omitempty"`
	ForkOf  string `json:"fork_of,omitempty"` // header: session this one was forked/cloned from
	// message
	Message *llm.Message `json:"message,omitempty"`
	// compaction: the path's history up to here is replaced by Messages.
	Messages []llm.Message `json:"messages,omitempty"`
	// title
	Title string `json:"title,omitempty"`
	// blob: a media block's Data, referenced by its sha256
	SHA  string `json:"sha,omitempty"`
	Data string `json:"data,omitempty"`
}

// A reference block keeps every field of the block but Data, and its type
// in Name. Its type is one no provider sends, so an older tego reading the
// file leaves the media out instead of sending empty data, and keeps the
// reference whole when it copies the block (compaction, fork).
const (
	blockRef  llm.BlockType = "media_ref"
	refPrefix               = "sha256:"
	minBlob                 = 1024 // shorter data stays inline
)

// blobLoc is where a blob entry's line is in the file.
type blobLoc struct {
	off  int64
	size int
}

// Node is one message (or compaction) in the session tree.
//
// Nodes stay small whatever the session holds: Message is an outline of
// the message (its role, text blocks and the types of the others), enough
// to show it. The whole message, and a compaction's history, are read from
// the file when History or Replacement asks for them.
type Node struct {
	ID       string
	Parent   string
	Time     time.Time
	Message  *llm.Message // message node: outline (role, text blocks, block types)
	Children []*Node

	compaction bool
	off        int64 // where the node's line starts in the file; -1 = not written
	size       int   // its length, without the newline
	// What could not be written (the file was closed) is kept here.
	full *llm.Message
	repl []llm.Message
}

// IsCompaction reports whether n replaces the history before it
// (Session.Replacement holds what with).
func (n *Node) IsCompaction() bool { return n.compaction }

type Session struct {
	ID   string
	Path string
	f    *os.File

	nodes map[string]*Node
	roots []*Node
	leaf  string // "" = before the first node

	mu     sync.Mutex         // serialises writes
	blobs  map[string]blobLoc // sha -> blob entry in the file (under mu)
	Title  string             // generated title ("" = none yet)
	Cwd    string             // working directory the session was started in ("" in files without a header)
	ForkOf string             // id of the session this one was forked or cloned from ("" = none)
}

type Info struct {
	ID       string
	Path     string
	Modified time.Time
	Preview  string // the session title, else the first user message on the active path
	Count    int
	Cwd      string // working directory the session was started in
	Size     int64  // file size in bytes
	ForkOf   string // id of the session this one was forked or cloned from
}

// Dir is the per-project session directory. The cwd is symlink-resolved so
// /tmp vs /private/tmp (or a symlinked checkout) share one history.
func Dir(base, cwd string) string {
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	slug := strings.Trim(strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '-'
		}
		return r
	}, cwd), "-")
	return filepath.Join(base, "sessions", slug)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func New(base, cwd string) (*Session, error) { return create(base, cwd, "") }

func create(base, cwd, forkOf string) (*Session, error) {
	dir := Dir(base, cwd)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	id := time.Now().Format("20060102-150405") + "-" + randHex(3)
	path := filepath.Join(dir, id+".jsonl")
	// Read as well: History reads the messages back (and still can once the
	// file is moved or deleted).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	s := &Session{ID: id, Path: path, f: f, nodes: map[string]*Node{}, Cwd: cwd, ForkOf: forkOf}
	_, _, err = s.write(entry{Type: "header", ID: id, Cwd: cwd, Created: time.Now().Format(time.RFC3339), ForkOf: forkOf})
	return s, err
}

// NewFrom starts a new session holding history (fork/clone of forkOf).
func NewFrom(base, cwd string, history []llm.Message, forkOf string) (*Session, error) {
	s, err := create(base, cwd, forkOf)
	if err != nil {
		return nil, err
	}
	for _, m := range history {
		if err := s.Append(m); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// Open loads an existing session, reopens it for appending and returns the
// active path's history.
func Open(path string) (*Session, []llm.Message, error) {
	s, err := load(path)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}
	s.f = f
	return s, s.History(s.leaf), nil
}

// lightEntry is what load reads of an entry: no compaction history, and
// of a message only its outline. The rest is skipped without being kept.
type lightEntry struct {
	Type    string        `json:"type"`
	ID      string        `json:"id"`
	Parent  string        `json:"parent"`
	Time    int64         `json:"ts"`
	Cwd     string        `json:"cwd"`
	ForkOf  string        `json:"fork_of"`
	Title   string        `json:"title"`
	SHA     string        `json:"sha"`
	Message *outlineEntry `json:"message"`
}

type outlineEntry struct {
	Role   llm.Role `json:"role"`
	Blocks []struct {
		Type llm.BlockType `json:"type"`
		Text string        `json:"text"`
		ID   string        `json:"id"`
		Name string        `json:"name"`
	} `json:"content"`
}

func (o *outlineEntry) message() *llm.Message {
	if o == nil {
		return nil
	}
	m := &llm.Message{Role: o.Role, Blocks: make([]llm.Block, len(o.Blocks))}
	for i, b := range o.Blocks {
		if b.Type == blockRef {
			b.Type, b.Name = llm.BlockType(b.Name), ""
		}
		m.Blocks[i] = outlineBlock(llm.Block{Type: b.Type, Text: b.Text, ID: b.ID, Name: b.Name})
	}
	return m
}

// outline is what a node keeps of m: its role, text blocks, and the type
// (and tool call id and name) of every other block.
func outline(m *llm.Message) *llm.Message {
	if m == nil {
		return nil
	}
	o := &llm.Message{Role: m.Role, Blocks: make([]llm.Block, len(m.Blocks))}
	for i, b := range m.Blocks {
		o.Blocks[i] = outlineBlock(b)
	}
	return o
}

func outlineBlock(b llm.Block) llm.Block {
	o := llm.Block{Type: b.Type, ID: b.ID, Name: b.Name}
	if b.Type == llm.BlockText {
		o.Text = b.Text
	}
	return o
}

func load(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := &Session{ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Path: path, nodes: map[string]*Node{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024*1024)
	legacy := 0
	var pos int64
	for sc.Scan() {
		line := sc.Bytes()
		off := pos
		pos += int64(len(line)) + 1 // tego writes \n line ends
		var e lightEntry
		if json.Unmarshal(line, &e) != nil {
			continue // tolerate a torn line
		}
		switch e.Type {
		case "header":
			s.Cwd, s.ForkOf = e.Cwd, e.ForkOf
		case "message", "compaction":
			if e.ID == "" { // pre-tree files: a linear chain
				legacy++
				e.ID, e.Parent = fmt.Sprintf("legacy-%d", legacy), s.leaf
			}
			s.add(&Node{ID: e.ID, Parent: e.Parent, Time: time.Unix(e.Time, 0), Message: e.Message.message(),
				compaction: e.Type == "compaction", off: off, size: len(line)})
		case "blob":
			if _, ok := s.blobs[e.SHA]; !ok && e.SHA != "" {
				if s.blobs == nil {
					s.blobs = map[string]blobLoc{}
				}
				s.blobs[e.SHA] = blobLoc{off, len(line)}
			}
		case "title":
			s.Title = e.Title
		case "leaf":
			if _, ok := s.nodes[e.ID]; ok || e.ID == "" {
				s.leaf = e.ID
			}
		}
	}
	return s, sc.Err()
}

func (s *Session) add(n *Node) *Node {
	s.nodes[n.ID] = n
	if p, ok := s.nodes[n.Parent]; ok {
		p.Children = append(p.Children, n)
	} else {
		n.Parent = ""
		s.roots = append(s.roots, n)
	}
	s.leaf = n.ID
	return n
}

// write appends e and says where its line is: off -1 when the session has
// no file open.
func (s *Session) write(e entry) (off int64, size int, err error) {
	if s == nil {
		return -1, 0, nil
	}
	s.mu.Lock() // titles are written from another goroutine than messages
	defer s.mu.Unlock()
	if s.f == nil {
		return -1, 0, nil
	}
	// Media first: the blobs a message refers to are in the file before it.
	if e.Message != nil {
		m := *e.Message
		if m.Blocks, err = s.refBlocks(m.Blocks); err != nil {
			return -1, 0, err
		}
		e.Message = &m
	}
	if e.Messages != nil {
		msgs := slices.Clone(e.Messages)
		for i := range msgs {
			if msgs[i].Blocks, err = s.refBlocks(msgs[i].Blocks); err != nil {
				return -1, 0, err
			}
		}
		e.Messages = msgs
	}
	data, err := json.Marshal(e)
	if err != nil {
		return -1, 0, err
	}
	return s.writeLine(data)
}

// writeLine appends data as a line; s.mu is held.
func (s *Session) writeLine(data []byte) (off int64, size int, err error) {
	if _, err := s.f.Write(append(data, '\n')); err != nil {
		return -1, 0, err
	}
	// The file is opened for appending: the offset is now the end of this
	// line, even when another process appended to the file.
	end, err := s.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return -1, 0, nil
	}
	return end - int64(len(data)) - 1, len(data), nil
}

// refBlocks is blocks with their media replaced by references, the blobs
// written first where the file lacks them. blocks is copied on the first
// change: the caller's are shared with the agent. s.mu is held.
func (s *Session) refBlocks(blocks []llm.Block) ([]llm.Block, error) {
	var out []llm.Block
	for i, b := range blocks {
		nb, changed, err := s.refBlock(b)
		if err != nil {
			return nil, err
		}
		if changed {
			if out == nil {
				out = slices.Clone(blocks)
			}
			out[i] = nb
		}
	}
	if out == nil {
		return blocks, nil
	}
	return out, nil
}

// refBlock keys on Data, whatever the block's type. A block that uses Name
// stays inline: the reference keeps the type there.
func (s *Session) refBlock(b llm.Block) (llm.Block, bool, error) {
	changed := false
	if len(b.Images) > 0 {
		imgs, err := s.refBlocks(b.Images)
		if err != nil {
			return b, false, err
		}
		if &imgs[0] != &b.Images[0] {
			b.Images, changed = imgs, true
		}
	}
	if len(b.Data) < minBlob || b.Name != "" || b.Type == blockRef {
		return b, changed, nil
	}
	sum := sha256.Sum256([]byte(b.Data))
	sha := hex.EncodeToString(sum[:])
	if _, ok := s.blobs[sha]; !ok {
		line, err := json.Marshal(entry{Type: "blob", SHA: sha, Data: b.Data})
		if err != nil {
			return b, false, err
		}
		off, size, err := s.writeLine(line)
		if err != nil {
			return b, false, err
		}
		if off < 0 { // where it went is unknown: keep the data inline
			return b, changed, nil
		}
		if s.blobs == nil {
			s.blobs = map[string]blobLoc{}
		}
		s.blobs[sha] = blobLoc{off, size}
	}
	b.Type, b.Name, b.Data = blockRef, string(b.Type), refPrefix+sha
	return b, true, nil
}

// fileReader reads the session file back: through the open file, or
// (closed, or loaded only) the file opened again until the reader is
// closed.
type fileReader struct {
	s     *Session
	g     *os.File
	blobs map[string]string // read so far: the messages of one History share each blob's data
}

func (s *Session) reader() *fileReader { return &fileReader{s: s} }

func (r *fileReader) readAt(buf []byte, off int64) error {
	if r.g == nil {
		r.s.mu.Lock()
		f := r.s.f
		r.s.mu.Unlock()
		if f != nil {
			if _, err := f.ReadAt(buf, off); err == nil {
				return nil
			}
		}
		g, err := os.Open(r.s.Path)
		if err != nil {
			return err
		}
		r.g = g
	}
	_, err := r.g.ReadAt(buf, off)
	return err
}

func (r *fileReader) close() {
	if r.g != nil {
		r.g.Close()
	}
}

// read reads n's entry back from the file.
func (r *fileReader) read(n *Node) (entry, bool) {
	var e entry
	if n.off < 0 {
		return e, false
	}
	buf := make([]byte, n.size)
	if r.readAt(buf, n.off) != nil {
		return e, false
	}
	if json.Unmarshal(buf, &e) != nil || (e.ID != "" && e.ID != n.ID) {
		return entry{}, false
	}
	if e.Message != nil {
		r.resolve(e.Message.Blocks)
	}
	for _, m := range e.Messages {
		r.resolve(m.Blocks)
	}
	return e, true
}

// resolve puts the data of the blobs that blocks refer to back in place.
// A blob the file lacks (a copy made by an older tego) leaves a note.
func (r *fileReader) resolve(blocks []llm.Block) {
	for i := range blocks {
		b := &blocks[i]
		r.resolve(b.Images)
		if b.Type != blockRef {
			continue
		}
		data, ok := r.blob(strings.TrimPrefix(b.Data, refPrefix))
		if !ok {
			*b = llm.Block{Type: llm.BlockText, Text: fmt.Sprintf("[%s %s is missing from the session file]", b.Name, b.MediaType)}
			continue
		}
		b.Type, b.Name, b.Data = llm.BlockType(b.Name), "", data
	}
}

func (r *fileReader) blob(sha string) (string, bool) {
	if d, ok := r.blobs[sha]; ok {
		return d, true
	}
	r.s.mu.Lock()
	loc, ok := r.s.blobs[sha]
	r.s.mu.Unlock()
	if !ok {
		return "", false
	}
	buf := make([]byte, loc.size)
	if r.readAt(buf, loc.off) != nil {
		return "", false
	}
	var e struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
		Data string `json:"data"`
	}
	if json.Unmarshal(buf, &e) != nil || e.Type != "blob" || e.SHA != sha {
		return "", false
	}
	if r.blobs == nil {
		r.blobs = map[string]string{}
	}
	r.blobs[sha] = e.Data
	return e.Data, true
}

// message is n's whole message.
func (r *fileReader) message(n *Node) llm.Message {
	if n.full != nil {
		return *n.full
	}
	if e, ok := r.read(n); ok && e.Message != nil {
		return *e.Message
	}
	return *n.Message // the file is gone: its outline is all there is
}

func (r *fileReader) replacement(n *Node) []llm.Message {
	if n.repl != nil {
		return n.repl
	}
	if e, ok := r.read(n); ok && e.Messages != nil {
		return e.Messages
	}
	return []llm.Message{}
}

// Replacement is the history compaction node n replaced the path's with.
func (s *Session) Replacement(n *Node) []llm.Message {
	if s == nil || n == nil || !n.compaction {
		return nil
	}
	r := s.reader()
	defer r.close()
	return r.replacement(n)
}

// SetTitle records a (generated) session title; the last one wins.
// Rename sets the title of the session file at path without loading it.
func Rename(path, title string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return (&Session{Path: path, f: f}).SetTitle(title)
}

func (s *Session) SetTitle(title string) error {
	if s == nil {
		return nil
	}
	s.Title = title
	_, _, err := s.write(entry{Type: "title", Title: title, Time: time.Now().Unix()})
	return err
}

func (s *Session) appendNode(e entry) error {
	if s == nil {
		return nil
	}
	e.ID, e.Parent, e.Time = randHex(4), s.leaf, time.Now().Unix()
	for s.nodes[e.ID] != nil {
		e.ID = randHex(4)
	}
	off, size, err := s.write(e)
	if err != nil {
		return err
	}
	n := &Node{ID: e.ID, Parent: e.Parent, Time: time.Unix(e.Time, 0), Message: outline(e.Message),
		compaction: e.Type == "compaction", off: off, size: size}
	if off < 0 { // not in a file: kept whole
		n.full = e.Message
		if n.compaction {
			n.repl = append([]llm.Message{}, e.Messages...)
		}
	}
	s.add(n)
	return nil
}

// Append adds m as a child of the leaf and makes it the new leaf.
func (s *Session) Append(m llm.Message) error {
	return s.appendNode(entry{Type: "message", Message: &m})
}

// Compacted records that the active path's history was replaced wholesale.
func (s *Session) Compacted(msgs []llm.Message) error {
	return s.appendNode(entry{Type: "compaction", Messages: msgs})
}

// LeafTime is when the active path's last node was written (zero if
// unknown).
func (s *Session) LeafTime() time.Time {
	if s == nil {
		return time.Time{}
	}
	if n := s.nodes[s.leaf]; n != nil && n.Time.Unix() > 0 {
		return n.Time
	}
	return time.Time{}
}

// Leaf is the id of the active path's last node.
func (s *Session) Leaf() string {
	if s == nil {
		return ""
	}
	return s.leaf
}

// History is the conversation along the path ending at id ("" = empty).
func (s *Session) History(id string) []llm.Message {
	if s == nil {
		return nil
	}
	return s.history(id, false)
}

// history is History, or with outlines only the messages after the last
// compaction (the compaction is still read whole).
func (s *Session) history(id string, outlines bool) []llm.Message {
	r := s.reader()
	defer r.close()
	var rev []*Node
	var base []llm.Message
	for n := s.nodes[id]; n != nil; n = s.nodes[n.Parent] {
		if n.compaction {
			base = r.replacement(n)
			break
		}
		rev = append(rev, n)
	}
	out := make([]llm.Message, 0, len(base)+len(rev))
	out = append(out, base...)
	for i := len(rev) - 1; i >= 0; i-- {
		if outlines {
			out = append(out, *rev[i].Message)
		} else {
			out = append(out, r.message(rev[i]))
		}
	}
	return out
}

// Checkout moves the leaf to id ("" = start over) and returns that path's
// history. The next Append branches from there; nothing is deleted.
func (s *Session) Checkout(id string) ([]llm.Message, error) {
	if id != "" {
		if _, ok := s.nodes[id]; !ok {
			return nil, fmt.Errorf("no node %q in session", id)
		}
	}
	if _, _, err := s.write(entry{Type: "leaf", ID: id}); err != nil {
		return nil, err
	}
	s.leaf = id
	return s.History(id), nil
}

// TreeRow is one node in depth-first display order.
type TreeRow struct {
	Node   *Node
	Depth  int  // increases below nodes with more than one child
	OnPath bool // part of the active path
	Branch bool // first node of a branch (its parent has several children)
}

// ActivePath returns the nodes of the active path after its last compaction
// node (inclusive), oldest first. History(Leaf()) is the compaction's
// replacement messages followed by these nodes' messages.
func (s *Session) ActivePath() []*Node {
	if s == nil {
		return nil
	}
	var rev []*Node
	for n := s.nodes[s.leaf]; n != nil; n = s.nodes[n.Parent] {
		rev = append(rev, n)
		if n.compaction {
			break
		}
	}
	out := make([]*Node, len(rev))
	for i, n := range rev {
		out[len(rev)-1-i] = n
	}
	return out
}

// ContinueFrom is the node to check out to continue right after n. For an
// assistant message with tool calls that is its tool-results child (on the
// active path if possible), so no call is left without a result; when it
// has none (tego stopped while the calls ran), n itself, and the agent marks
// them interrupted.
func (s *Session) ContinueFrom(n *Node) (string, error) {
	m := n.Message
	if m == nil || m.Role != llm.RoleAssistant {
		return n.ID, nil
	}
	calls := false
	for _, b := range m.Blocks {
		calls = calls || b.Type == llm.BlockToolUse
	}
	if !calls {
		return n.ID, nil
	}
	onPath := map[string]bool{}
	for p := s.nodes[s.leaf]; p != nil; p = s.nodes[p.Parent] {
		onPath[p.ID] = true
	}
	var pick *Node
	for _, c := range n.Children {
		if c.Message == nil || c.Message.Role != llm.RoleUser {
			continue
		}
		if pick == nil || onPath[c.ID] {
			pick = c
		}
	}
	if pick == nil {
		return n.ID, nil
	}
	return pick.ID, nil
}

// Tree lists every node depth-first, children in creation order.
func (s *Session) Tree() []TreeRow {
	onPath := map[string]bool{}
	for n := s.nodes[s.leaf]; n != nil; n = s.nodes[n.Parent] {
		onPath[n.ID] = true
	}
	var rows []TreeRow
	var walk func(nodes []*Node, depth int)
	walk = func(nodes []*Node, depth int) {
		for _, n := range nodes {
			branch := len(nodes) > 1
			d := depth
			if branch {
				d++
			}
			// Iterate linear chains without recursion to keep deep sessions cheap.
			for {
				rows = append(rows, TreeRow{Node: n, Depth: d, OnPath: onPath[n.ID], Branch: branch})
				branch = false
				if len(n.Children) != 1 {
					break
				}
				n = n.Children[0]
			}
			if len(n.Children) > 1 {
				walk(n.Children, d)
			}
		}
	}
	walk(s.roots, 0)
	return rows
}

func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// List returns sessions for cwd, newest first.
func List(base, cwd string) ([]Info, error) {
	out, err := listDir(Dir(base, cwd))
	sortNewest(out)
	return out, err
}

// ListAll returns the sessions of every project, newest first.
func ListAll(base string) ([]Info, error) {
	root := filepath.Join(base, "sessions")
	dirs, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Info
	for _, d := range dirs {
		if d.IsDir() {
			list, _ := listDir(filepath.Join(root, d.Name())) // one unreadable project must not hide the rest
			out = append(out, list...)
		}
	}
	sortNewest(out)
	return out, nil
}

func listDir(dir string) ([]Info, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Info
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		info := Info{ID: strings.TrimSuffix(e.Name(), ".jsonl"), Path: filepath.Join(dir, e.Name()), Modified: fi.ModTime(), Size: fi.Size()}
		info.Preview, info.Count, info.Cwd, info.ForkOf = preview(info.Path)
		if info.Count == 0 {
			continue
		}
		out = append(out, info)
	}
	return out, nil
}

func sortNewest(list []Info) {
	sort.SliceStable(list, func(i, j int) bool { return list[i].Modified.After(list[j].Modified) })
}

// Delete moves a session file to the Trash when it can (see CanTrash), else
// removes it. Only files inside base's session store are deleted.
func Delete(base, path string) error {
	root, err := filepath.Abs(filepath.Join(base, "sessions"))
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(root, abs); err != nil || strings.HasPrefix(rel, "..") || !strings.HasSuffix(abs, ".jsonl") {
		return fmt.Errorf("%s is not a tego session", path)
	}
	if !moveToTrash(abs) {
		if err := os.Remove(abs); err != nil {
			return err
		}
	}
	_ = os.Remove(filepath.Dir(abs)) // drop the project folder once it is empty
	return nil
}

// CanTrash reports whether Delete moves sessions to the Trash (a `trash`
// command exists, as on macOS 15+) rather than removing them for good.
func CanTrash() bool {
	if trashCommand == "" {
		return false
	}
	_, err := exec.LookPath(trashCommand)
	return err == nil
}

// trashCommand moves files to the Trash; tests clear it.
var trashCommand = "trash"

func moveToTrash(path string) bool {
	if !CanTrash() {
		return false
	}
	bin, _ := exec.LookPath(trashCommand)
	return exec.Command(bin, path).Run() == nil
}

// preview is what a session list shows of the file at path: its title (or
// first prompt), message count, folder and the session it was forked from.
func preview(path string) (label string, count int, cwd, forkOf string) {
	s, err := load(path)
	if err != nil {
		return "", 0, "", ""
	}
	msgs := s.history(s.leaf, true) // a count and the first prompt: outlines do
	label = s.Title
	if label != "" {
		label = Oneline(label, 70)
	} else {
		label = FirstPrompt(msgs, 70)
	}
	return label, len(msgs), s.Cwd, s.ForkOf
}

// FirstPrompt is the first user text in msgs, whitespace-collapsed and
// truncated to max runes.
func FirstPrompt(msgs []llm.Message, max int) string {
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Blocks {
			if b.Type == llm.BlockText && b.Text != "" {
				return Oneline(b.Text, max)
			}
		}
	}
	return ""
}

// Oneline collapses whitespace and truncates to max runes.
func Oneline(s string, max int) string {
	t := strings.Join(strings.Fields(s), " ")
	if r := []rune(t); len(r) > max {
		t = string(r[:max-1]) + "…"
	}
	return t
}

func matchID(list []Info, ref string) []string {
	var hits []string
	for _, s := range list {
		if strings.HasPrefix(s.ID, ref) {
			hits = append(hits, s.Path)
		}
	}
	return hits
}

// Find resolves an id prefix (or path) among cwd sessions, then among every
// project's.
func Find(base, cwd, ref string) (string, error) {
	if strings.HasSuffix(ref, ".jsonl") {
		return ref, nil
	}
	list, err := List(base, cwd)
	if err != nil {
		return "", err
	}
	hits := matchID(list, ref)
	if len(hits) == 0 { // ids are timestamps: one from another project is just as unique
		all, err := ListAll(base)
		if err != nil {
			return "", err
		}
		hits = matchID(all, ref)
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no session matching %q", ref)
	case 1:
		return hits[0], nil
	}
	return "", fmt.Errorf("%q is ambiguous (%d sessions)", ref, len(hits))
}
