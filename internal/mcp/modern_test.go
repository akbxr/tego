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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akbxr/tego/internal/tools"
)

// scriptedServer is a stdio server of one of these kinds:
//   - modern: MCP 2026-07-28 only, with input_required and tasks
//   - legacy-elicit: MCP 2025-11-25 that asks the user mid-call
//   - silent: 2025, ignores server/discover
//   - strict: 2025, exits when the first request isn't initialize
func scriptedServer(mode string) {
	out := bufio.NewWriter(os.Stdout)
	var wmu, mu sync.Mutex
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
	fail := func(id json.RawMessage, code int, msg string) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	}
	type fakeTask struct {
		status, msg string
		gets        int
		answer      json.RawMessage
	}
	var (
		tasks     = map[string]*fakeTask{}
		cancelled []string
		extra     bool
		listenID  json.RawMessage
		initCaps  string
		pendingEl json.RawMessage // a tools/call waiting for the user's answer (legacy)
		first     = true
	)
	boolForm := func(msg, field string) map[string]any {
		return map[string]any{"mode": "form", "message": msg, "requestedSchema": map[string]any{
			"type": "object", "properties": map[string]any{field: map[string]any{"type": "boolean", "title": "Go on?"}}, "required": []string{field}}}
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		if m.Method == "" { // a response: the user's answer (legacy-elicit)
			mu.Lock()
			if string(m.ID) == `"el-1"` && pendingEl != nil {
				respond(pendingEl, map[string]any{"content": []any{map[string]any{"type": "text", "text": "answer=" + string(m.Result)}}})
				pendingEl = nil
			}
			mu.Unlock()
			continue
		}
		if mode == "strict" && first && m.Method != "initialize" {
			os.Exit(1)
		}
		first = false
		if mode == "silent" && m.Method == "server/discover" {
			continue
		}
		var p struct {
			Meta           map[string]json.RawMessage `json:"_meta"`
			Name           string                     `json:"name"`
			TaskID         string                     `json:"taskId"`
			InputResponses map[string]json.RawMessage `json:"inputResponses"`
			RequestState   string                     `json:"requestState"`
			Capabilities   json.RawMessage            `json:"capabilities"`
		}
		json.Unmarshal(m.Params, &p)
		if mode == "modern" && len(m.ID) > 0 && string(p.Meta[metaVersion]) != `"`+modernVersion+`"` {
			fail(m.ID, -32022, "unsupported protocol version")
			continue
		}
		mu.Lock()
		switch m.Method {
		case "server/discover":
			if mode != "modern" {
				fail(m.ID, -32601, "method not found")
				break
			}
			respond(m.ID, map[string]any{"resultType": "complete", "supportedVersions": []string{modernVersion},
				"capabilities": map[string]any{"tools": map[string]any{"listChanged": true}, "extensions": map[string]any{tasksExt: map[string]any{}}},
				"instructions": "Call ping first."})
		case "initialize":
			if mode == "modern" {
				fail(m.ID, -32601, "this server speaks 2026-07-28 only")
				break
			}
			initCaps = string(p.Capabilities)
			respond(m.ID, map[string]any{"protocolVersion": legacyVersion, "capabilities": map[string]any{"tools": map[string]any{}},
				"instructions": "Legacy notes."})
		case "notifications/initialized", "notifications/cancelled":
		case "subscriptions/listen":
			listenID = m.ID
			write(map[string]any{"jsonrpc": "2.0", "method": "notifications/subscriptions/acknowledged",
				"params": map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/subscriptionId": m.ID}}})
		case "tools/list":
			names := []string{"ask", "caps", "quicktask", "slowtask", "hangtask", "state", "add_tool", "confirm"}
			if extra {
				names = append(names, "extra")
			}
			var list []any
			for _, n := range names {
				list = append(list, map[string]any{"name": n, "inputSchema": schema})
			}
			respond(m.ID, map[string]any{"tools": list})
		case "tools/call":
			text := func(s string) map[string]any {
				return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}
			}
			newTask := func(id string) {
				tasks[id] = &fakeTask{status: "working", msg: "starting"}
				respond(m.ID, map[string]any{"resultType": "task", "taskId": id, "status": "working", "statusMessage": "starting",
					"createdAt": "2026-11-25T00:00:00Z", "lastUpdatedAt": "2026-11-25T00:00:00Z", "ttlMs": nil, "pollIntervalMs": 20})
			}
			switch p.Name {
			case "ask":
				if p.InputResponses == nil {
					respond(m.ID, map[string]any{"resultType": "input_required", "requestState": "st-1",
						"inputRequests": map[string]any{"confirm": map[string]any{"method": "elicitation/create", "params": boolForm("Delete it?", "ok")}}})
					break
				}
				respond(m.ID, text(fmt.Sprintf("state=%s answer=%s", p.RequestState, p.InputResponses["confirm"])))
			case "caps":
				if mode == "modern" {
					respond(m.ID, text(string(p.Meta[metaCaps])))
				} else {
					respond(m.ID, text(initCaps))
				}
			case "quicktask":
				newTask("q1")
			case "slowtask":
				newTask("s1")
			case "hangtask":
				newTask("h1")
			case "state":
				respond(m.ID, text(fmt.Sprintf("cancelled=%v listen=%s", cancelled, listenID)))
			case "add_tool":
				extra = true
				respond(m.ID, text("added"))
				write(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed",
					"params": map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/subscriptionId": listenID}}})
			case "confirm": // legacy: asks the user through a request of its own
				pendingEl = m.ID
				write(map[string]any{"jsonrpc": "2.0", "id": "el-1", "method": "elicitation/create", "params": boolForm("Proceed?", "ok")})
			default:
				respond(m.ID, text("ok"))
			}
		case "tasks/get":
			tk := tasks[p.TaskID]
			if tk == nil {
				fail(m.ID, -32602, "unknown task")
				break
			}
			tk.gets++
			res := map[string]any{"resultType": "complete", "taskId": p.TaskID, "pollIntervalMs": 20}
			switch {
			case p.TaskID == "q1":
				tk.status = "completed"
				res["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": "quick done"}}}
			case p.TaskID == "s1" && tk.answer != nil:
				tk.status = "completed"
				res["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": "slow done answer=" + string(tk.answer)}}}
			case p.TaskID == "s1" && tk.gets >= 2:
				tk.status = "input_required"
				res["inputRequests"] = map[string]any{"go": map[string]any{"method": "elicitation/create", "params": boolForm("Render it?", "go")}}
			case p.TaskID == "s1":
				tk.msg = "step 1"
			}
			res["status"], res["statusMessage"] = tk.status, tk.msg
			respond(m.ID, res)
		case "tasks/update":
			if tk := tasks[p.TaskID]; tk != nil {
				tk.answer = p.InputResponses["go"]
				tk.status = "working"
			}
			respond(m.ID, map[string]any{"resultType": "complete"})
		case "tasks/cancel":
			cancelled = append(cancelled, p.TaskID)
			if tk := tasks[p.TaskID]; tk != nil {
				tk.status = "cancelled"
			}
			respond(m.ID, map[string]any{"resultType": "complete"})
		default:
			if len(m.ID) > 0 {
				fail(m.ID, -32601, "method not found")
			}
		}
		mu.Unlock()
	}
}

