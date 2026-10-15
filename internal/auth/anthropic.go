package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// anthropicProfileURL returns the signed-in account (a variable for tests).
var anthropicProfileURL = "https://api.anthropic.com/api/oauth/profile"

// anthropicAfterLogin and anthropicAfterRefresh fill in the account uuid
// when the token response left it out; Claude Code sends it with every
// request. A failed lookup is not fatal: the uuid is just left empty.
func anthropicAfterLogin(ctx context.Context, c *Credential, _ io.Writer) error {
	return anthropicAfterRefresh(ctx, c)
}

func anthropicAfterRefresh(ctx context.Context, c *Credential) error {
	if c.AccountID == "" {
		c.AccountID, _ = anthropicAccountID(ctx, c.Access)
	}
	return nil
}

// anthropicAccountID asks the profile endpoint for the account uuid.
func anthropicAccountID(ctx context.Context, token string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropicProfileURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "claude-code/"+llm.ClaudeCodeVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("profile: HTTP %d", resp.StatusCode)
	}
	var p struct {
		Account struct {
			UUID string `json:"uuid"`
		} `json:"account"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&p); err != nil {
		return "", err
	}
	return p.Account.UUID, nil
}

// AccountID returns the account uuid of an Anthropic login. Credentials
// saved before tego stored it are looked up once and updated.
func (t *tokenSource) AccountID(ctx context.Context) string {
	if t.provider != "anthropic" {
		return ""
	}
	t.mu.Lock()
	if t.accountID != "" || t.lookedUp {
		defer t.mu.Unlock()
		return t.accountID
	}
	t.mu.Unlock()
	t.store.mu.Lock()
	_ = t.store.reload()
	var c Credential
	if i, ok := t.account(t.store.data); ok {
		c = t.store.data[t.provider][i]
	}
	t.store.mu.Unlock()
	id := c.AccountID
	if id == "" && c.Type == "oauth" {
		if tok, err := t.Token(ctx); err == nil {
			if id, err = anthropicAccountID(ctx, tok); err == nil && id != "" {
				_ = t.store.locked(ctx, func() error {
					if i, ok := t.account(t.store.data); ok {
						t.store.data[t.provider][i].AccountID = id
						return t.store.write()
					}
					return nil
				})
			}
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.accountID, t.lookedUp = id, true
	return id
}
