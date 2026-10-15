package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/tools"
)

func TestMain(m *testing.M) {
	switch os.Getenv("TEGO_MCP_FAKE") {
	case "1":
		fakeServer()
		os.Exit(0)
	case "exit":
		fmt.Fprintln(os.Stderr, "fatal: bad config")
		os.Exit(3)
	}
	os.Exit(m.Run())
}

type fakeMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

func textResult(texts ...string) map[string]any {
	var c []map[string]any
	for _, t := range texts {
		c = append(c, map[string]any{"type": "text", "text": t})
	}
	return map[string]any{"content": c}
}

// fakeServer is a stdio MCP server run as a helper process.
func fakeServer() {
	fmt.Fprintln(os.Stderr, "fake server stderr hello")
	fmt.Println("not json noise")
	var (
		wmu       sync.Mutex
		mu        sync.Mutex
		pongs     int
		roots     string
		cancelled []string
		extra     bool
	)
	out := bufio.NewWriter(os.Stdout)
	write := func(v any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
		wmu.Unlock()
	}
	respond := func(id json.RawMessage, result any) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		var m fakeMsg
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		switch m.Method {
		case "":
			mu.Lock()
			switch string(m.ID) {
			case `"ping-1"`:
				pongs++
			case `"roots-1"`:
				roots = string(m.Result)
			}
			mu.Unlock()
		case "initialize":
			respond(m.ID, map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			})
		case "notifications/initialized":
			write(map[string]any{"jsonrpc": "2.0", "id": "ping-1", "method": "ping"})
			write(map[string]any{"jsonrpc": "2.0", "id": "roots-1", "method": "roots/list"})
		case "notifications/cancelled":
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			json.Unmarshal(m.Params, &p)
			mu.Lock()
			cancelled = append(cancelled, string(p.RequestID))
			mu.Unlock()
		case "tools/list":
			var p struct {
				Cursor string `json:"cursor"`
			}
			json.Unmarshal(m.Params, &p)
			schema := map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}
			if p.Cursor == "" {
				respond(m.ID, map[string]any{"tools": []any{
					map[string]any{"name": "echo", "description": "Echo text", "inputSchema": schema},
					map[string]any{"name": "image", "description": "Returns an image", "inputSchema": schema},
				}, "nextCursor": "p2"})
				continue
			}
			list := []any{
				map[string]any{"name": "fail", "inputSchema": schema},
				map[string]any{"name": "slow", "inputSchema": schema},
				map[string]any{"name": "state", "inputSchema": schema},
				map[string]any{"name": "add_tool", "inputSchema": schema},
			}
			mu.Lock()
			if extra {
				list = append(list, map[string]any{"name": "extra"})
			}
			mu.Unlock()
			respond(m.ID, map[string]any{"tools": list})
		case "tools/call":
			var p struct {
				Name      string `json:"name"`
				Arguments struct {
					Text string `json:"text"`
				} `json:"arguments"`
			}
			json.Unmarshal(m.Params, &p)
			switch p.Name {
			case "echo":
				respond(m.ID, textResult(p.Arguments.Text, "second"))
			case "image":
				respond(m.ID, map[string]any{"content": []any{
					map[string]any{"type": "text", "text": "here"},
					map[string]any{"type": "image", "data": "aGVsbG8=", "mimeType": "image/png"},
				}})
			case "fail":
				r := textResult("boom")
				r["isError"] = true
				respond(m.ID, r)
			case "slow":
				// never answers
			case "state":
				mu.Lock()
				respond(m.ID, textResult(fmt.Sprintf("pongs=%d roots=%s cancelled=%v", pongs, roots, cancelled)))
				mu.Unlock()
			case "add_tool":
				mu.Lock()
				extra = true
				mu.Unlock()
				respond(m.ID, textResult("added"))
				write(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
			}
		}
	}
}

func toolMap(m *Manager) map[string]tools.ImageTool {
	out := map[string]tools.ImageTool{}
	for _, t := range m.Tools() {
		out[t.Def().Name] = t.(tools.ImageTool)
	}
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func call(t *testing.T, tool tools.ImageTool, args string) string {
	t.Helper()
	out, _, err := tool.RunImages(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tool.Def().Name, err)
	}
	return out
}

