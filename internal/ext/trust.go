package ext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

// A project's extensions run code from whoever wrote the repository, so
// they run only once trusted: the trust is for the path and its content
// (a folder's files, all of them), and lapses when that changes.

type trust map[string]string // path -> sha256 of its content

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
	return d != "" && t[c.Path] == d
}

func (t trust) add(c Candidate) { t[c.Path] = digest(c) }

func (t trust) save(home string) error {
	data, err := json.MarshalIndent(t, "", "  ")
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
