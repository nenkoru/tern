// The social recovery of a people's L1 (docs/abuse/VALIDATORS.md, F4/F5):
// when more than a third of the validators stop (or more than two thirds
// censor), honest citizens restart the network from its last final state
// with the honest set, and clients follow the fork they signed.
//
//	ternchain fork-from-state --rpc ARCHIVE_IPC [--at BLOCK] --validators a=DIR,b=DIR,… --out DIR [--genesis OLD] [--net NET] [--chain-id N] [--why TEXT]
//	ternchain checkpoint sign   --checkpoint DIR/checkpoint.json --dir IDENT --rpc OLD_RPC [--genesis DIR/genesis.json]
//	ternchain checkpoint verify --checkpoint DIR/checkpoint.json --rpc FORK_RPC --old-rpc OLD_RPC|--net NET [--min N] [--follow --net NET [--dirs a,b]]
//
// The state comes from an archive node (`validator run --archive`: Besu's
// FOREST storage, whose debug_accountRange/debug_storageRangeAt enumerate
// every account and slot; Besu's default BONSAI storage returns nothing;
// DEBUG is on the node's IPC socket only, D/besu/besu.ipc), from a block a
// quorum of the old set sealed, checked against the block's state root
// before anything is written. The fork gets its own chain id.
//
// Trust: the old chain is the root (VA-1). A signer re-derives the genesis
// from the old chain's state (eth_getStorageAt at the block) before signing;
// a client checks the signers as citizens of the old chain at that block, and
// the fork's block 0 against the state root and chain id they signed. The new
// genesis holds that state as its alloc (balances, nonces, code, storage),
// with Validators rewritten to the honest set, its parentHash the old block's
// hash, its time the old block's time. Besu ignores a genesis "number", so
// block numbers restart at 0: the FeeOracle's congestion meter (the one
// contract keyed by block number) is reset; event logs stay on the old chain.
package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
)

// feeOracleLoadSlot: FeeOracle's `load` (multiplier uint128 | lastBlock
// uint64 << 128 | used uint64 << 192), checked by TestFeeOracleLoadSlot.
const feeOracleLoadSlot = 13

// Checkpoint: "this fork is the continuation", signed by citizens. Each
// field is checked against the old chain (from_*, the signers) or the fork
// (fork_*, genesis_keccak), never taken from the fork alone (VA-1).
type Checkpoint struct {
	Network     string   `json:"network,omitempty"`
	ChainID     int64    `json:"chain_id"`        // the old chain's
	ForkChainID int64    `json:"fork_chain_id"`   // the fork's own (VA-5): nothing signed for one replays on the other
	FromBlock   uint64   `json:"from_block"`      // the last final block of the old chain
	FromHash    string   `json:"from_hash"`       // its hash (the new genesis's parentHash)
	FromRoot    string   `json:"from_state_root"` // its state root (the dump matched it)
	FromTime    uint64   `json:"from_time"`
	ForkRoot    string   `json:"fork_state_root"`      // the fork's block-0 state root (that state with the honest set)
	Genesis     string   `json:"genesis_keccak"`       // keccak256 of the new genesis.json, byte for byte
	FeeOracle   string   `json:"fee_oracle,omitempty"` // the FeeOracle whose congestion meter the fork reset
	Validators  []string `json:"validators"`           // the honest set: name=0x…
	Removed     []string `json:"removed"`              // who the old set had and the fork doesn't
	Reason      string   `json:"reason"`
	Made        string   `json:"made"`
}

// CheckpointSig: one citizen's signature over the checkpoint's digest.
type CheckpointSig struct {
	Name      string `json:"name"`
	Address   string `json:"address"`
	Signature string `json:"signature"`
}

func (cp Checkpoint) digest() []byte {
	b, _ := json.Marshal(cp)
	return accounts.TextHash(crypto.Keccak256(append([]byte("ternchain checkpoint\n"), b...)))
}

type dumpAccount struct {
	Balance *big.Int
	Nonce   uint64
	Code    []byte
	Storage map[common.Hash]common.Hash
}

// dumpState: every account and slot at block h (after it), from an archive node.
func dumpState(ctx context.Context, rc *rpc.Client, h *types.Header, hash common.Hash, txIndex int) (map[common.Address]*dumpAccount, int, error) {
	num := hexutil.EncodeUint64(h.Number.Uint64())
	var addrs []common.Address
	start := common.Hash{}
	for page := 0; ; page++ {
		var r struct {
			AddressMap map[common.Hash]common.Address `json:"addressMap"`
			NextKey    *common.Hash                   `json:"nextKey"`
		}
		if err := rc.CallContext(ctx, &r, "debug_accountRange", num, 0, start.Hex(), 256); err != nil {
			return nil, 0, fmt.Errorf("debug_accountRange (an archive node: validator run --archive; DEBUG is on its IPC socket only: --rpc D/besu/besu.ipc): %w", err)
		}
		if page == 0 && len(r.AddressMap) == 0 {
			return nil, 0, errors.New("debug_accountRange returned no accounts: the node keeps no full state (run it with --archive: Besu FOREST storage)")
		}
		for _, a := range r.AddressMap {
			addrs = append(addrs, a)
		}
		if r.NextKey == nil || *r.NextKey == (common.Hash{}) || *r.NextKey == start || len(r.AddressMap) == 0 {
			break
		}
		start = *r.NextKey
	}
	cl := ethclient.NewClient(rc)
	out := map[common.Address]*dumpAccount{}
	var mu sync.Mutex
	var firstErr error
	slots := 0
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, a := range addrs {
		wg.Add(1)
		sem <- struct{}{}
		go func(a common.Address) {
			defer wg.Done()
			defer func() { <-sem }()
			acc, n, err := dumpAccountAt(ctx, cl, rc, h, hash, txIndex, a)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", a.Hex(), err)
				}
				return
			}
			out[a] = acc
			slots += n
		}(a)
	}
	wg.Wait()
	return out, slots, firstErr
}

