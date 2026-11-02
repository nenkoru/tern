//go:build !unix

package main

import "errors"

// execOps: no exec here (Windows); forwardOps runs ternchain-ops as a child.
func execOps(string, []string) error { return errors.ErrUnsupported }
