// Package auth persists provider credentials and implements subscription
// OAuth logins (Claude Pro/Max, ChatGPT Plus/Pro via Codex, Google
// Antigravity).
package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Credential is one stored provider account.
type Credential struct {
	ID      string `json:"id,omitempty"` // stable account id within the provider
	Type    string `json:"type"`         // "oauth" | "api_key"
	Access  string `json:"access,omitempty"`
	Refresh string `json:"refresh,omitempty"`
	Expires int64  `json:"expires,omitempty"` // unix ms
	Key     string `json:"key,omitempty"`
	// ProjectID is the Cloud Code Assist project (google-antigravity).
	ProjectID string `json:"project_id,omitempty"`
	// Email identifies the account (shown in /logout, the status line, and
	// used to recognise a re-login of the same account).
	Email string `json:"email,omitempty"`
	// AccountID is the provider's account uuid (Anthropic), sent in
	// request metadata as Claude Code does.
	AccountID string `json:"account_id,omitempty"`
}

// Label is a short human name for the account.
func (c Credential) Label() string {
	switch {
	case c.Email != "":
		return c.Email
	case c.Type == "api_key" && len(c.Key) > 8:
		return "key …" + c.Key[len(c.Key)-4:]
	}
	return "account " + c.ID
}

// Store is a JSON credential file shared between processes. Each provider
// maps to a list of accounts. Every read reloads the file and every write
// is a locked read-modify-write, so concurrent tego processes do not
// clobber each other's refreshes.
type Store struct {
	path string
	mu   sync.Mutex
	data map[string][]Credential
}

// Open loads (or prepares) the credential file at path, creating its parent
// directory with 0700 permissions. A symlinked path is resolved first so the
// lock file and atomic replace land next to the real file: a TEGO_HOME whose
// auth.json links to another one shares that login (and its token refreshes)
// instead of forking a stale copy.
func Open(path string) (*Store, error) {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: path, data: map[string][]Credential{}}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// reload re-reads the file; a missing file is an empty store. Callers hold
// s.mu. Files from before multi-account support (one object per provider)
// load as single-account lists; accounts without an ID get a stable one
// derived from their content, persisted on the next write.
func (s *Store) reload() error {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		s.data = map[string][]Credential{}
		return nil
	}
	if err != nil {
		return err
	}
	raw := map[string]json.RawMessage{}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &raw); err != nil {
			return fmt.Errorf("auth: parse %s: %w", s.path, err)
		}
	}
	data := make(map[string][]Credential, len(raw))
	for provider, v := range raw {
		var accts []Credential
		if t := bytes.TrimSpace(v); len(t) > 0 && t[0] == '[' {
			if err := json.Unmarshal(t, &accts); err != nil {
				return fmt.Errorf("auth: parse %s (%s): %w", s.path, provider, err)
			}
		} else {
			var c Credential
			if err := json.Unmarshal(t, &c); err != nil {
				return fmt.Errorf("auth: parse %s (%s): %w", s.path, provider, err)
			}
			accts = []Credential{c}
		}
		for i := range accts {
			if accts[i].ID == "" {
				sum := sha256.Sum256([]byte(provider + "\x00" + accts[i].Email + "\x00" + accts[i].Key + "\x00" + accts[i].Refresh + "\x00" + accts[i].Access))
				accts[i].ID = hex.EncodeToString(sum[:4])
			}
		}
		if len(accts) > 0 {
			data[provider] = accts
		}
	}
	s.data = data
	return nil
}

// write atomically replaces the file with s.data. Callers hold s.mu and the file lock.
func (s *Store) write() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".auth-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// Get returns the provider's first account (re-reading the file first; the
// last successfully loaded state is used if the file is unreadable).
func (s *Store) Get(provider string) (Credential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reload()
	if accts := s.data[provider]; len(accts) > 0 {
		return accts[0], true
	}
	return Credential{}, false
}

// Accounts returns all stored accounts for provider, in login order.
func (s *Store) Accounts(provider string) []Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reload()
	return append([]Credential(nil), s.data[provider]...)
}

// Add stores c as an account of provider. An account with the same email
// (OAuth) or key (API key) is replaced in place, keeping its ID; otherwise c
// is appended with a new ID. It returns the stored credential and whether
// an existing account was replaced.
func (s *Store) Add(provider string, c Credential) (Credential, bool, error) {
	replaced := false
	err := s.update(context.Background(), func(data map[string][]Credential) error {
		accts := data[provider]
		for i, old := range accts {
			if sameAccount(old, c) {
				c.ID = old.ID
				accts[i], replaced = c, true
				return nil
			}
		}
		if c.ID == "" {
			c.ID = newAccountID()
		}
		data[provider] = append(accts, c)
		return nil
	})
	return c, replaced, err
}

func sameAccount(a, b Credential) bool {
	switch {
	case a.Type != b.Type:
		return false
	case a.Type == "api_key":
		return a.Key != "" && a.Key == b.Key
	default:
		return a.Email != "" && strings.EqualFold(a.Email, b.Email)
	}
}

func newAccountID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Delete removes every account of provider.
func (s *Store) Delete(provider string) error {
	return s.update(context.Background(), func(data map[string][]Credential) error {
		delete(data, provider)
		return nil
	})
}

// DeleteAccount removes one account of provider by ID.
func (s *Store) DeleteAccount(provider, id string) error {
	return s.update(context.Background(), func(data map[string][]Credential) error {
		accts := data[provider]
		for i, c := range accts {
			if c.ID == id {
				accts = append(accts[:i:i], accts[i+1:]...)
				if len(accts) == 0 {
					delete(data, provider)
				} else {
					data[provider] = accts
				}
				return nil
			}
		}
		return fmt.Errorf("auth: no %s account %q", provider, id)
	})
}

// Providers lists providers with stored credentials, sorted.
func (s *Store) Providers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reload()
	out := make([]string, 0, len(s.data))
	for k := range s.data {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// update runs fn on freshly loaded data under the cross-process lock and
// writes the result.
func (s *Store) update(ctx context.Context, fn func(map[string][]Credential) error) error {
	return s.locked(ctx, func() error {
		if err := fn(s.data); err != nil {
			return err
		}
		return s.write()
	})
}

const (
	lockStale = 30 * time.Second
	lockWait  = 60 * time.Second
	lockPoll  = 50 * time.Millisecond
)

// locked holds s.mu and the lock file while fn runs, with s.data freshly
// reloaded. A lock file older than lockStale is considered abandoned.
func (s *Store) locked(ctx context.Context, fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lockPath := s.path + ".lock"
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		if st, serr := os.Stat(lockPath); serr == nil && time.Since(st.ModTime()) > lockStale {
			os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("auth: timed out waiting for lock %s", lockPath)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockPoll):
		}
	}
	defer os.Remove(lockPath)
	if err := s.reload(); err != nil {
		return err
	}
	return fn()
}