func dumpAccountAt(ctx context.Context, cl *ethclient.Client, rc *rpc.Client, h *types.Header, hash common.Hash, txIndex int, a common.Address) (*dumpAccount, int, error) {
	bal, err := cl.BalanceAt(ctx, a, h.Number)
	if err != nil {
		return nil, 0, err
	}
	nonce, err := cl.NonceAt(ctx, a, h.Number)
	if err != nil {
		return nil, 0, err
	}
	code, err := cl.CodeAt(ctx, a, h.Number)
	if err != nil {
		return nil, 0, err
	}
	acc := &dumpAccount{Balance: bal, Nonce: nonce, Code: code, Storage: map[common.Hash]common.Hash{}}
	if len(code) == 0 {
		return acc, 0, nil
	}
	start := common.Hash{}
	for {
		var r struct {
			Storage map[common.Hash]struct {
				Key   *common.Hash `json:"key"`
				Value common.Hash  `json:"value"`
			} `json:"storage"`
			NextKey *common.Hash `json:"nextKey"`
		}
		if err := rc.CallContext(ctx, &r, "debug_storageRangeAt", hash.Hex(), txIndex, a.Hex(), start.Hex(), 1024); err != nil {
			return nil, 0, err
		}
		for hk, e := range r.Storage {
			if e.Key == nil {
				return nil, 0, fmt.Errorf("slot %s: the node has no preimage of its key", hk.Hex())
			}
			if e.Value != (common.Hash{}) {
				acc.Storage[*e.Key] = e.Value
			}
		}
		if r.NextKey == nil || *r.NextKey == (common.Hash{}) || *r.NextKey == start {
			break
		}
		start = *r.NextKey
	}
	return acc, len(acc.Storage), nil
}

func toAlloc(st map[common.Address]*dumpAccount) types.GenesisAlloc {
	alloc := types.GenesisAlloc{}
	for a, x := range st {
		alloc[a] = types.Account{Balance: x.Balance, Nonce: x.Nonce, Code: x.Code, Storage: x.Storage}
	}
	return alloc
}

// allocRoot: the state root of an alloc (go-ethereum's genesis hashing).
func allocRoot(alloc types.GenesisAlloc) common.Hash {
	g := &core.Genesis{Config: params.AllDevChainProtocolChanges, Alloc: alloc, GasLimit: 1, Difficulty: big.NewInt(1), BaseFee: big.NewInt(1)}
	return g.ToBlock().Root()
}

// rewriteValidators: the Validators storage with the honest set instead of
// the old one; governance, registry, citizens, rules, challenges stay.
// Scheduled removals and the change log restart (a new window).
func rewriteValidators(st map[common.Hash]common.Hash, vs []common.Address, names []common.Hash) (removed []common.Address) {
	get := func(k common.Hash) common.Hash { return st[k] }
	n := get(slotWord(valSlotList)).Big().Int64()
	var old []common.Address
	for i := 0; i < int(n); i++ {
		old = append(old, common.BytesToAddress(get(arraySlot(valSlotList, i)).Bytes()))
	}
	keep := map[common.Address]bool{}
	for _, v := range vs {
		keep[v] = true
	}
	for i, v := range old {
		delete(st, arraySlot(valSlotList, i))
		delete(st, mapSlot(addrKey(v), valSlotPosition))
		delete(st, mapSlot(addrKey(v), valSlotRemovalAt))
		delete(st, mapSlot(addrKey(v), valSlotRemovalWhy))
		c := get(mapSlot(addrKey(v), valSlotCitizenOf))
		if c != (common.Hash{}) {
			delete(st, mapSlot(addrKey(v), valSlotCitizenOf))
			delete(st, mapSlot(c, valSlotValidatorOf))
		}
		if !keep[v] {
			removed = append(removed, v)
		}
	}
	delete(st, slotWord(valSlotChanges)) // changes.length = 0
	st[slotWord(valSlotList)] = slotWord(int64(len(vs)))
	for i, v := range vs {
		st[arraySlot(valSlotList, i)] = addrKey(v)
		st[mapSlot(addrKey(v), valSlotPosition)] = slotWord(int64(i + 1))
		if names[i] != (common.Hash{}) {
			if prev := get(mapSlot(names[i], valSlotValidatorOf)); prev != (common.Hash{}) {
				delete(st, mapSlot(prev, valSlotCitizenOf))
			}
			st[mapSlot(addrKey(v), valSlotCitizenOf)] = names[i]
			st[mapSlot(names[i], valSlotValidatorOf)] = addrKey(v)
		}
	}
	return removed
}

// resetFeeMeter: FeeOracle's congestion meter keyed to the old block numbers.
func resetFeeMeter(st map[common.Hash]common.Hash) {
	k := slotWord(feeOracleLoadSlot)
	v := st[k].Big()
	mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	st[k] = common.BigToHash(v.And(v, mask)) // keep the multiplier; lastBlock = used = 0
}

type forkOpts struct {
	rpc, validators, out, oldGenesis, net, why string
	at                                         int64
	period, timeout                            uint64
	chainID                                    int64 // the fork's chain id (0: derived from the block it continues)
}

// chainBlock: a block as its node reports it: the header, the node's own
// hash (Besu's QBFT hash: go-ethereum's header hashing differs on these
// headers, never recompute it from the sealed header), its transactions.
type chainBlock struct {
	h    *types.Header
	hash common.Hash
	txs  []common.Hash
}

func readBlock(ctx context.Context, rc *rpc.Client, n *big.Int) (*chainBlock, error) {
	arg := "latest"
	if n != nil {
		arg = hexutil.EncodeBig(n)
	}
	var raw json.RawMessage
	if err := rc.CallContext(ctx, &raw, "eth_getBlockByNumber", arg, false); err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("no block %s", arg)
	}
	var h types.Header
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, err
	}
	var x struct {
		Hash         common.Hash   `json:"hash"`
		Transactions []common.Hash `json:"transactions"`
	}
	if err := json.Unmarshal(raw, &x); err != nil {
		return nil, err
	}
	return &chainBlock{&h, x.Hash, x.Transactions}, nil
}

