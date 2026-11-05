package main

import (
	"os"
	"runtime"
	"strings"
	"syscall"
)

// dropMallocStackLogging restarts tego without MallocStackLogging* in its
// environment. With one set (even to 0), macOS's malloc prints a line to
// the terminal on every fork ("can't turn off malloc stack logging…"),
// once per command tego runs, over its screen. libmalloc reads it at
// start, so unsetting it now would be too late: the process is replaced.
func dropMallocStackLogging() {
	if runtime.GOOS != "darwin" {
		return
	}
	env, found := withoutMallocStackLogging(os.Environ())
	if !found {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	_ = syscall.Exec(exe, os.Args, env) // returns only on failure: run as is
}

func withoutMallocStackLogging(environ []string) ([]string, bool) {
	env := make([]string, 0, len(environ))
	found := false
	for _, kv := range environ {
		if strings.HasPrefix(kv, "MallocStackLogging") {
			found = true
			continue
		}
		env = append(env, kv)
	}
	return env, found
}
