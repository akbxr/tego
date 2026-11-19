package lineedit

import "golang.org/x/sys/unix"

func dup2(from, to int) error { return unix.Dup2(from, to) }
