package main

import (
	"context"
	"math/big"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
)

// The genesis table: no key holds the supply; the Treasury gets the rest.
func TestGenesisTable(t *testing.T) {
	dep, _ := crypto.GenerateKey()
	da := crypto.PubkeyToAddress(dep.PublicKey)
	v1, _ := crypto.GenerateKey()
	fa, _ := crypto.GenerateKey()
	p := allocPlan{supply: exact("1000000000"), deployer: da, deployCoins: coins(100), founderCoins: coins(1000),
		validators: []common.Address{crypto.PubkeyToAddress(v1.PublicKey)}, validatorCoins: coins(10),
		faucet: crypto.PubkeyToAddress(fa.PublicKey), faucetCoins: exact("10000000"), chainID: l1TestnetChainID}
	rows, err := genesisTable(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := new(big.Int)
	for _, r := range rows {
		sum.Add(sum, r.wei)
	}
	last := rows[len(rows)-1]
	if sum.Cmp(p.supply) != 0 || last.Who != "treasury" || last.Address != treasuryAddress(da).Hex() || last.wei.Cmp(exact("989999890")) != 0 {
		t.Fatalf("table: %+v", rows)
	}
	big1 := p
	big1.faucetCoins = exact("10000001") // over 1%
	if _, err := genesisTable(big1); err == nil || !strings.Contains(err.Error(), "1%") {
		t.Fatalf("a key over 1%%: %v", err)
	}
	main := p
	main.chainID = l1MainnetChainID
	if _, err := genesisTable(main); err == nil || !strings.Contains(err.Error(), "no faucet on mainnet") {
		t.Fatalf("faucet on mainnet: %v", err)
	}
	dup := p
	dup.faucet = da
	if _, err := genesisTable(dup); err == nil {
		t.Fatal("one address twice")
	}
	g, err := validatorGenesis(genesisOpts{chainID: l1TestnetChainID, period: 1, timeout: 4, validators: p.validators, deployer: da,
		supply: p.supply, gasLimit: 60_000_000, alloc: rows})
	if err != nil {
		t.Fatal(err)
	}
	alloc := g["alloc"].(map[string]any)
	if b := alloc[strings.ToLower(da.Hex())].(map[string]any)["balance"]; b != "0x"+coins(100).Text(16) {
		t.Fatalf("the deployer got %v", b)
	}
	if g["config"].(map[string]any)["chainId"] != int64(l1TestnetChainID) {
		t.Fatalf("chain id %v", g["config"])
	}
}

// deploy from a fresh deployer puts the Treasury where the genesis put its
// coins, and leaves the deployer with gas only.
func TestTreasuryLandsOnGenesisCoins(t *testing.T) {
	dep, _ := crypto.GenerateKey()
	da := crypto.PubkeyToAddress(dep.PublicKey)
	sim := simulated.NewBackend(types.GenesisAlloc{da: {Balance: coins(100)}, treasuryAddress(da): {Balance: coins(5000)}}, withOsaka)
	defer sim.Close()
	ch, err := deploy(sim.Client(), dep, Params{EpochLength: 86400, RecoveryDelay: 72 * 3600, Fee: big.NewInt(1e12)}, "", func() { sim.Commit() })
	if err != nil {
		t.Fatal(err)
	}
	if ch.dep.Contracts["Treasury"] != treasuryAddress(da).Hex() {
		t.Fatalf("the Treasury is at %s, the genesis coins at %s (deploy's order changed: fix treasuryNonce)", ch.dep.Contracts["Treasury"], treasuryAddress(da).Hex())
	}
	ctx := context.Background()
	tb, _ := sim.Client().BalanceAt(ctx, treasuryAddress(da), nil)
	db, _ := sim.Client().BalanceAt(ctx, da, nil)
	if tb.Cmp(coins(5000)) < 0 || db.Cmp(coins(100)) >= 0 {
		t.Fatalf("treasury %s, deployer %s", fmtCoins(tb), fmtCoins(db))
	}
}

// The public RPC profile has no ADMIN, DEBUG, TXPOOL or QBFT, and no CORS.
func TestRPCProfile(t *testing.T) {
	a := nodeProfile(besuArgs("/d", "/g", "/k", "1.2.3.4", 30303, 8545, "", false), runOpts{profile: "rpc", allowHosts: "rpc.example.org", metrics: true}, "127.0.0.1")
	j := strings.Join(a, " ")
	for _, bad := range []string{"ADMIN", "DEBUG", "TXPOOL", "QBFT", "cors", "--host-allowlist=*"} {
		if strings.Contains(j, bad) {
			t.Errorf("rpc profile has %s: %s", bad, j)
		}
	}
	for _, want := range []string{"--rpc-http-api=ETH,NET,WEB3", "--host-allowlist=rpc.example.org", "--metrics-enabled", "--metrics-host=127.0.0.1"} {
		if !slices.Contains(a, want) {
			t.Errorf("rpc profile lacks %s: %s", want, j)
		}
	}
	if v := strings.Join(nodeProfile(besuArgs("/d", "/g", "/k", "h", 30303, 8545, "", false), runOpts{}, "127.0.0.1"), " "); !strings.Contains(v, "ADMIN") || strings.Contains(v, "metrics") {
		t.Errorf("validator profile changed: %s", v)
	}
}

// net.json's rpcs: a dead first RPC fails over to the next; an RPC of another
// chain with the same id (genesis_hash) is refused.
func TestNetFailover(t *testing.T) {
	dir := t.TempDir()
	cfg := devnetConfig{net: filepath.Join(dir, "net.json"), listen: "127.0.0.1:0", admin: "127.0.0.1:0", faucetCoins: coins(20),
		p: Params{EpochLength: 86400, RecoveryDelay: 72 * 3600, Fee: coins(0.000001)}}
	d, err := startDevnet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var f DevnetFile
	readJSON(cfg.net, &f)
	live := f.RPC
	f.RPC, f.RPCs = "http://127.0.0.1:1", []string{live}
	two := filepath.Join(dir, "two.json")
	writeJSON(two, f)
	ch, df, err := connect(two)
	if err != nil {
		t.Fatalf("no failover: %v", err)
	}
	if df.RPC != live || ch.dep.RPC != live {
		t.Fatalf("took %s / %s, want %s", df.RPC, ch.dep.RPC, live)
	}
	f.GenesisHash = "0x" + strings.Repeat("ab", 32)
	other := filepath.Join(dir, "other.json")
	writeJSON(other, f)
	if _, _, err := connect(other); err == nil || !strings.Contains(err.Error(), "another chain with the same id") {
		t.Fatalf("connected to another chain: %v", err)
	}
}
