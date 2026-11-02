// A people's devnet: ternchain's own L1, whose blocks are made by validators
// run by different people (Hyperledger Besu, QBFT: a block is final once
// more than 2/3 of the validators have sealed it), and whose validator set is
// a contract (Validators.sol) that changes only through the network's two
// houses and the exit window.
//
//	ternchain validator init    --dir D                      a node key, its address and enode
//	ternchain validator genesis --validators a,b,c,d --out genesis.json [--deployer-key F]
//	ternchain validator run     --dir D --genesis G [--bootnodes E,…] [--p2p-host IP] …
//	ternchain validator stop    --dir D                      (a Docker-run node)
//	ternchain validator status  [--rpc URL]                  who validates, who sealed the last blocks
//	ternchain deploy --rpc URL --deployer-key F --net NET [--genesis founders] [--voting-period S --exit-window S]
//
// A test network run by its operator uses `validator genesis --selection
// header` instead: the set is in the genesis header and changes by QBFT
// validator votes (qbft_proposeValidatorVote), with no contract and no
// citizens; grow it from 1 to 4 in one sitting (help: ternchain-ops help validator).
//
// The genesis (validatorGenesis) puts Validators.sol into the alloc: QBFT
// reads the set from block 0, before any transaction could deploy it, so the
// contract is its runtime code plus storage computed here (layout in the
// contract's header; validators_test.go checks it against the constructor).
package main

//lint:file-ignore SA1019 raw curve coordinates: secp256k1 (go-ethereum) and the P-256 passkey verifier need them; crypto/ecdh has no secp256k1

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
)

// validatorsAddress: where the genesis puts Validators.sol (any address
// outside the precompiles would do; every node's genesis must agree).
var validatorsAddress = common.HexToAddress("0x0000000000000000000000000000000000007e40")

// besuImage: the Besu release `validator run` starts in Docker when no
// `besu` is installed. Pinned: every validator of a network runs the same
// release, and a new one comes by a scheduled upgrade (Validators.scheduleUpgrade).
const besuImage = "hyperledger/besu:26.9.0"

// qbftMixHash: QBFT's marker in the genesis mixHash ("ctical byzantine fault tolerance").
const qbftMixHash = "0x63746963616c2062797a616e74696e65206661756c7420746f6c6572616e6365"

// ---- the Validators contract in genesis

func validatorsRuntime() ([]byte, error) {
	b, err := artifacts.ReadFile("build/Validators_sol_Validators.runtime.bin")
	if err != nil {
		return nil, err
	}
	return common.FromHex(strings.TrimSpace(string(b))), nil
}

// valRules: Validators.Rules (the rolling cap, the removal delay, the
// minimum set, same-operator challenges).
type valRules struct {
	Window       uint64 `json:"window"`       // seconds the rolling cap counts changes over
	RemovalDelay uint64 `json:"removalDelay"` // seconds a scheduled removal waits (after the exit window)
	LapseGrace   uint64 `json:"lapseGrace"`   // seconds after the handover before a non-citizen's validator lapses
	MinSet       uint32 `json:"minSet"`       // the set never drops below this
	VoteWindow   uint64 `json:"voteWindow"`   // seconds a same-operator challenge is voted on
	QuorumBps    uint16 `json:"quorumBps"`    // a challenge's turnout, share of citizens
}

// launchRules: a month's window (⌊n/3⌋ changes a month); a removal waits two
// weeks after the exit window, so a cancel proposed the day the removal
// passes (7 d vote + 7 d exit) lands first; at least 4 validators; founders
// a first term to claim.
var launchRules = valRules{Window: 30 * 86400, RemovalDelay: 14 * 86400, LapseGrace: 90 * 86400, MinSet: 4, VoteWindow: 7 * 86400, QuorumBps: 2000}

// devnetRules: the same in minutes.
var devnetRules = valRules{Window: 600, RemovalDelay: 60, LapseGrace: 600, MinSet: 4, VoteWindow: 60, QuorumBps: 2000}

// Validators.sol's slots (its header): what genesis writes and fork-from-state rewrites.
const (
	valSlotList, valSlotPosition, valSlotGovernance, valSlotBootstrap, valSlotUpgrades = 0, 1, 2, 3, 4
	valSlotRules, valSlotRules2, valSlotCitizenOf, valSlotValidatorOf, valSlotChanges  = 5, 6, 7, 8, 9
	valSlotRemovalAt, valSlotRemovalWhy                                                = 10, 11
)

func slotWord(n int64) common.Hash { return common.BigToHash(big.NewInt(n)) }

// mapSlot: where mapping(key => …) at slot n keeps key's value.
func mapSlot(key common.Hash, n int64) common.Hash {
	return crypto.Keccak256Hash(key.Bytes(), slotWord(n).Bytes())
}

func addrKey(a common.Address) common.Hash { return common.BytesToHash(a.Bytes()) }

// arraySlot: element i of the dynamic array at slot n.
func arraySlot(n int64, i int) common.Hash {
	base := new(big.Int).SetBytes(crypto.Keccak256(slotWord(n).Bytes()))
	return common.BigToHash(base.Add(base, big.NewInt(int64(i))))
}

// validatorsStorage: the storage the constructor would leave, slot by slot
// (Solidity's layout for Validators.sol's state variables in order):
//
//	slot 0                         list.length = n
//	keccak256(slot 0) + i          list[i] (an address, right-aligned)
//	keccak256(pad32(a) ‖ pad32(1)) position[a] = i + 1  (mapping at slot 1)
//	slot 2                         governance = 0 (set once by the bootstrap after deploy)
//	slot 3                         bootstrap
//	slot 4                         upgrades.length = 0 (zero slots are left out)
//	slot 5, 6                      rules (packed)
//	keccak256(pad32(a) ‖ pad32(7)) citizenOf[a];  keccak256(c ‖ pad32(8)) validatorOf[c]
func validatorsStorage(bootstrap common.Address, vs []common.Address, names []common.Hash, r valRules) map[common.Hash]common.Hash {
	st := map[common.Hash]common.Hash{}
	st[slotWord(valSlotList)] = slotWord(int64(len(vs)))
	for i, v := range vs {
		st[arraySlot(valSlotList, i)] = addrKey(v)
		st[mapSlot(addrKey(v), valSlotPosition)] = slotWord(int64(i + 1))
		if i < len(names) && names[i] != (common.Hash{}) {
			st[mapSlot(addrKey(v), valSlotCitizenOf)] = names[i]
			st[mapSlot(names[i], valSlotValidatorOf)] = addrKey(v)
		}
	}
	if bootstrap != (common.Address{}) {
		st[slotWord(valSlotBootstrap)] = addrKey(bootstrap)
	}
	for k, v := range rulesStorage(r) {
		st[k] = v
	}
	return st
}

func rulesStorage(r valRules) map[common.Hash]common.Hash {
	w1 := new(big.Int).SetUint64(r.Window)
	w1.Or(w1, new(big.Int).Lsh(new(big.Int).SetUint64(r.RemovalDelay), 64))
	w1.Or(w1, new(big.Int).Lsh(new(big.Int).SetUint64(r.LapseGrace), 128))
	w1.Or(w1, new(big.Int).Lsh(new(big.Int).SetUint64(uint64(r.MinSet)), 192))
	w2 := new(big.Int).SetUint64(r.VoteWindow)
	w2.Or(w2, new(big.Int).Lsh(new(big.Int).SetUint64(uint64(r.QuorumBps)), 64))
	return map[common.Hash]common.Hash{slotWord(valSlotRules): common.BigToHash(w1), slotWord(valSlotRules2): common.BigToHash(w2)}
}

// qbftExtraData: RLP([vanity 32 bytes, validators, vote, round, seals]). With
// contract validator selection the list in the header is empty: Besu reads
// the set from the contract; with header voting it is the genesis set.
func qbftExtraData(vs []common.Address) []byte {
	type extra struct {
		Vanity     []byte
		Validators []common.Address
		Vote       []any
		Round      uint
		Seals      [][]byte
	}
	b, _ := rlp.EncodeToBytes(extra{Vanity: make([]byte, 32), Validators: vs, Vote: []any{}, Seals: [][]byte{}})
	return b
}

type genesisOpts struct {
	chainID    int64
	period     uint64 // seconds per block
	timeout    uint64 // seconds before a round change (a missing proposer)
	validators []common.Address
	names      []common.Hash // each validator's citizen (idOf(name); zero: unbound)
	rules      valRules
	deployer   common.Address // bootstrap of Validators (hands it to the timelock once); without alloc, gets the supply (a local demo)
	supply     *big.Int
	gasLimit   uint64
	alloc      []allocRow // the genesis table (genesis_plan.go): who holds the coins at block 0
	// headerVoting: QBFT's own validator votes (qbft_proposeValidatorVote) pick
	// the set; the validators go in extraData and no Validators contract is
	// in genesis. For operator-run test networks (audit 6 VB-1): no
	// Parliament, no citizens needed to grow the set.
	headerVoting bool
}