// A server that exits before the first message is written: the error names
// the exit, not "broken pipe" (seen on CI, where the process ends sooner).
func TestStdioWriteAfterExit(t *testing.T) {
	tr := &stdioTransport{exited: make(chan struct{})}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "exit 3")
	cmd.Stdin = r
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	tr.stdin = w
	go func() { tr.waitErr = cmd.Wait(); close(tr.exited) }()
	<-tr.exited
	err = tr.send(context.Background(), []byte(strings.Repeat("x", 1<<20)), 1)
	if err == nil || !strings.Contains(err.Error(), "server exited (exit status 3)") {
		t.Fatalf("want the exit reason, got %v", err)
	}
}

func TestStdio(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "logs")
	cwd := t.TempDir()
	m := Start([]ServerConfig{
		{Name: "fake", Type: "stdio", Command: os.Args[0], Env: map[string]string{"TEGO_MCP_FAKE": "1"}},
		{Name: "dead", Type: "stdio", Command: os.Args[0], Env: map[string]string{"TEGO_MCP_FAKE": "exit"}},
		{Name: "off", Type: "stdio", Command: "nonexistent", Disabled: true},
	}, cwd, logDir)
	defer m.Close()
	m.Wait(20 * time.Second)

	st := m.Status()
	if st[0].State != StateConnected || st[0].Tools != 6 || st[0].Transport != "stdio" {
		t.Fatalf("fake status: %+v", st[0])
	}
	if st[1].State != StateFailed || !strings.Contains(st[1].Err, "fatal: bad config") {
		t.Fatalf("dead status: %+v", st[1])
	}
	if st[2].State != StateDisabled {
		t.Fatalf("off status: %+v", st[2])
	}

	tm := toolMap(m)
	if len(tm) != 6 || tm["mcp__fake__echo"] == nil || tm["mcp__fake__add_tool"] == nil {
		t.Fatalf("tools: %v", tm)
	}
	if d := tm["mcp__fake__echo"].Def(); d.Description != "Echo text" || !strings.Contains(string(d.Schema), `"text"`) {
		t.Fatalf("def: %+v", d)
	}

	if got := call(t, tm["mcp__fake__echo"], `{"text":"hi"}`); got != "hi\n\nsecond" {
		t.Fatalf("echo: %q", got)
	}
	big := strings.Repeat("x", 300<<10) // well past any 64K line limit
	if got := call(t, tm["mcp__fake__echo"], `{"text":"`+big+`"}`); got != big+"\n\nsecond" {
		t.Fatalf("big echo: len %d", len(got))
	}

	text, imgs, err := tm["mcp__fake__image"].RunImages(context.Background(), nil)
	if err != nil || text != "here" || len(imgs) != 1 || imgs[0].MediaType != "image/png" || imgs[0].Data != "aGVsbG8=" {
		t.Fatalf("image: %q %+v %v", text, imgs, err)
	}

	if _, _, err := tm["mcp__fake__fail"].RunImages(context.Background(), nil); err == nil || err.Error() != "boom" {
		t.Fatalf("fail: %v", err)
	}

	// Server-initiated ping and roots/list were answered.
	eventually(t, "ping+roots", func() bool {
		s := call(t, tm["mcp__fake__state"], `{}`)
		return strings.Contains(s, "pongs=1") && strings.Contains(s, `"uri":"file://`+cwd+`"`)
	})

	// Cancellation returns promptly and notifies the server.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	start := time.Now()
	_, _, err = tm["mcp__fake__slow"].RunImages(ctx, nil)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("slow: %v after %s", err, time.Since(start))
	}
	eventually(t, "cancel notification", func() bool {
		return !strings.Contains(call(t, tm["mcp__fake__state"], `{}`), "cancelled=[]")
	})

	// list_changed triggers a refetch.
	call(t, tm["mcp__fake__add_tool"], `{}`)
	eventually(t, "extra tool", func() bool { return toolMap(m)["mcp__fake__extra"] != nil })
	if s := string(toolMap(m)["mcp__fake__extra"].Def().Schema); s != `{"type":"object","properties":{}}` {
		t.Fatalf("default schema: %s", s)
	}

	log, _ := os.ReadFile(filepath.Join(logDir, "mcp-fake.log"))
	if !strings.Contains(string(log), "fake server stderr hello") || !strings.Contains(string(log), "stdout: not json noise") {
		t.Fatalf("log: %q", log)
	}

	m.Close()
	if s := m.Status()[0]; s.State != StateClosed || len(m.Tools()) != 0 {
		t.Fatalf("after close: %+v", s)
	}
	if _, _, err := tm["mcp__fake__echo"].RunImages(context.Background(), nil); err == nil {
		t.Fatal("call after close succeeded")
	}
}

