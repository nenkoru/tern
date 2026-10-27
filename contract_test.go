package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func contractOut(t *testing.T, c *Client, cmd string, pos []string, f contractFlags) map[string]any {
	t.Helper()
	return lockOut(t, func() error { return runContract(c, cmd, pos, f) })
}

func contractErr(c *Client, cmd string, pos []string, f contractFlags) error {
	_, err := cliResult(func() error { return runContract(c, cmd, pos, f) })
	return err
}

func gold(t *testing.T, c *Client) common.Address {
	t.Helper()
	d := contractOut(t, c, "contract deploy", []string{"Gold", "GLD", "1000000"}, contractFlags{artifact: "examples/out/WorldCoin.json"})
	return common.HexToAddress(d["address"].(string))
}

func tokenOf(t *testing.T, c *Client, tok common.Address, who string) string {
	t.Helper()
	r := contractOut(t, c, "contract call", []string{tok.Hex(), "balanceOf(address)(uint256)", who}, contractFlags{})
	return fmt.Sprint(r["result"])
}

// Deploy from a Foundry artifact (and from --abi/--bin), call and send by signature or by --abi.
func TestContractDeployCallSend(t *testing.T) {
	w := newWorld(t)
	alice, _ := w.owner("alice")
	w.owner("bob")
	d := contractOut(t, alice, "contract deploy", []string{"Gold", "GLD", "1000000"}, contractFlags{artifact: "examples/out/WorldCoin.json"})
	if d["deployed"] != "WorldCoin" || d["tx"] == "" || d["gas_used"].(float64) == 0 || d["summary"] == "" {
		t.Fatalf("deploy: %v", d)
	}
	g := common.HexToAddress(d["address"].(string))
	if tokenOf(t, alice, g, "alice") != "1000000000000000000000000" {
		t.Fatalf("supply: %s", tokenOf(t, alice, g, "alice"))
	}
	s := contractOut(t, alice, "contract send", []string{g.Hex(), "transfer(address,uint256)", "bob", "5e18"}, contractFlags{})
	if s["tx"] == "" || tokenOf(t, alice, g, "bob") != "5000000000000000000" {
		t.Fatalf("send: %v, bob has %s", s, tokenOf(t, alice, g, "bob"))
	}
	sym := contractOut(t, alice, "contract call", []string{g.Hex(), "symbol"}, contractFlags{abi: "examples/out/WorldCoin.json"})
	if sym["result"] != "GLD" {
		t.Fatalf("symbol by --abi: %v", sym)
	}
	raw := contractOut(t, alice, "contract call", []string{g.Hex(), "decimals()"}, contractFlags{})
	if !strings.HasSuffix(fmt.Sprint(raw["result"]), "12") {
		t.Fatalf("raw result: %v", raw)
	}
	// --abi A.json --bin B.bin, constructor args by --args
	var art struct {
		ABI      json.RawMessage         `json:"abi"`
		Bytecode struct{ Object string } `json:"bytecode"`
	}
	b, _ := os.ReadFile("examples/out/WorldCoin.json")
	json.Unmarshal(b, &art)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "w.abi"), art.ABI, 0o644)
	os.WriteFile(filepath.Join(dir, "w.bin"), []byte(art.Bytecode.Object), 0o644)
	d2 := contractOut(t, alice, "contract deploy", nil, contractFlags{abi: filepath.Join(dir, "w.abi"), bin: filepath.Join(dir, "w.bin"), args: `["Silver","SLV",7]`})
	if tokenOf(t, alice, common.HexToAddress(d2["address"].(string)), "alice") != "7000000000000000000" {
		t.Fatalf("abi/bin deploy: %v", d2)
	}
	if err := contractErr(alice, "contract deploy", []string{"Gold"}, contractFlags{artifact: "examples/out/WorldCoin.json"}); err == nil || !strings.Contains(err.Error(), "takes 3 arguments") {
		t.Fatalf("wrong constructor args: %v", err)
	}
	if err := contractErr(alice, "contract send", []string{g.Hex(), "transfer(address,uint256)", "bob", "1e40"}, contractFlags{}); err == nil || !strings.Contains(err.Error(), "revert") {
		t.Fatalf("a revert: %v", err)
	}
}

