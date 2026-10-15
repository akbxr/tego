package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func guardEnv(t *testing.T) (*Env, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := filepath.Join(home, "code", "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Env{Cwd: proj}, home
}

func TestGuardBlocks(t *testing.T) {
	e, _ := guardEnv(t)
	for _, cmd := range []string{
		"rm -rf /",
		"rm -rf /*",
		"rm -fr ~",
		"rm -r ~/",
		"rm -rf $HOME",
		`rm -rf "${HOME}"/*`,
		"sudo rm -rf --no-preserve-root /",
		"rm -rf /usr",
		"/bin/rm -Rf /etc/",
		"cd /tmp && rm -rf ~",
		"echo cleaning; rm -rf ~/.",
		"rm -rf ../..", // proj -> code -> home
		"\\rm -rf ~",
		"FOO=1 rm -rf ~",
		"chmod -R 777 /",
		"sudo chown -R me ~",
		"find ~ -name '*.tmp' -delete",
		"find / -exec rm {} +",
		"dd if=/dev/zero of=/dev/disk2 bs=1m",
		"cat image.iso > /dev/sda",
		"mkfs.ext4 /dev/sdb1",
		"diskutil eraseDisk APFS X disk2",
		":(){ :|:& };:",
		"git push --force origin main",
		"git push -f origin HEAD:master",
		"git push origin +main",
		"git push origin --delete main",
		"git push origin :master",
		"git -C repo push --force-with-lease origin main",
		"echo $(rm -rf ~)",
	} {
		if why := e.blocked(cmd); why == "" {
			t.Errorf("not blocked: %s", cmd)
		}
	}
}

func TestGuardAllows(t *testing.T) {
	e, _ := guardEnv(t)
	for _, cmd := range []string{
		"rm -rf build",
		"rm -rf ./node_modules dist",
		"rm -rf /tmp/tbench/trials",
		"rm -rf ~/code/proj/tmp",
		"rm file.txt",
		"rm -f /",         // not recursive: rm refuses a directory anyway
		`echo "rm -rf ~"`, // just text
		"grep -r 'rm -rf /' .",
		"chmod -R 755 ./scripts",
		"find . -name '*.pyc' -delete",
		"dd if=/dev/zero of=./disk.img bs=1m count=10",
		"git push origin feature",
		"git push --force origin feature/x",
		"git push origin main",
		"git status && go test ./...",
		"python3 - <<'EOF'\nprint('rm -rf ~')\nEOF",
	} {
		if why := e.blocked(cmd); why != "" {
			t.Errorf("blocked %q: %s", cmd, why)
		}
	}
}

func TestGuardForcePushOfCurrentBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	e, _ := guardEnv(t)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = e.Cwd
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if e.blocked("git push --force") == "" {
		t.Fatal("force push of main not blocked")
	}
	if e.blocked("git push") != "" {
		t.Fatal("plain push blocked")
	}
}

func TestBashToolRefusesBlockedCommand(t *testing.T) {
	e, home := guardEnv(t)
	keep := filepath.Join(home, "keep.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]string{"command": "rm -rf ~"})
	_, err := (&bashTool{e}).Run(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "tego blocked this command") || !strings.Contains(err.Error(), "!<command>") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("the home folder was touched")
	}
}

func TestBashStreamsProgress(t *testing.T) {
	var chunks []string
	ctx := WithProgress(context.Background(), func(b []byte) { chunks = append(chunks, string(b)) })
	out, err := runShell(ctx, t.TempDir(), "echo one; sleep 0.2; echo two >&2", 5*time.Second)
	if err != nil || !strings.Contains(out, "one") || !strings.Contains(out, "two") {
		t.Fatalf("out %q err %v", out, err)
	}
	if strings.Join(chunks, "") != "one\ntwo\n" || len(chunks) < 2 {
		t.Fatalf("progress %q", chunks)
	}
}