// sealQuorum (VA-1, VA-10): the header carries commit seals of at least
// ⌈2n/3⌉ of the set (QBFT's quorum), and hashes, as QBFT hashes a block on
// chain (extraData without seals, round 0), to the hash the node gives.
func sealQuorum(h *types.Header, hash common.Hash, set []common.Address) error {
	var items []rlp.RawValue
	if err := rlp.DecodeBytes(h.Extra, &items); err != nil || len(items) < 5 {
		return fmt.Errorf("block %s: not a QBFT header (no commit seals)", h.Number)
	}
	onchain, _ := rlp.EncodeToBytes([]rlp.RawValue{items[0], items[1], items[2], {0x80}, {0xc0}})
	hh := types.CopyHeader(h)
	hh.Extra = onchain
	if hh.Hash() != hash {
		return fmt.Errorf("block %s: its header doesn't hash to %s, the hash the node gives", h.Number, hash.Hex())
	}
	_, sealers, _, err := qbftSigners(h)
	if err != nil {
		return err
	}
	in := map[common.Address]bool{}
	for _, v := range set {
		in[v] = true
	}
	got := map[common.Address]bool{}
	for _, s := range sealers {
		if in[s] {
			got[s] = true
		}
	}
	need := (2*len(set) + 2) / 3
	if len(set) == 0 || len(got) < need {
		return fmt.Errorf("block %s: sealed by %d of the %d validators, %d needed", h.Number, len(got), len(set), need)
	}
	return nil
}

// checkSealed: sealQuorum against the set the parent block's state names.
// (The node could still lie about state; it can no longer hand out a
// header no quorum sealed.)
func checkSealed(ctx context.Context, rc *rpc.Client, b *chainBlock) error {
	if b.h.Number.Sign() == 0 {
		return errors.New("block 0: a genesis has no seals")
	}
	va, err := loadArtifact("Validators")
	if err != nil {
		return err
	}
	data, _ := va.abi.Pack("getValidators")
	out, err := ethclient.NewClient(rc).CallContract(ctx, ethereum.CallMsg{To: &validatorsAddress, Data: data}, new(big.Int).Sub(b.h.Number, big.NewInt(1)))
	if err != nil {
		return fmt.Errorf("the validator set before block %s: %w", b.h.Number, err)
	}
	v, err := va.abi.Unpack("getValidators", out)
	if err != nil {
		return err
	}
	return sealQuorum(b.h, b.hash, v[0].([]common.Address))
}

// forkChainID (VA-5): the fork's own chain id, derived from the block it
// continues (two forks of different blocks differ), never the old chain's:
// no transaction or user operation replays between the two.
func forkChainID(old int64, from common.Hash) int64 {
	id := int64(binary.BigEndian.Uint32(crypto.Keccak256(from[:])[:4])>>1) | 1<<20
	if id == old {
		id++
	}
	return id
}

// forkSetCheck (VA-10): the honest set: no address or citizen twice, and at
// least 4 (3f+1 for f = 1: fewer survive no failure).
func forkSetCheck(vs []common.Address, names []common.Hash) error {
	if len(vs) < 4 {
		return fmt.Errorf("--validators: %d survive no failure: a fork needs at least 4 (QBFT: 3f+1)", len(vs))
	}
	seen, named := map[common.Address]bool{}, map[common.Hash]bool{}
	for i, v := range vs {
		if seen[v] {
			return fmt.Errorf("validator %s listed twice", v.Hex())
		}
		seen[v] = true
		if n := names[i]; n != (common.Hash{}) {
			if named[n] {
				return errors.New("one validator per citizen: a name is listed twice")
			}
			named[n] = true
		}
	}
	return nil
}

func forkFromState(o forkOpts) (map[string]any, error) {
	if o.rpc == "" || o.validators == "" || o.out == "" {
		return nil, errors.New("usage: ternchain fork-from-state --rpc ARCHIVE_IPC|RPC [--at BLOCK] --validators a=DIR,… --out DIR [--genesis OLD.json] [--net NET.json] [--chain-id N] [--why TEXT]")
	}
	vs, names, shown, err := parseNamedValidators(o.validators)
	if err != nil {
		return nil, err
	}
	if err := forkSetCheck(vs, names); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	rc, err := rpc.DialContext(ctx, o.rpc)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	cl := ethclient.NewClient(rc)
	var at *big.Int
	if o.at >= 0 {
		at = big.NewInt(o.at)
	}
	b, err := readBlock(ctx, rc, at)
	if err != nil {
		return nil, err
	}
	if err := checkSealed(ctx, rc, b); err != nil {
		return nil, fmt.Errorf("refusing to fork from it: %w", err)
	}
	h := b.h
	chainID, err := cl.ChainID(ctx)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	// The state after the block: debug_storageRangeAt counts in transactions; the
	// state root tells which reading is the block's end.
	var st map[common.Address]*dumpAccount
	slots := 0
	matched := false
	tried := map[int]bool{}
	for _, ti := range []int{len(b.txs), 0, len(b.txs) - 1} {
		if ti < 0 || tried[ti] {
			continue
		}
		tried[ti] = true
		if st, slots, err = dumpState(ctx, rc, h, b.hash, ti); err != nil {
			return nil, err
		}
		if allocRoot(toAlloc(st)) == h.Root {
			matched = true
			break
		}
	}
	if !matched {
		return nil, fmt.Errorf("the dumped state doesn't hash to block %d's state root %s: refusing to write a genesis from it (%s)", h.Number, h.Root.Hex(),
			strings.Join(dumpDiff(ctx, rc, h, st), "; "))
	}
	dumpS := time.Since(t0).Seconds()
	var fee common.Address
	feeNote := "no --net: the FeeOracle's congestion meter was not reset (fees revert until the new chain passes the old block number)"
	network := ""
	if o.net != "" {
		var f DevnetFile
		if err := readJSON(o.net, &f); err != nil {
			return nil, err
		}
		network = f.Network
		if a, ok := f.Contracts["FeeOracle"]; ok {
			if _, ok := st[common.HexToAddress(a)]; ok {
				fee = common.HexToAddress(a)
				feeNote = "FeeOracle " + a + ": the congestion meter starts at block 0 (the multiplier kept)"
			}
		}
	}
	// The genesis: the old config (or ours), the state as its alloc.
	var cfg map[string]any
	if o.oldGenesis != "" {
		var old map[string]any
		if err := readJSON(o.oldGenesis, &old); err != nil {
			return nil, err
		}
		cfg, _ = old["config"].(map[string]any)
		if cfg == nil {
			return nil, errors.New("--genesis: no config")
		}
		if id, ok := cfg["chainId"].(float64); ok && int64(id) != chainID.Int64() {
			return nil, fmt.Errorf("--genesis is chain %d, the RPC is chain %d", int64(id), chainID.Int64())
		}
		delete(cfg, "transitions")
	} else {
		g, err := validatorGenesis(genesisOpts{chainID: chainID.Int64(), period: o.period, timeout: o.timeout, validators: vs, names: names, gasLimit: h.GasLimit, supply: new(big.Int)})
		if err != nil {
			return nil, err
		}
		cfg = g["config"].(map[string]any)
	}
	forkID := o.chainID
	if forkID == 0 {
		forkID = forkChainID(chainID.Int64(), b.hash)
	}
	if forkID == chainID.Int64() {
		return nil, fmt.Errorf("--chain-id %d is the old chain's: the fork needs its own (else every transaction replays on both)", forkID)
	}
	gb, cp, err := forkGenesis(st, cfg, b, chainID.Int64(), forkID, vs, names, shown, fee)
	if err != nil {
		return nil, err
	}
	cp.Network, cp.Reason, cp.Made = network, o.why, time.Now().UTC().Format(time.RFC3339)
	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return nil, err
	}
	gpath := filepath.Join(o.out, "genesis.json")
	if err := os.WriteFile(gpath, gb, 0o644); err != nil {
		return nil, err
	}
	cpath := filepath.Join(o.out, "checkpoint.json")
	if err := writeJSON(cpath, cp); err != nil {
		return nil, err
	}
	os.Chmod(cpath, 0o644)
	return map[string]any{"genesis": gpath, "checkpoint": cpath, "from_block": h.Number.Uint64(), "from_hash": b.hash.Hex(),
		"state_root_matches": true, "accounts": len(st), "slots": slots, "dump_s": fmt.Sprintf("%.1f", dumpS), "validators": shown,
		"removed": cp.Removed, "fee_meter": feeNote, "genesis_keccak": cp.Genesis, "fork_state_root": cp.ForkRoot, "fork_chain_id": forkID,
		"next": "citizens: ternchain checkpoint sign --checkpoint " + cpath + " --dir IDENT --rpc OLD_RPC (it re-derives the genesis from the old chain first); each honest validator: ternchain validator run --dir D --genesis " +
			gpath + " --fresh …; clients: ternchain checkpoint verify --checkpoint " + cpath + " --rpc NEW_RPC --follow --net NET"}, nil
}

