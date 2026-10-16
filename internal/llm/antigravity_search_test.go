package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	return b.String()
}

func TestParseAntigravitySearch(t *testing.T) {
	body := sse(
		`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"thinking…","thought":true},{"text":"Go 1.27.2 "}]}}]}}`,
		`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"is current."}]},"finishReason":"STOP",`+
			`"groundingMetadata":{"webSearchQueries":["latest go","go release"],`+
			`"groundingChunks":[{"web":{"uri":"https://a.example/x","title":"a.example"}},{"web":{"uri":"https://go.dev/dl/","title":"go.dev"}},{"web":{"uri":"https://b.example/","title":"b.example"}}],`+
			`"groundingSupports":[{"segment":{"text":"Go 1.27.2"},"groundingChunkIndices":[1]}]}}],`+
			`"usageMetadata":{"promptTokenCount":41,"candidatesTokenCount":24,"thoughtsTokenCount":6,"totalTokenCount":71}}}`,
	)
	res, err := parseAntigravitySearch(context.Background(), "google-antigravity", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Go 1.27.2 is current." {
		t.Errorf("text %q (thoughts must not be in it)", res.Text)
	}
	// The cited page first, then the others in order.
	want := []Source{{"go.dev", "https://go.dev/dl/"}, {"a.example", "https://a.example/x"}, {"b.example", "https://b.example/"}}
	if !reflect.DeepEqual(res.Sources, want) {
		t.Errorf("sources %+v", res.Sources)
	}
	if res.searches != 2 || res.usage != (Usage{Input: 41, Output: 30}) {
		t.Errorf("searches %d, usage %+v", res.searches, res.usage)
	}

	// A model that answers without searching: an error, and not one that
	// is retried (it would not search the next time either).
	_, err = parseAntigravitySearch(context.Background(), "google-antigravity",
		strings.NewReader(sse(`{"response":{"candidates":[{"content":{"parts":[{"text":"Go 1.22"}]},"finishReason":"STOP"}]}}`)))
	if err == nil {
		t.Fatal("an answer without a search was accepted")
	}
	if _, retry := retryDelay(err, 0); retry {
		t.Fatalf("retried: %v", err)
	}
}

func TestAntigravityWebSearch(t *testing.T) {
	var got struct {
		Model   string `json:"model"`
		Request struct {
			Contents []agContent `json:"contents"`
			Tools    []agTool    `json:"tools"`
		} `json:"request"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/redirect/") {
			if r.Method != http.MethodHead {
				t.Errorf("redirect resolved with %s", r.Method)
			}
			http.Redirect(w, r, "https://go.dev/dl/", http.StatusFound)
			return
		}
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &got); err != nil {
			t.Errorf("body %s: %v", data, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse(`{"response":{"candidates":[{"content":{"parts":[{"text":"Go 1.27.2"}]},"finishReason":"STOP",`+
			`"groundingMetadata":{"webSearchQueries":["latest go"],"groundingChunks":[{"web":{"uri":"`+groundingRedirect+`abc","title":"go.dev"}}],`+
			`"groundingSupports":[{"groundingChunkIndices":[0]}]}}],"usageMetadata":{"promptTokenCount":41,"candidatesTokenCount":24}}}`))
	}))
	defer srv.Close()
	old := groundingRedirect
	groundingRedirect = srv.URL + "/redirect/"
	defer func() { groundingRedirect = old }()

	stats := captureRequests(t)
	p := newAntigravityProvider(AntigravityKind, srv.URL, "proj", nil, staticToken("tok"))
	res, err := p.WebSearch(context.Background(), "gemini-3.5-flash-lite", "latest go")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Request.Tools) != 1 || got.Request.Tools[0].GoogleSearch == nil || got.Request.Tools[0].FunctionDeclarations != nil {
		t.Errorf("tools %+v", got.Request.Tools)
	}
	if !strings.HasPrefix(got.Model, "gemini-3.5-flash-lite") || len(got.Request.Contents) != 1 || got.Request.Contents[0].Parts[0].Text != "latest go" {
		t.Errorf("model %q, contents %+v", got.Model, got.Request.Contents)
	}
	// The redirect became the page itself.
	if want := []Source{{"go.dev", "https://go.dev/dl/"}}; !reflect.DeepEqual(res.Sources, want) {
		t.Errorf("sources %+v", res.Sources)
	}
	st := stats()
	if len(st) != 1 || st[0].Class != ClassAuxiliary || st[0].Model != "gemini-3.5-flash-lite" || st[0].WebSearches != 1 || st[0].Usage.Input != 41 {
		t.Fatalf("stats %+v", st)
	}

	// Grounding is Gemini's: another model is refused before any request.
	if _, err := p.WebSearch(context.Background(), "claude-opus-4-6", "q"); err == nil {
		t.Fatal("searched with a Claude model")
	}
}
