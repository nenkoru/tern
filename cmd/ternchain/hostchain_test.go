package main

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
)

// tern-testnet-1 was deployed by this contract set: a binary whose hash
// differs speaks to it with ABIs it wasn't made with (ternchain version:
// matches_network false). Contracts for host chains (Base Sepolia) are built
// apart (build/ext) and don't count here.
const l1TestnetContractsHash = "55034bfd8aa4bd627e0d323fe5270ff09f210dcdf39e5eb06a522910df81aaf4"

func TestContractsHashMatchesL1Testnet(t *testing.T) {
	if h, _ := contractsHash(); h != l1TestnetContractsHash {
		t.Fatalf("contracts hash %s: tern-testnet-1 runs %s (a change to contracts/*.sol or build/*.abi|bin breaks clients of the running L1; "+
			"put new contracts in contracts/ext)", h, l1TestnetContractsHash)
	}
}

type countingLogs struct {
	head  uint64
	calls [][2]uint64
}

func (c *countingLogs) FilterLogs(_ context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	from, to := q.FromBlock.Uint64(), c.head
	if q.ToBlock != nil {
		to = q.ToBlock.Uint64()
	}
	c.calls = append(c.calls, [2]uint64{from, to})
	return []types.Log{{BlockNumber: from}}, nil
}

func (c *countingLogs) BlockNumber(context.Context) (uint64, error) { return c.head, nil }

func TestFilterLogsRanged(t *testing.T) {
	c := &countingLogs{head: 25_000}
	logs, err := filterLogsRanged(context.Background(), c, 10_000, ethereum.FilterQuery{FromBlock: big.NewInt(100)})
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]uint64{{100, 10_099}, {10_100, 20_099}, {20_100, 25_000}}
	if len(c.calls) != 3 || len(logs) != 3 {
		t.Fatalf("windows %v, logs %d", c.calls, len(logs))
	}
	for i, w := range want {
		if c.calls[i] != w {
			t.Fatalf("window %d: %v, want %v", i, c.calls[i], w)
		}
	}
	c.calls = nil // narrower than the cap: one query as asked
	if _, err := filterLogsRanged(context.Background(), c, 10_000, ethereum.FilterQuery{FromBlock: big.NewInt(24_000)}); err != nil || len(c.calls) != 1 {
		t.Fatalf("narrow query: %v %v", c.calls, err)
	}
}

func TestUserOpRPCRoundTrip(t *testing.T) {
	f := common.HexToAddress("0x1111111111111111111111111111111111111111")
	pm := common.HexToAddress("0x2222222222222222222222222222222222222222")
	gl := pack128(200_000, 80_000)
	op := packedUserOp{Sender: common.HexToAddress("0x3333333333333333333333333333333333333333"), Nonce: big.NewInt(7),
		InitCode: append(f.Bytes(), 0xaa, 0xbb), CallData: []byte{1, 2, 3}, AccountGasLimits: pack128(300_000, 250_000),
		PreVerificationGas: big.NewInt(100_000), GasFees: pack128(1_000, 9_000),
		PaymasterAndData: append(append(pm.Bytes(), gl[:]...), 0xcc), Signature: []byte{9}}
	r := toRPCOp(op)
	if r.Factory == nil || *r.Factory != f || r.Paymaster == nil || *r.Paymaster != pm || bigOf(r.CallGasLimit).Uint64() != 250_000 ||
		bigOf(r.VerificationGasLimit).Uint64() != 300_000 || bigOf(r.MaxFeePerGas).Uint64() != 9_000 || bigOf(r.PaymasterPostOpGasLimit).Uint64() != 80_000 {
		t.Fatalf("unpacked: %+v", r)
	}
	back := fromRPCOp(r)
	if back.Sender != op.Sender || back.Nonce.Cmp(op.Nonce) != 0 || string(back.InitCode) != string(op.InitCode) || back.AccountGasLimits != op.AccountGasLimits ||
		back.GasFees != op.GasFees || string(back.PaymasterAndData) != string(op.PaymasterAndData) || string(back.Signature) != string(op.Signature) {
		t.Fatalf("round trip: %+v", back)
	}
}

// newHostedWorld: the contracts on a chain that already has an EntryPoint
// (as Base Sepolia has the canonical one), and smart accounts' operations
// sent to a data node's bundler over JSON-RPC: no bundler key in the client.
func newHostedWorld(t *testing.T) (*world, common.Address) {
	return newHostedWorldWith(t, nil)
}