// validatorGenesis: a Besu QBFT genesis on the newest forks Besu runs (Osaka:
// P256VERIFY, EIP-7951), with the validator set read from the contract.
func validatorGenesis(o genesisOpts) (map[string]any, error) {
	if len(o.validators) == 0 {
		return nil, errors.New("--validators: at least one (4 or more to survive a validator going down)")
	}
	seen := map[common.Address]bool{}
	for _, v := range o.validators {
		if seen[v] {
			return nil, fmt.Errorf("validator %s listed twice", v.Hex())
		}
		seen[v] = true
	}
	named := map[common.Hash]bool{}
	for _, n := range o.names {
		if n != (common.Hash{}) && named[n] {
			return nil, errors.New("one validator per citizen: a name is listed twice")
		}
		named[n] = true
	}
	if o.rules.Window == 0 {
		o.rules = launchRules
	}
	cfg := map[string]any{"chainId": o.chainID}
	for _, f := range []string{"homesteadBlock", "eip150Block", "eip155Block", "eip158Block", "byzantiumBlock", "constantinopleBlock",
		"petersburgBlock", "istanbulBlock", "muirGlacierBlock", "berlinBlock", "londonBlock"} {
		cfg[f] = 0
	}
	for _, f := range []string{"shanghaiTime", "cancunTime", "pragueTime", "osakaTime"} {
		cfg[f] = 0
	}
	qbft := map[string]any{"blockperiodseconds": o.period, "epochlength": 30000, "requesttimeoutseconds": o.timeout}
	alloc := map[string]any{}
	extra := qbftExtraData(o.validators)
	if !o.headerVoting {
		code, err := validatorsRuntime()
		if err != nil {
			return nil, err
		}
		storage := map[string]string{}
		for k, v := range validatorsStorage(o.deployer, o.validators, o.names, o.rules) {
			storage[k.Hex()] = v.Hex()
		}
		qbft["validatorcontractaddress"] = strings.ToLower(validatorsAddress.Hex())
		alloc[strings.ToLower(validatorsAddress.Hex())] = map[string]any{"balance": "0x0", "code": "0x" + hex.EncodeToString(code), "storage": storage}
		extra = qbftExtraData(nil)
	}
	cfg["qbft"] = qbft
	switch {
	case len(o.alloc) > 0:
		for _, r := range o.alloc {
			alloc[strings.ToLower(r.Address)] = map[string]any{"balance": "0x" + r.wei.Text(16)}
		}
	case o.deployer != (common.Address{}):
		alloc[strings.ToLower(o.deployer.Hex())] = map[string]any{"balance": "0x" + o.supply.Text(16)}
	}
	return map[string]any{
		"config":        cfg,
		"nonce":         "0x0",
		"timestamp":     fmt.Sprintf("0x%x", time.Now().Unix()),
		"gasLimit":      fmt.Sprintf("0x%x", o.gasLimit),
		"difficulty":    "0x1",
		"mixHash":       qbftMixHash,
		"coinbase":      "0x0000000000000000000000000000000000000000",
		"baseFeePerGas": "0x3b9aca00", // 1 gwei at block 0; EIP-1559 moves it with load
		"extraData":     "0x" + hex.EncodeToString(extra),
		"alloc":         alloc,
	}, nil
}

// ---- a validator's folder

// ValidatorFile: D/validator.json. The node key (D/key, Besu's format) signs
// blocks and seals; its address is what governance adds to the set.
type ValidatorFile struct {
	Address   string `json:"address"`
	PublicKey string `json:"public_key"`      // the enode id
	Enode     string `json:"enode,omitempty"` // once run: enode://ID@HOST:PORT
	Container string `json:"container,omitempty"`
	RPC       string `json:"rpc,omitempty"`
}

func validatorInit(dir string) (ValidatorFile, error) {
	if dir == "" {
		return ValidatorFile{}, errors.New("--dir is required (the validator's folder: its node key and chain data)")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ValidatorFile{}, err
	}
	kp := filepath.Join(dir, "key")
	if _, err := os.Stat(kp); err == nil {
		return ValidatorFile{}, fmt.Errorf("%s exists: not overwriting a node key", kp)
	}
	k, err := crypto.GenerateKey()
	if err != nil {
		return ValidatorFile{}, err
	}
	if err := os.WriteFile(kp, []byte("0x"+keyHex(k)), 0o600); err != nil {
		return ValidatorFile{}, err
	}
	v := validatorFileOf(k)
	return v, writeJSON(filepath.Join(dir, "validator.json"), v)
}

func validatorFileOf(k *ecdsa.PrivateKey) ValidatorFile {
	pub := hex.EncodeToString(crypto.FromECDSAPub(&k.PublicKey)[1:])
	return ValidatorFile{Address: crypto.PubkeyToAddress(k.PublicKey).Hex(), PublicKey: pub}
}

func loadValidator(dir string) (ValidatorFile, error) {
	var v ValidatorFile
	if err := readJSON(filepath.Join(dir, "validator.json"), &v); err != nil {
		return v, fmt.Errorf("%s: no validator here (ternchain validator init --dir %s): %w", dir, dir, err)
	}
	return v, nil
}

// addrOrValidatorDir: 0xADDRESS, or a folder made by validator init.
func addrOrValidatorDir(s string) (common.Address, error) {
	s = strings.TrimSpace(s)
	if common.IsHexAddress(s) {
		return common.HexToAddress(s), nil
	}
	v, err := loadValidator(s)
	if err != nil {
		return common.Address{}, fmt.Errorf("%q: not an address, and %w", s, err)
	}
	return common.HexToAddress(v.Address), nil
}

// keyFileOrNew: a key file, made (0600) if it isn't there yet.
func keyFileOrNew(path string) (*ecdsa.PrivateKey, bool, error) {
	if _, err := os.Stat(path); err == nil {
		k, err := readKeyFile(path)
		return k, false, err
	}
	if _, err := keygen(path); err != nil {
		return nil, false, err
	}
	k, err := readKeyFile(path)
	return k, true, err
}

// ---- running Besu

type runOpts struct {
	dir, genesis, bootnodes, p2pHost, rpcHost, image, network, ip, name string
	p2pPort, rpcPort                                                    int
	docker, print                                                       bool
	archive                                                             bool     // FOREST storage: every past state (fork-from-state dumps it; a data node)
	fresh                                                               bool     // a new chain (fork-from-state's genesis): the old data moves aside, the container is made anew
	recreate                                                            bool     // the container made anew with these flags, on the same data
	heap                                                                string   // the JVM's heap (e.g. 512m): many nodes on one machine
	extra                                                               []string // more Besu flags (e.g. a censor's tx-pool permissioning, in sim/people/capture.sh)
	profile                                                             string   // "validator" (default) or "rpc": a public RPC node (rpcProfile)
	allowHosts                                                          string   // --rpc-allow-hosts: the rpc profile's host allowlist
	metrics                                                             bool     // Prometheus metrics on 127.0.0.1:9545
	restart                                                             string   // the Docker restart policy (no, unless-stopped, …)
}

// besuArgs: the flags every validator of a ternchain L1 runs with. HTTP:
// read-only APIs, no CORS (a web page can't read it), Host checked (no DNS
// rebinding); ADMIN and DEBUG only on the node's IPC socket (DATA/besu.ipc:
// fork-from-state's dump, admin_* by the operator).
func besuArgs(dataDir, genesis, key, p2pHost string, p2pPort, rpcPort int, bootnodes string, archive bool) []string {
	hosts := "localhost,127.0.0.1"
	if p2pHost != "" && p2pHost != "127.0.0.1" && p2pHost != "localhost" {
		hosts += "," + p2pHost
	}
	a := []string{
		"--data-path=" + dataDir, "--genesis-file=" + genesis, "--node-private-key-file=" + key,
		"--p2p-host=" + p2pHost, fmt.Sprintf("--p2p-port=%d", p2pPort),
		"--rpc-http-enabled", "--rpc-http-host=0.0.0.0", fmt.Sprintf("--rpc-http-port=%d", rpcPort),
		"--rpc-http-api=ETH,NET,WEB3,QBFT,TXPOOL", "--host-allowlist=" + hosts,
		"--Xrpc-ipc-enabled", "--Xrpc-ipc-path=" + dataDir + "/besu.ipc", "--Xrpc-ipc-apis=ETH,NET,WEB3,QBFT,TXPOOL,ADMIN,DEBUG",
		"--rpc-ws-enabled", "--rpc-ws-host=0.0.0.0", fmt.Sprintf("--rpc-ws-port=%d", rpcPort+1), "--rpc-ws-api=ETH,NET,WEB3",
		"--sync-mode=FULL", "--min-gas-price=0", "--revert-reason-enabled", "--logging=INFO",
	}
	if bootnodes != "" {
		a = append(a, "--bootnodes="+bootnodes)
	}
	if archive {
		a = append(a, "--data-storage-format=FOREST")
	}
	return a
}

// metricsPort: Besu's Prometheus endpoint with --metrics (ops/monitoring).
const metricsPort = 9545

// nodeProfile: besuArgs for the node's role. "rpc": a public RPC node (behind
// a TLS proxy, ops/besu/rpc-node.md): only ETH, NET and WEB3, no CORS, the
// hosts it is reached by, bounded connections and batches. A validator's RPC
// (ADMIN, DEBUG, TXPOOL, QBFT; any host) must never face the internet.
// --metrics: Prometheus metrics on metricsHost:9545.
func nodeProfile(a []string, o runOpts, metricsHost string) []string {
	var out []string
	for _, x := range a {
		if o.profile == "rpc" {
			switch {
			case strings.HasPrefix(x, "--rpc-http-api="):
				x = "--rpc-http-api=ETH,NET,WEB3"
			case strings.HasPrefix(x, "--rpc-http-cors-origins="), strings.HasPrefix(x, "--Xrpc-ipc-"):
				continue // a public RPC node gets no IPC admin socket either (VA-6)
			case strings.HasPrefix(x, "--host-allowlist="):
				hosts := o.allowHosts
				if hosts == "" {
					hosts = "localhost,127.0.0.1"
				}
				x = "--host-allowlist=" + hosts
			}
		}
		out = append(out, x)
	}
	if o.profile == "rpc" {
		out = append(out, "--rpc-http-max-active-connections=200", "--rpc-http-max-batch-size=20", "--rpc-ws-max-active-connections=200")
	}
	if o.metrics {
		out = append(out, "--metrics-enabled", "--metrics-host="+metricsHost, fmt.Sprintf("--metrics-port=%d", metricsPort))
	}
	return out
}

