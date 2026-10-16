package tools

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
)

// Plan mode: the agent investigates and proposes, and changes nothing until
// the user approves. Shell commands are allowed only when every simple
// command in them is known to only read (inspect files, git history, run
// tests) and nothing is redirected into a file. Anything else waits until
// plan mode is off. It is an allowlist, so an unknown command is refused.

// readOnlyCmds are commands that only read, whatever their arguments
// (within reason: see readOnlyArgs for the ones with writing options).
var readOnlyCmds = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true, "grep": true, "egrep": true, "fgrep": true,
	"rg": true, "ag": true, "ack": true, "find": true, "fd": true, "tree": true, "file": true, "stat": true,
	"du": true, "df": true, "pwd": true, "echo": true, "printf": true, "which": true, "type": true, "whereis": true,
	"date": true, "uname": true, "whoami": true, "id": true, "hostname": true, "env": true, "printenv": true,
	"sort": true, "uniq": true, "cut": true, "tr": true, "nl": true, "column": true, "comm": true, "diff": true,
	"cmp": true, "basename": true, "dirname": true, "realpath": true, "readlink": true, "jq": true, "yq": true,
	"sha256sum": true, "shasum": true, "md5": true, "md5sum": true, "xxd": true, "od": true, "hexdump": true,
	"strings": true, "less": true, "more": true, "true": true, "false": true, "test": true, "[": true,
	"ps": true, "lsof": true, "pgrep": true, "uptime": true, "sed": true, "curl": true,
	"go": true, "git": true, "npm": true, "pnpm": true, "yarn": true, "bun": true, "cargo": true, "pytest": true,
	"python": true, "python3": true, "node": true, "sleep": true, "cd": true,
}

// readOnlySub lists, for commands that also write, the subcommands that
// only read (or only run tests).
var readOnlySub = map[string]map[string]bool{
	"git": {"status": true, "log": true, "diff": true, "show": true, "blame": true, "ls-files": true, "ls-tree": true,
		"rev-parse": true, "describe": true, "grep": true, "shortlog": true, "branch": true, "tag": true, "remote": true,
		"config": true, "reflog": true, "cat-file": true, "merge-base": true, "rev-list": true, "show-ref": true,
		"name-rev": true, "worktree": true, "stash": true},
	"go":     {"test": true, "vet": true, "list": true, "version": true, "env": true, "doc": true},
	"npm":    {"test": true, "ls": true, "list": true, "view": true, "outdated": true, "run": true},
	"pnpm":   {"test": true, "ls": true, "list": true, "outdated": true},
	"yarn":   {"test": true, "list": true, "info": true},
	"bun":    {"test": true},
	"cargo":  {"test": true, "check": true, "tree": true, "metadata": true},
	"python": {"-m": true}, "python3": {"-m": true}, // only -m pytest/unittest, see readOnlyArgs
	"node": {"--version": true, "-v": true},
}

// quotedTargetRe finds a redirection into a quoted file name (quotes already
// blanked to ”), which redirectRe can't read.
var quotedTargetRe = regexp.MustCompile(`(?:^|[^0-9&>])>>?\s*''`)

// planBashBlocked returns why command may not run in plan mode ("" if it may).
func planBashBlocked(command string) string {
	for _, q := range quotedRe.FindAllString(command, -1) {
		if q[0] == '"' && (strings.Contains(q, "$(") || strings.Contains(q, "`")) {
			return "it runs a command substitution inside quotes"
		}
	}
	bare := quotedRe.ReplaceAllString(command, "''")
	for _, m := range redirectRe.FindAllStringSubmatch(bare, -1) {
		if m[1] != "/dev/null" {
			return "it writes to a file (" + m[1] + ")"
		}
	}
	if quotedTargetRe.MatchString(bare) {
		return "it writes to a file"
	}
	for _, argv := range simpleCommands(command) {
		argv = stripWrappers(argv)
		if len(argv) == 0 {
			continue
		}
		if why := readOnlyArgv(argv); why != "" {
			return why
		}
	}
	return ""
}

func readOnlyArgv(argv []string) string {
	name, args := path.Base(argv[0]), argv[1:]
	if !readOnlyCmds[name] {
		return name + " is not a read-only command"
	}
	if subs, ok := readOnlySub[name]; ok {
		sub := firstOperand(name, args)
		if !subs[sub] {
			return name + " " + sub + " may change things"
		}
		switch name + " " + sub {
		case "git branch", "git tag", "git remote", "git stash", "git worktree":
			// Listing only: any other operand creates, deletes or renames.
			if sub == "stash" && !hasArg(args, "list") && !hasArg(args, "show") {
				return "git stash changes the working tree"
			}
			for _, a := range args {
				if a == "-d" || a == "-D" || a == "-m" || a == "-M" || a == "--delete" || a == "add" || a == "remove" ||
					a == "rm" || a == "rename" || a == "set-url" || a == "prune" || a == "--set-upstream-to" || a == "-u" {
					return "git " + sub + " " + a + " changes the repository"
				}
			}
		case "git config":
			if !hasArg(args, "--get") && !hasArg(args, "--list") && !hasArg(args, "-l") && !hasArg(args, "--get-all") {
				return "git config without --get or --list may change settings"
			}
		case "npm run":
			if s := firstOperand("", args[1:]); s != "test" && !strings.HasPrefix(s, "test:") && s != "lint" && s != "typecheck" {
				return "npm run " + s + " is not a known read-only script"
			}
		case "python -m", "python3 -m":
			if m := firstOperand("", args[1:]); m != "pytest" && m != "unittest" {
				return name + " -m " + m + " is not a test runner"
			}
		}
	}
	switch name {
	case "find":
		for _, a := range args {
			if a == "-delete" || a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir" || strings.HasPrefix(a, "-fprint") || a == "-fls" {
				return "find " + a + " changes or runs things"
			}
		}
	case "fd":
		if hasArg(args, "-x") || hasArg(args, "-X") || hasArg(args, "--exec") || hasArg(args, "--exec-batch") {
			return "fd --exec runs other commands"
		}
	case "sed":
		for _, a := range args {
			if a == "-i" || strings.HasPrefix(a, "--in-place") || (len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(shortFlags(a), 'i')) {
				return "sed -i edits files"
			}
		}
	case "curl":
		for _, a := range args {
			if a == "-o" || a == "-O" || strings.HasPrefix(a, "--output") || a == "--remote-name" || a == "-X" ||
				strings.HasPrefix(a, "--request") || a == "-d" || strings.HasPrefix(a, "--data") || a == "-T" ||
				strings.HasPrefix(a, "--upload") || a == "-F" || strings.HasPrefix(a, "--form") {
				return "curl " + a + " writes a file or sends data"
			}
		}
	case "env":
		if len(args) > 0 { // stripWrappers left env with a command: unwrap failed
			return "env runs another command"
		}
	case "sort":
		if hasArg(args, "-o") {
			return "sort -o writes a file"
		}
	case "python", "python3":
		if len(args) == 0 || args[0] != "-m" {
			return name + " runs code that may change files"
		}
	}
	return ""
}

