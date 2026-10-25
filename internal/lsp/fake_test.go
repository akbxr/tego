package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The test binary doubles as a language server: with TEGO_FAKE_LSP set it
// serves LSP on stdin/stdout instead of running tests. A line containing
// ERR is an error, WARN a warning, and "BREAKS <path>" an error in that
// other file. TEGO_FAKE_LSP=pull answers textDocument/diagnostic instead
// of publishing; =noversion publishes without versions; =crash exits at
// initialize.
func TestMain(m *testing.M) {
	if mode, ok := os.LookupEnv("TEGO_FAKE_LSP"); ok {
		fakeServer(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeServer(mode string) {
	br := bufio.NewReader(os.Stdin)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(b), b)
	}
	diagsFor := func(text string) (mine []map[string]any, others map[string][]map[string]any) {
		others = map[string][]map[string]any{}
		for i, l := range strings.Split(text, "\n") {
			d := func(sev int, msg string) map[string]any {
				return map[string]any{
					"range":    map[string]any{"start": map[string]any{"line": i, "character": 2}, "end": map[string]any{"line": i, "character": 5}},
					"severity": sev, "message": msg,
				}
			}
			switch {
			case strings.Contains(l, "ERR"):
				mine = append(mine, d(1, "bad: "+strings.TrimSpace(l)))
			case strings.Contains(l, "WARN"):
				mine = append(mine, d(2, "meh"))
			}
			if _, p, ok := strings.Cut(l, "BREAKS "); ok {
				others[fileURI(strings.TrimSpace(p))] = append(others[fileURI(strings.TrimSpace(p))], d(1, "broken by the edit"))
			}
		}
		return
	}
	publish := func(uri string, version int, ds []map[string]any) {
		p := map[string]any{"uri": uri, "diagnostics": ds}
		if mode != "noversion" {
			p["version"] = version
		}
		if ds == nil {
			p["diagnostics"] = []any{}
		}
		send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics", "params": p})
	}
	texts := map[string]string{}
	broken := map[string]bool{} // other files that have errors now
	for {
		body, err := readMessage(br)
		if err != nil {
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				TextDocument struct {
					URI     string `json:"uri"`
					Version int    `json:"version"`
					Text    string `json:"text"`
				} `json:"textDocument"`
				ContentChanges []struct {
					Text string `json:"text"`
				} `json:"contentChanges"`
			} `json:"params"`
		}
		json.Unmarshal(body, &m)
		td := m.Params.TextDocument
		switch m.Method {
		case "initialize":
			if mode == "crash" {
				fmt.Fprintln(os.Stderr, "fake: cannot start")
				os.Exit(3)
			}
			caps := map[string]any{"textDocumentSync": 1}
			if mode == "pull" {
				caps["diagnosticProvider"] = map[string]any{"interFileDependencies": false}
			}
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"capabilities": caps}})
			// Ask something of the client, as real servers do.
			send(map[string]any{"jsonrpc": "2.0", "id": "cfg", "method": "workspace/configuration",
				"params": map[string]any{"items": []any{map[string]any{"section": "fake"}}}})
		case "textDocument/didOpen", "textDocument/didChange":
			text := td.Text
			if len(m.Params.ContentChanges) > 0 {
				text = m.Params.ContentChanges[0].Text
			}
			texts[td.URI] = text
			if mode == "pull" {
				continue
			}
			mine, others := diagsFor(text)
			// Other files first, as gopls does.
			for u := range broken {
				if others[u] == nil {
					publish(u, 0, nil)
					delete(broken, u)
				}
			}
			for u, ds := range others {
				broken[u] = true
				publish(u, 0, ds)
			}
			publish(td.URI, td.Version, mine)
		case "textDocument/diagnostic":
			mine, _ := diagsFor(texts[td.URI])
			if mine == nil {
				mine = []map[string]any{}
			}
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"kind": "full", "items": mine}})
		case "shutdown":
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": nil})
		case "exit":
			return
		}
	}
}