func validatorRun(o runOpts) (map[string]any, error) {
	v, err := loadValidator(o.dir)
	if err != nil {
		return nil, err
	}
	if o.genesis == "" {
		return nil, errors.New("--genesis FILE is required (the network's genesis.json: every validator runs the same one)")
	}
	dir, _ := filepath.Abs(o.dir)
	gb, err := os.ReadFile(o.genesis)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "genesis.json"), gb, 0o644); err != nil { // the node's own copy
		return nil, err
	}
	if o.profile != "" && o.profile != "validator" && o.profile != "rpc" {
		return nil, fmt.Errorf("--profile %q: validator (the default) or rpc (a public RPC node: no ADMIN, DEBUG, TXPOOL or QBFT)", o.profile)
	}
	if o.restart == "" {
		o.restart = "no"
	}
	besu, _ := exec.LookPath("besu")
	useDocker := o.docker || besu == ""
	if o.name == "" {
		o.name = "tern-" + strings.ToLower(v.Address[2:10])
	}
	res := map[string]any{"address": v.Address}
	if o.recreate && !o.fresh && !o.print && useDocker {
		exec.Command("docker", "rm", "-f", o.name).Run()
	}
	if o.fresh && !o.print { // a new genesis: Besu refuses a data folder made for another one
		if useDocker {
			exec.Command("docker", "rm", "-f", o.name).Run()
		}
		if _, err := os.Stat(filepath.Join(dir, "besu")); err == nil {
			aside := filepath.Join(dir, fmt.Sprintf("besu.before-%d", time.Now().Unix()))
			if err := os.Rename(filepath.Join(dir, "besu"), aside); err != nil {
				return nil, err
			}
			res["old_data"] = aside
		}
	}
	var argv []string
	if useDocker {
		host := o.p2pHost
		if host == "" {
			host = o.ip
		}
		if host == "" {
			host = "127.0.0.1"
		}
		v.Enode = fmt.Sprintf("enode://%s@%s:%d", v.PublicKey, host, 30303)
		if o.ip == "" && o.p2pPort != 0 {
			v.Enode = fmt.Sprintf("enode://%s@%s:%d", v.PublicKey, host, o.p2pPort)
		}
		argv = []string{"docker", "run", "-d", "--name", o.name, "--restart", o.restart, "-v", dir + ":/data"}
		if o.heap != "" {
			argv = append(argv, "-e", "BESU_OPTS=-Xmx"+o.heap)
		}
		if o.network != "" {
			argv = append(argv, "--network", o.network)
			if o.ip != "" {
				argv = append(argv, "--ip", o.ip)
			}
		}
		if o.ip == "" { // reachable from other machines: publish the p2p port
			argv = append(argv, "-p", fmt.Sprintf("%d:30303", o.p2pPort), "-p", fmt.Sprintf("%d:30303/udp", o.p2pPort))
		}
		argv = append(argv, "-p", fmt.Sprintf("%s:%d:8545", o.rpcHost, o.rpcPort), "-p", fmt.Sprintf("%s:%d:8546", o.rpcHost, o.rpcPort+1))
		if o.metrics { // Prometheus scrapes it from this machine only
			argv = append(argv, "-p", fmt.Sprintf("127.0.0.1:%d:%d", metricsPort, metricsPort))
		}
		argv = append(argv, o.image)
		argv = append(argv, nodeProfile(besuArgs("/data/besu", "/data/genesis.json", "/data/key", host, 30303, 8545, o.bootnodes, o.archive), o, "0.0.0.0")...)
		argv = append(argv, o.extra...)
		v.Container = o.name
	} else {
		host := o.p2pHost
		if host == "" {
			host = "127.0.0.1"
		}
		v.Enode = fmt.Sprintf("enode://%s@%s:%d", v.PublicKey, host, o.p2pPort)
		argv = append([]string{besu}, nodeProfile(besuArgs(filepath.Join(dir, "besu"), filepath.Join(dir, "genesis.json"), filepath.Join(dir, "key"),
			host, o.p2pPort, o.rpcPort, o.bootnodes, o.archive), o, "127.0.0.1")...)
		argv = append(argv, o.extra...)
		if o.rpcHost != "0.0.0.0" {
			for i, a := range argv {
				if a == "--rpc-http-host=0.0.0.0" {
					argv[i] = "--rpc-http-host=" + o.rpcHost
				}
				if a == "--rpc-ws-host=0.0.0.0" {
					argv[i] = "--rpc-ws-host=" + o.rpcHost
				}
			}
		}
	}
	v.RPC = fmt.Sprintf("http://%s:%d", strings.Replace(o.rpcHost, "0.0.0.0", "127.0.0.1", 1), o.rpcPort)
	res["enode"], res["rpc"], res["command"] = v.Enode, v.RPC, shellJoin(argv)
	if o.print {
		return res, nil
	}
	if err := writeJSON(filepath.Join(dir, "validator.json"), v); err != nil {
		return nil, err
	}
	if useDocker {
		// A container of that name already: start it again (a restart keeps the chain data in D/besu).
		if exec.Command("docker", "inspect", o.name).Run() == nil {
			if b, err := exec.Command("docker", "start", o.name).CombinedOutput(); err != nil {
				return nil, fmt.Errorf("docker start %s: %v: %s", o.name, err, b)
			}
			res["container"], res["restarted"] = o.name, true
			return res, nil
		}
		if b, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
			exec.Command("docker", "rm", "-f", o.name).Run() // a container docker made but couldn't start
			return nil, fmt.Errorf("%s: %v: %s", shellJoin(argv[:5]), err, b)
		}
		res["container"] = o.name
		res["logs"] = "docker logs -f " + o.name
		return res, nil
	}
	fmt.Fprintln(os.Stderr, "ternchain validator: running", shellJoin(argv))
	return nil, syscall.Exec(argv[0], argv, os.Environ()) // the node in the foreground (systemd, a terminal)
}

func validatorStop(dir, name string) (map[string]any, error) {
	if name == "" {
		v, err := loadValidator(dir)
		if err != nil {
			return nil, err
		}
		if v.Container == "" {
			return nil, errors.New("this validator runs outside Docker: stop its besu process (Ctrl-C, or systemctl stop)")
		}
		name = v.Container
	}
	if b, err := exec.Command("docker", "stop", "-t", "10", name).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker stop %s: %v: %s", name, err, b)
	}
	return map[string]any{"stopped": name, "restart": "ternchain validator run with the same flags (the chain data stays)"}, nil
}

func shellJoin(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		if strings.ContainsAny(a, " \t'\"*$") {
			b.WriteString("'" + strings.ReplaceAll(a, "'", `'\''`) + "'")
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}

// ---- status: who validates, who sealed the last blocks

// qbftSigners: who proposed a block (its coinbase) and who sealed it (the
// commit seals in extraData: each signs keccak(rlp(header)) where the
// header's extraData keeps the round and has an empty list for the seals;
// checked against Besu 26.9).
func qbftSigners(h *types.Header) (proposer common.Address, sealers []common.Address, round uint64, err error) {
	var items []rlp.RawValue
	if err = rlp.DecodeBytes(h.Extra, &items); err != nil || len(items) < 5 {
		return h.Coinbase, nil, 0, fmt.Errorf("not QBFT extraData: %v", err)
	}
	_ = rlp.DecodeBytes(items[3], &round)
	var seals [][]byte
	if err = rlp.DecodeBytes(items[4], &seals); err != nil {
		return h.Coinbase, nil, round, err
	}
	noSeals, _ := rlp.EncodeToBytes(append(append([]rlp.RawValue{}, items[:4]...), rlp.RawValue{0xc0})) // seals: an empty list
	hh := types.CopyHeader(h)
	hh.Extra = noSeals
	digest := hh.Hash()
	for _, s := range seals {
		if len(s) != 65 {
			continue
		}
		sig := append([]byte{}, s...)
		if sig[64] >= 27 {
			sig[64] -= 27
		}
		pub, e := crypto.SigToPub(digest.Bytes(), sig)
		if e != nil {
			continue
		}
		sealers = append(sealers, crypto.PubkeyToAddress(*pub))
	}
	return h.Coinbase, sealers, round, nil
}

// statusNames: names to show for citizen ids the Registry's log doesn't
// have (a fork keeps state, not logs): --names a,b,…
var statusNames []string

func validatorStatus(rpcURL string, blocks int) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rc, err := rpc.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	cl := ethclient.NewClient(rc)
	head, err := cl.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"rpc": rpcURL, "head": head.Number.Uint64(), "head_age_s": time.Now().Unix() - int64(head.Time)}
	var qv []common.Address
	if err := rc.CallContext(ctx, &qv, "qbft_getValidatorsByBlockNumber", "latest"); err == nil {
		res["validators_qbft"] = qv
	}
	a, _ := loadArtifact("Validators")
	call := func(method string, args ...any) ([]any, error) {
		data, err := a.abi.Pack(method, args...)
		if err != nil {
			return nil, err
		}
		out, err := cl.CallContract(ctx, ethereum.CallMsg{To: &validatorsAddress, Data: data}, nil)
		if err != nil {
			return nil, err
		}
		return a.abi.Unpack(method, out)
	}
	var set []common.Address
	if out, err := call("getValidators"); err == nil {
		set = out[0].([]common.Address)
		res["validators_contract"] = set
		if g, err := call("governance"); err == nil {
			res["governance"] = g[0].(common.Address).Hex()
		}
		for k, v := range validatorSafety(ctx, cl, call, set) {
			res[k] = v
		}
		if n, err := call("upgradeCount"); err == nil {
			var ups []map[string]any
			for i := int64(0); i < n[0].(*big.Int).Int64(); i++ {
				if u, err := call("upgradeAt", big.NewInt(i)); err == nil {
					up := abiConvert[struct {
						AtBlock uint64
						Release [32]byte
						Uri     string
					}](u[0])
					ups = append(ups, map[string]any{"at_block": up.AtBlock, "release": common.Hash(up.Release).Hex(), "uri": up.Uri,
						"in_blocks": int64(up.AtBlock) - head.Number.Int64()})
				}
			}
			res["upgrades"] = ups
		}
	} else {
		res["validators_contract"] = "no Validators contract at " + validatorsAddress.Hex() + ": " + err.Error()
		var hs []common.Address // header voting (--selection header): the set QBFT keeps
		if rc.CallContext(ctx, &hs, "qbft_getValidatorsByBlockNumber", "latest") == nil && len(hs) > 0 {
			set = hs
			res["validators"] = hs
			res["selection"] = "header: QBFT validator votes (qbft_proposeValidatorVote); grow 1 -> 4 in one sitting, never stay at 2"
		}
	}
	res["p256verify"] = p256Probe(ctx, cl)
	var peers string
	if rc.CallContext(ctx, &peers, "net_peerCount") == nil {
		n, _ := new(big.Int).SetString(strings.TrimPrefix(peers, "0x"), 16)
		res["peers"] = n
	}
	// The last blocks: interval, rounds (>0: a proposer was missing), who proposed and sealed.
	type tally struct {
		Proposed, Sealed int
		LastSealed       uint64
	}
	t := map[common.Address]*tally{}
	for _, v := range set {
		t[v] = &tally{}
	}
	from := head.Number.Int64() - int64(blocks) + 1
	if from < 1 {
		from = 1
	}
	var times []uint64
	var recent []map[string]any
	roundChanges, minSeals := 0, -1
	for n := from; n <= head.Number.Int64(); n++ {
		h, err := cl.HeaderByNumber(ctx, big.NewInt(n))
		if err != nil {
			return nil, err
		}
		times = append(times, h.Time)
		p, sealers, round, err := qbftSigners(h)
		if err != nil {
			continue
		}
		if round > 0 {
			roundChanges++
		}
		if minSeals < 0 || len(sealers) < minSeals {
			minSeals = len(sealers)
		}
		if t[p] == nil {
			t[p] = &tally{}
		}
		t[p].Proposed++
		for _, s := range sealers {
			if t[s] == nil {
				t[s] = &tally{}
			}
			t[s].Sealed++
			t[s].LastSealed = uint64(n)
		}
		if n > head.Number.Int64()-5 {
			ss := make([]string, len(sealers))
			for i, s := range sealers {
				ss[i] = s.Hex()[:10]
			}
			sort.Strings(ss)
			recent = append(recent, map[string]any{"block": n, "time": h.Time, "proposer": p.Hex()[:10], "round": round, "sealed_by": ss})
		}
	}
	if len(times) > 1 {
		res["blocks_looked_at"] = len(times)
		res["avg_block_s"] = float64(times[len(times)-1]-times[0]) / float64(len(times)-1)
		maxGap := uint64(0)
		for i := 1; i < len(times); i++ {
			if g := times[i] - times[i-1]; g > maxGap {
				maxGap = g
			}
		}
		res["max_gap_s"], res["round_changes"], res["min_seals"] = maxGap, roundChanges, minSeals
	}
	per := map[string]any{}
	for a, x := range t {
		in := false
		for _, v := range set {
			in = in || v == a
		}
		per[a.Hex()] = map[string]any{"in_set": in, "proposed": x.Proposed, "sealed": x.Sealed, "last_sealed": x.LastSealed}
	}
	res["by_validator"], res["recent"] = per, recent
	if n := len(set); n > 0 {
		f := (n - 1) / 3
		res["fault_tolerance"] = fmt.Sprintf("%d validators: blocks need %d seals (more than 2/3); the chain survives %d down, halts with %d", n, n-f, f, f+1)
	}
	return res, nil
}