// fakeJobs records the background jobs tools start.
type fakeJobs struct {
	mu   sync.Mutex
	runs []func(ctx context.Context, progress func(string)) (string, error)
}

func (j *fakeJobs) Start(_, _ string, run func(ctx context.Context, progress func(string)) (string, error)) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.runs = append(j.runs, run)
	return fmt.Sprintf("job-%d", len(j.runs))
}

// yes accepts every form, every boolean true.
func yes(asked *[]string, mu *sync.Mutex) Elicitor {
	return func(_ context.Context, e *Elicitation) ElicitResult {
		mu.Lock()
		*asked = append(*asked, e.Server+": "+e.Message)
		mu.Unlock()
		content := map[string]any{}
		for _, f := range e.Fields {
			content[f.Name] = true
		}
		return ElicitResult{Action: ElicitAccept, Content: content}
	}
}

func TestModernStdio(t *testing.T) {
	old := taskGrace
	taskGrace = 300 * time.Millisecond
	defer func() { taskGrace = old }()
	var (
		mu    sync.Mutex
		asked []string
	)
	m := StartWith([]ServerConfig{{Name: "mod", Type: "stdio", Command: os.Args[0], Env: map[string]string{"TEGO_MCP_FAKE": "modern"}}},
		t.TempDir(), "", Options{Elicit: yes(&asked, &mu)})
	defer m.Close()
	m.Wait(20 * time.Second)
	st := m.Status()[0]
	if st.State != StateConnected || st.Protocol != modernVersion || !st.Tasks || st.Instructions == 0 {
		t.Fatalf("status: %+v", st)
	}
	tm := toolMap(m)
	if server, ins := tm["mcp__mod__ask"].(*mcpTool).ServerInstructions(); server != "mod" || ins != "Call ping first." {
		t.Fatalf("instructions: %q %q", server, ins)
	}

	// input_required: the user is asked, and the call made again with the
	// answer and the server's state.
	if got := call(t, tm["mcp__mod__ask"], `{}`); got != `state=st-1 answer={"action":"accept","content":{"ok":true}}` {
		t.Fatalf("ask: %q", got)
	}
	mu.Lock()
	if len(asked) != 1 || asked[0] != "mod: Delete it?" {
		t.Fatalf("asked: %v", asked)
	}
	mu.Unlock()
	caps := call(t, tm["mcp__mod__caps"], `{}`)
	if !strings.Contains(caps, `"elicitation":{"form":{},"url":{}}`) || !strings.Contains(caps, `"`+tasksExt+`":{}`) {
		t.Fatalf("capabilities: %s", caps)
	}

	// Tasks, waited for where the session has no Jobs; a task asks the
	// user too.
	if got := call(t, tm["mcp__mod__quicktask"], `{}`); got != "quick done" {
		t.Fatalf("quicktask: %q", got)
	}
	var progress []string
	ctx := tools.WithProgress(context.Background(), func(b []byte) { progress = append(progress, string(b)) })
	got, _, err := tm["mcp__mod__slowtask"].RunImages(ctx, json.RawMessage(`{}`))
	if err != nil || got != `slow done answer={"action":"accept","content":{"go":true}}` {
		t.Fatalf("slowtask: %q %v", got, err)
	}
	if p := strings.Join(progress, ""); !strings.Contains(p, "task working: starting") || !strings.Contains(p, "task working: step 1") || !strings.Contains(p, "task input required") {
		t.Fatalf("progress: %q", p)
	}

	// With Jobs, a task that runs past the grace goes to the background;
	// stopping the job cancels the task.
	jobs := &fakeJobs{}
	got, _, err = tm["mcp__mod__hangtask"].RunImages(tools.WithJobs(context.Background(), jobs), json.RawMessage(`{}`))
	if err != nil || !strings.Contains(got, "in the background as job-1") || len(jobs.runs) != 1 {
		t.Fatalf("hangtask: %q %v", got, err)
	}
	jctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var lines []string
	go func() {
		_, err := jobs.runs[0](jctx, func(s string) { lines = append(lines, s) })
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("job: %v", err)
	}
	if s := call(t, tm["mcp__mod__state"], `{}`); !strings.Contains(s, "cancelled=[h1]") {
		t.Fatalf("state: %s", s)
	}

	// tools/list_changed comes through subscriptions/listen.
	call(t, tm["mcp__mod__add_tool"], `{}`)
	eventually(t, "extra tool", func() bool { return toolMap(m)["mcp__mod__extra"] != nil })
}

