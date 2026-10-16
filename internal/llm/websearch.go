package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Source is one web page backing a search answer.
type Source struct{ Title, URL string }

// WebResult is a provider-native web search answer.
type WebResult struct {
	Text    string
	Sources []Source
}

// WebSearcher is implemented by providers with a server-side web search tool.
type WebSearcher interface {
	WebSearch(ctx context.Context, model, query string) (*WebResult, error)
}

const (
	webSearchInstruction = "Search the web to answer the user's query. Answer concisely with concrete facts (names, numbers, dates, versions) and cite your sources."
	webSearchMaxTokens   = 4096
	webSearchMaxPauses   = 3
)

// sourceSet collects unique sources in insertion order; a later title fills
// in a missing one.
type sourceSet struct {
	list []Source
	at   map[string]int
}

func (s *sourceSet) add(title, url string) {
	url = strings.TrimSpace(url)
	if url == "" {
		return
	}
	if s.at == nil {
		s.at = map[string]int{}
	}
	title = strings.TrimSpace(title)
	if i, ok := s.at[url]; ok {
		if (s.list[i].Title == "" || s.list[i].Title == url) && title != "" {
			s.list[i].Title = title
		}
		return
	}
	s.at[url] = len(s.list)
	s.list = append(s.list, Source{Title: title, URL: url})
}

// withRetry runs fn, retrying transient failures like streamWithRetry.
func withRetry(ctx context.Context, fn func() error) error {
	for try := 0; ; try++ {
		err := fn()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay, ok := retryDelay(err, try)
		if !ok {
			return err
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// ---- Anthropic: web_search_20250305 server tool ----

type aServerTool struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	MaxUses int    `json:"max_uses,omitempty"`
}

// aRawMessage carries either the query string or replayed assistant
// content (after a pause_turn).
type aRawMessage struct {
	Role    Role `json:"role"`
	Content any  `json:"content"`
}

// aWebRequest fields are in Claude Code's order; "system" must serialise
// as in aRequest for patchBilling.
type aWebRequest struct {
	Model     string         `json:"model"`
	Messages  []aRawMessage  `json:"messages"`
	System    []aSystemBlock `json:"system,omitempty"`
	Tools     []aServerTool  `json:"tools"`
	Metadata  *aMetadata     `json:"metadata,omitempty"`
	MaxTokens int            `json:"max_tokens"`
	Stream    bool           `json:"stream"`
}

type aWebResponse struct {
	StopReason string            `json:"stop_reason"`
	Content    []json.RawMessage `json:"content"`
}

type aWebBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Citations []struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	} `json:"citations"`
	Content json.RawMessage `json:"content"`
}

type aWebSearchResult struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

// webSearchBody encodes a web search request; t is the Claude Code context
// of an OAuth request (nil with an API key).
func (p *anthropicProvider) webSearchBody(model, query, version string, msgs []aRawMessage, t *ccTurn) ([]byte, error) {
	r := aWebRequest{
		Model:     model,
		MaxTokens: webSearchMaxTokens,
		Messages:  msgs,
		Tools:     []aServerTool{{Type: "web_search_20250305", Name: "web_search", MaxUses: 5}},
	}
	if t != nil {
		r.System = []aSystemBlock{
			{Type: "text", Text: billingHeader(query, version, t)},
			{Type: "text", Text: claudeCodeInstruction, CacheControl: ephemeral},
		}
		r.Metadata = &aMetadata{UserID: claudeUserID(p.deviceID, t)}
	}
	r.System = append(r.System, aSystemBlock{Type: "text", Text: webSearchInstruction})
	body, err := encodeJSON(&r)
	if err != nil {
		return nil, err
	}
	if t != nil {
		patchBilling(body)
	}
	return body, nil
}

