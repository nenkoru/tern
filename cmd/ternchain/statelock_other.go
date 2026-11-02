//go:build !unix

package main

import (
	"errors"
	"os"
	"time"
)

// lockFile: an exclusive lock file (O_EXCL), retried for a few seconds; a
// lock older than a minute is taken as left by a crashed process.
func lockFile(path string) (func(), error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if st, e := os.Stat(path); e == nil && time.Since(st.ModTime()) > time.Minute {
			os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, errors.New("state.json is locked by another process")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