func TestLegacyElicitation(t *testing.T) {
	var (
		mu    sync.Mutex
		asked []string
	)
	cfg := []ServerConfig{{Name: "old", Type: "stdio", Command: os.Args[0], Env: map[string]string{"TEGO_MCP_FAKE": "legacy-elicit"}}}
	m := StartWith(cfg, t.TempDir(), "", Options{Elicit: yes(&asked, &mu)})
	defer m.Close()
	m.Wait(20 * time.Second)
	st := m.Status()[0]
	if st.State != StateConnected || st.Protocol != legacyVersion || st.Tasks || st.Instructions == 0 {
		t.Fatalf("status: %+v", st)
	}
	tm := toolMap(m)
	if got := call(t, tm["mcp__old__confirm"], `{}`); got != `answer={"action":"accept","content":{"ok":true}}` {
		t.Fatalf("confirm: %q", got)
	}
	if caps := call(t, tm["mcp__old__caps"], `{}`); !strings.Contains(caps, `"elicitation":{"form":{},"url":{}}`) || !strings.Contains(caps, `"roots"`) {
		t.Fatalf("capabilities: %s", caps)
	}

	// Without an Elicitor (tego -p) servers are not told tego asks.
	m2 := StartWith(cfg, t.TempDir(), "", Options{})
	defer m2.Close()
	m2.Wait(20 * time.Second)
	if caps := call(t, toolMap(m2)["mcp__old__caps"], `{}`); strings.Contains(caps, "elicitation") {
		t.Fatalf("capabilities without an elicitor: %s", caps)
	}
}

