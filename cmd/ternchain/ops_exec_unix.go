//go:build unix

package main

import (
	"os"
	"syscall"
)

// execOps replaces this process with ternchain-ops p (argv0 p, then args);
// it returns only if the exec failed.
func execOps(p string, args []string) error {
	return syscall.Exec(p, append([]string{p}, args...), os.Environ())
}
