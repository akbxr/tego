package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/akbxr/tego/internal/llm"
)

const (
	defaultResults = 8
	maxResults     = 20
	maxOutput      = 8000
	maxSnippet     = 300
	backendTimeout = 20 * time.Second
)

// NativeFunc runs a provider-native web search (see llm.WebSearcher).
type NativeFunc func(ctx context.Context, query string) (*llm.WebResult, error)

// SearchTool is the model-facing web_search tool. It satisfies the
// tools.Tool interface structurally.
type SearchTool struct {
	native NativeFunc
}

// NewSearchTool returns the tool; native may be nil when the current
// provider has no server-side search.
func NewSearchTool(native NativeFunc) *SearchTool { return &SearchTool{native: native} }

func (t *SearchTool) ReadOnly() bool { return true }

func (t *SearchTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "web_search",
		Description: "Search the web for current information. Returns an answer with sources, or ranked results (title, URL, snippet). Use read on a URL to open a page.",
		Schema: json.RawMessage(`{"type":"object","properties":{` +
			`"query":{"type":"string","description":"Search query"},` +
			`"max_results":{"type":"integer","description":"Max results (default 8)"}},` +
			`"required":["query"]}`),
	}
}

// hit is one search result; Snippet may be empty.
type hit struct{ Title, URL, Snippet string }

// outcome is a backend's answer: a summary (native, Tavily) and/or hits.
type outcome struct {
	answer string
	hits   []hit
}

type backend struct {
	name string
	run  func(ctx context.Context, query string, n int) (*outcome, error)
}

func (t *SearchTool) backends() []backend {
	var bs []backend
	if t.native != nil {
		native := t.native
		bs = append(bs, backend{"native provider search", func(ctx context.Context, q string, n int) (*outcome, error) {
			r, err := native(ctx, q)
			if err != nil {
				return nil, err
			}
			o := &outcome{answer: r.Text}
			for _, s := range r.Sources {
				o.hits = append(o.hits, hit{Title: s.Title, URL: s.URL})
			}
			return o, nil
		}})
	}
	if k := os.Getenv("EXA_API_KEY"); k != "" {
		bs = append(bs, backend{"Exa", func(ctx context.Context, q string, n int) (*outcome, error) { return searchExa(ctx, k, q, n) }})
	}
	if k := os.Getenv("BRAVE_API_KEY"); k != "" {
		bs = append(bs, backend{"Brave", func(ctx context.Context, q string, n int) (*outcome, error) { return searchBrave(ctx, k, q, n) }})
	}
	if k := os.Getenv("TAVILY_API_KEY"); k != "" {
		bs = append(bs, backend{"Tavily", func(ctx context.Context, q string, n int) (*outcome, error) { return searchTavily(ctx, k, q, n) }})
	}
	return append(bs, backend{"DuckDuckGo", searchDuckDuckGo})
}

// Run tries each backend in order (native, Exa, Brave, Tavily, DuckDuckGo)
// until one returns results.
func (t *SearchTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("invalid input: %v", err)
		}
	}
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" {
		return "", errors.New("query is required")
	}
	n := in.MaxResults
	if n <= 0 {
		n = defaultResults
	}
	n = min(n, maxResults)
	var failed []string
	for _, b := range t.backends() {
		o, err := b.run(ctx, in.Query, n)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err == nil && o.answer == "" && len(o.hits) == 0 {
			err = errors.New("no results")
		}
		if err != nil {
			failed = append(failed, b.name+": "+oneLine(err.Error(), 200))
			continue
		}
		return format(b.name, in.Query, o, n, failed), nil
	}
	return "", fmt.Errorf("web search failed (%s); set EXA_API_KEY, BRAVE_API_KEY or TAVILY_API_KEY for a reliable backend", strings.Join(failed, "; "))
}

func format(via, query string, o *outcome, n int, failed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Web search via %s: %q\n", via, query)
	if len(failed) > 0 {
		fmt.Fprintf(&b, "(skipped: %s)\n", strings.Join(failed, "; "))
	}
	b.WriteByte('\n')
	hits := o.hits
	if len(hits) > n {
		hits = hits[:n]
	}
	if o.answer != "" {
		b.WriteString(strings.TrimSpace(o.answer))
		b.WriteString("\n\nSources:\n")
	}
	for i, h := range hits {
		title := h.Title
		if title == "" {
			title = h.URL
		}
		fmt.Fprintf(&b, "%d. %s — %s\n", i+1, oneLine(title, 200), h.URL)
		if s := oneLine(h.Snippet, maxSnippet); s != "" {
			fmt.Fprintf(&b, "   %s\n", s)
		}
	}
	return truncate(strings.TrimRight(b.String(), "\n"), maxOutput)
}

// oneLine collapses whitespace and cuts s to at most n runes.
func oneLine(s string, n int) string {
	s = collapseSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…[truncated]"
}

// ---- keyed JSON backends ----

func doJSON(ctx context.Context, method, endpoint string, header http.Header, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, backendTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rd)
	if err != nil {
		return err
	}
	req.Header = header
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, oneLine(string(data), 200))
	}
	return json.Unmarshal(data, out)
}

