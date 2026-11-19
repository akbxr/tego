package lineedit

import "golang.org/x/sys/unix"

// dup2 is Dup3, which every Linux has; arm64 has no dup2.
func dup2(from, to int) error { return unix.Dup3(from, to, 0) }
