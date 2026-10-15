package tools

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// A hard stop for the few shell commands that destroy a machine or shared
// history and are almost never what a coding task needs: recursive deletes
// of /, the home folder or a top-level system folder, force pushes to
// main/master, writes to raw disks, filesystem formatting and fork bombs.
// The bash tool refuses them whatever the model says; the user can still
// run them with tego's ! prefix. It is a guard rail, not a sandbox: a
// determined command (a script, eval, variables) gets past a text check.

// blocked returns why command must not run, or "" if it may.
func (e *Env) blocked(command string) string {
	if forkBombRe.MatchString(command) {
		return "it is a fork bomb"
	}
	if m := rawDiskWriteRe.FindString(command); m != "" {
		return "it writes to a raw disk device (" + strings.TrimSpace(strings.TrimLeft(m, ">")) + ")"
	}
	for _, argv := range simpleCommands(command) {
		if why := e.blockedArgv(argv); why != "" {
			return why
		}
	}
	return ""
}

var (
	forkBombRe     = regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`)
	rawDiskWriteRe = regexp.MustCompile(`>\s*/dev/(disk|rdisk|sd[a-z]|nvme|hd[a-z]|mmcblk|vd[a-z]|xvd)`)
)

func (e *Env) blockedArgv(argv []string) string {
	argv = stripWrappers(argv)
	if len(argv) == 0 {
		return ""
	}
	name, args := path.Base(argv[0]), argv[1:]
	switch {
	case name == "rm":
		recursive, noRoot := false, false
		for _, a := range args {
			switch {
			case a == "--recursive" || a == "-r" || a == "-R" || (len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a, "rR")):
				recursive = true
			case a == "--no-preserve-root":
				noRoot = true
			}
		}
		if noRoot {
			return "rm --no-preserve-root deletes the whole system"
		}
		if recursive {
			if t := e.protectedTarget(args); t != "" {
				return "it recursively deletes " + t
			}
		}
	case name == "chmod" || name == "chown" || name == "chgrp":
		if hasFlag(args, "-R", "--recursive") {
			if t := e.protectedTarget(args); t != "" {
				return "it recursively changes the permissions or owner of " + t
			}
		}
	case name == "find":
		if hasFlag(args, "-delete") || hasExecRm(args) {
			if t := e.protectedTarget(args); t != "" {
				return "it deletes files under " + t
			}
		}
	case name == "dd":
		for _, a := range args {
			if of, ok := strings.CutPrefix(a, "of="); ok && rawDisk(of) {
				return "dd writes to a raw disk device (" + of + ")"
			}
		}
	case strings.HasPrefix(name, "mkfs") || strings.HasPrefix(name, "newfs"):
		return name + " formats a filesystem"
	case name == "diskutil":
		for _, a := range args {
			switch strings.ToLower(a) {
			case "erasedisk", "erasevolume", "partitiondisk", "zerodisk", "randomdisk", "secureerase", "reformat":
				return "diskutil " + a + " erases a disk"
			}
		}
	case name == "git" && len(args) > 0:
		if why := e.blockedPush(args); why != "" {
			return why
		}
	}
	return ""
}

// blockedPush refuses force pushes and deletions of main or master.
func (e *Env) blockedPush(args []string) string {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") { // git -C dir, -c k=v
		if args[i] == "-C" || args[i] == "-c" {
			i++
		}
		i++
	}
	if i >= len(args) || args[i] != "push" {
		return ""
	}
	force, del := false, false
	var refs []string
	for _, a := range args[i+1:] {
		switch {
		case a == "-f" || a == "--force" || strings.HasPrefix(a, "--force-with-lease") || a == "--force-if-includes" || a == "--mirror":
			force = true
		case a == "-d" || a == "--delete":
			del = true
		case strings.HasPrefix(a, "-"):
		default:
			refs = append(refs, a)
		}
	}
	if len(refs) > 0 {
		refs = refs[1:] // the first is the remote
	}
	for _, r := range refs {
		plus := strings.HasPrefix(r, "+")
		r = strings.TrimPrefix(r, "+")
		dst := r
		if i := strings.LastIndexByte(r, ':'); i >= 0 {
			dst = r[i+1:]
			if r[:i] == "" { // ":main" deletes it
				del = true
			}
		}
		dst = strings.TrimPrefix(dst, "refs/heads/")
		if dst == "main" || dst == "master" {
			switch {
			case del:
				return "it deletes the remote " + dst + " branch"
			case force || plus:
				return "it force-pushes to " + dst
			}
		}
	}
	if force && len(refs) == 0 {
		if b := e.currentBranch(); b == "main" || b == "master" {
			return "it force-pushes the current branch, " + b
		}
	}
	return ""
}

func (e *Env) currentBranch() string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = e.Cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// protectedTarget returns the first non-flag argument that resolves to /,
// a top-level folder, the home folder or one of its parents.
func (e *Env) protectedTarget(args []string) string {
	home, _ := os.UserHomeDir()
	for _, a := range args {
		if a == "" || strings.HasPrefix(a, "-") {
			continue
		}
		p := a
		for _, h := range []string{"$HOME", "${HOME}", "~"} {
			if rest, ok := strings.CutPrefix(p, h); ok && (rest == "" || rest[0] == '/') && home != "" {
				p = home + rest
			}
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(e.Cwd, p)
		}
		p = filepath.Clean(p)
		for _, glob := range []string{"/*", "/.*", "/."} {
			p = strings.TrimSuffix(p, glob)
		}
		if p == "" {
			p = "/"
		}
		switch {
		case p == "/" || filepath.Dir(p) == "/":
			return a
		case home != "" && (p == home || strings.HasPrefix(home, p+"/")):
			return a
		}
	}
	return ""
}

func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f {
				return true
			}
		}
	}
	return false
}

func hasExecRm(args []string) bool {
	for i, a := range args {
		if (a == "-exec" || a == "-execdir") && i+1 < len(args) && path.Base(args[i+1]) == "rm" {
			return true
		}
	}
	return false
}

func rawDisk(p string) bool {
	return rawDiskWriteRe.MatchString(">" + p)
}

// stripWrappers drops what runs the real command: sudo and its options,
// env and its assignments, VAR=value prefixes, command, exec, nice, time
// and a leading backslash.
func stripWrappers(argv []string) []string {
	for len(argv) > 0 {
		a := strings.TrimPrefix(argv[0], `\`)
		switch {
		case a == "sudo" || a == "doas":
			argv = argv[1:]
			for len(argv) > 0 && strings.HasPrefix(argv[0], "-") {
				if argv[0] == "-u" || argv[0] == "-g" {
					argv = argv[1:]
				}
				argv = argv[1:]
			}
		case a == "env" || a == "command" || a == "exec" || a == "nice" || a == "time" || a == "nohup":
			argv = argv[1:]
			for len(argv) > 0 && strings.HasPrefix(argv[0], "-") {
				argv = argv[1:]
			}
		case strings.Contains(a, "=") && !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "="):
			argv = argv[1:]
		default:
			argv[0] = a
			return argv
		}
	}
	return argv
}

// simpleCommands splits a shell command line into the argument lists of
// its simple commands: at ; & | && || newlines and parentheses outside
// quotes, with quotes removed. Command substitutions are split out too.
func simpleCommands(s string) [][]string {
	var cmds [][]string
	var argv []string
	var word strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			argv = append(argv, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCmd := func() {
		endWord()
		if len(argv) > 0 {
			cmds = append(cmds, argv)
		}
		argv = nil
	}
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(s) {
				i++
				word.WriteByte(s[i])
			} else {
				word.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\' && i+1 < len(s):
			i++
			if s[i] != '\n' {
				word.WriteByte(s[i])
				inWord = true
			}
		case c == '&' && (i > 0 && s[i-1] == '>' || i+1 < len(s) && s[i+1] == '>'):
			word.WriteByte(c) // 2>&1, &>file: a redirection, not a separator
			inWord = true
		case c == ';' || c == '&' || c == '|' || c == '\n' || c == '(' || c == ')' || c == '`' || c == '{' || c == '}':
			endCmd()
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			endCmd()
			i++
		case c == '$' && i+1 < len(s) && s[i+1] == '{': // ${VAR} stays one word
			j := strings.IndexByte(s[i:], '}')
			if j < 0 {
				j = len(s) - i - 1
			}
			word.WriteString(s[i : i+j+1])
			inWord = true
			i += j
		case c == ' ' || c == '\t':
			endWord()
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	endCmd()
	return cmds
}
