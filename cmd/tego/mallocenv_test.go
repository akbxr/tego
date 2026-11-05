package main

import (
	"slices"
	"testing"
)

func TestWithoutMallocStackLogging(t *testing.T) {
	env, found := withoutMallocStackLogging([]string{"HOME=/h", "MallocStackLogging=0", "MallocStackLoggingNoCompact=1", "PATH=/bin"})
	if !found || !slices.Equal(env, []string{"HOME=/h", "PATH=/bin"}) {
		t.Fatalf("%v %v", found, env)
	}
	if _, found := withoutMallocStackLogging([]string{"HOME=/h"}); found {
		t.Fatal("found in a clean environment")
	}
}
