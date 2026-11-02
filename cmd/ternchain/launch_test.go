package main

import (
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// docShCommands: the ternchain lines of a document's ```sh blocks, in order,
// with $VARS expanded from vars.
func docShCommands(t *testing.T, file string, vars map[string]string) [][]string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var cmds [][]string
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "```") {
			in = !in && strings.HasPrefix(l, "```sh")
			continue
		}
		if in && strings.HasPrefix(l, "ternchain ") {
			l = os.Expand(l, func(k string) string {
				if v, ok := vars[k]; ok {
					return v
				}
				t.Fatalf("%s: $%s in %q is not set", file, k, l)
				return ""
			})
			cmds = append(cmds, finalBWords(l)[1:])
		}
	}
	return cmds
}

// launchNet: a local network shaped like the public Base Sepolia profile:
// payments and gas in USDC (each user pays their own gas through an ERC-20
// paymaster), a bundler that isn't the network's, no relay and no faucet of
// its own. It returns the newcomer's folder (holding only net.json) and a
// stand-in for Circle's faucet (100 USDC: a devnet's gas costs far more than
// Base Sepolia's, about 0.005 USDC an operation there).
func launchNet(t *testing.T) (home string, faucet func(common.Address)) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TERNCHAIN_RELAY", "")
	t.Setenv("TERNCHAIN_DIR", "")
	cfg := devnetConfig{net: filepath.Join(dir, "op", "net.json"), listen: "127.0.0.1:0", admin: "127.0.0.1:0", faucetCoins: coins(20),
		p: Params{EpochLength: 86400, RecoveryDelay: 72 * 3600, GasPaymaster: "test", Profile: "public"}}
	d, err := startDevnet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	op, err := os.ReadFile(cfg.net)
	if err != nil {
		t.Fatal(err)
	}
	var f DevnetFile
	if err := json.Unmarshal(op, &f); err != nil {
		t.Fatal(err)
	}
	var opf DevnetFile // the operator's file beside it: the faucet key
	if err := readJSON(operatorPath(cfg.net), &opf); err != nil {
		t.Fatal(err)
	}
	fk, err := crypto.HexToECDSA(strings.TrimPrefix(opf.Faucet, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	usdc, pm := d.ch.dep.Contracts["TestUSDC"], d.ch.dep.Contracts["TestTokenPaymaster"]
	if usdc == "" || pm == "" {
		t.Fatal("no TestUSDC or TestTokenPaymaster on the devnet")
	}
	bk, _ := crypto.GenerateKey()
	if err := d.ch.transfer(fk, crypto.PubkeyToAddress(bk.PublicKey), coins(10)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newBundlerServer(d.ch, bk, func(*http.Request) string { return "" }))
	t.Cleanup(srv.Close)
	f.Faucet, f.Admin, f.FaucetURL, f.Relay = "", "", "", ""
	f.Bundler, f.Profile = srv.URL, "public"
	f.Settle = &Settlement{Token: usdc, Symbol: "USDC", Decimals: 6, Faucet: "https://faucet.circle.com"}
	f.Gas = &GasConfig{Pays: "user", Paymaster: pm, Token: usdc}
	pub, _ := json.MarshalIndent(f, "", "  ")
	home = filepath.Join(dir, "tern")
	os.MkdirAll(home, 0o755)
	os.WriteFile(filepath.Join(home, "net.json"), pub, 0o644)
	return home, func(a common.Address) {
		t.Helper()
		if _, err := d.ch.send(fk, nil, "TestUSDC", "mint", a, big.NewInt(100_000_000)); err != nil {
			t.Fatal(err)
		}
	}
}

// launchRun: runs one command; when it stops for want of USDC in an account
// (a newcomer's first owner init), the faucet sends that account some and it
// runs again, as AGENTS.md tells the human to.
func launchRun(t *testing.T, faucet func(common.Address), c []string) string {
	t.Helper()
	var buf strings.Builder
	prev := outW
	defer func() { outW = prev }()
	for try := 0; ; try++ {
		buf.Reset()
		outW = &buf
		err := run(c)
		outW = prev
		if err == nil {
			return buf.String()
		}
		m := regexp.MustCompile(`account (0x[0-9a-fA-F]{40}) pays its own gas`).FindStringSubmatch(err.Error())
		if m == nil || try > 0 {
			t.Fatalf("ternchain %s: %v", strings.Join(c, " "), err)
		}
		faucet(common.HexToAddress(m[1]))
	}
}

// AGENTS.md, the page an agent is sent to: every ternchain line of its sh
// blocks runs, in order, on a network that pays gas in test USDC the way the
// public Base Sepolia profile does; the human's only step is the faucet.
func TestAgentsDocJoin(t *testing.T) {
	cmds := docShCommands(t, repo("AGENTS.md"), map[string]string{"OWNER": "nina", "AGENT": "ai", "HOME": "~"})
	if len(cmds) < 8 {
		t.Fatalf("only %d commands in AGENTS.md", len(cmds))
	}
	old := approvalWait
	approvalWait = 0
	t.Cleanup(func() { approvalWait = old })
	home, faucet := launchNet(t)
	t.Chdir(home)
	t.Setenv("TERNCHAIN_NET", filepath.Join(home, "net.json"))
	for _, c := range cmds {
		o := launchRun(t, faucet, c)
		if strings.Join(c, " ") == "inbox --dir id/nina" && !strings.Contains(o, "Hello from your agent") {
			t.Fatalf("nina's inbox after her agent's hello: %s", o)
		}
	}
}

// launchPeople: a fresh network where each person joined with AGENTS.md (an
// agent named "ai"), the way an example app's README starts.
func launchPeople(t *testing.T, people ...string) (faucet func(common.Address)) {
	t.Helper()
	doc := repo("AGENTS.md")
	old := approvalWait
	approvalWait = 0
	t.Cleanup(func() { approvalWait = old })
	home, faucet := launchNet(t)
	t.Chdir(home)
	t.Setenv("TERNCHAIN_NET", filepath.Join(home, "net.json"))
	for _, p := range people {
		for _, c := range docShCommands(t, doc, map[string]string{"OWNER": p, "AGENT": "ai", "HOME": "~"}) {
			if c[0] == "send" {
				break // joined: the rest is the hello
			}
			launchRun(t, faucet, c)
		}
	}
	return faucet
}

// runExample: every ternchain line of an example's README, in order; check
// looks at a command's output (keyed by the command line).
func runExample(t *testing.T, dir string, people []string, check map[string]string) {
	t.Helper()
	readme, err := filepath.Abs(repo(filepath.Join("examples", dir, "README.md")))
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(readme), "*.md"))
	faucet := launchPeople(t, people...)
	for _, f := range files { // the example's own files (scenario.md…), as a person copies them
		b, _ := os.ReadFile(f)
		os.WriteFile(filepath.Base(f), b, 0o644)
	}
	cmds := docShCommands(t, readme, nil)
	if len(cmds) < 3 {
		t.Fatalf("only %d commands in %s", len(cmds), readme)
	}
	// x402 serve's default address, as the README shows it, becomes a free
	// port: another test process may hold 8402, and so does this process's
	// server from an earlier -count run (it runs until the process ends).
	x402Addr := freeAddr(t)
	for _, c := range cmds {
		for i := range c {
			c[i] = strings.ReplaceAll(c[i], "127.0.0.1:8402", x402Addr)
		}
		line := strings.Join(c, " ")
		if c[0] == "x402" && c[1] == "serve" { // a server: in the background, until the test ends
			served := make(chan error, 1)
			go func() { served <- run(c) }()
			waitHTTP(t, "http://"+flagValue(c, "--listen")+"/", served)
			continue
		}
		o := launchRun(t, faucet, c)
		for k, want := range check {
			if strings.HasPrefix(line, k) && !strings.Contains(o, want) {
				t.Fatalf("ternchain %s: want %q in\n%s", line, want, o)
			}
		}
	}
}

