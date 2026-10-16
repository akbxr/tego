// Package session persists conversations as append-only JSONL files under
// ~/.tego/sessions/<cwd-slug>/.
//
// A session is a tree: every message entry records its parent,
// so jumping back to an earlier point (/tree) and continuing creates a new
// branch while the old path stays in the file. The active path ends at the
// "leaf": the last appended node, or the target of the latest leaf entry.
package session

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

type entry struct {
	Type    string `json:"type"` // header | message | compaction | leaf
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
}

// Node is one message (or compaction) in the session tree.
type Node struct {
	ID       string
	Parent   string
	Time     time.Time
	Message  *llm.Message  // message node
	Replace  []llm.Message // compaction node: complete history at this point
	Children []*Node
}

type Session struct {
	ID   string
	Path string
	f    *os.File

	nodes map[string]*Node
	roots []*Node
	leaf  string // "" = before the first node

	mu    sync.Mutex // serialises writes
	Title string     // generated title ("" = none yet)
	Cwd   string     // working directory the session was started in ("" in files without a header)
}

type Info struct {
	ID       string
	Path     string
	Modified time.Time
	Preview  string // the session title, else the first user message on the active path
	Count    int
	Cwd      string // working directory the session was started in
	Size     int64  // file size in bytes
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
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	s := &Session{ID: id, Path: path, f: f, nodes: map[string]*Node{}, Cwd: cwd}
	return s, s.write(entry{Type: "header", ID: id, Cwd: cwd, Created: time.Now().Format(time.RFC3339), ForkOf: forkOf})
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
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}
	s.f = f
	return s, s.History(s.leaf), nil
}

func load(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := &Session{ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Path: path, nodes: map[string]*Node{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	legacy := 0
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue // tolerate a torn final line
		}
		switch e.Type {
		case "header":
			s.Cwd = e.Cwd
		case "message", "compaction":
			if e.ID == "" { // pre-tree files: a linear chain
				legacy++
				e.ID, e.Parent = fmt.Sprintf("legacy-%d", legacy), s.leaf
			}
			s.add(e)
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

func (s *Session) add(e entry) *Node {
	n := &Node{ID: e.ID, Parent: e.Parent, Time: time.Unix(e.Time, 0), Message: e.Message, Replace: e.Messages}
	if e.Type == "compaction" && n.Replace == nil {
		n.Replace = []llm.Message{}
	}
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

func (s *Session) write(e entry) error {
	if s == nil {
		return nil
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s.mu.Lock() // titles are written from another goroutine than messages
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	_, err = s.f.Write(append(data, '\n'))
	return err
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
	return s.write(entry{Type: "title", Title: title, Time: time.Now().Unix()})
}

func (s *Session) appendNode(e entry) error {
	if s == nil {
		return nil
	}
	e.ID, e.Parent, e.Time = randHex(4), s.leaf, time.Now().Unix()
	for s.nodes[e.ID] != nil {
		e.ID = randHex(4)
	}
	if err := s.write(e); err != nil {
		return err
	}
	s.add(e)
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
	var rev []*Node
	var base []llm.Message
	for n := s.nodes[id]; n != nil; n = s.nodes[n.Parent] {
		if n.Replace != nil {
			base = n.Replace
			break
		}
		rev = append(rev, n)
	}
	out := make([]llm.Message, 0, len(base)+len(rev))
	out = append(out, base...)
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, *rev[i].Message)
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
	if err := s.write(entry{Type: "leaf", ID: id}); err != nil {
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
		if n.Replace != nil {
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
		info.Preview, info.Count, info.Cwd = preview(info.Path)
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

func preview(path string) (string, int, string) {
	s, err := load(path)
	if err != nil {
		return "", 0, ""
	}
	msgs := s.History(s.leaf)
	if s.Title != "" {
		return Oneline(s.Title, 70), len(msgs), s.Cwd
	}
	return FirstPrompt(msgs, 70), len(msgs), s.Cwd
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