// p256Probe: is P256VERIFY (EIP-7951 / RIP-7212, address 0x100) there, and
// what does one passkey signature check cost? Without it a passkey account
// verifies P-256 in Solidity (OpenZeppelin's fallback), some 300k gas more.
func p256Probe(ctx context.Context, cl *ethclient.Client) map[string]any {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	digest := crypto.Keccak256([]byte("ternchain passkey probe"))
	r, s, _ := ecdsa.Sign(rand.Reader, k, digest)
	in := append(append(append(append(append([]byte{}, digest...), common.LeftPadBytes(r.Bytes(), 32)...), common.LeftPadBytes(s.Bytes(), 32)...),
		common.LeftPadBytes(k.PublicKey.X.Bytes(), 32)...), common.LeftPadBytes(k.PublicKey.Y.Bytes(), 32)...)
	to := common.BytesToAddress([]byte{0x01, 0x00})
	out, err := cl.CallContract(ctx, ethereum.CallMsg{To: &to, Data: in}, nil)
	ok := err == nil && len(out) == 32 && out[31] == 1
	res := map[string]any{"address": "0x100", "available": ok}
	if gas, err := cl.EstimateGas(ctx, ethereum.CallMsg{To: &to, Data: in}); err == nil {
		res["tx_gas"] = gas // 21000 + calldata + the precompile's 6900 (EIP-7951)
	}
	return res
}

// ---- deploy on an RPC: the ternchain contracts on a chain someone else runs

// deployOpts: ternchain deploy's flags.
type deployOpts struct {
	rpc, keyPath, netPath, founders, name, faucetKey string
	votingPeriod, exitWindow                         uint64
	short                                            bool
	rpcs, bootnodes                                  []string // net.json: more RPCs (the client fails over in order) and the bootnodes
	keepValidators                                   bool     // don't hand the Validators contract to Parliament (VB-1)
	testNetwork                                      bool     // net.json "test_network": operator zk keys allowed, and the notice says so
	faucetURL, relayURL, operator, contact, notice   string   // net.json: faucet_url, relay, operator, contact, notice
	shared                                           bool     // net.json mode "shared" (and the default refusals)
	admitBps                                         uint16   // Citizens / Parliament rules fixed at deploy (0: the launch values)
	admitFloor, criticalX, minTurnout                uint32
	superNum, superDen                               uint8
	// On a chain someone else runs (--host base-sepolia): its EntryPoint, the
	// network's bundler and the RPC's log range go into net.json; fees and
	// stakes in that chain's coin (a test ETH that costs a faucet trip, not ours to print).
	host, entryPoint, bundler, explorer string
	logRange                            uint64
	fee, daStake, daBond                string // coins ("": the defaults)
	settle                              string // net.json "settle": usdc (Circle's, on Base Sepolia) or a token address
	sponsorPerName, sponsorPerDay       string // a SponsorPaymaster's daily gas caps, in the chain's coin ("": none deployed)
	sponsorDeposit                      string
	erc8004                             string     // official (the chain's registries) | deploy
	gasPaymaster                        string     // circle | test | 0xADDR: users pay their gas in USDC through it (gas.go)
	profile                             string     // public: the public launch's contract set (netmodules.go)
	caps                                *ValueCaps // net.json caps (mainnet.go): required on a host with real money
	soloZK                              bool       // the deployer's own zk keys on a real-money host (public set only)
}

