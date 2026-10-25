package tools

import (
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Edits made with shell commands (sed -i, a Python script that rewrites a
// file, a heredoc into a source file) skip what the edit tool does: the
// check that the file did not change since it was read, the syntax check,
// the diff the user sees and the list of changed files kept across
// compaction. They are not blocked, since generating or rewriting files
// from a script is sometimes the right tool, but the result says so.
//
// Only targeted edits get the note, where the edit tool does the same job
// better. Left alone: bulk mechanical changes, the shell's job (sed or perl
// over a file list from $(...), a glob, find -exec or xargs, or over 3+
// files; a script that loops over a glob or walks a tree; gofmt -w, gopls
// rename, go generate and other tools are never matched), and scratch files
// (removed again by the same command, or named zz_*, tmp_*, *_tmp*, scratch*).

// bulkFiles is how many files one sed or perl call must name to count as a
// bulk change.
const bulkFiles = 3

// shellWriteNote is appended to the output of a command that writes source
// files, at most shellWriteNotes times per session.
const (
	shellWriteNote = "\n\n[note: this command changed source files from the shell. To change a file, use the edit tool: " +
		"several changes go in one call (edits), and a file you only saw through grep or sed needs no read when each old_string matches exactly once. " +
		"Shell edits skip the stale-file check, the error check and the diff the user sees.]"
	shellWriteNotes = 2
)

var (
	redirectRe  = regexp.MustCompile(`(?:^|[^0-9&>])>>?\s*['"]?([^\s'";&|)<>]+)`)
	teeRe       = regexp.MustCompile(`\btee\s+(?:-a\s+)?['"]?([^\s'";&|)]+)`)
	scriptRe    = regexp.MustCompile(`open\([^)]*,\s*['"][wa]\+?['"]|\.write_text\(|writeFileSync\(|os\.WriteFile\(|ioutil\.WriteFile\(`)
	codeLitRe   = regexp.MustCompile(`['"]([^'"\s]+\.(?:go|py|ts|tsx|js|jsx|mjs|rs|java|kt|swift|c|h|cc|cpp|hpp|rb|php|lua|luau|cs|sh|sql|vue|svelte|css|scss|html|yaml|yml|toml|json|md))['"]`)
	quotedRe    = regexp.MustCompile(`'[^']*'|"(?:[^"\\]|\\.)*"`)
	bulkScript  = regexp.MustCompile(`glob\.glob\(|\.r?glob\(|os\.walk\(|\.iterdir\(|filepath\.Walk|readdirSync\(|globSync\(`)
	sourceExtRe = regexp.MustCompile(`\.(?:go|py|ts|tsx|js|jsx|mjs|rs|java|kt|swift|c|h|cc|cpp|hpp|rb|php|lua|luau|cs|sh|sql|vue|svelte|css|scss|html|yaml|yml|toml|json|md)$`)
)

// writesSource reports whether command looks like it edits source files of
// the project (not scratch files under a temp folder).
func writesSource(command string) bool {
	// In-place editors, per simple command (quotes resolved, so a pattern
	// searched for or printed doesn't count). sed run by find or xargs is
	// never argv[0], so it never counts: that is a bulk change.
	for _, argv := range simpleCommands(command) {
		argv = stripWrappers(argv)
		if len(argv) == 0 {
			continue
		}
		name := path.Base(argv[0])
		if name != "sed" && name != "perl" {
			continue
		}
		targets, inPlace := inPlaceTargets(name, argv[1:])
		// No file named: they come from $(...); a glob or many: a bulk change.
		if !inPlace || len(targets) == 0 || len(targets) >= bulkFiles || anyGlob(targets) {
			continue
		}
		for _, t := range targets {
			if projectTarget(command, t) {
				return true
			}
		}
	}
	bare := quotedRe.ReplaceAllString(command, "''")
	for _, re := range []*regexp.Regexp{redirectRe, teeRe} {
		for _, m := range re.FindAllStringSubmatch(bare, -1) {
			if projectTarget(command, m[1]) {
				return true
			}
		}
	}
	if scriptRe.MatchString(command) && !bulkScript.MatchString(command) {
		for _, m := range codeLitRe.FindAllStringSubmatch(command, -1) {
			if projectTarget(command, m[1]) {
				return true
			}
		}
	}
	return false
}

// inPlaceTargets returns the files a sed or perl call edits in place
// (inPlace false when it doesn't edit in place).
func inPlaceTargets(name string, args []string) (targets []string, inPlace bool) {
	script := false // seen the script operand (or -e)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "" || a == "--":
		case a == "-e" || a == "-f" || a == "--expression" || a == "--file":
			i++
			script = true
		case strings.HasPrefix(a, "--"):
			if a == "--in-place" || strings.HasPrefix(a, "--in-place=") {
				inPlace = true
			}
		case len(a) > 1 && a[0] == '-':
			flags := shortFlags(a)
			if strings.ContainsRune(flags, 'i') {
				inPlace = true
				// BSD sed takes the backup suffix as the next argument: -i '' or -i .bak
				if name == "sed" && a == "-i" && i+1 < len(args) {
					if n := args[i+1]; n == "" || strings.HasPrefix(n, ".") && !strings.ContainsAny(n, "/ ") {
						i++
					}
				}
			}
			if name == "perl" && strings.HasSuffix(flags, "e") { // -pe 's/a/b/'
				i++
				script = true
			}
		case !script:
			script = true // sed's first operand is its script
		default:
			targets = append(targets, a)
		}
	}
	return targets, inPlace
}

// shortFlags is the letters and digits of a short option cluster: "-pi.bak"
// gives "pi".
func shortFlags(a string) string {
	a = strings.TrimPrefix(a, "-")
	for i, r := range a {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return a[:i]
		}
	}
	return a
}

func anyGlob(paths []string) bool {
	for _, p := range paths {
		if strings.ContainsAny(p, "*?[") {
			return true
		}
	}
	return false
}

// projectTarget: a project source file that is not a scratch file.
func projectTarget(command, p string) bool {
	return projectSource(p) && !scratchName(p) && !removedLater(command, p)
}

// scratchName: a file named as a throwaway.
func scratchName(p string) bool {
	b := strings.ToLower(filepath.Base(p))
	return strings.HasPrefix(b, "zz_") || strings.HasPrefix(b, "tmp_") || strings.HasPrefix(b, "scratch") ||
		strings.Contains(b, "_tmp") || strings.HasPrefix(b, ".tmp")
}

// removedLater: the same command deletes p again (a file made only to try something).
func removedLater(command, p string) bool {
	re, err := regexp.Compile(`(?:\brm\b|\bunlink\b|os\.remove\(|os\.unlink\(|\.unlink\()[^;&|\n]*?` + regexp.QuoteMeta(p))
	return err == nil && re.MatchString(command)
}

// projectSource: a source file path outside the temp folders.
func projectSource(p string) bool {
	if !sourceExtRe.MatchString(p) {
		return false
	}
	for _, tmp := range []string{"/tmp/", "/var/folders/", "/private/tmp/", "/dev/"} {
		if strings.HasPrefix(p, tmp) {
			return false
		}
	}
	return !strings.HasPrefix(filepath.Base(p), ".tmp")
}

// noteShellWrite returns the note for a command that wrote source files,
// "" otherwise or once the session had its share.
func (e *Env) noteShellWrite(command string) string {
	if !writesSource(command) {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.shellWrites >= shellWriteNotes {
		return ""
	}
	e.shellWrites++
	return shellWriteNote
}