func searchExa(ctx context.Context, key, q string, n int) (*outcome, error) {
	var out struct {
		Results []struct {
			Title, URL, Text string
		} `json:"results"`
	}
	body := map[string]any{
		"query":      q,
		"numResults": n,
		"contents":   map[string]any{"text": map[string]any{"maxCharacters": maxSnippet}},
	}
	if err := doJSON(ctx, http.MethodPost, "https://api.exa.ai/search", http.Header{"X-Api-Key": {key}}, body, &out); err != nil {
		return nil, err
	}
	o := &outcome{}
	for _, r := range out.Results {
		o.hits = append(o.hits, hit{r.Title, r.URL, r.Text})
	}
	return o, nil
}

func searchBrave(ctx context.Context, key, q string, n int) (*outcome, error) {
	var out struct {
		Web struct {
			Results []struct {
				Title, URL, Description string
			} `json:"results"`
		} `json:"web"`
	}
	u := "https://api.search.brave.com/res/v1/web/search?" + url.Values{"q": {q}, "count": {fmt.Sprint(n)}}.Encode()
	if err := doJSON(ctx, http.MethodGet, u, http.Header{"X-Subscription-Token": {key}}, nil, &out); err != nil {
		return nil, err
	}
	o := &outcome{}
	for _, r := range out.Web.Results {
		o.hits = append(o.hits, hit{stripTags(r.Title), r.URL, stripTags(r.Description)})
	}
	return o, nil
}

func searchTavily(ctx context.Context, key, q string, n int) (*outcome, error) {
	var out struct {
		Answer  string `json:"answer"`
		Results []struct {
			Title, URL, Content string
		} `json:"results"`
	}
	body := map[string]any{"query": q, "max_results": n, "include_answer": true}
	if err := doJSON(ctx, http.MethodPost, "https://api.tavily.com/search", http.Header{"Authorization": {"Bearer " + key}}, body, &out); err != nil {
		return nil, err
	}
	o := &outcome{answer: out.Answer}
	for _, r := range out.Results {
		o.hits = append(o.hits, hit{r.Title, r.URL, r.Content})
	}
	return o, nil
}

// stripTags removes markup and decodes entities.
func stripTags(s string) string { return innerText(tokenize(s)) }

// ---- DuckDuckGo (keyless) ----

var ddgEndpoint = "https://html.duckduckgo.com/html/"

var errBlocked = errors.New("blocked by a bot-detection challenge")

func searchDuckDuckGo(ctx context.Context, q string, n int) (*outcome, error) {
	ctx, cancel := context.WithTimeout(ctx, backendTimeout)
	defer cancel()
	form := url.Values{"q": {q}, "kl": {"us-en"}, "b": {""}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ddgEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", "https://html.duckduckgo.com/")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	page := string(data)
	if strings.Contains(page, "anomaly-modal") || strings.Contains(page, "anomaly.js") {
		return nil, errBlocked
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	hits := parseDuckDuckGo(page)
	if len(hits) > n {
		hits = hits[:n]
	}
	return &outcome{hits: hits}, nil
}

func hasClass(t *token, class string) bool {
	c, _ := t.attr("class")
	for _, f := range strings.Fields(c) {
		if f == class {
			return true
		}
	}
	return false
}

// parseDuckDuckGo extracts results from the html.duckduckgo.com page,
// unwrapping /l/?uddg= redirects and dropping ads.
func parseDuckDuckGo(page string) []hit {
	toks := tokenize(page)
	var hits []hit
	seen := map[string]bool{}
	skip := true // the current result is an ad or unusable
	for i := 0; i < len(toks); i++ {
		t := &toks[i]
		if t.kind != tokStart {
			continue
		}
		switch {
		case hasClass(t, "result__a"):
			href, _ := t.attr("href")
			end := skipElement(toks, i)
			title := innerText(toks[min(i+1, end):end])
			i = end
			u := ddgURL(href)
			skip = u == "" || title == "" || seen[u]
			if skip {
				continue
			}
			seen[u] = true
			hits = append(hits, hit{Title: title, URL: u})
		case hasClass(t, "result__snippet"):
			end := skipElement(toks, i)
			if !skip && len(hits) > 0 && hits[len(hits)-1].Snippet == "" {
				hits[len(hits)-1].Snippet = innerText(toks[min(i+1, end):end])
			}
			i = end
		}
	}
	return hits
}

// innerText joins the decoded text tokens with collapsed whitespace.
func innerText(toks []token) string {
	var b strings.Builder
	for _, t := range toks {
		if t.kind == tokText {
			b.WriteString(decodeEntities(t.text))
		}
	}
	return collapseSpace(b.String())
}

// ddgURL resolves a result link, unwrapping DuckDuckGo redirects; ads
// (duckduckgo.com/y.js) and other internal links yield "".
func ddgURL(href string) string {
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	} else if strings.HasPrefix(href, "/") {
		href = "https://duckduckgo.com" + href
	}
	u, err := url.Parse(href)
	if err != nil || u.Host == "" {
		return ""
	}
	if host := strings.TrimPrefix(u.Hostname(), "www."); host == "duckduckgo.com" || strings.HasSuffix(host, ".duckduckgo.com") {
		target := u.Query().Get("uddg")
		if u.Path != "/l/" || target == "" {
			return ""
		}
		return ddgURL(target)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.String()
}
