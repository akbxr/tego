package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// ModelLister is implemented by providers that can enumerate their models.
type ModelLister interface {
	ListModels(ctx context.Context) ([]string, error)
}

func getJSON(ctx context.Context, provider, endpoint string, header http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header = header
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return newAPIError(provider, resp)
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// ListModels pages through GET /v1/models; the API returns newest first.
func (p *anthropicProvider) ListModels(ctx context.Context) ([]string, error) {
	var token string
	if p.oauth != nil {
		t, err := p.oauth.Token(ctx)
		if err != nil {
			return nil, err
		}
		token = t
	}
	h := p.header(token, p.claudeCodeVersion(), &ccTurn{sessionID: p.sessionID(&Request{}), class: ClassAuxiliary})
	var ids []string
	after := ""
	for range 20 {
		q := url.Values{"limit": {"1000"}}
		if after != "" {
			q.Set("after_id", after)
		}
		var page struct {
			Data    []struct{ ID string } `json:"data"`
			HasMore bool                  `json:"has_more"`
			LastID  string                `json:"last_id"`
		}
		if err := getJSON(ctx, p.name, p.base+"/v1/models?"+q.Encode(), h, &page); err != nil {
			return nil, err
		}
		for _, m := range page.Data {
			ids = append(ids, m.ID)
		}
		if !page.HasMore || page.LastID == "" {
			break
		}
		after = page.LastID
	}
	return ids, nil
}

// ListModels uses the OpenAI-style GET /models, sorted by id.
func (p *openaiProvider) ListModels(ctx context.Context) ([]string, error) {
	key := p.key
	if p.oauth != nil {
		t, err := p.oauth.Token(ctx)
		if err != nil {
			return nil, err
		}
		key = t
	}
	h := http.Header{}
	h.Set("Accept", "application/json")
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	setHeaders(h, p.headers)
	var page struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := getJSON(ctx, p.name, p.base+"/models", h, &page); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(page.Data))
	for _, m := range page.Data {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

// ListModels queries the Codex backend's model catalogue,
// skipping models marked hidden.
func (p *codexProvider) ListModels(ctx context.Context) ([]string, error) {
	token, err := p.oauth.Token(ctx)
	if err != nil {
		return nil, err
	}
	h := p.header(token, "")
	h.Set("Accept", "application/json")
	h.Del("Content-Type")
	endpoint := strings.TrimSuffix(p.url, "/responses") + "/models?client_version=" + url.QueryEscape(codexClientVersion)
	type entry struct {
		Slug       string `json:"slug"`
		ID         string `json:"id"`
		Visibility string `json:"visibility"`
	}
	var page struct {
		Models []entry `json:"models"`
		Data   []entry `json:"data"`
	}
	if err := getJSON(ctx, p.name, endpoint, h, &page); err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range append(page.Models, page.Data...) {
		id := m.Slug
		if id == "" {
			id = m.ID
		}
		if v := strings.ToLower(m.Visibility); id == "" || v == "hide" || v == "hidden" {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}
