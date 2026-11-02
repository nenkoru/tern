// Operator commands and where they run (ops_on.go, ops_off.go).
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
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
// exit status are the command's), or says where the command lives. On Unix
// ternchain becomes ternchain-ops (exec, the same pid): a signal sent to
// ternchain (Ctrl-C, kill, a closed terminal) is the operator command's and
// nothing outlives it. Elsewhere, or if that fails, ternchain-ops runs as a
// child that gets ternchain's SIGINT/SIGTERM/SIGHUP, and ternchain exits
// with its status.
func forwardOps(args []string) error {
	p := opsBinary()
	if p == "" {
		return fmt.Errorf("%q is an operator command: it lives in ternchain-ops, not in this binary (build it beside this one: "+
			"go build -tags ops -o ternchain-ops ./cmd/ternchain ; docs/BUILD.md), then run ternchain-ops %s", strings.Join(args, " "), args[0])
	}
	if os.Getenv("TERNCHAIN_OPS_EXEC") != "0" { // 0: always a child (the tests of the relay)
		execOps(p, args) // returns only if it could not exec
	}
	cmd := exec.Command(p, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case s := <-sig:
			cmd.Process.Signal(s) // Windows can't deliver it; its console sent Ctrl-C to the child too
		case err := <-done:
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					os.Exit(128 + int(ws.Signal())) // the shell's status for a command a signal ended
				}
				os.Exit(ee.ExitCode())
			}
			if err != nil {
				return err
			}
			return errForwarded
		}
	}
}
