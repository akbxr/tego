package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// updateRepo hosts the releases that `tego update` installs.
const updateRepo = "akbxr/tego"

// updateAPI is the GitHub API base (tests point it elsewhere).
var updateAPI = "https://api.github.com"

const maxUpdateBytes = 100 << 20 // a release archive is ~5 MB

type ghRelease struct {
	Tag    string `json:"tag_name"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func cmdUpdate(dir string, args []string) error {
	check := false
	for _, a := range args {
		switch a {
		case "--check":
			check = true
		case "-h", "--help":
			fmt.Println("usage: tego update [--check]\n\nInstalls the latest release over this binary. --check only reports whether there is one.")
			return nil
		default:
			return fmt.Errorf("unknown argument %q (usage: tego update [--check])", a)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	return selfUpdate(ctx, os.Stdout, dir, exe, version, check)
}

// selfUpdate replaces the binary at exe with the latest release, if it is
// newer than current.
func selfUpdate(ctx context.Context, out io.Writer, dir, exe, current string, check bool) error {
	rel, err := latestRelease(ctx)
	if err != nil {
		return err
	}
	if dir != "" {
		defer recordLatest(dir, rel.Tag) // the startup notice now knows
	}
	latest := strings.TrimPrefix(rel.Tag, "v")
	if !releaseVersion(current) {
		// A build from a checkout: replacing it would quietly undo the
		// user's own changes, and it may well be newer than the release.
		fmt.Fprintf(out, "tego %s is a development build; the latest release is %s.\n", current, latest)
		if !check {
			fmt.Fprintf(out, "Not replacing it. Rebuild from source, or install the release: curl -fsSL https://raw.githubusercontent.com/%s/main/install.sh | sh\n", updateRepo)
		}
		return nil
	}
	if !newerVersion(latest, current) {
		fmt.Fprintf(out, "tego %s is the latest version\n", current)
		return nil
	}
	if check {
		fmt.Fprintf(out, "tego %s is available (this is %s). Run: tego update\n", latest, current)
		return nil
	}
	asset := fmt.Sprintf("tego_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	var archiveURL, sumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case asset:
			archiveURL = a.URL
		case "checksums.txt":
			sumsURL = a.URL
		}
	}
	if archiveURL == "" {
		return fmt.Errorf("release %s has no build for %s/%s (build from source: go install github.com/%s/cmd/tego@latest)",
			rel.Tag, runtime.GOOS, runtime.GOARCH, updateRepo)
	}
	if sumsURL == "" {
		return fmt.Errorf("release %s has no checksums.txt; not installing an unverified binary", rel.Tag)
	}
	fmt.Fprintf(out, "Downloading tego %s (%s)...\n", latest, asset)
	archive, err := download(ctx, archiveURL)
	if err != nil {
		return err
	}
	sums, err := download(ctx, sumsURL)
	if err != nil {
		return err
	}
	if err := verifySum(archive, sums, asset); err != nil {
		return err
	}
	bin, err := extractBinary(archive)
	if err != nil {
		return err
	}
	return replaceBinary(ctx, exe, bin, latest, current, out)
}

// The startup check. Interactive sessions read the last known latest
// version from a small cache file, so the notice costs nothing at startup,
// and refresh that file in the background at most once a day. The check
// runs only in release builds, and "update_check": "off" (or
// TEGO_NO_UPDATE_CHECK=1) turns it off.
const updateCheckEvery = 24 * time.Hour

type updateCache struct {
	Checked time.Time `json:"checked"`
	Latest  string    `json:"latest"`
}

func updateCachePath(dir string) string { return filepath.Join(dir, "update-check.json") }

func readUpdateCache(dir string) updateCache {
	var c updateCache
	if b, err := os.ReadFile(updateCachePath(dir)); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

// updateNotice is what to show at startup: the newer version, if the cache
// knows one ("" when up to date, unknown, or the check is off).
func updateNotice(dir, current string, off bool) string {
	if off || !releaseVersion(current) {
		return ""
	}
	if c := readUpdateCache(dir); newerVersion(c.Latest, current) {
		return strings.TrimPrefix(c.Latest, "v")
	}
	return ""
}

// refreshUpdateCache asks GitHub for the latest release if the cache is
// older than a day, and records the answer. Errors are ignored: offline, the
// notice just waits for a later start.
func refreshUpdateCache(dir, current string, off bool) {
	if off || !releaseVersion(current) {
		return
	}
	if c := readUpdateCache(dir); time.Since(c.Checked) < updateCheckEvery {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rel, err := latestRelease(ctx)
	c := updateCache{Checked: time.Now()}
	if err == nil {
		c.Latest = rel.Tag
	} else {
		c.Latest = readUpdateCache(dir).Latest // keep what we knew
	}
	if b, err := json.Marshal(c); err == nil {
		tmp := updateCachePath(dir) + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, updateCachePath(dir))
		}
	}
}

// releaseVersion: a version a release could be newer than; a dev build
// ("dev", "0.1.0-dirty" from go build) shouldn't nag.
func releaseVersion(v string) bool {
	_, ok := parseVersion(v)
	return ok && !strings.ContainsAny(v, "-+")
}

func latestRelease(ctx context.Context) (*ghRelease, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", updateAPI+"/repos/"+updateRepo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("checking for updates: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("checking for updates: GitHub answered %s", resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("checking for updates: %w", err)
	}
	if rel.Tag == "" {
		return nil, errors.New("checking for updates: the latest release has no tag")
	}
	return &rel, nil
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: %s for %s", resp.Status, url)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxUpdateBytes+1))
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	if len(b) > maxUpdateBytes {
		return nil, fmt.Errorf("download failed: %s is larger than %d MB", url, maxUpdateBytes>>20)
	}
	return b, nil
}

// verifySum checks archive against its line in checksums.txt
// ("<sha256>  <name>", as sha256sum writes it).
func verifySum(archive, sums []byte, name string) error {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || strings.TrimPrefix(f[1], "*") != name {
			continue
		}
		got := sha256.Sum256(archive)
		if !strings.EqualFold(f[0], hex.EncodeToString(got[:])) {
			return fmt.Errorf("checksum mismatch for %s; not installing it", name)
		}
		return nil
	}
	return fmt.Errorf("checksums.txt has no entry for %s; not installing it", name)
}

// extractBinary returns the tego executable from a release archive.
func extractBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("reading the archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("the archive holds no tego binary")
		}
		if err != nil {
			return nil, fmt.Errorf("reading the archive: %w", err)
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "tego" {
			return io.ReadAll(io.LimitReader(tr, maxUpdateBytes))
		}
	}
}

// replaceBinary writes bin next to exe, checks that it runs, and renames it
// over exe. A rename (not a rewrite in place) leaves the running process and
// its code signature alone; the file is swapped atomically.
func replaceBinary(ctx context.Context, exe string, bin []byte, latest, current string, out io.Writer) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".tego-update-*")
	if err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("can't write to %s; run the update with permission to write there, or reinstall:\n  curl -fsSL https://raw.githubusercontent.com/%s/main/install.sh | sh", dir, updateRepo)
		}
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	// It must run here and be the version we expect.
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	got, err := exec.CommandContext(cctx, tmpPath, "--version").Output()
	if err != nil {
		return fmt.Errorf("the downloaded binary does not run here: %w", err)
	}
	if !strings.Contains(string(got), latest) {
		return fmt.Errorf("the downloaded binary reports %q, not %s", strings.TrimSpace(string(got)), latest)
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated tego %s → %s (%s)\n", current, latest, exe)
	return nil
}

// recordLatest notes a version just installed or seen, so the startup
// notice doesn't show an update that was already made.
func recordLatest(dir, tag string) {
	c := updateCache{Checked: time.Now(), Latest: tag}
	if b, err := json.Marshal(c); err == nil {
		_ = os.WriteFile(updateCachePath(dir), b, 0o600)
	}
}

// newerVersion reports whether a is a later version than b ("0.2.0" >
// "0.1.9"). A version that doesn't parse (a dev build) counts as oldest.
func newerVersion(a, b string) bool {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka {
		return false
	}
	if !okb {
		return true
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i] // pre-release and build metadata don't order releases here
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
