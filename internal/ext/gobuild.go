package ext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	sdk "github.com/akbxr/tego/extension"
)

// Go extensions are built with the user's go toolchain, once per content:
// the program lands in ~/.tego/cache/ext-bin/<name>-<hash>. A .go file, or
// a folder of them without go.mod, is built in a module tego writes, with
// the extension package as this tego ships it (sdk.Source): no
// download, and the protocol always matches. A folder with its own go.mod
// is built as it is.

const goBuildTimeout = 3 * time.Minute

var goBuildMu sync.Mutex // one build at a time

// goModule is the go.mod tego writes for an extension without one.
const goModule = `module tego-extension

go 1.22

require github.com/akbxr/tego v0.0.0

replace github.com/akbxr/tego => ./tegosdk
`

// goSources are the files of a Go extension without a go.mod: a file, or
// a folder's .go files (tests aside).
func goSources(c Candidate) (map[string][]byte, bool, error) {
	fi, err := os.Stat(c.Path)
	if err != nil {
		return nil, false, err
	}
	if !fi.IsDir() {
		data, err := os.ReadFile(c.Path)
		return map[string][]byte{"main.go": data}, false, err
	}
	if _, err := os.Stat(filepath.Join(c.Path, "go.mod")); err == nil {
		return nil, true, nil
	}
	ents, err := os.ReadDir(c.Path)
	if err != nil {
		return nil, false, err
	}
	files := map[string][]byte{}
	for _, e := range ents {
		if n := e.Name(); !e.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			data, err := os.ReadFile(filepath.Join(c.Path, n))
			if err != nil {
				return nil, false, err
			}
			files[n] = data
		}
	}
	return files, false, nil
}

// buildGo builds c (if it changed) and returns the program.
func buildGo(home string, c Candidate) (string, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", errors.New("needs the Go toolchain (go) to build .go extensions; or put the built program there instead")
	}
	files, ownModule, err := goSources(c)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n", runtime.GOOS, sdk.Source)
	if ownModule {
		fmt.Fprintln(h, dirDigest(c.Path))
	} else {
		names := make([]string, 0, len(files))
		for n := range files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(h, "%s\n%d\n%s", n, len(files[n]), files[n])
		}
	}
	binDir := filepath.Join(home, "cache", "ext-bin")
	prefix := c.Name() + "-"
	bin := filepath.Join(binDir, prefix+hex.EncodeToString(h.Sum(nil))[:16])
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}

	goBuildMu.Lock()
	defer goBuildMu.Unlock()
	if _, err := os.Stat(bin); err == nil {
		return bin, nil // built meanwhile
	}
	dir := c.Path
	if !ownModule {
		dir = filepath.Join(home, "cache", "ext-go", c.Name())
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		sdkDir := filepath.Join(dir, "tegosdk", "extension")
		if err := os.MkdirAll(sdkDir, 0o700); err != nil {
			return "", err
		}
		write := map[string][]byte{
			filepath.Join(dir, "go.mod"):            []byte(goModule),
			filepath.Join(dir, "tegosdk", "go.mod"): []byte("module github.com/akbxr/tego\n\ngo 1.22\n"),
			filepath.Join(sdkDir, "extension.go"):   []byte(sdk.Source),
		}
		for n, data := range files {
			write[filepath.Join(dir, n)] = data
		}
		for p, data := range write {
			if err := os.WriteFile(p, data, 0o600); err != nil {
				return "", err
			}
		}
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), goBuildTimeout)
	defer cancel()
	tmp := bin + ".tmp"
	cmd := exec.CommandContext(ctx, goBin, "build", "-o", tmp, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("go build: %s", strings.ReplaceAll(msg, "\n", " | "))
	}
	if err := os.Rename(tmp, bin); err != nil {
		return "", err
	}
	// Older builds of the same extension.
	if old, _ := filepath.Glob(filepath.Join(binDir, prefix+"*")); len(old) > 0 {
		for _, p := range old {
			if p != bin && len(filepath.Base(p)) == len(filepath.Base(bin)) {
				os.Remove(p)
			}
		}
	}
	return bin, nil
}

// dirDigest is the content of a folder's files, for trust and builds; it
// skips .git and node_modules.
func dirDigest(dir string) string {
	h := sha256.New()
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if name := d.Name(); p != dir && (name == ".git" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if n++; n > 5000 {
			return filepath.SkipAll
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(h, "%s\n%d\n", filepath.ToSlash(rel), len(data))
		h.Write(data)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}