func newHostedWorldWith(t *testing.T, tweak func(*Params)) (*world, common.Address) {
	t.Helper()
	faucet, _ := crypto.GenerateKey()
	alloc := types.GenesisAlloc{crypto.PubkeyToAddress(faucet.PublicKey): {Balance: new(big.Int).Mul(coin, big.NewInt(1_000_000))}}
	sim := simulated.NewBackend(alloc, withOsaka)
	t.Cleanup(func() { sim.Close() })
	ea, err := loadArtifact("EntryPoint")
	if err != nil {
		t.Fatal(err)
	}
	opts, _ := bind.NewKeyedTransactorWithChainID(faucet, big.NewInt(1337))
	ep, _, _, err := bind.DeployContract(opts, ea.abi, ea.bin, sim.Client())
	if err != nil {
		t.Fatal(err)
	}
	sim.Commit()
	var last DeployProgress
	p := Params{EpochLength: 86400, RecoveryDelay: 72 * 3600, Fee: big.NewInt(1e12), EntryPoint: ep, Progress: func(d DeployProgress) { last = d }}
	if tweak != nil {
		tweak(&p)
	}
	ch, err := deploy(sim.Client(), faucet, p, "", func() { sim.Commit() })
	if err != nil {
		t.Fatal(err)
	}
	if ch.dep.Contracts["EntryPoint"] != ep.Hex() || last.Contracts["EntryPoint"] != ep.Hex() {
		t.Fatalf("EntryPoint %s, want the chain's %s", ch.dep.Contracts["EntryPoint"], ep.Hex())
	}
	bk, _ := crypto.GenerateKey()
	if err := ch.transfer(faucet, crypto.PubkeyToAddress(bk.PublicKey), coins(10)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newBundlerServer(ch, bk, func(*http.Request) string { return "" }))
	t.Cleanup(srv.Close)
	ch.bundlerURL, ch.hosted = srv.URL, true
	ch.unsafePaymaster = true // a devnet: TernPaymaster deposits allowed (accounts.go paymasterFundOK)
	return &world{t: t, sim: sim, ch: ch, dir: t.TempDir(), faucet: faucet}, ep
}

func TestHostedEntryPointAndRemoteBundler(t *testing.T) {
	w, _ := newHostedWorld(t)
	alice := w.passkeyOwner("alice") // its account made and registered through the bundler
	bob, _ := w.owner("bob")
	_ = bob
	if code, _ := w.ch.b.CodeAt(context.Background(), alice.me(), nil); len(code) == 0 {
		t.Fatal("alice's account was not deployed through the remote bundler")
	}
	if err := alice.deposit(coins(5)); err != nil {
		t.Fatal(err)
	}
	bot := w.accountAgent(alice, "bot", coins(1), exact("0.01"))
	if err := bot.pay("bob", exact("0.5"), "through the bundler"); err != nil {
		t.Fatal(err)
	}
	if err := bot.pay("bob", exact("0.6"), "over the limit"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("a failing operation's reason through the bundler: %v", err)
	}
}

func TestDeployResumes(t *testing.T) {
	faucet, _ := crypto.GenerateKey()
	alloc := types.GenesisAlloc{crypto.PubkeyToAddress(faucet.PublicKey): {Balance: new(big.Int).Mul(coin, big.NewInt(1_000_000))}}
	sim := simulated.NewBackend(alloc, withOsaka)
	defer sim.Close()
	var last DeployProgress
	p := Params{EpochLength: 86400, RecoveryDelay: 72 * 3600, Fee: big.NewInt(1e12), Progress: func(d DeployProgress) {
		last = DeployProgress{Contracts: map[string]string{}, Block: d.Block, Done: append([]string(nil), d.Done...)}
		for k, v := range d.Contracts {
			last.Contracts[k] = v
		}
	}}
	ch, err := deploy(sim.Client(), faucet, p, "", func() { sim.Commit() })
	if err != nil {
		t.Fatal(err)
	}
	nonce, _ := sim.Client().NonceAt(context.Background(), crypto.PubkeyToAddress(faucet.PublicKey), nil)
	// Run again on what it recorded: nothing deployed or sent twice (the
	// Directory is the timelock's now: a second transferOwnership would revert).
	p.Resume = &last
	ch2, err := deploy(sim.Client(), faucet, p, "", func() { sim.Commit() })
	if err != nil {
		t.Fatal(err)
	}
	if n2, _ := sim.Client().NonceAt(context.Background(), crypto.PubkeyToAddress(faucet.PublicKey), nil); n2 != nonce {
		t.Fatalf("resumed deploy sent %d transactions", n2-nonce)
	}
	for k, v := range ch.dep.Contracts {
		if ch2.dep.Contracts[k] != v {
			t.Fatalf("%s at %s, resumed at %s", k, v, ch2.dep.Contracts[k])
		}
	}
	if ch2.dep.Block != ch.dep.Block {
		t.Fatalf("first block %d, resumed %d", ch.dep.Block, ch2.dep.Block)
	}
}

type limitedLogs struct {
	countingLogs
	limit uint64
}

func (l *limitedLogs) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	to := l.head
	if q.ToBlock != nil {
		to = q.ToBlock.Uint64()
	}
	if to-q.FromBlock.Uint64()+1 > l.limit {
		return nil, fmt.Errorf("eth_getLogs is limited to a %d range", l.limit)
	}
	return l.countingLogs.FilterLogs(ctx, q)
}

// An RPC that caps log queries below net.json's log_range (sepolia.base.org: 200).
func TestRangeLimitLearned(t *testing.T) {
	l := &limitedLogs{countingLogs{head: 1000}, 200}
	w := new(atomic.Uint64)
	w.Store(10_000)
	logs, err := filterLogsAdaptive(context.Background(), l, w, ethereum.FilterQuery{FromBlock: big.NewInt(0)})
	if err != nil {
		t.Fatal(err)
	}
	if w.Load() != 199 || len(logs) != 6 { // 1001 blocks in windows of 199
		t.Fatalf("window %d, %d logs, calls %v", w.Load(), len(logs), l.calls)
	}
}
