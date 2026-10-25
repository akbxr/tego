package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Diagnostic is a problem a server reports in a file.
type Diagnostic struct {
	Range struct {
		Start Position `json:"start"`
	} `json:"range"`
	Severity int             `json:"severity"` // 1 error, 2 warning, 3 information, 4 hint
	Code     json.RawMessage `json:"code,omitempty"`
	Source   string          `json:"source,omitempty"`
	Message  string          `json:"message"`
}

// Position is zero-based; Character counts UTF-16 units.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// published is what a server last published for a file.
type published struct {
	version int // the document version it is for; 0 when the server did not say
	seq     uint64
	diags   []Diagnostic
}

// server is one running language server for one root.
type server struct {
	spec Spec
	root string

	cmd  *exec.Cmd
	conn *conn
	done chan struct{} // closed when the process has exited

	pull bool // answers textDocument/diagnostic (LSP 3.17)

	mu      sync.Mutex
	docs    map[string]int // open documents: uri -> version
	diags   map[string]published
	seq     uint64        // counts publishes
	changed chan struct{} // closed and replaced at every publish

	// Guarded by the Manager's mu.
	lastUsed time.Time
	busy     int  // checks in progress; not stopped while > 0
	checked  bool // a check has run: the server has seen the project
}

// startServer starts spec's server for root and initializes it.
func startServer(ctx context.Context, spec Spec, root string, logw io.Writer) (*server, error) {
	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	cmd.Dir = root
	cmd.Stderr = logw
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &server{
		spec: spec, root: root, cmd: cmd, conn: newConn(stdin), done: make(chan struct{}),
		docs: map[string]int{}, diags: map[string]published{}, changed: make(chan struct{}),
		lastUsed: time.Now(),
	}
	s.conn.notify = s.onNotify
	s.conn.request = s.onRequest
	go func() {
		s.conn.read(stdout)
		cmd.Wait()
		close(s.done)
	}()

	rootURI := fileURI(root)
	ch, err := s.conn.call("initialize", map[string]any{
		"processId":  os.Getpid(),
		"clientInfo": map[string]any{"name": "tego"},
		"rootUri":    rootURI,
		"rootPath":   root,
		"workspaceFolders": []map[string]any{
			{"uri": rootURI, "name": filepath.Base(root)},
		},
		"initializationOptions": spec.InitOptions,
		"capabilities": map[string]any{
			"general": map[string]any{"positionEncodings": []string{"utf-16"}},
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"didSave": true},
				"publishDiagnostics": map[string]any{"versionSupport": true},
				"diagnostic":         map[string]any{},
			},
			"workspace": map[string]any{
				"workspaceFolders": true,
				"configuration":    true,
			},
			"window": map[string]any{"workDoneProgress": false},
		},
	})
	if err != nil {
		s.kill()
		return nil, err
	}
	var res response
	select {
	case res = <-ch:
	case <-ctx.Done():
		s.kill()
		return nil, ctx.Err()
	case <-s.done:
		return nil, errors.New("exited while starting")
	}
	if res.err != nil {
		s.kill()
		return nil, res.err
	}
	var init struct {
		Capabilities struct {
			DiagnosticProvider json.RawMessage `json:"diagnosticProvider"`
		} `json:"capabilities"`
	}
	json.Unmarshal(res.result, &init)
	dp := string(init.Capabilities.DiagnosticProvider)
	s.pull = dp != "" && dp != "null" && dp != "false"
	if err := s.conn.notifyServer("initialized", map[string]any{}); err != nil {
		s.kill()
		return nil, err
	}
	return s, nil
}

