// Operator commands and where they run (ops_on.go, ops_off.go).
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// opsCommands: what an operator runs, not a person or an agent.
var opsCommands = map[string]bool{"devnet": true, "faucet": true, "advance": true, "deploy": true, "validator": true,
	"fork-from-state": true, "checkpoint": true, "ceremony": true, "simulate": true, "relay": true}

// isOpsCommand: args[0] (and its subcommand) is an operator command.
func isOpsCommand(args []string) bool {
	return len(args) > 0 && opsCommands[args[0]]
}

// opsBinary: ternchain-ops beside this binary, else on the PATH ("": none).
func opsBinary() string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "ternchain-ops")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("ternchain-ops"); err == nil {
		return p
	}
	return ""
}

// errForwarded: the operator binary ran the command (and printed its result).
var errForwarded = errors.New("forwarded")

// forwardOps runs an operator command with ternchain-ops (its output and
// exit status are the command's), or says where the command lives.
func forwardOps(args []string) error {
	p := opsBinary()
	if p == "" {
		return fmt.Errorf("%q is an operator command: it lives in ternchain-ops, not in this binary (build it beside this one: "+
			"go build -tags ops -o ternchain-ops . ; BUILD.md), then run ternchain-ops %s", strings.Join(args, " "), args[0])
	}
	cmd := exec.Command(p, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return errForwarded
}