// A server that lets server/discover go unanswered is initialized after
// discoverWait, and not asked again; one that dies at it is started again.
func TestDiscoverFallbacks(t *testing.T) {
	old := discoverWait
	discoverWait = 300 * time.Millisecond
	defer func() { discoverWait = old }()
	starts := filepath.Join(t.TempDir(), "starts")
	m := StartWith([]ServerConfig{
		{Name: "silent", Type: "stdio", Command: os.Args[0], Env: map[string]string{"TEGO_MCP_FAKE": "silent"}},
		{Name: "strict", Type: "stdio", Command: os.Args[0], Env: map[string]string{"TEGO_MCP_FAKE": "strict", "TEGO_MCP_STARTS": starts}},
	}, t.TempDir(), "", Options{})
	defer m.Close()
	m.Wait(20 * time.Second)
	for i, st := range m.Status() {
		if st.State != StateConnected || st.Protocol != legacyVersion {
			t.Fatalf("status: %+v", st)
		}
		if s := m.servers[i]; !s.noProbe {
			t.Fatalf("%s: discover would be tried again", st.Name)
		}
	}
	if b, _ := os.ReadFile(starts); strings.Count(string(b), "\n") != 2 {
		t.Fatalf("strict started %d times", strings.Count(string(b), "\n"))
	}
}

