package ext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/akbxr/tego/internal/trustkey"
)

// A project's extensions run code from whoever wrote the repository, so
// they run only once trusted: the trust is for the path and its content
// (a folder's files, all of them), and lapses when that changes. A path is
// known by its trustkey.Key, so trusting a project covers its worktrees.

type trust map[string]string // path's key -> sha256 of its content

func trustFile(home string) string { return filepath.Join(home, "trusted-extensions.json") }

func loadTrust(home string) trust {
	t := trust{}
	if data, err := os.ReadFile(trustFile(home)); err == nil {
		_ = json.Unmarshal(data, &t)
	}
	return t
}

func digest(c Candidate) string {
	if fi, err := os.Stat(c.Path); err == nil && fi.IsDir() {
		return dirDigest(c.Path)
	}
	data, err := os.ReadFile(c.Path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (t trust) has(c Candidate) bool {
	d := digest(c)
	return d != "" && (t[trustkey.Key(c.Path)] == d || t[c.Path] == d) // c.Path: trusted before keys
}

func (t trust) add(c Candidate) { t[trustkey.Key(c.Path)] = digest(c) }

// TrustProject trusts cwd's project extensions as they are now, without
// starting them (tego trust); it returns their names.
func TrustProject(home, cwd string) ([]string, error) {
	t := loadTrust(home)
	var names []string
	for _, c := range Discover(home, cwd) {
		if c.Project {
			t.add(c)
			names = append(names, c.Name())
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	return names, t.save(home)
}

func (t trust) save(home string) error {
	// Another session may have trusted something since t was read: keep it.
	merged := loadTrust(home)
	for k, v := range t {
		merged[k] = v
	}
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	tmp := trustFile(home) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, trustFile(home))
}
