package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Web search on Antigravity: a Gemini request with Google Search grounding,
// through the same Cloud Code endpoint and account as the conversation (as
// omp does). The answer cites grounding chunks whose links go through a
// Google redirect, resolved here to the pages themselves.

type agGroundingMetadata struct {
	WebSearchQueries []string `json:"webSearchQueries"`
	GroundingChunks  []struct {
		Web *struct {
			URI   string `json:"uri"`
			Title string `json:"title"`
		} `json:"web"`
	} `json:"groundingChunks"`
	GroundingSupports []struct {
		GroundingChunkIndices []int `json:"groundingChunkIndices"`
	} `json:"groundingSupports"`
}

type agSearchChunk struct {
	Response *struct {
		Candidates []struct {
			Content *struct {
				Parts []agStreamPart `json:"parts"`
			} `json:"content"`
			GroundingMetadata *agGroundingMetadata `json:"groundingMetadata"`
		} `json:"candidates"`
		UsageMetadata *agUsageMetadata `json:"usageMetadata"`
	} `json:"response"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// WebSearch answers query with Gemini grounded on Google Search. model must
// be a Gemini model.
func (p *antigravityProvider) WebSearch(ctx context.Context, model, query string) (res *WebResult, err error) {
	defer func(start time.Time) {
		var u Usage
		n := 0
		if res != nil {
			u, n = res.usage, res.searches
		}
		reportWebSearch(p.name, model, start, u, n, err)
	}(time.Now())
	if p.project == "" {
		return nil, fmt.Errorf("%s: no Cloud Code project; run `tego login %s`", p.name, AntigravityKind)
	}
	if !strings.HasPrefix(model, "gemini") {
		return nil, fmt.Errorf("%s: web search needs a Gemini model, not %s", p.name, model)
	}
	req := &Request{Model: model, System: webSearchInstruction,
		Messages: []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: query}}}}}
	t := resolveAntigravityModel(model, ThinkingOff, 0)
	tr := p.turn(req, t)
	r := antigravityRequest(req, p.name, p.project, t, tr)
	r.Request.Tools = []agTool{{GoogleSearch: &struct{}{}}}
	body, err := encodeAntigravity(r)
	if err != nil {
		return nil, err
	}
	err = withRetry(ctx, func() error {
		token, err := p.oauth.Token(ctx)
		if err != nil {
			return err
		}
		h := p.header(ctx, token, t)
		for i, ep := range tr.endpoints {
			last := i == len(tr.endpoints)-1
			resp, err := postJSON(ctx, p.name, ep+"/v1internal:streamGenerateContent?alt=sse", body, h)
			if err == nil {
				res, err = parseAntigravitySearch(ctx, p.name, resp.Body)
				resp.Body.Close()
			}
			if err != nil {
				err = antigravityAPIError(err)
				if !last && ctx.Err() == nil && antigravityFallback(err) {
					continue
				}
				return err
			}
			return nil
		}
		return errors.New("antigravity: no endpoints")
	})
	if err != nil {
		return nil, err
	}
	resolveGroundingLinks(ctx, res.Sources)
	return res, nil
}

// parseAntigravitySearch reads a grounded answer: its text, the pages it
// cites (first) and the other pages it found, the search queries it ran
// (each is billed), and the usage.
func parseAntigravitySearch(ctx context.Context, provider string, body io.Reader) (*WebResult, error) {
	var text strings.Builder
	var meta agGroundingMetadata
	var usage *agUsageMetadata
	sse := newSSEReader(body)
	for {
		e, rerr := sse.Next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &streamError{rerr}
		}
		var ch agSearchChunk
		if err := json.Unmarshal(e.Data, &ch); err != nil {
			return nil, fmt.Errorf("%s: bad stream event %q: %w", provider, truncate(string(e.Data), 200), err)
		}
		if ch.Error != nil {
			msg := ch.Error.Message
			if msg == "" {
				msg = ch.Error.Status
			}
			status := ch.Error.Code
			if status < 400 {
				status = 500
			}
			return nil, &APIError{Provider: provider, Status: status, Body: "Cloud Code Assist stream error: " + msg}
		}
		r := ch.Response
		if r == nil {
			continue
		}
		if r.UsageMetadata != nil {
			usage = r.UsageMetadata
		}
		for _, c := range r.Candidates {
			if c.Content != nil {
				for _, part := range c.Content.Parts {
					if !part.Thought {
						text.WriteString(part.Text)
					}
				}
			}
			if g := c.GroundingMetadata; g != nil {
				// Sent once, with the last chunk; keep the fullest.
				if len(g.GroundingChunks) >= len(meta.GroundingChunks) {
					meta.GroundingChunks, meta.GroundingSupports = g.GroundingChunks, g.GroundingSupports
				}
				meta.WebSearchQueries = append(meta.WebSearchQueries, g.WebSearchQueries...)
			}
		}
	}
	queries := map[string]bool{}
	for _, q := range meta.WebSearchQueries {
		queries[q] = true
	}
	if len(queries) == 0 {
		// Not retried: models without grounding (gemini-3.5-flash) never search.
		return nil, errors.New(provider + ": the model answered without a Google search")
	}
	var cited, found sourceSet
	isCited := map[int]bool{}
	for _, s := range meta.GroundingSupports {
		for _, i := range s.GroundingChunkIndices {
			isCited[i] = true
		}
	}
	for i, c := range meta.GroundingChunks {
		if c.Web == nil || c.Web.URI == "" {
			continue
		}
		if isCited[i] {
			cited.add(c.Web.Title, c.Web.URI)
		} else {
			found.add(c.Web.Title, c.Web.URI)
		}
	}
	for _, s := range found.list {
		cited.add(s.Title, s.URL)
	}
	answer := strings.TrimSpace(text.String())
	if answer == "" && len(cited.list) == 0 {
		return nil, errors.New(provider + ": web search returned no results")
	}
	res := &WebResult{Text: answer, Sources: cited.list, searches: len(queries)}
	if usage != nil {
		res.usage = *agUsage(usage)
	}
	return res, nil
}

// groundingRedirect is the prefix of the links grounding cites: a Google
// redirect to the page, which would mean nothing to the model. A variable
// for the tests.
var groundingRedirect = "https://vertexaisearch.cloud.google.com/grounding-api-redirect/"

// redirectClient asks for a redirect without following it.
var redirectClient = &http.Client{
	Timeout:       5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// resolveGroundingLinks replaces grounding redirects in sources with the
// pages they lead to, all at once; a link that can't be resolved stays.
func resolveGroundingLinks(ctx context.Context, sources []Source) {
	var wg sync.WaitGroup
	for i := range sources {
		if !strings.HasPrefix(sources[i].URL, groundingRedirect) {
			continue
		}
		wg.Add(1)
		go func(s *Source) {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.URL, nil)
			if err != nil {
				return
			}
			resp, err := redirectClient.Do(req)
			if err != nil {
				return
			}
			resp.Body.Close()
			if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "http") {
				s.URL = loc
			}
		}(&sources[i])
	}
	wg.Wait()
}