// firstOperand is the first argument that is not an option (after git's -C
// dir / -c k=v), or "" when there is none.
func firstOperand(name string, args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if name == "git" && (a == "-C" || a == "-c") {
			i++
			continue
		}
		if name == "python" || name == "python3" {
			return a // -m is the "subcommand"
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			return a
		}
		if name == "node" {
			return a
		}
	}
	return ""
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// withSubcommands are the commands whose first operand picks what they do
// ("go test", "git push"): a rule covers that subcommand only. For other
// commands the operands are files and such, and a rule covers the command.
var withSubcommands = map[string]bool{"git": true, "go": true, "npm": true, "pnpm": true, "yarn": true, "bun": true,
	"npx": true, "cargo": true, "docker": true, "podman": true, "kubectl": true, "helm": true, "make": true, "just": true,
	"gh": true, "pip": true, "pip3": true, "uv": true, "poetry": true, "brew": true, "apt": true, "apt-get": true,
	"dotnet": true, "mvn": true, "gradle": true, "terraform": true, "composer": true, "bundle": true, "rails": true,
	"rake": true, "mix": true, "deno": true, "nx": true, "turbo": true, "swift": true, "zig": true, "flutter": true}

// runsArgs are commands that run code given in their arguments: a rule for
// "bash" or "xargs" would allow anything.
var runsArgs = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
	"eval": true, "source": true, ".": true, "xargs": true, "parallel": true, "watch": true, "timeout": true,
	"ssh": true, "su": true}

// interpreters run code given with one of inlineCode's options.
var interpreters = map[string]bool{"python": true, "python3": true, "node": true, "ruby": true, "perl": true,
	"php": true, "deno": true, "bun": true, "lua": true, "osascript": true, "awk": true}

var inlineCode = map[string]bool{"-c": true, "-e": true, "--eval": true, "-p": true, "--print": true, "-r": true}

// CommandPrefixes returns each simple command of a shell command line as its
// name and subcommand ("go test", "npm run", "ls"), for rules that allow
// commands by prefix. ok is false when the line does more than run simple
// commands (a command substitution, a redirection into a file, a shell or
// interpreter given code to run): what that runs or writes is not in the
// prefixes, so no rule may cover it.
func CommandPrefixes(command string) (prefixes []string, ok bool) {
	if strings.Contains(command, "$(") || strings.Contains(command, "`") || strings.Contains(command, "<(") || strings.Contains(command, ">(") {
		return nil, false
	}
	bare := quotedRe.ReplaceAllString(command, "''")
	for _, m := range redirectRe.FindAllStringSubmatch(bare, -1) {
		if m[1] != "/dev/null" {
			return nil, false
		}
	}
	if quotedTargetRe.MatchString(bare) {
		return nil, false
	}
	for _, argv := range simpleCommands(command) {
		if len(argv) > 0 && (path.Base(argv[0]) == "sudo" || path.Base(argv[0]) == "doas") {
			return nil, false // a rule for rm must not allow sudo rm
		}
		argv = stripWrappers(argv)
		if len(argv) == 0 {
			continue
		}
		p := path.Base(argv[0])
		if runsArgs[p] {
			return nil, false
		}
		for _, a := range argv[1:] {
			if interpreters[p] && (inlineCode[a] || p == "awk") {
				return nil, false
			}
		}
		if withSubcommands[p] {
			if sub := firstOperand(p, argv[1:]); sub != "" {
				p += " " + sub
			}
		}
		prefixes = append(prefixes, p)
	}
	return prefixes, len(prefixes) > 0
}

// PlanBlocked returns why a call of tool name with input may not run in plan
// mode ("" if it may). Tools other than bash and bash_output are decided by
// the caller.
func PlanBlocked(name string, input json.RawMessage) string {
	switch name {
	case "bash":
		var in struct {
			Command    string `json:"command"`
			Background bool   `json:"background"`
		}
		_ = json.Unmarshal(input, &in)
		if in.Background {
			return "background commands (servers, watchers) wait until plan mode is off"
		}
		return planBashBlocked(in.Command)
	case "bash_output":
		return "" // reading or stopping a shell started before changes nothing
	}
	return name + " changes files"
}
