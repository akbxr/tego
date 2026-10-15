package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParsePasted(t *testing.T) {
	cases := []struct{ in, code, state string }{
		{"http://localhost:54545/callback?code=abc&state=xyz", "abc", "xyz"},
		{"https://console.anthropic.com/oauth/code/callback?code=c1&state=s1", "c1", "s1"},
		{"?code=q&state=r", "q", "r"},
		{"code123#state456", "code123", "state456"},
		{"barecode", "barecode", ""},
	}
	for _, c := range cases {
		code, state := parsePasted(c.in)
		if code != c.code || state != c.state {
			t.Errorf("parsePasted(%q) = (%q, %q), want (%q, %q)", c.in, code, state, c.code, c.state)
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "auth.json")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(path) // a second "process"
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Add("anthropic", Credential{Type: "oauth", Access: "x", Refresh: "r", Expires: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Add("openai", Credential{Type: "api_key", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	if got := a.Providers(); len(got) != 2 || got[0] != "anthropic" || got[1] != "openai" {
		t.Fatalf("providers = %v", got)
	}
	if c, ok := a.Get("openai"); !ok || c.Key != "k" {
		t.Fatalf("Get(openai) = %+v %v", c, ok)
	}
	if err := a.Delete("openai"); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Get("openai"); ok {
		t.Fatal("delete not visible to other store")
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, %v", st.Mode(), err)
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatal("lock file left behind")
	}
}

// A TEGO_HOME whose auth.json is a symlink writes through to the target, so
// refreshed tokens are not forked into a copy that leaves the original stale.
func TestStoreFollowsSymlink(t *testing.T) {
	realDir, linkDir := t.TempDir(), t.TempDir()
	real := filepath.Join(realDir, "auth.json")
	if _, _, err := mustOpen(t, real).Add("anthropic", Credential{Type: "oauth", Access: "old", Refresh: "r"}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "auth.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mustOpen(t, link).Add("openai", Credential{Type: "api_key", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced by a regular file: %v %v", fi, err)
	}
	if _, ok := mustOpen(t, real).Get("openai"); !ok {
		t.Fatal("write through the symlink did not reach the real file")
	}
}

func mustOpen(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreMultipleAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	s, _ := Open(path)
	a1, _, _ := s.Add("anthropic", Credential{Type: "oauth", Access: "a1", Email: "one@x.com"})
	a2, _, _ := s.Add("anthropic", Credential{Type: "oauth", Access: "a2", Email: "two@x.com"})
	// Re-login of the same account updates it in place.
	upd, replaced, _ := s.Add("anthropic", Credential{Type: "oauth", Access: "a1-new", Email: "ONE@x.com"})
	if !replaced || upd.ID != a1.ID {
		t.Fatalf("same email should replace account %s, got %+v replaced=%v", a1.ID, upd, replaced)
	}
	accts := s.Accounts("anthropic")
	if len(accts) != 2 || accts[0].Access != "a1-new" || accts[1].ID != a2.ID {
		t.Fatalf("accounts = %+v", accts)
	}
	// Each account has its own token source.
	if tok, _ := s.TokenSource("anthropic", a2.ID).Token(context.Background()); tok != "a2" {
		t.Fatalf("token for account 2 = %q", tok)
	}
	if err := s.DeleteAccount("anthropic", a1.ID); err != nil {
		t.Fatal(err)
	}
	if accts := s.Accounts("anthropic"); len(accts) != 1 || accts[0].Email != "two@x.com" {
		t.Fatalf("after delete: %+v", accts)
	}
}

func TestStoreReadsSingleAccountFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	legacy := `{"anthropic":{"type":"oauth","access":"x","refresh":"r","email":"me@x.com"},"openai":{"type":"api_key","key":"k"}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Accounts("anthropic")
	if len(a) != 1 || a[0].Access != "x" || a[0].ID == "" {
		t.Fatalf("legacy anthropic = %+v", a)
	}
	// IDs derived for legacy entries are stable across loads and survive a write.
	s2, _ := Open(path)
	if id := s2.Accounts("anthropic")[0].ID; id != a[0].ID {
		t.Fatalf("legacy id not stable: %s vs %s", id, a[0].ID)
	}
	_, _, _ = s.Add("anthropic", Credential{Type: "oauth", Access: "y", Email: "other@x.com"})
	if got := s2.Accounts("anthropic"); len(got) != 2 || got[0].ID != a[0].ID {
		t.Fatalf("after migration write: %+v", got)
	}
}