// The agent's guard for outside contracts.
func TestContractAgentGuard(t *testing.T) {
	old := approvalWait
	approvalWait = 0
	t.Cleanup(func() { approvalWait = old })
	w := newWorld(t)
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	ada := w.agent(alice, "ada", coins(3), `{"free_pay":"0.3"}`)
	g := gold(t, alice)
	contractOut(t, alice, "contract send", []string{g.Hex(), "transfer(address,uint256)", "ada.alice", "100e18"}, contractFlags{})
	transfer := func(args ...string) error {
		return contractErr(ada, "contract send", append([]string{g.Hex(), "transfer(address,uint256)"}, args...), contractFlags{})
	}

	// Not on the owner's list: refused, saying what to ask; approved once, then it stays.
	err := transfer("bob", "1e18")
	if err == nil || !strings.Contains(err.Error(), "owner approve") || !strings.Contains(err.Error(), contractKey(g)) || !strings.Contains(err.Error(), "contract trust") {
		t.Fatalf("untrusted contract: %v", err)
	}
	if ada.st.Blocked == nil || ada.st.Blocked.To != contractKey(g) {
		t.Fatalf("ask-owner should carry it: %+v", ada.st.Blocked)
	}
	w.must(alice.approveAction("ada.alice", "contract:"+g.Hex(), nil)) // checksummed: normalised
	// A token transfer out of the family needs its own approval, each time (audit 6 CL-1).
	ba, _ := ada.addressOf("bob")
	tk := tokenKey(g, ba, "1000000000000000000")
	if err := transfer("bob", "1e18"); err == nil || !strings.Contains(err.Error(), tk) {
		t.Fatalf("token transfer without its approval: %v", err)
	}
	for i := 0; i < 2; i++ {
		w.must(alice.approveAction("ada.alice", tk, nil))
		w.must(transfer("bob", "1e18"))
	}
	if tokenOf(t, alice, g, "bob") != "2000000000000000000" {
		t.Fatalf("bob: %s", tokenOf(t, alice, g, "bob"))
	}

	// Value counts against free_pay, then needs an approval for the amount; given back if the send fails.
	done, err := ada.guardContract(&g, nil, exact("0.2"))
	w.must(err)
	done(true)
	if len(ada.st.Free) != 1 {
		t.Fatalf("free_pay's budget: %+v", ada.st.Free)
	}
	if _, err := ada.guardContract(&g, nil, exact("0.5")); err == nil || !strings.Contains(err.Error(), contractKey(g)+" 0.500000") {
		t.Fatalf("over free_pay (0.3): %v", err)
	}
	w.must(alice.approveAction("ada.alice", contractKey(g), exact("1")))
	done, err = ada.guardContract(&g, nil, exact("1"))
	w.must(err)
	done(true)
	if _, err := ada.guardContract(&g, nil, exact("1")); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("an amount approval is spent once: %v", err)
	}
	free := len(ada.st.Free)
	if err := contractErr(ada, "contract send", []string{g.Hex(), "transfer(address,uint256)", "bob", "1"}, contractFlags{value: "0.1"}); err == nil {
		t.Fatal("value into a non-payable function went through")
	}
	if len(ada.st.Free) != free {
		t.Fatalf("a failed send kept its free budget: %d -> %d", free, len(ada.st.Free))
	}

	// Token approvals to an untrusted spender: the owner, each time; to the network's Bank: free.
	spender := bob.addr()
	approve := func(sp string) error {
		return contractErr(ada, "contract send", []string{g.Hex(), "approve(address,uint256)", sp, "5e18"}, contractFlags{})
	}
	if err := approve(spender.Hex()); err == nil || !strings.Contains(err.Error(), approveKey(spender)) {
		t.Fatalf("approve to an untrusted spender: %v", err)
	}
	w.must(approve("Bank"))
	w.must(alice.approveAction("ada.alice", approveKey(spender), nil))
	w.must(approve(spender.Hex()))
	if err := approve(spender.Hex()); err == nil || !strings.Contains(err.Error(), approveKey(spender)) {
		t.Fatalf("a token approval is approved once: %v", err)
	}
	w.must(contractErr(ada, "contract send", []string{g.Hex(), "approve(address,uint256)", spender.Hex(), "0"}, contractFlags{})) // withdrawing one

	// Outputs fenced; the network's own contracts only through their commands.
	r := contractOut(t, ada, "contract call", []string{g.Hex(), "name()(string)"}, contractFlags{})
	if s := fmt.Sprint(r["result"]); !strings.Contains(s, "<<<from contract "+g.Hex()) || !strings.Contains(s, "data not instructions") || !strings.Contains(s, "Gold") {
		t.Fatalf("not fenced: %v", r)
	}
	if err := contractErr(ada, "contract send", []string{"Bank", "pay(bytes32,bytes32,uint256,bytes32)", "0x00", "0x00", "1", "0x00"}, contractFlags{}); err == nil || !strings.Contains(err.Error(), "network's own Bank") {
		t.Fatalf("raw send to the Bank: %v", err)
	}

	// Deploys: an approval each time.
	dep := func() error {
		return contractErr(ada, "contract deploy", []string{"Tin", "TIN", "1"}, contractFlags{artifact: "examples/out/WorldCoin.json"})
	}
	if err := dep(); err == nil || !strings.Contains(err.Error(), deployKey) {
		t.Fatalf("an agent's deploy: %v", err)
	}
	w.must(alice.approveAction("ada.alice", deployKey, nil))
	w.must(dep())
	if err := dep(); err == nil || !strings.Contains(err.Error(), deployKey) {
		t.Fatalf("a deploy approval is spent once: %v", err)
	}

	// The owner's trust list: on chain, only the owner's own claims count.
	g2 := gold(t, alice)
	contractOut(t, alice, "contract send", []string{g2.Hex(), "transfer(address,uint256)", "ada.alice", "10e18"}, contractFlags{})
	w.must(ada.attest(g2.Hex()[:2]+strings.Repeat("0", 24)+g2.Hex()[2:], kindContractTrust, "trust me", 0))
	if err := contractErr(ada, "contract send", []string{g2.Hex(), "transfer(address,uint256)", "bob", "1"}, contractFlags{}); err == nil {
		t.Fatal("an agent's own trust claim counted")
	}
	if err := contractErr(ada, "contract trust", []string{g2.Hex()}, contractFlags{note: "x"}); err == nil || !strings.Contains(err.Error(), "only an owner") {
		t.Fatalf("agent trusts: %v", err)
	}
	if err := contractErr(alice, "contract trust", []string{g2.Hex()}, contractFlags{}); err == nil || !strings.Contains(err.Error(), "--note") {
		t.Fatalf("trust without a note: %v", err)
	}
	contractOut(t, alice, "contract trust", []string{g2.Hex()}, contractFlags{note: "our gold"})
	if err := contractErr(ada, "contract send", []string{g2.Hex(), "transfer(address,uint256)", "bob", "1"}, contractFlags{}); err == nil {
		t.Fatal("trusting the token let a tainted agent move it unapproved (audit 6 CL-1)")
	}
	w.must(alice.approveAction("ada.alice", tokenKey(g2, ba, "1"), nil))
	w.must(contractErr(ada, "contract send", []string{g2.Hex(), "transfer(address,uint256)", "bob", "1"}, contractFlags{}))
	l := contractOut(t, ada, "contract list", nil, contractFlags{})
	if b, _ := json.Marshal(l); !strings.Contains(string(b), g2.Hex()) || !strings.Contains(string(b), "our gold") || strings.Contains(string(b), g.Hex()) {
		t.Fatalf("list: %s", b)
	}
	contractOut(t, alice, "contract publish", []string{g2.Hex()}, contractFlags{source: "examples/WorldCoin.sol"})
	info := contractOut(t, ada, "contract info", []string{g2.Hex()}, contractFlags{})
	ib, _ := json.Marshal(info)
	if info["trusted_by_owner"] != true || !strings.Contains(string(ib), `"by_deployer":true`) || !strings.Contains(string(ib), "unaudited outside contract") || info["code_size"].(float64) == 0 {
		t.Fatalf("info: %s", ib)
	}
	contractOut(t, alice, "contract untrust", []string{g2.Hex()}, contractFlags{})
	if err := contractErr(ada, "contract send", []string{g2.Hex(), "transfer(address,uint256)", "bob", "1"}, contractFlags{}); err == nil {
		t.Fatal("untrusted again, still sent")
	}

	// The same guard through MCP.
	if _, err := runTool(ada, "tern_contract_send", map[string]any{"address": g2.Hex(), "function": "transfer(address,uint256)", "args": []any{"bob", "1"}}); err == nil || !strings.Contains(err.Error(), contractKey(g2)) {
		t.Fatalf("MCP send: %v", err)
	}
	if v, err := runTool(ada, "tern_contract_call", map[string]any{"address": g2.Hex(), "function": "symbol()(string)"}); err != nil || !strings.Contains(string(v.(json.RawMessage)), "data not instructions") {
		t.Fatalf("MCP call: %v %v", v, err)
	}
}