func runDeployCmd(o deployOpts) error {
	if o.rpc == "" || o.keyPath == "" {
		return errors.New("usage: ternchain deploy --rpc URL --deployer-key FILE --net NET.json [--genesis name=0x…,…] [--voting-period S --exit-window S] [--launch] [--faucet-key F] [--rpcs URL,…] [--bootnodes ENODE,…]")
	}
	key, err := readKeyFile(o.keyPath)
	if err != nil {
		return err
	}
	gen, err := parseGenesisStrict(o.founders)
	if err != nil {
		return err
	}
	ctx := context.Background()
	cl, err := ethclient.DialContext(ctx, o.rpc)
	if err != nil {
		return err
	}
	defer cl.Close()
	deployer := crypto.PubkeyToAddress(key.PublicKey)
	// The genesis table put the treasury's coins where the Treasury lands from
	// a fresh deployer (genesis_plan.go): anything sent from it before would
	// move the contract elsewhere and leave those coins at an address nobody controls.
	want := treasuryAddress(deployer)
	held, _ := cl.BalanceAt(ctx, want, nil)
	if o.host != "" { // a chain someone else runs has no genesis table of ours: whatever is at that address isn't the treasury's
		held = nil
	}
	nonce, err := cl.NonceAt(ctx, deployer, nil)
	if err != nil {
		return err
	}
	if held != nil && held.Sign() > 0 && nonce != 0 && !fileExists(strings.TrimSuffix(o.netPath, ".json")+".deploy.json") {
		return fmt.Errorf("the genesis gave the treasury %s coins at %s, where the Treasury lands only if the deploy is this deployer's first transactions; "+
			"it has sent %d already: deploy can't put the Treasury there (make the genesis again with a fresh --deployer-key)", fmtCoins(held), want.Hex(), nonce)
	}
	p := Params{EpochLength: 86400, RecoveryDelay: 72 * 3600, Gov: GovParams{Genesis: gen, Short: o.short, AdmitBps: o.admitBps, AdmitFloor: o.admitFloor,
		CriticalDelayX: o.criticalX, MinTurnout: o.minTurnout, SuperNum: o.superNum, SuperDen: o.superDen},
		VotingPeriod: o.votingPeriod, ExitWindow: o.exitWindow, KeepValidators: o.keepValidators, TestNetwork: o.testNetwork}
	for _, c := range []struct {
		s   string
		dst **big.Int
		f   string
	}{{o.fee, &p.Fee, "--fee"}, {o.daStake, &p.DAStake, "--da-stake"}, {o.daBond, &p.DABond, "--da-bond"}} {
		if c.s != "" {
			v, err := parseCoins(c.s)
			if err != nil {
				return fmt.Errorf("%s: %w", c.f, err)
			}
			*c.dst = v
		}
	}
	if o.sponsorPerName != "" || o.sponsorPerDay != "" {
		sp := &SponsorParams{}
		for _, c := range []struct {
			s   string
			dst **big.Int
			f   string
		}{{o.sponsorPerName, &sp.PerName, "--sponsor-per-name"}, {o.sponsorPerDay, &sp.PerDay, "--sponsor-per-day"}, {firstNonEmpty(o.sponsorDeposit, "0"), &sp.Deposit, "--sponsor-deposit"}} {
			if c.s == "" {
				return fmt.Errorf("%s: needed with the other --sponsor- flags", c.f)
			}
			v, err := parseCoins(c.s)
			if err != nil {
				return fmt.Errorf("%s: %w", c.f, err)
			}
			*c.dst = v
		}
		p.Sponsor = sp
	}
	p.ERC8004, p.GasPaymaster, p.Profile = o.erc8004, o.gasPaymaster, o.profile
	var settle *Settlement
	if o.settle != "" {
		if settle, err = settlementFor(cl, o.settle); err != nil {
			return err
		}
		p.JobsToken = common.HexToAddress(settle.Token)
	}
	switch o.entryPoint {
	case "":
	case "canonical":
		p.EntryPoint = common.HexToAddress(canonicalEntryPoint)
	default:
		if !common.IsHexAddress(o.entryPoint) {
			return fmt.Errorf("--entry-point %q: an address, or canonical (%s)", o.entryPoint, canonicalEntryPoint)
		}
		p.EntryPoint = common.HexToAddress(o.entryPoint)
	}
	// An interrupted deploy (a public RPC that dropped, gas that ran out) goes
	// on from where it stopped: NET.deploy.json keeps what it did.
	progPath := strings.TrimSuffix(o.netPath, ".json") + ".deploy.json"
	var prev DeployProgress
	if readJSON(progPath, &prev) == nil && len(prev.Contracts) > 0 {
		p.Resume = &prev
	}
	p.Progress = func(d DeployProgress) {
		if b, err := json.MarshalIndent(d, "", "  "); err == nil {
			os.MkdirAll(filepath.Dir(progPath), 0o755)
			os.WriteFile(progPath, b, 0o644)
		}
	}
	selection := "header (QBFT validator votes: no Validators contract)"
	if code, err := cl.CodeAt(ctx, validatorsAddress, nil); err == nil && len(code) > 0 {
		p.Validators = validatorsAddress
		selection = "contract (Validators, handed to Parliament)"
		if o.keepValidators {
			selection = "contract (Validators, kept by the deployer: --keep-validators)"
		}
	}
	if o.testNetwork {
		if id, err := cl.ChainID(ctx); err == nil && id.Uint64() == l1MainnetChainID {
			return fmt.Errorf("--test-network: chain %d is the mainnet", l1MainnetChainID)
		}
	}
	if o.host != "" {
		selection = "none: the host chain's (" + o.host + ")"
	}
	// A known host is the chain it names (an RPC of another chain is refused);
	// one with real money gets the guard rails a launch without an owner can have.
	realMoney := false
	if hp, ok := hostProfiles[o.host]; ok {
		id, err := cl.ChainID(ctx)
		if err != nil {
			return err
		}
		if id.Int64() != hp.ChainID {
			return fmt.Errorf("--host %s is chain %d, but %s serves chain %s", o.host, hp.ChainID, o.rpc, id)
		}
		realMoney = hp.RealMoney
	} else if id, err := cl.ChainID(ctx); err == nil && realMoneyChain(id.Int64()) {
		return fmt.Errorf("--host %q: this RPC serves a mainnet; name it (--host base)", o.host)
	}
	if realMoney {
		if o.testNetwork {
			return fmt.Errorf("--test-network: %s is a mainnet, with real money", o.host)
		}
		if o.profile != "public" {
			return fmt.Errorf("--host %s (real money): only --contract-set public (no shielded pool, stealth, pots or fiat)", o.host)
		}
		if len(gen) > 0 {
			return fmt.Errorf("--host %s (real money): no founders (--genesis): the public set is nobody's", o.host)
		}
		if p.Sponsor != nil {
			return fmt.Errorf("--host %s (real money): no sponsor: each user pays their own gas", o.host)
		}
		if o.caps == nil {
			return fmt.Errorf("--host %s (real money): --cap-per-payment and --cap-per-name are needed (unaudited contracts: clients refuse more at once)", o.host)
		}
		dec := uint8(6)
		if settle != nil {
			dec = settle.Decimals
		}
		if err := validCaps(o.caps, dec); err != nil {
			return err
		}
	} else if o.caps != nil {
		dec := uint8(18)
		if settle != nil {
			dec = settle.Decimals
		}
		if err := validCaps(o.caps, dec); err != nil {
			return err
		}
	}
	if o.soloZK {
		if !realMoney || o.profile != "public" {
			return errors.New("--solo-zk: for the public set on a mainnet host (--host base --contract-set public); a test network uses --test-network, the L1 a multi-party ceremony")
		}
		p.SoloZK = true
	}
	t0 := time.Now()
	before, _ := cl.BalanceAt(ctx, deployer, nil)
	ch, err := deploy(cl, key, p, o.rpc, nil)
	if err != nil {
		return err
	}
	if held != nil && held.Sign() > 0 && !strings.EqualFold(ch.dep.Contracts["Treasury"], want.Hex()) {
		return fmt.Errorf("the Treasury landed at %s, not at %s where the genesis put %s coins: stop, those coins are at an address nobody controls (report it)",
			ch.dep.Contracts["Treasury"], want.Hex(), fmtCoins(held))
	}
	hash, _ := contractsHash()
	f := DevnetFile{Deployment: ch.dep, Network: o.name, ContractsHash: hash, RPCs: o.rpcs, Bootnodes: o.bootnodes,
		Notice:    "a ternchain L1 run by its validators (QBFT); the deployer had a gas allowance at genesis and holds no other power; the treasury is spent only by both houses",
		FaucetURL: o.faucetURL, Relay: o.relayURL, Operator: o.operator, Contact: o.contact, TestNetwork: o.testNetwork}
	if o.testNetwork {
		f.Notice = testNetworkNotice(o.name, o.operator)
	}
	f.Settle, f.Profile = settle, o.profile
	if o.gasPaymaster != "" { // users pay their own gas, in the settlement token
		pm := o.gasPaymaster
		switch pm {
		case "circle":
			hp, ok := hostByChain(ch.chainID.Int64())
			if !ok {
				return fmt.Errorf("--gas-paymaster circle: Circle Paymaster is known on Base and Base Sepolia only; give its address")
			}
			pm = hp.CirclePaymaster
		case "test":
			pm = ch.dep.Contracts["TestTokenPaymaster"]
		}
		if code, err := cl.CodeAt(ctx, common.HexToAddress(pm), nil); err != nil || len(code) == 0 {
			return fmt.Errorf("--gas-paymaster %s: no paymaster at %s on this chain", o.gasPaymaster, pm)
		}
		token := ch.dep.Contracts["TestUSDC"]
		if settle != nil {
			token = settle.Token
		}
		f.Gas = &GasConfig{Pays: "user", Paymaster: common.HexToAddress(pm).Hex(), Token: token,
			Note: "each user pays their own gas, in USDC from their account (an ERC-20 paymaster); the network pays nobody's"}
	}
	if _, ok := ch.dep.Contracts["SponsorPaymaster"]; ok {
		f.ExtContractsHash, _ = extContractsHash()
	}
	if o.host != "" {
		f.Host, f.Bundler, f.LogRange, f.Explorer = o.host, o.bundler, o.logRange, o.explorer
		f.Notice = hostNetworkNotice(o.name, o.host, o.operator)
		if o.profile == "public" {
			f.Notice = publicNetworkNotice(o.name, o.host)
		}
	}
	if o.caps != nil {
		f.Caps = o.caps
	}
	if o.soloZK { // net.json says so when the keys really are one party's
		if dir := os.Getenv("TERNCHAIN_ZK_KEYS"); dir != "" {
			if p1, p2, err := ceremonyParties(dir); err != nil || p1 < minCeremonyParties || p2 < minCeremonyParties {
				f.ZKSolo = true
			}
		}
	}
	if realMoney {
		sym := "USDC"
		if settle != nil {
			sym = settle.Symbol
		}
		f.Notice = mainnetNetworkNotice(o.name, o.host, o.caps, sym, f.ZKSolo)
		if o.notice != "" { // never instead of the warning
			f.Notice += " " + o.notice
		}
	} else if o.notice != "" {
		f.Notice = o.notice
	}
	if o.shared {
		f.Mode, f.Refuse = "shared", defaultSharedRefusals
	}
	if g, err := genesisHashAt(ctx, cl); err == nil {
		f.GenesisHash = g
	}
	op := f
	op.Faucet = keyHex(key) // owner init on this machine funds new names from this key's coins
	if o.faucetKey != "" {
		fk, err := readKeyFile(o.faucetKey)
		if err != nil {
			return fmt.Errorf("--faucet-key: %w", err)
		}
		op.Faucet = keyHex(fk)
	}
	if err := os.MkdirAll(filepath.Dir(o.netPath), 0o755); err != nil {
		return err
	}
	pb, _ := json.MarshalIndent(f, "", "  ")
	ob, _ := json.MarshalIndent(op, "", "  ")
	if err := os.WriteFile(o.netPath, pb, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(operatorPath(o.netPath), ob, 0o600); err != nil {
		return err
	}
	os.Remove(progPath) // finished: a next deploy with this --net starts afresh
	r := map[string]any{"net": o.netPath, "operator_file": operatorPath(o.netPath), "chain_id": ch.dep.ChainID, "contracts": len(ch.dep.Contracts),
		"validator_selection": selection, "seconds": int(time.Since(t0).Seconds()), "deployer": deployer.Hex(),
		"treasury": ch.dep.Contracts["Treasury"], "genesis_hash": f.GenesisHash, "test_network": o.testNetwork, "notice": f.Notice}
	if p.Validators != (common.Address{}) {
		r["validators_contract"] = p.Validators.Hex()
	}
	if o.keepValidators && p.Validators != (common.Address{}) {
		r["deployer_powers"] = "the deployer is still the Validators bootstrap (--keep-validators): keep its key; hand the set over later with Validators.setGovernance once there are >= 4 citizens"
	}
	if b, err := cl.BalanceAt(ctx, common.HexToAddress(ch.dep.Contracts["Treasury"]), nil); err == nil {
		r["treasury_coins"] = fmtCoins(b)
	}
	if b, err := cl.BalanceAt(ctx, deployer, nil); err == nil {
		r["deployer_coins_left"] = fmtCoins(b)
		if before != nil {
			r["deployer_spent"] = fmtCoins(new(big.Int).Sub(before, b)) // gas, L1 data fees and the sponsor's deposit (this run)
		}
	}
	r["gas_used"] = ch.gasUsed.Load() // this run's transactions (a resumed run: only its own)
	out(r)
	return nil
}

// testNetworkNotice: net.json's notice on an operator-run test network (deploy --test-network).
func testNetworkNotice(name, operator string) string {
	if name == "" {
		name = "(unnamed)"
	}
	if operator == "" {
		operator = "its operator"
	}
	return fmt.Sprintf("ternchain test network %s, run by %s: test coins with no value, may be reset. The operator runs the validators and made the zk keys alone: "+
		"shielded transfers and secret votes are NOT private from the operator on this network.", name, operator)
}

// settlementFor: deploy --settle: Circle's USDC on Base Sepolia, or any
// ERC-20 by address (its symbol and decimals as it says on chain).
func settlementFor(b Backend, s string) (*Settlement, error) {
	id, err := b.ChainID(ctxb())
	if err != nil {
		return nil, err
	}
	hp, known := hostByChain(id.Int64())
	if strings.EqualFold(s, "usdc") {
		if !known {
			return nil, fmt.Errorf("--settle usdc: Circle's USDC is known on Base (%d) and Base Sepolia (%d) only; give the token's address", baseMainnetChainID, baseSepoliaChainID)
		}
		s = hp.USDC.Token
	}
	if !common.IsHexAddress(s) {
		return nil, fmt.Errorf("--settle %q: usdc, or a token's address", s)
	}
	t := common.HexToAddress(s)
	if known && strings.EqualFold(t.Hex(), hp.USDC.Token) {
		if code, err := b.CodeAt(ctxb(), t, nil); err != nil || len(code) == 0 {
			return nil, fmt.Errorf("--settle usdc: no token at %s on chain %d", t.Hex(), id.Int64())
		}
		v := hp.USDC
		return &v, nil
	}
	erc20, err := erc20ABI()
	if err != nil {
		return nil, err
	}
	read := func(m string) ([]any, error) {
		data, _ := erc20.Pack(m)
		out, err := b.CallContract(ctxb(), ethereum.CallMsg{To: &t, Data: data}, nil)
		if err != nil {
			return nil, fmt.Errorf("--settle %s: %s(): %w", t.Hex(), m, err)
		}
		return erc20.Unpack(m, out)
	}
	sym, err := read("symbol")
	if err != nil {
		return nil, err
	}
	dec, err := read("decimals")
	if err != nil {
		return nil, err
	}
	return &Settlement{Token: t.Hex(), Symbol: sym[0].(string), Decimals: dec[0].(uint8)}, nil
}

// hostNetworkNotice: net.json's notice for contracts on a chain someone else runs.
func hostNetworkNotice(name, host, operator string) string {
	if name == "" {
		name = "(unnamed)"
	}
	if operator == "" {
		operator = "its operator"
	}
	return fmt.Sprintf("ternchain test network %s: ternchain's contracts on %s, a public test chain; deployed by %s, who holds no power over them "+
		"(governance is the network's own) beyond running the gas sponsor (its daily caps and top-ups). Test money only: Sepolia ETH for gas, Circle's test USDC for payments. "+
		"The deployer made the zk keys alone: shielded transfers and secret votes are NOT private from it on this network.", name, host, operator)
}

// publicNetworkNotice: net.json's notice for the public contract set.
func publicNetworkNotice(name, host string) string {
	if name == "" {
		name = "(unnamed)"
	}
	return fmt.Sprintf("ternchain test network %s: ternchain's contracts on %s, a public test chain. Nobody runs it and nobody can change "+
		"these contracts: the deployer gave up every role and there are no citizens to vote (check: ternchain roles). Test money only: "+
		"you pay your own gas, in Circle's test USDC from your account (or in Sepolia ETH). Secret votes use zk keys their deployer made "+
		"alone: they are not private from it.", name, host)
}

// genesisHashAt: block 0's hash as the node reports it (a QBFT header's hash
// leaves out the seals, so it is read, not computed).
func genesisHashAt(ctx context.Context, cl *ethclient.Client) (string, error) {
	var b struct {
		Hash string `json:"hash"`
	}
	if err := cl.Client().CallContext(ctx, &b, "eth_getBlockByNumber", "0x0", false); err != nil {
		return "", err
	}
	if b.Hash == "" {
		return "", errors.New("no block 0")
	}
	return strings.ToLower(b.Hash), nil
}

// ---- the commands

func runValidatorCmd(args []string) error {
	cmd := args[0]
	rest := args[1:]
	if cmd == "validator" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New("ternchain validator init|genesis|run|stop|status|watch (ternchain help validator)")
		}
		cmd, rest = "validator "+rest[0], rest[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	dir := fs.String("dir", "", "")
	netPath := fs.String("net", envOr("TERNCHAIN_NET", "net.json"), "")
	validators := fs.String("validators", "", "")
	outF := fs.String("out", "genesis.json", "")
	deployerKey := fs.String("deployer-key", "", "")
	deployerAddr := fs.String("deployer", "", "")
	chainID := fs.Int64("chain-id", l1TestnetChainID, "")
	period := fs.Uint64("block-period", 1, "")
	timeout := fs.Uint64("request-timeout", 4, "")
	supplyS := fs.String("supply", "1000000000", "")
	gasLimit := fs.Uint64("gas-limit", 60_000_000, "")
	genesis := fs.String("genesis", "", "")
	bootnodes := fs.String("bootnodes", "", "")
	p2pHost := fs.String("p2p-host", "", "")
	p2pPort := fs.Int("p2p-port", 30303, "")
	rpcHost := fs.String("rpc-host", "127.0.0.1", "")
	rpcPort := fs.Int("rpc-port", 8545, "")
	dockerF := fs.Bool("docker", false, "")
	image := fs.String("image", besuImage, "")
	network := fs.String("docker-network", "", "")
	ip := fs.String("ip", "", "")
	name := fs.String("name", "", "")
	printF := fs.Bool("print", false, "")
	rpcURL := fs.String("rpc", "", "")
	blocks := fs.Int("blocks", 20, "")
	votingPeriod := fs.Uint64("voting-period", 0, "")
	exitWindow := fs.Uint64("exit-window", 0, "")
	launch := fs.Bool("launch", false, "")
	devnetR := fs.Bool("devnet", false, "")
	changeWindow := fs.Uint64("change-window", 0, "")
	removalDelay := fs.Uint64("removal-delay", 0, "")
	lapseGrace := fs.Uint64("lapse-grace", 0, "")
	minSet := fs.Uint64("min-set", 0, "")
	challengeWindow := fs.Uint64("challenge-window", 0, "")
	archive := fs.Bool("archive", false, "")
	fresh := fs.Bool("fresh", false, "")
	recreate := fs.Bool("recreate", false, "")
	namesF := fs.String("names", "", "")
	address := fs.String("address", "", "")
	heap := fs.String("heap", "", "")
	besuExtra := fs.String("besu-args", "", "")
	kBlocks := fs.Uint64("k", 10, "")
	seconds := fs.Uint64("seconds", 0, "")
	reportDir := fs.String("report-dir", "", "")
	foundersF := fs.String("founders", "", "")
	deployCoins := fs.String("deploy-coins", "100", "")
	founderCoins := fs.String("founder-coins", "1000", "")
	validatorCoins := fs.String("validator-coins", "10", "")
	faucetF := fs.String("faucet", "", "")
	faucetCoins := fs.String("faucet-coins", "10000000", "")
	faucetKey := fs.String("faucet-key", "", "")
	rpcsF := fs.String("rpcs", "", "")
	profile := fs.String("profile", "validator", "")
	rpcAllow := fs.String("rpc-allow-hosts", "", "")
	metrics := fs.Bool("metrics", false, "")
	restart := fs.String("restart", "no", "")
	selection := fs.String("selection", "contract", "")
	keepValidators := fs.Bool("keep-validators", false, "")
	testNetwork := fs.Bool("test-network", false, "")
	faucetURL := fs.String("faucet-url", "", "")
	relayURL := fs.String("relay-url", "", "")
	operatorF := fs.String("operator", "", "")
	contactF := fs.String("contact", "", "")
	noticeF := fs.String("notice", "", "")
	sharedF := fs.Bool("shared", false, "")
	admitBps := fs.Uint("admit-bps", 0, "")
	admitFloor := fs.Uint("admit-floor", 0, "")
	criticalX := fs.Uint("critical-delay-x", 0, "")
	minTurnout := fs.Uint("min-turnout", 0, "")
	superF := fs.String("super", "", "")
	hostF := fs.String("host", "", "")
	entryPointF := fs.String("entry-point", "", "")
	bundlerF := fs.String("bundler", "", "")
	explorerF := fs.String("explorer", "", "")
	logRangeF := fs.Uint64("log-range", 0, "")
	feeF := fs.String("fee", "", "")
	daStakeF := fs.String("da-stake", "", "")
	daBondF := fs.String("da-bond", "", "")
	settleF := fs.String("settle", "", "")
	sponsorPerNameF := fs.String("sponsor-per-name", "", "")
	sponsorPerDayF := fs.String("sponsor-per-day", "", "")
	sponsorDepositF := fs.String("sponsor-deposit", "", "")
	erc8004F := fs.String("erc8004", "", "")
	gasPaymasterF := fs.String("gas-paymaster", "", "")
	contractSetF := fs.String("contract-set", "", "")
	capPerPaymentF := fs.String("cap-per-payment", "", "")
	capPerNameF := fs.String("cap-per-name", "", "")
	capCoinPerPaymentF := fs.String("cap-coin-per-payment", "", "")
	capCoinPerNameF := fs.String("cap-coin-per-name", "", "")
	soloZKF := fs.Bool("solo-zk", false, "")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	switch cmd {
	case "validator init":
		v, err := validatorInit(*dir)
		if err != nil {
			return err
		}
		out(map[string]any{"address": v.Address, "enode_id": v.PublicKey, "key": filepath.Join(*dir, "key") + " (0600: the node key signs blocks; back it up, never share it)",
			"next": "send the address to the network (genesis, or a 'gov propose param validators add ADDRESS'); then: ternchain validator run --dir " + *dir + " --genesis genesis.json --bootnodes …"})
	case "validator genesis":
		vs, names, shown, err := parseNamedValidators(*validators)
		if err != nil {
			return err
		}
		rules := launchRules
		if *devnetR {
			rules = devnetRules
		}
		for _, x := range []struct {
			f *uint64
			v *uint64
		}{{changeWindow, &rules.Window}, {removalDelay, &rules.RemovalDelay}, {lapseGrace, &rules.LapseGrace}, {challengeWindow, &rules.VoteWindow}} {
			if *x.f != 0 {
				*x.v = *x.f
			}
		}
		header := false
		switch *selection {
		case "contract":
		case "header":
			header = true
		default:
			return fmt.Errorf("--selection %q: contract (the Validators contract, governed by Parliament: the mainnet) or header (QBFT validator votes: an operator-run test network)", *selection)
		}
		if *minSet != 0 {
			if *minSet < 4 && !header {
				return fmt.Errorf("--min-set %d: the Validators contract's floor is 4 (audit 6 VB-6); a test network with fewer validators uses --selection header", *minSet)
			}
			rules.MinSet = uint32(*minSet)
		}
		if header && *chainID == l1MainnetChainID {
			return errors.New("--selection header: the mainnet's validator set is the Validators contract's (both houses); header voting is for test networks")
		}
		var dep common.Address
		made := false
		switch {
		case *deployerAddr != "":
			if !common.IsHexAddress(*deployerAddr) {
				return fmt.Errorf("--deployer %q: not an address", *deployerAddr)
			}
			dep = common.HexToAddress(*deployerAddr)
		case *deployerKey != "":
			k, m, err := keyFileOrNew(*deployerKey)
			if err != nil {
				return err
			}
			dep, made = crypto.PubkeyToAddress(k.PublicKey), m
		default:
			return errors.New("--deployer-key FILE (made if missing) or --deployer 0xADDRESS: who gets the supply and deploys the contracts")
		}
		supply, err := parseCoins(*supplyS)
		if err != nil {
			return err
		}
		var rows []allocRow
		if *devnetR { // a local demo: the deployer gets everything and funds the owners
			if *chainID == l1MainnetChainID {
				return errors.New("--devnet gives the whole supply to the deployer: a local demo, never the mainnet's chain id (leave --devnet out for the genesis table)")
			}
		} else {
			if rows, err = genesisRows(*foundersF, *faucetF, *deployCoins, *founderCoins, *validatorCoins, *faucetCoins, supply, dep, vs, shown, *chainID); err != nil {
				return err
			}
		}
		g, err := validatorGenesis(genesisOpts{chainID: *chainID, period: *period, timeout: *timeout, validators: vs, names: names, rules: rules,
			deployer: dep, supply: supply, gasLimit: *gasLimit, alloc: rows, headerVoting: header})
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(g, "", "  ")
		if err := os.WriteFile(*outF, b, 0o644); err != nil {
			return err
		}
		sum := crypto.Keccak256Hash(b)
		r := map[string]any{"genesis": *outF, "keccak": sum.Hex(), "chain_id": *chainID, "validators": shown, "validators_contract": validatorsAddress.Hex(),
			"rules":    rules,
			"deployer": dep.Hex(), "supply": *supplyS, "block_period_s": *period,
			"note": "every validator runs this exact file (compare the keccak); then the deployer runs: ternchain deploy --rpc URL --deployer-key FILE --net net.json --genesis founders"}
		if len(rows) > 0 {
			r["allocation"] = rows
			maps.Copy(r, planSummary(rows, supply))
			r["deployer_must"] = "be a fresh key: its first transactions must be the deploy, so the Treasury lands at " + treasuryAddress(dep).Hex() + " (deploy checks it)"
		} else {
			r["allocation"] = "everything to the deployer (--devnet: a local demo)"
		}
		if made {
			r["deployer_key"] = *deployerKey + " (new, 0600)"
		}
		if header {
			delete(r, "validators_contract")
			delete(r, "rules")
			r["selection"] = "header: QBFT validator votes (qbft_proposeValidatorVote) change the set; no Validators contract, no Parliament over it"
			r["grow"] = "add validators 1 -> 4 in one sitting, each new node synced first (ternchain-ops help validator); never stay at 2"
		}
		out(r)
	case "validator run":
		o := runOpts{dir: *dir, genesis: *genesis, bootnodes: *bootnodes, p2pHost: *p2pHost, rpcHost: *rpcHost, image: *image, network: *network,
			ip: *ip, name: *name, p2pPort: *p2pPort, rpcPort: *rpcPort, docker: *dockerF, print: *printF, archive: *archive, fresh: *fresh, recreate: *recreate, heap: *heap,
			extra: strings.Fields(*besuExtra), profile: *profile, allowHosts: *rpcAllow, metrics: *metrics, restart: *restart}
		r, err := validatorRun(o)
		if err != nil {
			return err
		}
		out(r)
	case "validator stop":
		r, err := validatorStop(*dir, *name)
		if err != nil {
			return err
		}
		out(r)
	case "validator status":
		u := *rpcURL
		if u == "" && *dir != "" {
			if v, err := loadValidator(*dir); err == nil {
				u = v.RPC
			}
		}
		if u == "" {
			var f DevnetFile
			if err := readJSON(*netPath, &f); err != nil {
				return errors.New("--rpc URL (a validator's RPC), --dir D, or --net NET.json")
			}
			u = f.RPC
		}
		for _, n := range strings.Split(*namesF, ",") {
			if n = strings.TrimSpace(n); n != "" {
				statusNames = append(statusNames, n)
			}
		}
		r, err := validatorStatus(u, *blocks)
		if err != nil {
			return err
		}
		if h := validatorHandoverAt(u, *netPath); h != nil {
			r["handover"] = h
		}
		out(r)
	case "validator powers":
		u := *rpcURL
		if u == "" {
			return errors.New("ternchain validator powers --rpc URL --address 0x… | --deployer-key FILE")
		}
		who := common.Address{}
		switch {
		case common.IsHexAddress(*address):
			who = common.HexToAddress(*address)
		case *deployerKey != "":
			k, err := readKeyFile(*deployerKey)
			if err != nil {
				return err
			}
			who = crypto.PubkeyToAddress(k.PublicKey)
		default:
			return errors.New("--address 0x… or --deployer-key FILE: whose powers to check")
		}
		r, err := validatorPowers(u, who, *netPath)
		if err != nil {
			return err
		}
		if h := validatorHandoverAt(u, *netPath); h != nil {
			r["handover"] = h
		}
		out(r)
	case "validator watch":
		u := *rpcURL
		if u == "" {
			return errors.New("--rpc URL: a node to watch (its tx pool and blocks), best a data node no validator runs")
		}
		return validatorWatch(u, *kBlocks, *seconds, *reportDir, os.Stdout)
	case "deploy":
		o := deployOpts{rpc: *rpcURL, keyPath: *deployerKey, netPath: *netPath, founders: *genesis, votingPeriod: *votingPeriod,
			exitWindow: *exitWindow, short: !*launch, name: *name, faucetKey: *faucetKey, rpcs: splitList(*rpcsF), bootnodes: splitList(*bootnodes),
			keepValidators: *keepValidators, testNetwork: *testNetwork, faucetURL: *faucetURL, relayURL: *relayURL, operator: *operatorF,
			contact: *contactF, notice: *noticeF, shared: *sharedF, host: *hostF, entryPoint: *entryPointF, bundler: *bundlerF,
			explorer: *explorerF, logRange: *logRangeF, fee: *feeF, daStake: *daStakeF, daBond: *daBondF, settle: *settleF, sponsorPerName: *sponsorPerNameF, sponsorPerDay: *sponsorPerDayF, sponsorDeposit: *sponsorDepositF, erc8004: *erc8004F, gasPaymaster: *gasPaymasterF, profile: *contractSetF,
			admitBps: uint16(*admitBps), admitFloor: uint32(*admitFloor), criticalX: uint32(*criticalX), minTurnout: uint32(*minTurnout),
			soloZK: *soloZKF}
		if *capPerPaymentF != "" || *capPerNameF != "" || *capCoinPerPaymentF != "" || *capCoinPerNameF != "" {
			o.caps = &ValueCaps{PerPayment: *capPerPaymentF, PerName: *capPerNameF, CoinPerPayment: *capCoinPerPaymentF, CoinPerName: *capCoinPerNameF, Note: capsNote}
		}
		if *superF != "" {
			var n, d uint8
			if _, err := fmt.Sscanf(*superF, "%d/%d", &n, &d); err != nil {
				return fmt.Errorf("--super %q: a fraction such as 2/3", *superF)
			}
			o.superNum, o.superDen = n, d
		}
		return runDeployCmd(o)
	default:
		return unknownCommand(cmd)
	}
	return nil
}

// validatorSafety: what status shows of the capture defences (docs/abuse/VALIDATORS.md):
// the rules, the rolling cap's state, who each validator belongs to,
// scheduled removals, challenges, and the contract's recent alerts.
func validatorSafety(ctx context.Context, cl *ethclient.Client, call func(string, ...any) ([]any, error), set []common.Address) map[string]any {
	res := map[string]any{}
	r, err := call("rules")
	if err != nil || len(r) < 6 { // an older Validators (no rules)
		return res
	}
	rules := valRules{Window: r[0].(uint64), RemovalDelay: r[1].(uint64), LapseGrace: r[2].(uint64), MinSet: r[3].(uint32), VoteWindow: r[4].(uint64), QuorumBps: r[5].(uint16)}
	res["rules"] = map[string]any{"window_s": rules.Window, "removal_delay_s": rules.RemovalDelay, "lapse_grace_s": rules.LapseGrace,
		"min_set": rules.MinSet, "challenge_window_s": rules.VoteWindow, "challenge_quorum_bps": rules.QuorumBps}
	if c, err := call("churn"); err == nil {
		k, n0 := c[0].(*big.Int).Int64(), c[1].(*big.Int).Int64()
		cp := n0 / 3
		if cp == 0 {
			cp = 1
		}
		left, _ := call("changesLeft")
		m := map[string]any{"changes_in_window": k, "set_before_window": n0, "cap": cp, "alert": k*4 > n0}
		if len(left) > 0 {
			m["changes_left"] = left[0].(*big.Int).Int64()
		}
		res["churn"] = m
	}
	names := registryNames(ctx, cl, call)
	for _, n := range statusNames {
		if _, ok := names[idOf(n)]; !ok {
			names[idOf(n)] = n
		}
	}
	nameOf := func(h [32]byte) string {
		if h == ([32]byte{}) {
			return "unbound"
		}
		if n, ok := names[common.Hash(h)]; ok {
			return n
		}
		return common.Hash(h).Hex()
	}
	var bound []map[string]any
	for _, v := range set {
		m := map[string]any{"validator": v.Hex()}
		if c, err := call("citizenOf", v); err == nil {
			m["citizen"] = nameOf(c[0].([32]byte))
		}
		if at, err := call("removalAt", v); err == nil && at[0].(uint64) != 0 {
			why, _ := call("removalWhy", v)
			m["removal_at"] = time.Unix(int64(at[0].(uint64)), 0).UTC().Format(time.RFC3339)
			if len(why) > 0 {
				m["removal_why"] = removalWhyName(why[0].(uint8))
			}
		}
		bound = append(bound, m)
	}
	res["bindings"] = bound
	if n, err := call("challengeCount"); err == nil {
		var chs []map[string]any
		for i := int64(0); i < n[0].(*big.Int).Int64(); i++ {
			x, err := call("challengeAt", big.NewInt(i))
			if err != nil {
				continue
			}
			c := abiConvert[valChallenge](x[0])
			chs = append(chs, map[string]any{"id": i, "a": c.A.Hex(), "b": c.B.Hex(), "by": nameOf(c.By), "yes": c.Yes, "no": c.No,
				"ends": time.Unix(int64(c.Ends), 0).UTC().Format(time.RFC3339), "closed": c.Closed, "passed": c.Passed})
		}
		res["challenges"] = chs
	}
	res["alerts"] = validatorAlerts(ctx, cl, nameOf, 20)
	return res
}

type valChallenge struct {
	A, B                common.Address
	By                  [32]byte
	Opened, Ends        uint64
	Electorate, Yes, No uint32
	Closed, Passed      bool
	Evidence            [32]byte
}

func removalWhyName(w uint8) string {
	switch w {
	case 0:
		return "by vote"
	case 1:
		return "its citizen lapsed or was revoked"
	case 2:
		return "same operator (a passed challenge)"
	}
	return fmt.Sprint(w)
}

// registryNames: owner names by id, from the Registry's OwnerRegistered log.
func registryNames(ctx context.Context, cl *ethclient.Client, call func(string, ...any) ([]any, error)) map[common.Hash]string {
	out := map[common.Hash]string{}
	ra, err := call("registry")
	if err != nil || ra[0].(common.Address) == (common.Address{}) {
		return out
	}
	reg, err := loadArtifact("Registry")
	if err != nil {
		return out
	}
	ev := reg.abi.Events["OwnerRegistered"]
	logs, err := cl.FilterLogs(ctx, ethereum.FilterQuery{Addresses: []common.Address{ra[0].(common.Address)}, Topics: [][]common.Hash{{ev.ID}}})
	if err != nil {
		return out
	}
	for _, l := range logs {
		if v, err := ev.Inputs.NonIndexed().Unpack(l.Data); err == nil && len(l.Topics) > 1 {
			out[l.Topics[1]] = v[0].(string)
		}
	}
	return out
}

// validatorAlerts: the Validators contract's last events, in words.
func validatorAlerts(ctx context.Context, cl *ethclient.Client, nameOf func([32]byte) string, last int) []string {
	a, err := loadArtifact("Validators")
	if err != nil {
		return nil
	}
	logs, err := cl.FilterLogs(ctx, ethereum.FilterQuery{Addresses: []common.Address{validatorsAddress}})
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range logs {
		if len(l.Topics) == 0 {
			continue
		}
		ev, err := a.abi.EventByID(l.Topics[0])
		if err != nil {
			continue
		}
		if t := describeValidatorEvent(ev.Name, l, *ev, nameOf); t != "" {
			out = append(out, fmt.Sprintf("block %d: %s", l.BlockNumber, t))
		}
	}
	if len(out) > last {
		out = out[len(out)-last:]
	}
	return out
}

// describeValidatorEvent: one Validators event in words ("" for the dull ones).
func describeValidatorEvent(name string, l types.Log, ev abi.Event, nameOf func([32]byte) string) string {
	args := map[string]any{}
	_ = ev.Inputs.NonIndexed().UnpackIntoMap(args, l.Data)
	topic := func(i int) common.Hash {
		if i < len(l.Topics) {
			return l.Topics[i]
		}
		return common.Hash{}
	}
	addr := func(i int) string { return common.BytesToAddress(topic(i).Bytes()).Hex() }
	switch name {
	case "ValidatorAdded":
		return fmt.Sprintf("validator %s added (citizen %s)", addr(1), nameOf(topic(2)))
	case "ValidatorRemoved":
		return fmt.Sprintf("validator %s removed (citizen %s; %s)", addr(1), nameOf(topic(2)), removalWhyName(args["why"].(uint8)))
	case "RemovalScheduled":
		return fmt.Sprintf("REMOVAL SCHEDULED: validator %s (citizen %s; %s), from %s", addr(1), nameOf(topic(2)), removalWhyName(args["why"].(uint8)),
			time.Unix(int64(args["at"].(uint64)), 0).UTC().Format(time.RFC3339))
	case "RemovalCancelled":
		return fmt.Sprintf("removal of %s cancelled", addr(1))
	case "ValidatorRotated":
		return fmt.Sprintf("citizen %s rotated its validator %s -> %s", nameOf(topic(1)), addr(2), addr(3))
	case "ChurnAlert":
		return fmt.Sprintf("CHURN ALERT: %v validator changes within %s, of a set of %v (more than a quarter): check every proposal touching the set",
			args["changes"], dur(args["window"].(uint64)), args["setBefore"])
	case "ChallengeOpened":
		return fmt.Sprintf("SAME-OPERATOR CHALLENGE %v: %s and %s, by %s, evidence %s; citizens vote until %s", topic(1).Big(), args["a"].(common.Address).Hex(),
			args["b"].(common.Address).Hex(), nameOf(topic(2)), args["uri"], time.Unix(int64(args["ends"].(uint64)), 0).UTC().Format(time.RFC3339))
	case "ChallengeClosed":
		return fmt.Sprintf("challenge %v closed: passed %v (%v yes, %v no)", topic(1).Big(), args["passed"], args["yes"], args["no"])
	case "CensorshipReported":
		ps := args["proposers"].([]common.Address)
		var ss []string
		for _, p := range ps {
			ss = append(ss, p.Hex()[:10])
		}
		return fmt.Sprintf("CENSORSHIP REPORTED by %s: tx %s pending %v blocks from block %v; proposers that skipped it: %s (%s)", nameOf(topic(2)),
			topic(1).Hex(), args["blocksPending"], args["firstSeenBlock"], strings.Join(ss, ", "), args["uri"])
	case "RulesSet":
		return "the set's rules changed"
	case "UpgradeScheduled":
		return fmt.Sprintf("chain software upgrade scheduled at block %v (%s)", args["atBlock"], args["uri"])
	}
	return ""
}

// parseNamedValidators: "ana=DIR,ben=0xADDR,…" (each validator bound to its
// citizen's name) or plain "DIR,0xADDR" (unbound: removed only by vote).
func parseNamedValidators(list string) ([]common.Address, []common.Hash, []string, error) {
	var vs []common.Address
	var names []common.Hash
	var shown []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		name, ref, ok := strings.Cut(s, "=")
		if !ok {
			name, ref = "", s
		}
		a, err := addrOrValidatorDir(ref)
		if err != nil {
			return nil, nil, nil, err
		}
		vs = append(vs, a)
		if name == "" {
			names = append(names, common.Hash{})
			shown = append(shown, a.Hex()+" (unbound)")
		} else {
			names = append(names, idOf(name))
			shown = append(shown, name+"="+a.Hex())
		}
	}
	return vs, names, shown, nil
}

