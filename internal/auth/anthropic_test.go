package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestAnthropicRefreshLikeClaudeCode(t *testing.T) {
	var body, ua string
	account := `,"account":{"uuid":"acc-new","email_address":"me@x.com"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, ua = string(b), r.Header.Get("User-Agent")
		io.WriteString(w, `{"access_token":"at","refresh_token":"rt2","expires_in":3600`+account+`}`)
	}))
	defer srv.Close()
	cfg := *oauthConfigs["anthropic"]
	cfg.tokenURL = srv.URL

	nc, err := cfg.refresh(context.Background(), Credential{Type: "oauth", Refresh: "rt1", AccountID: "acc-old"})
	if err != nil {
		t.Fatal(err)
	}
	// Claude Code's refresh body: fixed key order, scope without org:create_api_key.
	want := `{"grant_type":"refresh_token","refresh_token":"rt1","client_id":"` + cfg.clientID +
		`","scope":"user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload user:plugins"}`
	if body != want || ua != "axios/1.15.2" {
		t.Fatalf("refresh request:\n%s\n%s", body, ua)
	}
	if nc.Refresh != "rt2" || nc.AccountID != "acc-new" {
		t.Fatalf("refreshed = %+v", nc)
	}
	// A response without the account keeps the stored uuid.
	account = ""
	if nc, err = cfg.refresh(context.Background(), Credential{Type: "oauth", Refresh: "rt1", AccountID: "acc-old"}); err != nil || nc.AccountID != "acc-old" {
		t.Fatalf("account lost: %+v %v", nc, err)
	}
}

func TestAnthropicAccountIDLookup(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"account":{"uuid":"acc-1","email":"me@x.com"}}`)
	}))
	defer srv.Close()
	old := anthropicProfileURL
	anthropicProfileURL = srv.URL
	defer func() { anthropicProfileURL = old }()

	path := filepath.Join(t.TempDir(), "auth.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c, _, _ := s.Add("anthropic", Credential{Type: "oauth", Access: "tok", Email: "me@x.com"})
	ts := s.TokenSource("anthropic", c.ID).(*tokenSource)
	if id := ts.AccountID(context.Background()); id != "acc-1" {
		t.Fatalf("AccountID = %q", id)
	}
	// Looked up once, then remembered in memory and in the file.
	ts.AccountID(context.Background())
	s2, _ := Open(path)
	if calls != 1 || s2.Accounts("anthropic")[0].AccountID != "acc-1" {
		t.Fatalf("calls=%d stored=%+v", calls, s2.Accounts("anthropic"))
	}
}
