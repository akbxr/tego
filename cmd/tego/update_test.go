package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeRelease serves a GitHub release whose tego "binary" is a shell script
// printing its version.
func fakeRelease(t *testing.T, tag string, badSum, noAsset bool) {
	t.Helper()
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	script := []byte("#!/bin/sh\necho tego " + strings.TrimPrefix(tag, "v") + "\n")
	name := fmt.Sprintf("tego_%s_%s", runtime.GOOS, runtime.GOARCH)
	tw.WriteHeader(&tar.Header{Name: name + "/README.md", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi"))
	tw.WriteHeader(&tar.Header{Name: name + "/tego", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
	tw.Write(script)
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(tgz.Bytes())
	hexSum := hex.EncodeToString(sum[:])
	if badSum {
		hexSum = strings.Repeat("0", 64)
	}
	asset := name + ".tar.gz"
	if noAsset {
		asset = "tego_plan9_mips.tar.gz"
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + updateRepo + "/releases/latest":
			json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": []map[string]string{
				{"name": asset, "browser_download_url": srv.URL + "/dl/archive"},
				{"name": "checksums.txt", "browser_download_url": srv.URL + "/dl/sums"},
			}})
		case "/dl/archive":
			w.Write(tgz.Bytes())
		case "/dl/sums":
			fmt.Fprintf(w, "%s  %s\n%s  tego_other_arch.tar.gz\n", hexSum, name+".tar.gz", strings.Repeat("1", 64))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := updateAPI
	updateAPI = srv.URL
	t.Cleanup(func() { updateAPI = old })
}

func fakeExe(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "tego")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho tego 0.1.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func runUpdate(t *testing.T, exe, current string, check bool) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := selfUpdate(context.Background(), &out, "", exe, current, check)
	return out.String(), err
}

func TestUpdateInstallsANewerRelease(t *testing.T) {
	fakeRelease(t, "v0.2.0", false, false)
	exe := fakeExe(t)
	out, err := runUpdate(t, exe, "0.1.0", false)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	b, _ := os.ReadFile(exe)
	if !strings.Contains(string(b), "tego 0.2.0") || !strings.Contains(out, "Updated tego 0.1.0 → 0.2.0") {
		t.Fatalf("binary %q, output %q", b, out)
	}
	if st, _ := os.Stat(exe); st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("new binary not executable: %v", st.Mode())
	}
	entries, _ := os.ReadDir(filepath.Dir(exe))
	if len(entries) != 1 {
		t.Fatalf("leftover files next to the binary: %v", entries)
	}
}

func TestUpdateLeavesTheBinaryAlone(t *testing.T) {
	for _, c := range []struct {
		name, tag, current  string
		check, badSum, none bool
		wantErr, wantOut    string
	}{
		{name: "up to date", tag: "v0.1.0", current: "0.1.0", wantOut: "is the latest version"},
		{name: "older release", tag: "v0.0.9", current: "0.1.0", wantOut: "is the latest version"},
		{name: "check only", tag: "v0.2.0", current: "0.1.0", check: true, wantOut: "tego 0.2.0 is available"},
		{name: "bad checksum", tag: "v0.2.0", current: "0.1.0", badSum: true, wantErr: "checksum mismatch"},
		{name: "no build here", tag: "v0.2.0", current: "0.1.0", none: true, wantErr: "has no build for"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeRelease(t, c.tag, c.badSum, c.none)
			exe := fakeExe(t)
			out, err := runUpdate(t, exe, c.current, c.check)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err %v, want %q", err, c.wantErr)
				}
			} else if err != nil || !strings.Contains(out, c.wantOut) {
				t.Fatalf("err %v, output %q, want %q", err, out, c.wantOut)
			}
			if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "tego 0.1.0") {
				t.Fatalf("binary changed: %q", b)
			}
		})
	}
}

func TestStartupNotice(t *testing.T) {
	fakeRelease(t, "v0.2.0", false, false)
	dir := t.TempDir()
	if v := updateNotice(dir, "0.1.0", false); v != "" {
		t.Fatalf("notice %q before any check", v) // startup never waits for the network
	}
	refreshUpdateCache(dir, "0.1.0", false)
	if v := updateNotice(dir, "0.1.0", false); v != "0.2.0" {
		t.Fatalf("notice %q after the check", v)
	}
	for _, c := range []struct {
		name, current string
		off           bool
	}{
		{"up to date", "0.2.0", false}, {"newer build", "0.3.0", false}, {"turned off", "0.1.0", true},
		{"dev build", "dev", false}, {"dirty build", "0.1.0-dirty", false},
	} {
		if v := updateNotice(dir, c.current, c.off); v != "" {
			t.Errorf("%s: notice %q", c.name, v)
		}
	}

	// Checked recently: no request. An unreachable server would leave the
	// cache as it was either way, so point it at one that fails loudly.
	updateAPI = "http://127.0.0.1:1"
	before := readUpdateCache(dir)
	refreshUpdateCache(dir, "0.1.0", false)
	if after := readUpdateCache(dir); !after.Checked.Equal(before.Checked) {
		t.Fatal("checked again within a day")
	}
	// A day later and offline: the known version is kept.
	c := readUpdateCache(dir)
	c.Checked = c.Checked.Add(-25 * time.Hour)
	b, _ := json.Marshal(c)
	os.WriteFile(updateCachePath(dir), b, 0o600)
	refreshUpdateCache(dir, "0.1.0", false)
	if v := updateNotice(dir, "0.1.0", false); v != "0.2.0" {
		t.Fatalf("offline check lost the known version: %q", v)
	}
}

func TestUpdateRecordsTheLatestVersion(t *testing.T) {
	fakeRelease(t, "v0.2.0", false, false)
	dir := t.TempDir()
	exe := fakeExe(t)
	var out bytes.Buffer
	if err := selfUpdate(context.Background(), &out, dir, exe, "0.1.0", false); err != nil {
		t.Fatal(err)
	}
	// The updated binary is 0.2.0: it must not be told about 0.2.0.
	if v := updateNotice(dir, "0.2.0", false); v != "" {
		t.Fatalf("notice %q right after updating", v)
	}
}

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.1.9", true}, {"0.1.10", "0.1.9", true}, {"1.0.0", "0.9.9", true},
		{"0.1.0", "0.1.0", false}, {"0.1.0", "0.2.0", false}, {"v0.2", "0.1.9", true},
		{"0.2.0", "dev", true}, {"garbage", "0.1.0", false}, {"0.2.0-rc1", "0.1.0", true},
	} {
		if got := newerVersion(c.a, c.b); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
