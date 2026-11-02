package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// The host profiles: Base mainnet's standard contracts (checked on chain,
// networks/tern-base-1/README.md) and Base Sepolia's, by name and chain id.
func TestHostProfiles(t *testing.T) {
	b, ok := hostProfiles["base"]
	if !ok || b.ChainID != 8453 || !b.RealMoney {
		t.Fatalf("base: %+v", b)
	}
	for got, want := range map[string]string{
		b.USDC.Token:        "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913",
		b.CirclePaymaster:   "0x0578cFB241215b77442a541325d6A4E6dFE700Ec",
		b.ERC8004Identity:   "0x8004A169FB4a3325136EB29fA0ceB6D2e539a432",
		b.ERC8004Reputation: "0x8004BAa17C55a88189AE136b182e5fdA19dE9b63",
	} {
		if got != want || !common.IsHexAddress(got) || common.HexToAddress(got).Hex() != got {
			t.Fatalf("base: %s, want %s (checksummed)", got, want)
		}
	}
	if b.USDC.Decimals != 6 || b.USDC.Faucet != "" || b.X402Facilitator != "" {
		t.Fatalf("base USDC/x402: %+v", b)
	}
	s, ok := hostByChain(baseSepoliaChainID)
	if !ok || s.RealMoney || s.CirclePaymaster != circlePaymasterBaseSepolia || s.USDC.Token != baseSepoliaUSDC.Token || s.X402Facilitator != x402Facilitator {
		t.Fatalf("base-sepolia: %+v", s)
	}
	if !realMoneyChain(8453) || realMoneyChain(84532) || realMoneyChain(1337) || realMoneyChain(l1MainnetChainID) {
		t.Fatal("realMoneyChain")
	}
	n := mainnetNetworkNotice("tern-base-1", "base", &launchCaps, "USDC", true)
	for _, w := range []string{"UNAUDITED", "REAL MONEY", "few dollars", "5 USDC", "20 USDC", "forge"} {
		if !strings.Contains(n, w) {
			t.Fatalf("the mainnet notice lacks %q: %s", w, n)
		}
	}
	if err := validCaps(&launchCaps, 6); err != nil {
		t.Fatal(err)
	}
	if err := validCaps(&ValueCaps{PerPayment: "5"}, 6); err == nil {
		t.Fatal("caps without per_name accepted")
	}
	if err := validCaps(&ValueCaps{PerPayment: "0", PerName: "1"}, 6); err == nil {
		t.Fatal("a zero cap accepted")
	}
}

// net.json caps: the client refuses a payment, lock, job or deposit over the
// per-payment cap, and a deposit that takes a name's bank over its cap;
// tokens it can't price aren't capped.
func TestValueCaps(t *testing.T) {
	w := newWorld(t)
	usdc := common.HexToAddress(w.ch.dep.Contracts["TestUSDC"])
	usd := func(s string) *big.Int { v, _ := parseUnits(s, 6); return v }
	alice, _ := w.owner("alice")
	w.owner("bob")
	alice.asset = usdc
	if _, err := w.ch.send(w.faucet, nil, "TestUSDC", "mint", alice.addr(), usd("100")); err != nil {
		t.Fatal(err)
	}
	if err := alice.deposit(usd("30")); err != nil { // no caps: as before
		t.Fatal(err)
	}
	w.ch.settle = &Settlement{Token: usdc.Hex(), Symbol: "USDC", Decimals: 6}
	w.ch.caps = &ValueCaps{PerPayment: "5", PerName: "40", CoinPerPayment: "0.002", CoinPerName: "0.005"}
	defer func() { w.ch.caps, w.ch.settle = nil, nil }()
	if err := alice.deposit(usd("6")); err == nil || !strings.Contains(err.Error(), "caps one payment") {
		t.Fatalf("a deposit over the per-payment cap: %v", err)
	}
	if err := alice.deposit(usd("5")); err != nil {
		t.Fatal(err)
	}
	if err := alice.deposit(usd("5")); err != nil { // 40 now: at the cap
		t.Fatal(err)
	}
	if err := alice.deposit(usd("1")); err == nil || !strings.Contains(err.Error(), "caps a name's bank") {
		t.Fatalf("a deposit over the per-name cap: %v", err)
	}
	if err := alice.pay("bob", usd("5.000001"), "too much"); err == nil || !strings.Contains(err.Error(), "caps one payment") {
		t.Fatalf("a payment over the cap: %v", err)
	}
	if err := alice.pay("bob", usd("0.1"), "fine"); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.jobCreate("bob", "bob", usd("6"), 7, "a big job"); err == nil || !strings.Contains(err.Error(), "caps one payment") {
		t.Fatalf("a job over the cap: %v", err)
	}
	// The coin, and a token the caps don't price.
	if err := w.ch.capsCheck("Bank", "lock", nil, []any{alice.id(), idOf("bob"), mustCoins("0.003")}); err == nil {
		t.Fatal("a coin lock over coin_per_payment")
	}
	if err := w.ch.capsCheck("Bank", "deposit", mustCoins("0.001"), []any{idOf("alice")}); err != nil {
		t.Fatal(err)
	}
	other := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	if err := w.ch.capsCheck("Bank", "payToken", nil, []any{alice.id(), idOf("bob"), other, big.NewInt(1e18)}); err != nil {
		t.Fatalf("an unpriced token capped: %v", err)
	}
	if err := alice.x402Within(usdc, usd("6")); err == nil {
		t.Fatal("an x402 payment over the cap")
	}
}