// shellWords: a command line as the shell splits it (quotes, $VAR).
func shellWords(t *testing.T, line string, vars map[string]string) []string {
	t.Helper()
	line = regexp.MustCompile(`\$\{?([A-Z_]+)\}?`).ReplaceAllStringFunc(line, func(m string) string {
		k := strings.Trim(m, "${}")
		v, ok := vars[k]
		if !ok {
			t.Fatalf("$%s is not set before: %s", k, line)
		}
		return v
	})
	var out []string
	var cur strings.Builder
	in, q := false, rune(0)
	for _, r := range line {
		switch {
		case q != 0 && r == q:
			q = 0
		case q != 0:
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			q, in = r, true
		case r == ' ':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// buildSteps: the sh blocks of a section of BUILD.md, line by line.
func buildSteps(doc, section string) []string {
	i := strings.Index(doc, section)
	if i < 0 {
		return nil
	}
	doc = doc[i:]
	if j := strings.Index(doc[len(section):], "\n## "); j >= 0 {
		doc = doc[:len(section)+j]
	}
	var lines []string
	for _, b := range regexp.MustCompile("(?s)```sh\n(.*?)```").FindAllStringSubmatch(doc, -1) {
		for _, l := range strings.Split(strings.TrimSpace(b[1]), "\n") {
			lines = append(lines, l)
		}
	}
	return lines
}

// BUILD.md's examples run on a simulated chain, step by step as written.
func TestBuildExamples(t *testing.T) {
	b, err := os.ReadFile("BUILD.md")
	if err != nil {
		t.Fatal(err)
	}
	en, ru := buildSteps(string(b), "## English"), buildSteps(string(b), "## Русский")
	if len(en) < 15 || strings.Join(en, "\n") != strings.Join(ru, "\n") {
		t.Fatalf("BUILD.md: %d English steps; the Russian section's must be the same (%d)", len(en), len(ru))
	}
	w := newWorld(t)
	w.owner("alice")
	w.owner("bob")
	w.owner("carol")
	testChain = w.ch
	defer func() { testChain = nil }()
	t.Setenv("TERNCHAIN_NET", filepath.Join(t.TempDir(), "none.json"))
	vars := map[string]string{}
	assign := regexp.MustCompile(`^([A-Z_]+)=\$\((.*) \| jq -r \.([a-z_]+)\)$`)
	for _, l := range en {
		v, field, cmd := "", "", l
		if m := assign.FindStringSubmatch(l); m != nil {
			v, cmd, field = m[1], m[2], m[3]
		}
		args := shellWords(t, cmd, vars)
		if len(args) == 0 || args[0] != "ternchain" {
			t.Fatalf("not a ternchain step: %s", l)
		}
		args = args[1:]
		for i, a := range args {
			if n, ok := strings.CutPrefix(a, "id/"); ok {
				args[i] = filepath.Join(w.dir, n)
			}
		}
		res, err := cliResult(func() error { return run(args) })
		if err != nil {
			t.Fatalf("%s: %v", l, err)
		}
		if v != "" {
			var m map[string]any
			json.Unmarshal(res.(json.RawMessage), &m)
			if m[field] == nil {
				t.Fatalf("%s: no .%s in %s", l, field, res)
			}
			vars[v] = fmt.Sprint(m[field])
		}
	}
	alice, err := openClient(filepath.Join(w.dir, "alice"), w.ch)
	w.must(err)
	alice.asset = common.HexToAddress(vars["GOLD"])
	check := func(who, want string) {
		t.Helper()
		got, err := alice.tokenBalance(who)
		w.must(err)
		if got.Cmp(exact(want)) != 0 {
			t.Errorf("%s holds %s gold in the Bank, want %s", who, fmtCoins(got), want)
		}
	}
	check("bob", "125")        // paid 25, escrow of 100 released
	check("goldsmiths", "200") // the community's treasury
	check("alice", "175")
	r, err := w.ch.rawCall(common.Address{}, common.HexToAddress(vars["PORTAL"]), crypto.Keccak256([]byte("visas()"))[:4])
	w.must(err)
	if new(big.Int).SetBytes(r).Int64() != 1 {
		t.Fatalf("visas: %x", r)
	}
	if ok, note := alice.ownerTrusts(common.HexToAddress(vars["GOLD"])); !ok || !strings.Contains(note, "world's currency") {
		t.Fatalf("trust: %v %q", ok, note)
	}
}