// forkGenesis: the fork's genesis.json from the old chain's state st after
// block b (st already matched to b's state root): Validators rewritten to
// the honest set, the FeeOracle's meter reset (fee != 0), the fork's own
// chain id; and the checkpoint naming all of it, the fork's state root too.
func forkGenesis(st map[common.Address]*dumpAccount, cfg map[string]any, b *chainBlock, oldID, forkID int64,
	vs []common.Address, names []common.Hash, shown []string, fee common.Address) ([]byte, Checkpoint, error) {
	vacc, ok := st[validatorsAddress]
	if !ok {
		return nil, Checkpoint{}, fmt.Errorf("no Validators contract at %s in the state", validatorsAddress.Hex())
	}
	removed := rewriteValidators(vacc.Storage, vs, names)
	if fee != (common.Address{}) {
		x, ok := st[fee]
		if !ok {
			return nil, Checkpoint{}, fmt.Errorf("no FeeOracle at %s in the state", fee.Hex())
		}
		resetFeeMeter(x.Storage)
	}
	cfg["chainId"] = forkID
	alloc := map[string]any{}
	for a, x := range st {
		m := map[string]any{"balance": "0x" + x.Balance.Text(16)}
		if x.Nonce != 0 {
			m["nonce"] = hexutil.EncodeUint64(x.Nonce)
		}
		if len(x.Code) > 0 {
			m["code"] = "0x" + hex.EncodeToString(x.Code)
		}
		if len(x.Storage) > 0 {
			s := map[string]string{}
			for k, v := range x.Storage {
				s[k.Hex()] = v.Hex()
			}
			m["storage"] = s
		}
		alloc[strings.ToLower(a.Hex())] = m
	}
	h := b.h
	baseFee := "0x3b9aca00"
	if h.BaseFee != nil {
		baseFee = hexutil.EncodeBig(h.BaseFee)
	}
	gen := map[string]any{
		"config": cfg, "nonce": "0x0", "timestamp": hexutil.EncodeUint64(h.Time), "gasLimit": hexutil.EncodeUint64(h.GasLimit),
		"difficulty": "0x1", "mixHash": qbftMixHash, "coinbase": "0x0000000000000000000000000000000000000000", "baseFeePerGas": baseFee,
		"parentHash": b.hash.Hex(), "extraData": "0x" + hex.EncodeToString(qbftExtraData(nil)), "alloc": alloc,
	}
	gb, _ := json.MarshalIndent(gen, "", " ")
	var rm []string
	for _, a := range removed {
		rm = append(rm, a.Hex())
	}
	cp := Checkpoint{ChainID: oldID, ForkChainID: forkID, FromBlock: h.Number.Uint64(), FromHash: b.hash.Hex(), FromRoot: h.Root.Hex(), FromTime: h.Time,
		ForkRoot: allocRoot(toAlloc(st)).Hex(), Genesis: crypto.Keccak256Hash(gb).Hex(), Validators: shown, Removed: rm}
	if fee != (common.Address{}) {
		cp.FeeOracle = fee.Hex()
	}
	return gb, cp, nil
}

// checkpointSet: the honest set a checkpoint names ("name=0x…" or "0x… (unbound)"), addresses only.
func checkpointSet(cp Checkpoint) ([]common.Address, []common.Hash, error) {
	var vs []common.Address
	var names []common.Hash
	for _, s := range cp.Validators {
		s = strings.TrimSpace(strings.TrimSuffix(s, " (unbound)"))
		name, ref, ok := strings.Cut(s, "=")
		if !ok {
			name, ref = "", s
		}
		if !common.IsHexAddress(ref) {
			return nil, nil, fmt.Errorf("validators: %q is not an address", s)
		}
		vs = append(vs, common.HexToAddress(ref))
		if name == "" {
			names = append(names, common.Hash{})
		} else {
			names = append(names, idOf(name))
		}
	}
	return vs, names, nil
}