func TestModernHTTP(t *testing.T) {
	var (
		mu     sync.Mutex
		gets   int
		listen bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			mu.Lock()
			gets++
			mu.Unlock()
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("MCP-Protocol-Version") != modernVersion {
			http.Error(w, "legacy request", http.StatusBadRequest)
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string `json:"name"`
				Arguments struct {
					Region string `json:"region"`
				} `json:"arguments"`
			} `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&m)
		rpcErr := func(status, code int, msg string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": code, "message": msg}})
		}
		if r.Header.Get("Mcp-Method") != m.Method {
			rpcErr(400, codeHeaderMismatch, "Mcp-Method mismatch")
			return
		}
		result := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": v})
		}
		text := func(s string) map[string]any {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}
		}
		switch m.Method {
		case "server/discover":
			result(map[string]any{"supportedVersions": []string{modernVersion}, "capabilities": map[string]any{"tools": map[string]any{}}})
		case "subscriptions/listen":
			mu.Lock()
			listen = true
			mu.Unlock()
			rpcErr(404, -32601, "no")
		case "tools/list":
			result(map[string]any{"tools": []any{
				map[string]any{"name": "route", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"region": map[string]any{"type": "string", "x-mcp-header": "Region"}}}},
				map[string]any{"name": "bad", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"n": map[string]any{"type": "number", "x-mcp-header": "N"}}}},
				map[string]any{"name": "café", "inputSchema": map[string]any{"type": "object"}},
			}})
		case "tools/call":
			if r.Header.Get("Mcp-Name") != headerValue(m.Params.Name) {
				rpcErr(400, codeHeaderMismatch, "Mcp-Name mismatch")
				return
			}
			if m.Params.Name == "route" && r.Header.Get("Mcp-Param-Region") != m.Params.Arguments.Region {
				rpcErr(400, codeHeaderMismatch, "Mcp-Param-Region mismatch")
				return
			}
			result(text("called " + m.Params.Name + " as " + r.Header.Get("Mcp-Name") + " region=" + r.Header.Get("Mcp-Param-Region")))
		default:
			rpcErr(404, -32601, "method not found")
		}
	}))
	defer srv.Close()
	m := StartWith([]ServerConfig{{Name: "h", Type: "http", URL: srv.URL}}, t.TempDir(), "", Options{})
	defer m.Close()
	m.Wait(10 * time.Second)
	if st := m.Status()[0]; st.State != StateConnected || st.Protocol != modernVersion || st.Tools != 2 {
		t.Fatalf("status: %+v", st)
	}
	tm := toolMap(m)
	if tm["mcp__h__bad"] != nil {
		t.Fatal("a tool whose header can't be sent is listed")
	}
	if got := call(t, tm["mcp__h__route"], `{"region":"eu-west"}`); got != "called route as route region=eu-west" {
		t.Fatalf("route: %q", got)
	}
	if got := call(t, tm["mcp__h__caf_"], `{}`); got != "called café as =?base64?Y2Fmw6k=?= region=" {
		t.Fatalf("café: %q", got)
	}
	m.Close()
	mu.Lock()
	defer mu.Unlock()
	if gets != 0 || listen {
		t.Fatalf("a modern client opened a GET stream (%d) or listened without listChanged (%v)", gets, listen)
	}
}

func TestFormFields(t *testing.T) {
	fields, err := ParseSchema(json.RawMessage(`{"type":"object","properties":{
		"name":{"type":"string","title":"Name","minLength":2},
		"email":{"type":"string","format":"email"},
		"count":{"type":"integer","minimum":1,"maximum":5,"default":3},
		"size":{"type":"string","oneOf":[{"const":"s","title":"Small"},{"const":"l","title":"Large"}]},
		"tags":{"type":"array","items":{"enum":["a","b","c"]},"maxItems":2},
		"ok":{"type":"boolean","default":true}},"required":["name","ok"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range fields {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "name,email,count,size,tags,ok" {
		t.Fatalf("order: %v", names)
	}
	f := map[string]Field{}
	for _, x := range fields {
		f[x.Name] = x
	}
	for _, c := range []struct {
		field, in string
		want      any
		bad       bool
	}{
		{"name", "", nil, true},
		{"name", "x", nil, true},
		{"name", "Ann", "Ann", false},
		{"email", "", nil, false},
		{"email", "nope", nil, true},
		{"email", "a@b.co", "a@b.co", false},
		{"count", "9", nil, true},
		{"count", "2.5", nil, true},
		{"count", "4", int64(4), false},
		{"size", "large", "l", false},
		{"size", "m", nil, true},
		{"ok", "yes", true, false},
		{"ok", "maybe", nil, true},
	} {
		got, err := f[c.field].Parse(c.in)
		if (err != nil) != c.bad || (!c.bad && fmt.Sprint(got) != fmt.Sprint(c.want)) {
			t.Errorf("%s %q: %v %v", c.field, c.in, got, err)
		}
	}
	if got, err := f["tags"].Parse("a, C"); err != nil || fmt.Sprint(got) != "[a c]" {
		t.Errorf("tags: %v %v", got, err)
	}
	if _, err := f["tags"].Parse("a,b,c"); err == nil {
		t.Error("tags past maxItems taken")
	}
	if f["count"].DefaultText() != "3" || f["ok"].DefaultText() != "yes" {
		t.Errorf("defaults: %q %q", f["count"].DefaultText(), f["ok"].DefaultText())
	}
	if s := Summary(fields, map[string]any{"ok": true, "name": "Ann", "tags": []any{"a"}}); s != "Name: Ann\ntags: a\nok: yes" {
		t.Errorf("summary: %q", s)
	}
	if _, err := ParseSchema(json.RawMessage(`{"type":"object","properties":{"o":{"type":"object"}}}`)); err == nil {
		t.Error("a nested object taken")
	}
}

func TestHeaderMirroring(t *testing.T) {
	for in, want := range map[string]string{
		"us-east": "us-east", " pad": "=?base64?IHBhZA==?=", "日本": "=?base64?5pel5pys?=", "=?base64?x?=": "=?base64?PT9iYXNlNjQ/eD89?=",
	} {
		if got := headerValue(in); got != want {
			t.Errorf("headerValue(%q) = %q, want %q", in, got, want)
		}
	}
	hs, ok := paramHeaders(json.RawMessage(`{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"integer","x-mcp-header":"B"}}},"c":{"type":"boolean","x-mcp-header":"C"}}}`))
	if !ok || len(hs) != 2 {
		t.Fatalf("headers: %+v %v", hs, ok)
	}
	got := headerValues(hs, json.RawMessage(`{"a":{"b":7},"c":false}`))
	if got["Mcp-Param-B"] != "7" || got["Mcp-Param-C"] != "false" {
		t.Fatalf("values: %v", got)
	}
	for _, bad := range []string{
		`{"properties":{"a":{"type":"number","x-mcp-header":"A"}}}`,
		`{"properties":{"a":{"type":"string","x-mcp-header":"A B"}}}`,
		`{"properties":{"a":{"type":"string","x-mcp-header":"A"},"b":{"type":"string","x-mcp-header":"a"}}}`,
	} {
		if _, ok := paramHeaders(json.RawMessage(bad)); ok {
			t.Errorf("taken: %s", bad)
		}
	}
}