// WebSearch asks the model to answer query using Anthropic's server-side
// web search tool (non-streaming). Long searches that stop with pause_turn
// are continued a few times.
func (p *anthropicProvider) WebSearch(ctx context.Context, model, query string) (*WebResult, error) {
	msgs := []aRawMessage{{Role: RoleUser, Content: query}}
	var t *ccTurn
	if p.oauth != nil {
		t = p.turn(ctx, p.sessionID(&Request{}), ClassAuxiliary, nil)
	}
	var blocks []json.RawMessage
	for pause := 0; ; pause++ {
		var resp aWebResponse
		err := withRetry(ctx, func() error {
			r, err := p.send(ctx, t, false, func(version string) ([]byte, error) {
				return p.webSearchBody(model, query, version, msgs, t)
			})
			if err != nil {
				return err
			}
			defer r.Body.Close()
			data, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
			if err != nil {
				return &streamError{err}
			}
			resp = aWebResponse{}
			if err := json.Unmarshal(data, &resp); err != nil {
				return fmt.Errorf("%s: bad web search response %q: %w", p.name, truncate(string(data), 200), err)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, resp.Content...)
		if resp.StopReason != "pause_turn" || pause >= webSearchMaxPauses {
			break
		}
		msgs = append(msgs[:1:1], aRawMessage{Role: RoleAssistant, Content: blocks})
	}
	return parseAnthropicWebContent(blocks)
}

// parseAnthropicWebContent joins the answer text and collects sources:
// cited pages first, then the remaining search results.
func parseAnthropicWebContent(blocks []json.RawMessage) (*WebResult, error) {
	var text strings.Builder
	var cited, found sourceSet
	var searchErr string
	for _, raw := range blocks {
		var b aWebBlock
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
			for _, c := range b.Citations {
				cited.add(c.Title, c.URL)
			}
		case "web_search_tool_result":
			var results []aWebSearchResult
			if json.Unmarshal(b.Content, &results) != nil {
				var e struct {
					ErrorCode string `json:"error_code"`
				}
				if json.Unmarshal(b.Content, &e) == nil && e.ErrorCode != "" {
					searchErr = e.ErrorCode
				}
				continue
			}
			for _, r := range results {
				if r.Type == "web_search_result" {
					found.add(r.Title, r.URL)
				}
			}
		}
	}
	for _, s := range found.list {
		cited.add(s.Title, s.URL)
	}
	res := &WebResult{Text: strings.TrimSpace(text.String()), Sources: cited.list}
	if res.Text == "" && len(res.Sources) == 0 {
		if searchErr != "" {
			return nil, fmt.Errorf("anthropic web search failed: %s", searchErr)
		}
		return nil, errors.New("anthropic web search returned no results")
	}
	return res, nil
}

// ---- Codex: Responses web_search tool ----

const codexWebInstruction = "You are a helpful assistant with web search capabilities. Search the web to answer the user's question accurately and cite your sources."

type cxWebTool struct {
	Type              string `json:"type"`
	SearchContextSize string `json:"search_context_size,omitempty"`
}

type cxWebRequest struct {
	Model             string          `json:"model"`
	Instructions      string          `json:"instructions"`
	Input             []cxMessageItem `json:"input"`
	Tools             []cxWebTool     `json:"tools"`
	ToolChoice        cxWebTool       `json:"tool_choice"`
	ParallelToolCalls bool            `json:"parallel_tool_calls"`
	Store             bool            `json:"store"`
	Stream            bool            `json:"stream"`
	Include           []string        `json:"include"`
}

func codexWebBody(model, query string) ([]byte, error) {
	return encodeJSON(&cxWebRequest{
		Model:             model,
		Instructions:      codexWebInstruction,
		Input:             []cxMessageItem{{Type: "message", Role: "user", Content: []cxInPart{{Type: "input_text", Text: query}}}},
		Tools:             []cxWebTool{{Type: "web_search", SearchContextSize: "high"}},
		ToolChoice:        cxWebTool{Type: "web_search"},
		ParallelToolCalls: true,
		Stream:            true,
		Include:           []string{"web_search_call.action.sources"},
	})
}

// WebSearch runs a forced web_search call on the Codex Responses backend.
func (p *codexProvider) WebSearch(ctx context.Context, model, query string) (*WebResult, error) {
	body, err := codexWebBody(model, query)
	if err != nil {
		return nil, err
	}
	var res *WebResult
	err = withRetry(ctx, func() error {
		token, err := p.oauth.Token(ctx)
		if err != nil {
			return err
		}
		resp, err := postJSON(ctx, p.name, p.url, body, p.header(token, ""))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		res, err = parseCodexWebStream(ctx, p.name, resp.Body)
		return err
	})
	return res, err
}

type cxWebSourceRef struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Website string `json:"source_website_url"`
}