func writeSSE(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
	w.(http.Flusher).Flush()
}

func TestStreamableHTTP(t *testing.T) {
	var pongs, deleted atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		case http.MethodDelete:
			if r.Header.Get("Mcp-Session-Id") == "sess-1" {
				deleted.Add(1)
			}
			return
		}
		if r.Header.Get("X-Token") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var m fakeMsg
		json.NewDecoder(r.Body).Decode(&m)
		if m.Method != "initialize" && (r.Header.Get("Mcp-Session-Id") != "sess-1" || r.Header.Get("MCP-Protocol-Version") != protocolVersion) {
			http.Error(w, "missing session headers", http.StatusBadRequest)
			return
		}
		switch {
		case m.Method == "":
			if string(m.ID) == `"srv-ping"` {
				pongs.Add(1)
			}
			w.WriteHeader(http.StatusAccepted)
		case len(m.ID) == 0:
			w.WriteHeader(http.StatusAccepted)
		case m.Method == "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{
				"protocolVersion": protocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
			}})
		case m.Method == "tools/list":
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(w, map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"level": "info"}})
			writeSSE(w, map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"tools": []any{
				map[string]any{"name": "hello", "description": "Says hello", "inputSchema": map[string]any{"properties": map[string]any{}}},
			}}})
		case m.Method == "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(w, map[string]any{"jsonrpc": "2.0", "id": "srv-ping", "method": "ping"})
			for range 200 {
				if pongs.Load() != 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			writeSSE(w, map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": textResult(fmt.Sprintf("hello pongs=%d", pongs.Load()))})
		}
	}))
	defer srv.Close()

	m := Start([]ServerConfig{
		{Name: "h", Type: "http", URL: srv.URL, Headers: map[string]string{"X-Token": "secret"}},
		{Name: "noauth", Type: "http", URL: srv.URL},
	}, t.TempDir(), "")
	defer m.Close()
	m.Wait(10 * time.Second)
	st := m.Status()
	if st[0].State != StateConnected || st[0].Transport != "http" || st[0].Tools != 1 {
		t.Fatalf("status: %+v", st[0])
	}
	if st[1].State != StateFailed || !strings.Contains(st[1].Err, "unauthorized") {
		t.Fatalf("noauth status: %+v", st[1])
	}
	tool := toolMap(m)["mcp__h__hello"]
	if tool == nil {
		t.Fatalf("tools: %v", m.Tools())
	}
	if s := string(tool.Def().Schema); !strings.Contains(s, `"type":"object"`) {
		t.Fatalf("schema not normalized: %s", s)
	}
	if got := call(t, tool, `{}`); got != "hello pongs=1" {
		t.Fatalf("call: %q", got)
	}
	m.Close()
	if deleted.Load() != 1 {
		t.Fatal("session not deleted on close")
	}
}

func TestLegacySSEFallback(t *testing.T) {
	msgs := make(chan []byte, 16)
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed) // makes the "http" client fall back
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": hi\n\nevent: endpoint\ndata: /msg?session=1\n\n")
		w.(http.Flusher).Flush()
		for {
			select {
			case b := <-msgs:
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	mux.HandleFunc("/msg", func(w http.ResponseWriter, r *http.Request) {
		var m fakeMsg
		json.NewDecoder(r.Body).Decode(&m)
		w.WriteHeader(http.StatusAccepted)
		if r.URL.Query().Get("session") != "1" || m.Method == "" || len(m.ID) == 0 {
			return
		}
		var result any
		switch m.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "add", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = textResult("3")
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		msgs <- b
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := Start([]ServerConfig{{Name: "legacy", Type: "http", URL: srv.URL + "/sse"}}, t.TempDir(), "")
	defer m.Close()
	m.Wait(10 * time.Second)
	if st := m.Status()[0]; st.State != StateConnected || st.Transport != "sse" || st.Tools != 1 {
		t.Fatalf("status: %+v", st)
	}
	if got := call(t, toolMap(m)["mcp__legacy__add"], `{"a":1,"b":2}`); got != "3" {
		t.Fatalf("call: %q", got)
	}
}