// validatorPowers: which of the set's privileged calls an address could make
// now (each tried as an eth_call from it; F7: the deployer must have none).
func validatorPowers(rpcURL string, who common.Address, netPath string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cl, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	// The Directory's owner and the timelock's roles too (audit 6 VB-4): a
	// deploy that failed before the end leaves them with the deployer.
	kept := deployerKeeps(ctx, cl, who, netPath)
	if code, err := cl.CodeAt(ctx, validatorsAddress, nil); err == nil && len(code) == 0 {
		res := map[string]any{"address": who.Hex(), "selection": "header: no Validators contract; the validators' own QBFT votes change the set"}
		res["keeps"] = kept
		res["verdict"] = fmt.Sprintf("%s keeps %d deploy-time powers (Directory owner, timelock admin or proposer)", who.Hex(), len(kept))
		return res, nil
	}
	a, _ := loadArtifact("Validators")
	set, err := func() ([]common.Address, error) {
		d, _ := a.abi.Pack("getValidators")
		o, err := cl.CallContract(ctx, ethereum.CallMsg{To: &validatorsAddress, Data: d}, nil)
		if err != nil {
			return nil, err
		}
		v, err := a.abi.Unpack("getValidators", o)
		if err != nil {
			return nil, err
		}
		return v[0].([]common.Address), nil
	}()
	if err != nil || len(set) == 0 {
		return nil, fmt.Errorf("no Validators set at %s: %v", validatorsAddress.Hex(), err)
	}
	tries := []struct {
		m    string
		args []any
	}{{"setGovernance", []any{who, who, who}}, {"addValidator", []any{who, crypto.Keccak256Hash([]byte("anyone"))}}, {"removeValidator", []any{set[0]}},
		{"cancelRemoval", []any{set[0]}}, {"setRules", []any{devnetRules}}, {"scheduleUpgrade", []any{uint64(1 << 40), [32]byte{1}, "x"}},
		{"rotate", []any{crypto.Keccak256Hash([]byte("anyone")), who}}}
	res := map[string]any{"address": who.Hex()}
	var can []string
	refused := map[string]string{}
	for _, t := range tries {
		d, err := a.abi.Pack(t.m, t.args...)
		if err != nil {
			return nil, err
		}
		if _, err := cl.CallContract(ctx, ethereum.CallMsg{From: who, To: &validatorsAddress, Data: d}, nil); err == nil {
			can = append(can, t.m)
		} else {
			refused[t.m] = err.Error()
		}
	}
	res["can"], res["refused"], res["keeps"] = can, refused, kept
	res["verdict"] = fmt.Sprintf("%s can call %d of %d privileged Validators methods and keeps %d deploy-time powers", who.Hex(), len(can), len(tries), len(kept))
	return res, nil
}