type cxWebItem struct {
	Type   string `json:"type"`
	Action *struct {
		Sources []cxWebSourceRef `json:"sources"`
	} `json:"action"`
	Content []struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Annotations []struct {
			Type  string `json:"type"`
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"annotations"`
	} `json:"content"`
}

type cxWebEvent struct {
	Type     string     `json:"type"`
	Delta    string     `json:"delta"`
	Item     *cxWebItem `json:"item"`
	Response *struct {
		Error *cxErrorBody `json:"error"`
	} `json:"response"`
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Error   *cxErrorBody `json:"error"`
}

// parseCodexWebStream collects the answer text, url_citation annotations
// and the search call's sources (cited first). It refuses an
// answer produced without any web_search_call.
func parseCodexWebStream(ctx context.Context, provider string, body io.Reader) (*WebResult, error) {
	var text, deltas strings.Builder
	var cited, found sourceSet
	searched := false
	sse := newSSEReader(body)
	for {
		e, rerr := sse.Next()
		if rerr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if rerr == io.EOF {
				rerr = io.ErrUnexpectedEOF
			}
			return nil, &streamError{rerr}
		}
		var ev cxWebEvent
		if err := json.Unmarshal(e.Data, &ev); err != nil {
			return nil, fmt.Errorf("%s: bad stream event %q: %w", provider, truncate(string(e.Data), 200), err)
		}
		if strings.HasPrefix(ev.Type, "response.web_search_call") {
			searched = true
		}
		switch ev.Type {
		case "response.output_text.delta":
			deltas.WriteString(ev.Delta)
		case "response.output_item.done":
			it := ev.Item
			if it == nil {
				continue
			}
			switch it.Type {
			case "web_search_call":
				searched = true
				if it.Action != nil {
					for _, s := range it.Action.Sources {
						u := s.URL
						if u == "" {
							u = s.Website
						}
						found.add(s.Title, cleanCodexURL(u))
					}
				}
			case "message":
				for _, c := range it.Content {
					if c.Type != "output_text" {
						continue
					}
					if text.Len() > 0 {
						text.WriteByte('\n')
					}
					text.WriteString(c.Text)
					for _, a := range c.Annotations {
						if a.Type == "url_citation" {
							cited.add(a.Title, cleanCodexURL(a.URL))
						}
					}
				}
			}
		case "response.completed", "response.done", "response.incomplete":
			if !searched {
				return nil, &APIError{Provider: provider, Status: 502, Body: "completion did not run a web search"}
			}
			answer := strings.TrimSpace(text.String())
			if answer == "" {
				answer = strings.TrimSpace(deltas.String())
			}
			for _, s := range found.list {
				cited.add(s.Title, s.URL)
			}
			if answer == "" && len(cited.list) == 0 {
				return nil, errors.New(provider + ": web search returned no results")
			}
			return &WebResult{Text: answer, Sources: cited.list}, nil
		case "response.failed":
			var eb *cxErrorBody
			if ev.Response != nil {
				eb = ev.Response.Error
			}
			return nil, codexError(provider, eb, "web search failed")
		case "error":
			eb := ev.Error
			if eb == nil {
				eb = &cxErrorBody{Code: ev.Code, Message: ev.Message}
			}
			return nil, codexError(provider, eb, truncate(string(e.Data), 500))
		}
	}
}

// cleanCodexURL drops the utm_source=openai tracking parameter.
func cleanCodexURL(u string) string {
	for _, p := range []string{"?utm_source=openai", "&utm_source=openai"} {
		if i := strings.Index(u, p); i >= 0 {
			rest := u[i+len(p):]
			if p[0] == '?' && strings.HasPrefix(rest, "&") {
				return u[:i] + "?" + rest[1:]
			}
			return u[:i] + rest
		}
	}
	return u
}
