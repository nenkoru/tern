//go:build unix

package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ternchain devnet, forwarded to ternchain-ops: a SIGTERM to ternchain stops
// the devnet, leaves no ternchain-ops behind and frees its ports, whether
// ternchain became ternchain-ops (exec) or ran it as a child (the relay).
func TestOpsForwardSignal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds both binaries")
	}
	bin := t.TempDir()
	for _, b := range [][]string{{"-o", filepath.Join(bin, "ternchain")}, {"-tags", "ops", "-o", filepath.Join(bin, "ternchain-ops")}} {
		if out, err := exec.Command("go", append([]string{"build"}, append(b, ".")...)...).CombinedOutput(); err != nil {
			t.Fatalf("go build %v: %v\n%s", b, err, out)
		}
	}
	for _, mode := range []string{"exec", "child"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			netPath := filepath.Join(dir, "net.json")
			listen, admin := freeAddr(t), freeAddr(t)
			cmd := exec.Command(filepath.Join(bin, "ternchain"), "devnet", "--net", netPath, "--listen", listen, "--admin", admin)
			cmd.Env = append(os.Environ(), "HOME="+dir)
			if mode == "child" {
				cmd.Env = append(cmd.Env, "TERNCHAIN_OPS_EXEC=0")
			}
			log, _ := os.Create(filepath.Join(dir, "devnet.log"))
			defer log.Close()
			cmd.Stdout, cmd.Stderr = log, log
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			defer func() { // whatever happened, nothing of this test keeps running
				for _, pid := range opsPids(netPath) {
					syscall.Kill(pid, syscall.SIGKILL)
				}
			}()
			up := time.Now().Add(2 * time.Minute)
			for {
				if _, err := os.Stat(netPath); err == nil {
					if c, err := net.Dial("tcp", listen); err == nil {
						c.Close()
						break
					}
				}
				select {
				case err := <-done:
					b, _ := os.ReadFile(log.Name())
					t.Fatalf("devnet exited before it was up: %v\n%s", err, b)
				case <-time.After(200 * time.Millisecond):
				}
				if time.Now().After(up) {
					t.Fatal("devnet did not come up")
				}
			}
			if n := map[string]int{"exec": 1, "child": 2}[mode]; len(opsPids(netPath)) != n { // child: ternchain and ternchain-ops
				t.Fatalf("%s: processes before the signal: %v, not %d", mode, opsPids(netPath), n)
			}
			cmd.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				if err != nil {
					b, _ := os.ReadFile(log.Name())
					t.Fatalf("ternchain devnet after SIGTERM: %v\n%s", err, b)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("ternchain did not exit after SIGTERM")
			}
			for i := 0; ; i++ { // the child, if there was one, is the wrapper's to wait for: gone by now
				if p := opsPids(netPath); len(p) == 0 {
					break
				} else if i == 20 {
					t.Fatalf("ternchain-ops left running: %v", p)
				}
				time.Sleep(100 * time.Millisecond)
			}
			for _, a := range []string{listen, admin} {
				l, err := net.Listen("tcp", a)
				if err != nil {
					t.Fatalf("%s still taken: %v", a, err)
				}
				l.Close()
			}
		})
	}
}

// opsPids: the running processes whose command line names netPath (this
// test's devnet, under either binary's name).
func opsPids(netPath string) []int {
	out, _ := exec.Command("ps", "-A", "-o", "pid=,command=").Output()
	var pids []int
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) > 1 && strings.Contains(l, netPath) && !strings.Contains(l, "ps -A") {
			if pid, err := strconv.Atoi(f[0]); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}