// deployerKeeps: what who still holds of deploy's powers, by net.json's
// contracts: the Directory's ownership, the timelock's admin and proposer roles.
func deployerKeeps(ctx context.Context, cl *ethclient.Client, who common.Address, netPath string) []string {
	kept := []string{}
	var f DevnetFile
	if netPath == "" || readJSON(netPath, &f) != nil {
		return kept
	}
	call := func(name, method string, args ...any) []any {
		a, err := loadArtifact(name)
		addr := common.HexToAddress(f.Contracts[name])
		if err != nil || addr == (common.Address{}) {
			return nil
		}
		d, err := a.abi.Pack(method, args...)
		if err != nil {
			return nil
		}
		o, err := cl.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: d}, nil)
		if err != nil {
			return nil
		}
		v, _ := a.abi.Unpack(method, o)
		return v
	}
	if v := call("Directory", "owner"); len(v) == 1 && v[0].(common.Address) == who {
		kept = append(kept, "Directory owner")
	}
	for _, role := range []string{"DEFAULT_ADMIN_ROLE", "PROPOSER_ROLE"} {
		r := call("TernTimelock", role)
		if len(r) != 1 {
			continue
		}
		if v := call("TernTimelock", "hasRole", r[0].([32]byte), who); len(v) == 1 && v[0].(bool) {
			kept = append(kept, "timelock "+role)
		}
	}
	return kept
}