func flagValue(c []string, name string) string {
	for i, a := range c {
		if a == name && i+1 < len(c) {
			return c[i+1]
		}
	}
	return ""
}

// waitHTTP: until u answers; served reports the server stopping (it can't
// listen) before that.
func waitHTTP(t *testing.T, u string, served <-chan error) {
	t.Helper()
	for i := 0; i < 100; i++ {
		select {
		case err := <-served:
			t.Fatalf("%s: server stopped: %v", u, err)
		default:
		}
		if r, err := http.Get(u); err == nil {
			r.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never answered", u)
}

// freeAddr: a 127.0.0.1 port nothing listens on.
func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestExampleMailbox(t *testing.T) {
	runExample(t, "mailbox", []string{"nina", "oscar"}, map[string]string{
		"find --dir agent/ai.oscar bikes":  "ai.nina",
		"inbox --dir agent/ai.nina":        "stranger",
		"inbox --dir agent/ai.oscar":       "the river",
		"erc8004 register --dir agent/ai.": "agent",
	})
}

func TestExampleJobBoard(t *testing.T) {
	runExample(t, "jobboard", []string{"nina", "oscar", "eve"}, map[string]string{
		"job show --dir agent/ai.oscar 0": "\"status\"",
		"job show --dir id/nina 0":        "completed",
	})
}

func TestExampleSell(t *testing.T) {
	runExample(t, "sell", []string{"nina", "oscar"}, map[string]string{
		"find --dir agent/ai.oscar pastry": "ai.nina",
		"x402 pay":                         "Rest the dough",
	})
}

func TestExampleCowrite(t *testing.T) {
	runExample(t, "cowrite", []string{"nina", "oscar"}, map[string]string{
		"group read --dir id/oscar lighthouse-game": "letters from tomorrow",
		"inbox --dir agent/ai.nina":                 "second keeper",
	})
}