// fakeChain: a JSON-RPC endpoint that says it is chain id and has nothing on it
// (deploy's checks before its first transaction).
func fakeChain(t *testing.T, id int64) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		res := any("0x0")
		switch req.Method {
		case "eth_chainId":
			res = fmt.Sprintf("0x%x", id)
		case "eth_getCode":
			res = "0x"
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func founders4() string {
	var fs []string
	for _, n := range []string{"ann", "ben", "cat", "dan"} {
		k, _ := crypto.GenerateKey()
		fs = append(fs, n+"="+crypto.PubkeyToAddress(k.PublicKey).Hex())
	}
	return strings.Join(fs, ",")
}

// deploy --host base: real money gets the guard rails, and refuses what
// would undo them; --solo-zk is for that case only.
func TestMainnetDeployGuards(t *testing.T) {
	k, _ := crypto.GenerateKey()
	kf := filepath.Join(t.TempDir(), "deployer.key")
	os.WriteFile(kf, []byte(keyHex(k)), 0o600)
	caps := &ValueCaps{PerPayment: "5", PerName: "20"}
	base := func(id int64) deployOpts {
		return deployOpts{rpc: fakeChain(t, id), keyPath: kf, netPath: filepath.Join(t.TempDir(), "net.json"), host: "base", profile: "public",
			caps: caps, soloZK: true}
	}
	for _, c := range []struct {
		why  string
		o    func() deployOpts
		want string
	}{
		{"another chain", func() deployOpts { return base(baseSepoliaChainID) }, "is chain 8453"},
		{"test network", func() deployOpts { o := base(8453); o.testNetwork = true; return o }, "--test-network"},
		{"every contract", func() deployOpts { o := base(8453); o.profile = ""; return o }, "contract-set public"},
		{"no caps", func() deployOpts { o := base(8453); o.caps = nil; return o }, "--cap-per-payment"},
		{"a zero cap", func() deployOpts { o := base(8453); o.caps = &ValueCaps{PerPayment: "0", PerName: "1"}; return o }, "positive"},
		{"founders", func() deployOpts { o := base(8453); o.founders = founders4(); return o }, "no founders"},
		{"a sponsor", func() deployOpts { o := base(8453); o.sponsorPerName, o.sponsorPerDay = "0.1", "0.1"; return o }, "no sponsor"},
		{"no host on a mainnet", func() deployOpts { o := base(8453); o.host = ""; return o }, "--host base"},
		{"solo zk on a test chain", func() deployOpts { o := base(baseSepoliaChainID); o.host = "base-sepolia"; return o }, "--solo-zk"},
	} {
		err := runDeployCmd(c.o())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.why, err, c.want)
		}
	}
}