// checkOldBlock: the old chain (its own nodes, the chain the client
// followed) has block cp.FromBlock with the checkpoint's hash, state root
// and time, sealed by a quorum of its validators, on the checkpoint's chain id.
func checkOldBlock(ctx context.Context, old *rpc.Client, cp Checkpoint, unsealed bool) error {
	if id, err := ethclient.NewClient(old).ChainID(ctx); err != nil || id.Int64() != cp.ChainID {
		return fmt.Errorf("the old chain's RPC is not chain %d (%v %v)", cp.ChainID, id, err)
	}
	b, err := readBlock(ctx, old, new(big.Int).SetUint64(cp.FromBlock))
	if err != nil {
		return err
	}
	if b.hash != common.HexToHash(cp.FromHash) || b.h.Root != common.HexToHash(cp.FromRoot) || b.h.Time != cp.FromTime {
		return fmt.Errorf("block %d is %s (state root %s, time %d) on the old chain, not the checkpoint's", cp.FromBlock, b.hash.Hex(), b.h.Root.Hex(), b.h.Time)
	}
	if !unsealed {
		return checkSealed(ctx, old, b)
	}
	return nil
}

func sameSlots(a, b map[common.Hash]common.Hash) bool {
	for k, v := range a {
		if v != (common.Hash{}) && b[k] != v {
			return false
		}
	}
	for k, v := range b {
		if v != (common.Hash{}) && a[k] != v {
			return false
		}
	}
	return true
}