func (s *server) onNotify(method string, params json.RawMessage) {
	if method != "textDocument/publishDiagnostics" {
		return
	}
	var p struct {
		URI         string       `json:"uri"`
		Version     int          `json:"version"`
		Diagnostics []Diagnostic `json:"diagnostics"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	uri := normURI(p.URI)
	s.mu.Lock()
	s.seq++
	s.diags[uri] = published{version: p.Version, seq: s.seq, diags: p.Diagnostics}
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

// onRequest answers what servers ask of a client: no settings of our own,
// and yes to registrations and progress tokens.
func (s *server) onRequest(method string, params json.RawMessage) any {
	switch method {
	case "workspace/configuration":
		var p struct {
			Items []json.RawMessage `json:"items"`
		}
		json.Unmarshal(params, &p)
		out := make([]any, len(p.Items))
		for i := range out {
			out[i] = s.spec.Settings // nil unless configured
		}
		return out
	case "workspace/workspaceFolders":
		return []map[string]any{{"uri": fileURI(s.root), "name": filepath.Base(s.root)}}
	}
	return nil
}

// open sends text as the document's content: didOpen the first time,
// didChange (whole text) and didSave after. It returns the version sent.
func (s *server) open(uri, langID, text string) (int, error) {
	s.mu.Lock()
	v, isOpen := s.docs[uri]
	v++
	s.docs[uri] = v
	s.mu.Unlock()
	doc := map[string]any{"uri": uri, "version": v}
	if !isOpen {
		doc["languageId"] = langID
		doc["text"] = text
		return v, s.conn.notifyServer("textDocument/didOpen", map[string]any{"textDocument": doc})
	}
	if err := s.conn.notifyServer("textDocument/didChange", map[string]any{
		"textDocument":   doc,
		"contentChanges": []map[string]any{{"text": text}},
	}); err != nil {
		return v, err
	}
	// Some servers check more on save (rust-analyzer runs cargo check).
	return v, s.conn.notifyServer("textDocument/didSave", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"text":         text,
	})
}

func (s *server) isOpen(uri string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.docs[uri]
	return ok
}

// pullDiagnostics asks for a document's diagnostics (LSP 3.17).
func (s *server) pullDiagnostics(ctx context.Context, uri string) ([]Diagnostic, error) {
	ch, err := s.conn.call("textDocument/diagnostic", map[string]any{"textDocument": map[string]any{"uri": uri}})
	if err != nil {
		return nil, err
	}
	select {
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		var r struct {
			Kind  string       `json:"kind"`
			Items []Diagnostic `json:"items"`
		}
		if err := json.Unmarshal(res.result, &r); err != nil {
			return nil, err
		}
		if r.Kind != "full" {
			return nil, fmt.Errorf("diagnostic report of kind %q", r.Kind)
		}
		return r.Items, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, errors.New("language server exited")
	}
}

// snapshot returns what was last published for every file, and the
// channel closed at the next publish.
func (s *server) snapshot() (map[string]published, uint64, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[string]published, len(s.diags))
	for k, v := range s.diags {
		m[k] = v
	}
	return m, s.seq, s.changed
}

// stop asks the server to exit, and kills it if it does not.
func (s *server) stop() {
	select {
	case <-s.done:
		return
	default:
	}
	if ch, err := s.conn.call("shutdown", nil); err == nil {
		select {
		case <-ch:
		case <-time.After(time.Second):
		}
	}
	s.conn.notifyServer("exit", nil)
	select {
	case <-s.done:
	case <-time.After(time.Second):
		s.kill()
		select {
		case <-s.done:
		case <-time.After(2 * time.Second): // the reader holds on; give up
		}
	}
}

func (s *server) kill() {
	killProcessGroup(s.cmd)
}

func (s *server) exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// fileURI is the file:// URI of an absolute path.
func fileURI(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows: /C:/...
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// uriPath is the path of a file:// URI.
func uriPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return uri
	}
	p := u.Path
	if len(p) > 2 && p[0] == '/' && p[2] == ':' {
		p = p[1:] // Windows drive letter
	}
	return filepath.FromSlash(p)
}

// normURI rewrites a server's URI the way fileURI writes it (servers differ
// in what they escape).
func normURI(uri string) string {
	if !strings.HasPrefix(uri, "file:") {
		return uri
	}
	return fileURI(uriPath(uri))
}