func copySlots(m map[common.Hash]common.Hash) map[common.Hash]common.Hash {
	out := make(map[common.Hash]common.Hash, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// forkDerivationCheck (VA-1): the genesis gb is exactly the old chain's
// state after cp.FromBlock with Validators rewritten to cp.Validators (and
// the FeeOracle's meter reset), as the checkpoint says. Rebuilt from the
// genesis and the old chain's own slots, never from what the genesis says of
// itself: put back the old values of every slot the rewrite touches (read
// from the old chain at that block), and the whole alloc must hash to
// cp.FromRoot; then the rewrite of that must give the genesis's slots.
// Needs only the ETH API of the old chain (eth_getStorageAt at the block).
func forkDerivationCheck(ctx context.Context, old *rpc.Client, gb []byte, cp Checkpoint) error {
	if crypto.Keccak256Hash(gb).Hex() != cp.Genesis {
		return errors.New("the genesis file is not the one the checkpoint names (its keccak differs)")
	}
	var g struct {
		Config     map[string]any `json:"config"`
		Timestamp  hexutil.Uint64 `json:"timestamp"`
		ParentHash common.Hash    `json:"parentHash"`
		Alloc      map[common.Address]struct {
			Balance *hexutil.Big                `json:"balance"`
			Nonce   hexutil.Uint64              `json:"nonce"`
			Code    hexutil.Bytes               `json:"code"`
			Storage map[common.Hash]common.Hash `json:"storage"`
		} `json:"alloc"`
	}
	if err := json.Unmarshal(gb, &g); err != nil {
		return fmt.Errorf("genesis: %w", err)
	}
	if g.ParentHash != common.HexToHash(cp.FromHash) || uint64(g.Timestamp) != cp.FromTime {
		return errors.New("genesis: its parentHash or timestamp is not the checkpoint's block")
	}
	if id, _ := g.Config["chainId"].(float64); int64(id) != cp.ForkChainID || cp.ForkChainID == cp.ChainID || cp.ForkChainID == 0 {
		return fmt.Errorf("genesis: chain id %v, the checkpoint's fork chain id %d (the old chain's %d)", g.Config["chainId"], cp.ForkChainID, cp.ChainID)
	}
	if _, ok := g.Config["transitions"]; ok {
		return errors.New("genesis: transitions (a later switch of the validator selection or the rules)")
	}
	q, _ := g.Config["qbft"].(map[string]any)
	if va, _ := q["validatorcontractaddress"].(string); !strings.EqualFold(va, validatorsAddress.Hex()) {
		return fmt.Errorf("genesis: the validators don't come from the Validators contract at %s", validatorsAddress.Hex())
	}
	alloc := types.GenesisAlloc{}
	for a, x := range g.Alloc {
		bal := new(big.Int)
		if x.Balance != nil {
			bal = (*big.Int)(x.Balance)
		}
		alloc[a] = types.Account{Balance: bal, Nonce: uint64(x.Nonce), Code: x.Code, Storage: x.Storage}
	}
	if allocRoot(alloc).Hex() != common.HexToHash(cp.ForkRoot).Hex() {
		return errors.New("genesis: its state root is not the checkpoint's fork_state_root")
	}
	vs, names, err := checkpointSet(cp)
	if err != nil {
		return err
	}
	if err := forkSetCheck(vs, names); err != nil {
		return err
	}
	cl := ethclient.NewClient(old)
	at := new(big.Int).SetUint64(cp.FromBlock)
	cache := map[common.Address]map[common.Hash]common.Hash{}
	var readErr error
	get := func(a common.Address, k common.Hash) common.Hash {
		if cache[a] == nil {
			cache[a] = map[common.Hash]common.Hash{}
		}
		if v, ok := cache[a][k]; ok {
			return v
		}
		b, err := cl.StorageAt(ctx, a, k, at)
		if err != nil && readErr == nil {
			readErr = err
		}
		v := common.BytesToHash(b)
		cache[a][k] = v
		return v
	}
	// Every Validators slot the rewrite reads or writes, for the old set and the new.
	V := validatorsAddress
	touched := map[common.Hash]bool{slotWord(valSlotList): true, slotWord(valSlotChanges): true}
	byAddr := func(v common.Address) {
		for _, s := range []int64{valSlotPosition, valSlotRemovalAt, valSlotRemovalWhy, valSlotCitizenOf} {
			touched[mapSlot(addrKey(v), s)] = true
		}
	}
	n := get(V, slotWord(valSlotList)).Big()
	if !n.IsInt64() || n.Int64() > 1<<16 {
		return errors.New("the old chain's validator list is not a list")
	}
	for i := 0; i < int(n.Int64()); i++ {
		k := arraySlot(valSlotList, i)
		touched[k] = true
		v := common.BytesToAddress(get(V, k).Bytes())
		byAddr(v)
		if c := get(V, mapSlot(addrKey(v), valSlotCitizenOf)); c != (common.Hash{}) {
			touched[mapSlot(c, valSlotValidatorOf)] = true
		}
	}
	for i, v := range vs {
		touched[arraySlot(valSlotList, i)] = true
		byAddr(v)
		if names[i] != (common.Hash{}) {
			touched[mapSlot(names[i], valSlotValidatorOf)] = true
			if prev := get(V, mapSlot(names[i], valSlotValidatorOf)); prev != (common.Hash{}) {
				touched[mapSlot(prev, valSlotCitizenOf)] = true
			}
		}
	}
	gv, ok := alloc[V]
	if !ok {
		return errors.New("genesis: no Validators contract")
	}
	oldV := copySlots(gv.Storage)
	for k := range touched {
		if v := get(V, k); v == (common.Hash{}) {
			delete(oldV, k)
		} else {
			oldV[k] = v
		}
	}
	recon := types.GenesisAlloc{}
	for a, x := range alloc {
		recon[a] = x
	}
	rv := gv
	rv.Storage = oldV
	recon[V] = rv
	var fee common.Address
	var oldF map[common.Hash]common.Hash
	if cp.FeeOracle != "" {
		fee = common.HexToAddress(cp.FeeOracle)
		gf, ok := alloc[fee]
		if !ok || fee == V {
			return errors.New("genesis: no FeeOracle where the checkpoint says")
		}
		oldF = copySlots(gf.Storage)
		k := slotWord(feeOracleLoadSlot)
		if v := get(fee, k); v == (common.Hash{}) {
			delete(oldF, k)
		} else {
			oldF[k] = v
		}
		rf := gf
		rf.Storage = oldF
		recon[fee] = rf
	}
	if readErr != nil {
		return fmt.Errorf("the old chain's state at block %d: %w", cp.FromBlock, readErr)
	}
	if allocRoot(recon) != common.HexToHash(cp.FromRoot) {
		return fmt.Errorf("genesis: not the old chain's state at block %d (besides the validator set): accounts, balances, code or storage were changed", cp.FromBlock)
	}
	re := copySlots(oldV)
	removed := rewriteValidators(re, vs, names)
	if !sameSlots(re, gv.Storage) {
		return errors.New("genesis: its Validators storage is not the checkpoint's set put in the old contract")
	}
	var rm []string
	for _, a := range removed {
		rm = append(rm, a.Hex())
	}
	if strings.Join(rm, ",") != strings.Join(cp.Removed, ",") {
		return errors.New("checkpoint: its 'removed' list is not who the old set had and the fork doesn't")
	}
	if fee != (common.Address{}) {
		rf := copySlots(oldF)
		resetFeeMeter(rf)
		if !sameSlots(rf, alloc[fee].Storage) {
			return errors.New("genesis: the FeeOracle's storage differs beyond its congestion meter")
		}
	}
	return nil
}

func sigsPath(cp string) string { return strings.TrimSuffix(cp, ".json") + ".sigs.json" }

// checkpointSign: a citizen signs only after re-deriving the fork from the
// old chain (VA-1): its block, its seals, the genesis from its state.
func checkpointSign(ctx context.Context, cpPath, dir string, old *rpc.Client, genesis []byte, unsealed bool) (map[string]any, error) {
	var cp Checkpoint
	if err := readJSON(cpPath, &cp); err != nil {
		return nil, err
	}
	id, err := loadIdent(dir)
	if err != nil {
		return nil, err
	}
	if !id.isOwner() {
		return nil, errors.New("a citizen signs with its owner key (not an agent's)")
	}
	if old == nil || genesis == nil {
		return nil, errors.New("not signing blind: --rpc OLD_RPC (a node of the old chain) and the fork's genesis.json (--genesis, or next to the checkpoint)")
	}
	if err := checkOldBlock(ctx, old, cp, unsealed); err != nil {
		return nil, fmt.Errorf("not signing: %w", err)
	}
	if err := forkDerivationCheck(ctx, old, genesis, cp); err != nil {
		return nil, fmt.Errorf("not signing: %w", err)
	}
	sig, err := crypto.Sign(cp.digest(), id.key)
	if err != nil {
		return nil, err
	}
	var sigs []CheckpointSig
	readJSON(sigsPath(cpPath), &sigs)
	kept := sigs[:0]
	for _, s := range sigs {
		if s.Name != id.f.Name {
			kept = append(kept, s)
		}
	}
	kept = append(kept, CheckpointSig{Name: id.f.Name, Address: id.addr().Hex(), Signature: hexutil.Encode(sig)})
	if err := writeJSON(sigsPath(cpPath), kept); err != nil {
		return nil, err
	}
	os.Chmod(sigsPath(cpPath), 0o644)
	return map[string]any{"signed": id.f.Name, "signatures": len(kept), "file": sigsPath(cpPath), "derived": true}, nil
}

// checkpointVerify: checkpointCheck over RPCs; the genesis file, when there
// is one (--genesis, or next to the checkpoint), is re-derived too.
func checkpointVerify(cpPath, rpcURL, oldURL, genesisPath string, min int) (map[string]any, error) {
	var cp Checkpoint
	if err := readJSON(cpPath, &cp); err != nil {
		return nil, err
	}
	var sigs []CheckpointSig
	readJSON(sigsPath(cpPath), &sigs)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fork, err := rpc.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, err
	}
	defer fork.Close()
	old, err := rpc.DialContext(ctx, oldURL)
	if err != nil {
		return nil, err
	}
	defer old.Close()
	o := checkOpts{min: min}
	if genesisPath == "" {
		if p := filepath.Join(filepath.Dir(cpPath), "genesis.json"); fileExists(p) {
			genesisPath = p
		}
	}
	if genesisPath != "" {
		if o.genesis, err = os.ReadFile(genesisPath); err != nil {
			return nil, err
		}
	}
	res, err := checkpointCheck(ctx, old, fork, cp, sigs, o)
	if err != nil {
		return nil, err
	}
	res["checkpoint"], res["old_rpc"] = cpPath, oldURL
	return res, nil
}

type checkOpts struct {
	min      int    // more than half the citizens always; --min only raises it
	genesis  []byte // the fork's genesis.json: re-derived from the old chain when given
	unsealed bool   // tests only: a simulated chain has no QBFT seals
}

// checkpointCheck (VA-1): the trust root is the old chain, never the fork.
// Signers are checked as citizens and owner keys in the old chain's state at
// cp.FromBlock (whose hash, root and seals are checked first); the fork's
// block 0 must continue that block, hold the state root the citizens
// signed, and run on the fork chain id they signed.
func checkpointCheck(ctx context.Context, old, fork *rpc.Client, cp Checkpoint, sigs []CheckpointSig, o checkOpts) (map[string]any, error) {
	res := map[string]any{"from_block": cp.FromBlock, "validators": cp.Validators, "fork_chain_id": cp.ForkChainID}
	var why []string
	if err := checkOldBlock(ctx, old, cp, o.unsealed); err != nil {
		why = append(why, "old chain: "+err.Error())
	}
	res["old_block_matches"] = len(why) == 0
	fcl := ethclient.NewClient(fork)
	g, err := fcl.HeaderByNumber(ctx, big.NewInt(0))
	if err != nil {
		return nil, err
	}
	fid, ferr := fcl.ChainID(ctx)
	res["fork_genesis_parent_matches"] = g.ParentHash == common.HexToHash(cp.FromHash)
	res["fork_state_root_matches"] = cp.ForkRoot != "" && g.Root == common.HexToHash(cp.ForkRoot)
	res["fork_chain_id_matches"] = ferr == nil && cp.ForkChainID != 0 && cp.ForkChainID != cp.ChainID && fid.Int64() == cp.ForkChainID
	for _, k := range []string{"fork_genesis_parent_matches", "fork_state_root_matches", "fork_chain_id_matches"} {
		if res[k] != true {
			why = append(why, "fork: "+strings.TrimPrefix(k, "fork_")+" false")
		}
	}
	if o.genesis != nil {
		err := forkDerivationCheck(ctx, old, o.genesis, cp)
		res["genesis_derived"] = err == nil
		if err != nil {
			why = append(why, err.Error())
		}
	}
	at := &bind.CallOpts{Context: ctx, BlockNumber: new(big.Int).SetUint64(cp.FromBlock)}
	ocl := ethclient.NewClient(old)
	va, _ := loadArtifact("Validators")
	vc := bind.NewBoundContract(validatorsAddress, va.abi, ocl, ocl, ocl)
	one := func(c *bind.BoundContract, m string, args ...any) (any, error) {
		var out []any
		if err := c.Call(at, &out, m, args...); err != nil {
			return nil, err
		}
		return out[0], nil
	}
	ra, err := one(vc, "registry")
	if err != nil {
		return nil, fmt.Errorf("the old chain at block %d: %w", cp.FromBlock, err)
	}
	ca, err := one(vc, "citizens")
	if err != nil {
		return nil, err
	}
	rart, _ := loadArtifact("Registry")
	cart, _ := loadArtifact("Citizens")
	reg := bind.NewBoundContract(ra.(common.Address), rart.abi, ocl, ocl, ocl)
	cit := bind.NewBoundContract(ca.(common.Address), cart.abi, ocl, ocl, ocl)
	n, err := one(cit, "count")
	if err != nil {
		return nil, err
	}
	electorate := int(n.(uint32))
	digest := cp.digest()
	var good, bad []string
	seen := map[string]bool{}
	for _, s := range sigs {
		sig, err := hexutil.Decode(s.Signature)
		if err != nil || len(sig) != 65 {
			bad = append(bad, s.Name+": not a signature")
			continue
		}
		pub, err := crypto.SigToPub(digest, sig)
		if err != nil || !strings.EqualFold(crypto.PubkeyToAddress(*pub).Hex(), s.Address) {
			bad = append(bad, s.Name+": the signature isn't by "+s.Address)
			continue
		}
		var out []any
		if err := reg.Call(at, &out, "canAct", idOf(s.Name), common.HexToAddress(s.Address)); err != nil ||
			!out[0].(bool) || out[2].(bool) || common.Hash(out[1].([32]byte)) != idOf(s.Name) {
			bad = append(bad, s.Name+": not that name's owner key on the old chain")
			continue
		}
		if ok, err := one(cit, "isCitizen", idOf(s.Name)); err != nil || !ok.(bool) {
			bad = append(bad, s.Name+": not a citizen on the old chain")
			continue
		}
		if !seen[s.Name] {
			seen[s.Name] = true
			good = append(good, s.Name)
		}
	}
	sort.Strings(good)
	need := electorate/2 + 1
	if min := o.min; min > need {
		need = min
	}
	res["citizens"], res["signed_by"], res["rejected"], res["needed"], res["problems"] = electorate, good, bad, need, why
	follow := len(good) >= need && len(why) == 0
	res["follow"] = follow
	switch {
	case follow:
		res["verdict"] = fmt.Sprintf("%d of %d citizens (on the old chain at block %d) signed this fork as its continuation: follow it", len(good), electorate, cp.FromBlock)
	case len(why) > 0:
		res["verdict"] = "do not follow: the RPC's chain is not the fork the checkpoint names, or the checkpoint is not the old chain's (" + strings.Join(why, "; ") + ")"
	default:
		res["verdict"] = fmt.Sprintf("do not follow: %d of %d citizens signed, %d needed", len(good), electorate, need)
	}
	return res, nil
}

// followFork points a network file at the fork (keeping the old one) and
// restarts the given identities' scans at block 0 (the fork's numbering).
func followFork(netPath, rpcURL string, chainID int64, dirs []string) (map[string]any, error) {
	var f map[string]any
	if err := readJSON(netPath, &f); err != nil {
		return nil, err
	}
	b, _ := os.ReadFile(netPath)
	old := netPath + ".before-fork"
	if err := os.WriteFile(old, b, 0o644); err != nil {
		return nil, err
	}
	f["rpc"], f["block"], f["chain_id"] = rpcURL, 0, chainID
	nb, _ := json.MarshalIndent(f, "", "  ")
	if err := os.WriteFile(netPath, nb, 0o644); err != nil {
		return nil, err
	}
	if op := operatorPath(netPath); fileExists(op) {
		var o map[string]any
		if readJSON(op, &o) == nil {
			o["rpc"], o["block"], o["chain_id"] = rpcURL, 0, chainID
			ob, _ := json.MarshalIndent(o, "", "  ")
			os.WriteFile(op, ob, 0o600)
		}
	}
	var reset []string
	for _, d := range dirs {
		for _, fn := range []string{"state.json", "watch.json"} {
			p := filepath.Join(d, fn)
			var m map[string]any
			if readJSON(p, &m) != nil {
				continue
			}
			m["cursor"] = 0
			if w, ok := m["watch"].(map[string]any); ok {
				w["cursor"] = 0
			}
			if writeJSON(p, m) == nil {
				reset = append(reset, p)
			}
		}
	}
	return map[string]any{"net": netPath, "rpc": rpcURL, "chain_id": chainID, "old_net": old, "scans_reset": reset}, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// runForkCmd: fork-from-state and checkpoint sign|verify.
func runForkCmd(args []string) error {
	cmd, rest := args[0], args[1:]
	if cmd == "checkpoint" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New("ternchain checkpoint sign|verify --checkpoint FILE …")
		}
		cmd, rest = "checkpoint "+rest[0], rest[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	rpcURL := fs.String("rpc", "", "")
	oldRPC := fs.String("old-rpc", "", "")
	at := fs.Int64("at", -1, "")
	validators := fs.String("validators", "", "")
	outD := fs.String("out", "", "")
	genesis := fs.String("genesis", "", "")
	netPath := fs.String("net", "", "")
	why := fs.String("why", "", "")
	period := fs.Uint64("block-period", 1, "")
	timeout := fs.Uint64("request-timeout", 4, "")
	chainID := fs.Int64("chain-id", 0, "")
	cpPath := fs.String("checkpoint", "", "")
	dir := fs.String("dir", os.Getenv("TERNCHAIN_DIR"), "")
	min := fs.Int("min", 0, "")
	follow := fs.Bool("follow", false, "")
	dirs := fs.String("dirs", "", "")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	genesisOf := func() ([]byte, error) {
		p := *genesis
		if p == "" {
			p = filepath.Join(filepath.Dir(*cpPath), "genesis.json")
		}
		return os.ReadFile(p)
	}
	switch cmd {
	case "fork-from-state":
		r, err := forkFromState(forkOpts{rpc: *rpcURL, validators: *validators, out: *outD, oldGenesis: *genesis, net: *netPath, why: *why,
			at: *at, period: *period, timeout: *timeout, chainID: *chainID})
		if err != nil {
			return err
		}
		out(r)
	case "checkpoint sign":
		if *cpPath == "" || *rpcURL == "" {
			return errors.New("ternchain checkpoint sign --checkpoint FILE --dir IDENT --rpc OLD_RPC [--genesis FORK_GENESIS] (a node of the old chain: the fork is re-derived from it before you sign)")
		}
		gb, err := genesisOf()
		if err != nil {
			return fmt.Errorf("the fork's genesis.json (--genesis): %w", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		old, err := rpc.DialContext(ctx, *rpcURL)
		if err != nil {
			return err
		}
		defer old.Close()
		r, err := checkpointSign(ctx, *cpPath, *dir, old, gb, false)
		if err != nil {
			return err
		}
		out(r)
	case "checkpoint verify":
		if *cpPath == "" || *rpcURL == "" {
			return errors.New("ternchain checkpoint verify --checkpoint FILE --rpc FORK_RPC [--old-rpc OLD_RPC | --net NET] [--genesis FILE] [--min N] [--follow --net NET [--dirs a,b]]")
		}
		o := *oldRPC
		if o == "" && *netPath != "" {
			var f DevnetFile
			if err := readJSON(*netPath, &f); err == nil {
				o = f.RPC
			}
		}
		if o == "" {
			return errors.New("--old-rpc OLD_RPC, or --net NET.json (still on the old chain): signers are checked against the old chain, never the fork")
		}
		var cp Checkpoint
		if err := readJSON(*cpPath, &cp); err != nil {
			return err
		}
		r, err := checkpointVerify(*cpPath, *rpcURL, o, *genesis, *min)
		if err != nil {
			return err
		}
		if *follow {
			if r["follow"] != true {
				out(r)
				return errors.New("not following: " + r["verdict"].(string))
			}
			if *netPath == "" {
				return errors.New("--follow needs --net NET.json (the file clients read)")
			}
			var ds []string
			for _, d := range strings.Split(*dirs, ",") {
				if d = strings.TrimSpace(d); d != "" {
					ds = append(ds, d)
				}
			}
			f, err := followFork(*netPath, *rpcURL, cp.ForkChainID, ds)
			if err != nil {
				return err
			}
			r["followed"] = f
		}
		out(r)
	default:
		return unknownCommand(cmd)
	}
	return nil
}

// storageRoot: an account's storage trie root from its slots.
func storageRoot(st map[common.Hash]common.Hash) common.Hash {
	type kv struct{ k, v []byte }
	var kvs []kv
	for k, v := range st {
		if v == (common.Hash{}) {
			continue
		}
		enc, _ := rlp.EncodeToBytes(common.TrimLeftZeroes(v[:]))
		kvs = append(kvs, kv{crypto.Keccak256(k[:]), enc})
	}
	sort.Slice(kvs, func(i, j int) bool { return string(kvs[i].k) < string(kvs[j].k) })
	t := trie.NewStackTrie(nil)
	for _, x := range kvs {
		t.Update(x.k, x.v)
	}
	return t.Hash()
}

// dumpDiff: which accounts of a dump differ from the node's own proofs (eth_getProof).
func dumpDiff(ctx context.Context, rc *rpc.Client, h *types.Header, st map[common.Address]*dumpAccount) []string {
	var out []string
	for a, x := range st {
		var p struct {
			Balance     *hexutil.Big   `json:"balance"`
			Nonce       hexutil.Uint64 `json:"nonce"`
			CodeHash    common.Hash    `json:"codeHash"`
			StorageHash common.Hash    `json:"storageHash"`
		}
		if err := rc.CallContext(ctx, &p, "eth_getProof", a, []string{}, hexutil.EncodeBig(h.Number)); err != nil {
			return append(out, "eth_getProof: "+err.Error())
		}
		var why []string
		if p.Balance != nil && (*big.Int)(p.Balance).Cmp(x.Balance) != 0 {
			why = append(why, "balance")
		}
		if uint64(p.Nonce) != x.Nonce {
			why = append(why, "nonce")
		}
		if ch := crypto.Keccak256Hash(x.Code); len(x.Code) > 0 && ch != p.CodeHash {
			why = append(why, "code")
		}
		if r := storageRoot(x.Storage); r != p.StorageHash && !(len(x.Storage) == 0 && p.StorageHash == types.EmptyRootHash) {
			why = append(why, fmt.Sprintf("storage (%d slots)", len(x.Storage)))
		}
		if len(why) > 0 {
			out = append(out, a.Hex()+": "+strings.Join(why, ","))
		}
		if len(out) > 5 {
			break
		}
	}
	if len(out) == 0 {
		out = append(out, fmt.Sprintf("each of the %d accounts matches its proof: an account is missing from debug_accountRange", len(st)))
	}
	return out
}
