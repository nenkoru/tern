// A devnet in one process: a simulated Ethereum chain with HTTP JSON-RPC, a
// block every second, the contracts deployed, and a faucet. For demos,
// development and a test network for invited people; not a production chain.
//
// With --datadir the chain is kept on disk (geth's database, every block's
// state flushed: archive mode, hash scheme) and survives restarts: the first
// start deploys and writes DIR/operator.json (the genesis/faucet key and the
// deployment); later starts open the same database on the same genesis and
// deploy nothing. Without --datadir the chain lives in memory.
//
// Two files come out: NET (net.json), the public one to hand to people
// (RPC, chain id, contracts, faucet URL, default data node), and
// NET.operator.json beside it (0600), with the faucet key and the admin URL,
// for the operator's own tools only. POST /faucet {"address": "0x…"} on the
// RPC port gives a few test coins, once per address a day, a few per IP a
// day, and at most faucetPerHour grants an hour (--faucet-per-hour and
// --faucet-per-ip-day change them).
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/catalyst"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/nenkoru/tern/internal/netlimit"
)

// DevnetFile: a network file. The public net.json has no faucet key and no
// admin URL; the operator's file has them.
type DevnetFile struct {
	Deployment
	Network   string `json:"network,omitempty"`    // a name or season tag
	FaucetURL string `json:"faucet_url,omitempty"` // POST {"address": "0x…"}: test coins
	Relay     string `json:"relay,omitempty"`      // the default data node / relay
	// "shared": a test network for invited people; clients refuse real
	// currencies in fiat, minting, random, pots and passkeys (private payments are standard: allowed).
	Mode string `json:"mode,omitempty"`
	// Refuse: what clients of a shared network refuse (season.go: mint,
	// random, pot, passkey, fiat-real); absent: all of those. Operator policy
	// as data, not code in the user binary.
	Refuse        []string `json:"refuse,omitempty"`
	Operator      string   `json:"operator,omitempty"`       // who runs it (devnet --operator)
	Contact       string   `json:"contact,omitempty"`        // how to reach them (devnet --contact)
	Notice        string   `json:"notice,omitempty"`         // shown at owner init and in the faucet's reply
	Announce      string   `json:"announce,omitempty"`       // the network's news community (announce-only; agent join joins it: news.go)
	ContractsHash string   `json:"contracts_hash,omitempty"` // the contract set it was deployed with ('ternchain version')
	Faucet        string   `json:"faucet_key,omitempty"`     // operator only
	Admin         string   `json:"admin,omitempty"`          // operator only: http://…/advance?seconds=N
	// RPCs: more RPC URLs of the same network; connect tries rpc, then these
	// in order, and takes the first that answers with the right chain id
	// (and genesis_hash, when set).
	RPCs        []string `json:"rpcs,omitempty"`
	Bootnodes   []string `json:"bootnodes,omitempty"`    // enode://… of the L1's bootnodes (for validator run --bootnodes)
	GenesisHash string   `json:"genesis_hash,omitempty"` // block 0's hash: an RPC of another chain with the same id is refused
	// TestNetwork: an operator-run test network (deploy --test-network): the
	// operator made the zk keys alone, so shielded transfers and secret votes
	// are not private from it; notice says so.
	TestNetwork bool `json:"test_network,omitempty"`
}

// operatorPath: the operator-only file beside a net file.
func operatorPath(netPath string) string {
	return strings.TrimSuffix(netPath, ".json") + ".operator.json"
}

type devnetConfig struct {
	net, listen, admin, dataDir   string
	publicURL, relay, name        string // what net.json advertises
	operator, contact             string // the notice: who runs it, how to reach them
	shared                        bool   // net.json "mode": "shared"
	faucetPerHour, faucetPerIPDay int    // the faucet's limits (0: the defaults)
	faucetCoins                   *big.Int
	trustedProxy                  string // reverse proxies whose X-Forwarded-For the faucet believes ("": this machine)
	chainID                       int64  // 0: 1337, or the chain id an existing datadir has
	noLogIndex                    bool   // --log-index=false: no log search index (see openChain)
	bucketBits                    int    // --bucket-bits: tag buckets (bucket.go); -1: keep what the chain had
	p                             Params
}

const defaultChainID = 1337

// devnetNotice: what a private devnet (not --shared, no --operator) tells newcomers.
const devnetNotice = "A private development network (ternchain-ops devnet): test coins with no value, gone when its operator resets it. " +
	"Nothing here is a real contract or payment."

type devnet struct {
	stack  *node.Node
	beacon *catalyst.SimulatedBeacon
	cl     *ethclient.Client
	ch     *Chain
	mu     chan struct{}
	stop   chan struct{}
	admin  net.Listener
	db     ethdb.Database // the chain's database: synced to disk when paused for a backup
	rpc    string         // the address it listens on
	f      DevnetFile
}

const (
	faucetPerHour  = 100 // grants an hour, all addresses together
	faucetPerIPDay = 5
)

func runDevnet(cfg devnetConfig) error {
	d, err := startDevnet(cfg)
	if err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Printf("ternchain devnet: stopping")
	return d.Close()
}

func startDevnet(cfg devnetConfig) (*devnet, error) {
	host, portS, err := net.SplitHostPort(cfg.listen)
	if err != nil {
		return nil, err
	}
	port, _ := strconv.Atoi(portS)
	var key *ecdsa.PrivateKey
	var prev *DevnetFile // the deployment of an earlier run, in the datadir
	opFile := ""
	if cfg.dataDir != "" {
		if err := os.MkdirAll(cfg.dataDir, 0o700); err != nil {
			return nil, err
		}
		opFile = filepath.Join(cfg.dataDir, "operator.json")
		var f DevnetFile
		if err := readJSON(opFile, &f); err == nil {
			if key, err = crypto.HexToECDSA(f.Faucet); err != nil {
				return nil, fmt.Errorf("%s: bad faucet key: %w", opFile, err)
			}
			prev = &f
		} else if ents, _ := os.ReadDir(cfg.dataDir); len(ents) > 0 {
			return nil, fmt.Errorf("%s holds a chain but no operator.json: restore it from a backup, or start with an empty --datadir (a new network)", cfg.dataDir)
		}
	}
	if key == nil {
		key, _ = crypto.GenerateKey()
	}
	chainID := cfg.chainID
	if prev != nil && prev.ChainID != 0 {
		if chainID != 0 && chainID != prev.ChainID {
			return nil, fmt.Errorf("%s holds chain %d; --chain-id %d would be another network (a fork): use a new --datadir for it", cfg.dataDir, prev.ChainID, chainID)
		}
		chainID = prev.ChainID
	}
	if chainID == 0 {
		chainID = defaultChainID
	}
	if chainID == l1MainnetChainID || chainID == l1TestnetChainID { // a devnet mints test money (TestUSD): never on the L1's ids (audit 6 IT-5)
		return nil, fmt.Errorf("--chain-id %d is the L1's (mainnet %d, testnet %d): a devnet takes another id", chainID, l1MainnetChainID, l1TestnetChainID)
	}
	if prev != nil { // a restart: what the first start advertised stays unless a flag says otherwise
		inherit := func(v *string, was, flag string) {
			if *v == "" && was != "" {
				*v = was
				log.Printf("ternchain devnet: %s %q kept from %s (pass the flag to change it)", flag, was, opFile)
			}
		}
		inherit(&cfg.name, prev.Network, "--network")
		inherit(&cfg.relay, prev.Relay, "--relay-url")
		inherit(&cfg.operator, prev.Operator, "--operator")
		inherit(&cfg.contact, prev.Contact, "--contact")
		if !localURL(prev.RPC) {
			inherit(&cfg.publicURL, prev.RPC, "--public-url")
		}
		if prev.Mode == "shared" && !cfg.shared {
			cfg.shared = true
			log.Printf("ternchain devnet: shared mode kept from %s", opFile)
		}
	}
	if chainID < 0 {
		return nil, fmt.Errorf("--chain-id %d: a positive number", chainID)
	}
	if chainID == l1MainnetChainID || chainID == l1TestnetChainID { // audit 6 IT-5: a devnet mints TestUSD and uses the devnet zk keys
		return nil, fmt.Errorf("--chain-id %d is the L1's (mainnet %d, testnet %d): a devnet would put test tokens and devnet zk keys on it; pick another id",
			chainID, l1MainnetChainID, l1TestnetChainID)
	}
	if err := netlimit.SetTrustedProxies(cfg.trustedProxy); err != nil {
		return nil, err
	}
	notice := networkNotice(cfg.operator, cfg.contact)
	if !cfg.shared && cfg.operator == "" && cfg.contact == "" { // a private devnet: not the invited network's notice (PROOFREAD-6 #21)
		notice = devnetNotice
	}
	fh := &faucetHandler{coins: cfg.faucetCoins, byAddr: map[common.Address]int64{}, byIP: map[string][]int64{}, notice: notice,
		perHour: cfg.faucetPerHour, perIPDay: cfg.faucetPerIPDay}
	if cfg.dataDir != "" {
		fh.path = filepath.Join(cfg.dataDir, "faucet.json")
		fh.load()
	}
	alloc := types.GenesisAlloc{crypto.PubkeyToAddress(key.PublicKey): {Balance: new(big.Int).Mul(coinUnit, big.NewInt(1_000_000_000))}}
	d := &devnet{mu: make(chan struct{}, 1), stop: make(chan struct{})}
	d.stack, d.beacon, d.db, err = openChain(cfg.dataDir, alloc, map[string]http.Handler{"/faucet": fh}, func(nc *node.Config, ec *ethconfig.Config) {
		nc.HTTPHost, nc.HTTPPort = host, port
		nc.HTTPModules = []string{"eth", "net", "web3"}
		nc.HTTPVirtualHosts = []string{"*"}
		// websockets on the same port: eth_subscribe (newHeads, logs) for watchers
		nc.WSHost, nc.WSPort, nc.WSModules = host, port, []string{"eth", "net", "web3"}
	}, withOsaka, withChainID(chainID), func(nc *node.Config, ec *ethconfig.Config) {
		// geth's log index makes every log query near the head wait until the
		// index has caught up with the head; under load it falls behind and
		// queries time out (sim/scale/REPORT.md). Without it, a query reads the
		// headers' blooms of its range: fine for a devnet's recent ranges.
		ec.LogNoHistory = cfg.noLogIndex
	})
	if err != nil {
		return nil, err
	}
	d.cl = ethclient.NewClient(d.stack.Attach())
	d.rpc = d.stack.HTTPEndpoint()
	commit := func() {
		d.mu <- struct{}{}
		d.beacon.Commit()
		<-d.mu
	}
	public := cfg.publicURL
	if public == "" {
		public = d.rpc
	}
	if prev != nil { // a restart: the same chain, nothing to deploy
		if d.ch, err = newChain(d.cl, prev.Deployment, commit); err == nil {
			_, err = d.ch.refresh()
		}
		if err != nil {
			d.Close()
			return nil, fmt.Errorf("reopening the chain in %s: %w", cfg.dataDir, err)
		}
		head, _ := d.cl.BlockNumber(context.Background())
		log.Printf("ternchain devnet: reopened %s at block %d", cfg.dataDir, head)
	} else {
		p := cfg.p
		p.Devnet = true // a devnet, whatever its chain id: test money (TestUSD) too
		if p.Treasury == ([20]byte{}) {
			p.Treasury = crypto.PubkeyToAddress(key.PublicKey)
		}
		if d.ch, err = deploy(d.cl, key, p, public, commit); err != nil {
			d.Close()
			return nil, err
		}
	}
	d.ch.dep.RPC = public
	if cfg.bucketBits >= 0 {
		d.ch.dep.BucketBits = min(cfg.bucketBits, 16)
	}
	fh.ch, fh.key = d.ch, key
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-d.stop:
				return
			case <-t.C:
				commit()
			}
		}
	}()
	if d.admin, err = net.Listen("tcp", cfg.admin); err != nil {
		d.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	// /pause?seconds=N: no new blocks for up to N seconds (at most 600), or
	// until /resume. The chain's files stop changing (geth writes on blocks), so
	// deploy/backup.sh can copy the datadir consistently without stopping the
	// process. The RPC keeps answering reads; transactions wait in the pool.
	var resume chan struct{}
	var pmu sync.Mutex
	mux.HandleFunc("/pause", func(w http.ResponseWriter, r *http.Request) {
		s, _ := strconv.Atoi(r.URL.Query().Get("seconds"))
		if s <= 0 || s > 600 {
			s = 120
		}
		pmu.Lock()
		if resume != nil {
			pmu.Unlock()
			http.Error(w, "already paused", http.StatusConflict)
			return
		}
		done := make(chan struct{})
		resume = done
		pmu.Unlock()
		d.mu <- struct{}{} // no block in flight from here on
		synced := "synced to disk"
		if err := d.db.SyncKeyValue(); err != nil { // the write-ahead log's buffered tail, too
			synced = "not synced: " + err.Error()
		}
		go func() {
			select {
			case <-done:
			case <-time.After(time.Duration(s) * time.Second):
				pmu.Lock()
				if resume == done {
					resume = nil
				}
				pmu.Unlock()
			case <-d.stop:
			}
			<-d.mu
		}()
		head, _ := d.cl.BlockNumber(context.Background())
		fmt.Fprintf(w, "paused at block %d for at most %ds (%s)\n", head, s, synced)
	})
	mux.HandleFunc("/resume", func(w http.ResponseWriter, r *http.Request) {
		pmu.Lock()
		if resume != nil {
			close(resume)
			resume = nil
		}
		pmu.Unlock()
		fmt.Fprintln(w, "resumed")
	})
	mux.HandleFunc("/advance", func(w http.ResponseWriter, r *http.Request) {
		s, _ := strconv.Atoi(r.URL.Query().Get("seconds"))
		d.mu <- struct{}{}
		// The jump needs an empty pool: under load, mine what waits and try again.
		err := d.beacon.AdjustTime(time.Duration(s) * time.Second)
		for i := 0; err != nil && strings.Contains(err.Error(), "non-empty") && i < 100; i++ {
			d.beacon.Commit()
			err = d.beacon.AdjustTime(time.Duration(s) * time.Second)
		}
		d.beacon.Commit()
		<-d.mu
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(w, "advanced %ds\n", s)
	})
	go http.Serve(d.admin, adminGuard(mux))
	faucetURL := strings.TrimRight(public, "/") + "/faucet"
	hash, _ := contractsHash()
	if prev != nil && prev.ContractsHash != hash {
		was := prev.ContractsHash
		if was == "" {
			was = "unknown (deployed before versions were recorded)"
		}
		log.Printf("ternchain devnet: WARNING: this binary's contract set (%s) is not the one this chain was deployed with (%s): "+
			"the chain keeps its old contracts; clients of this binary may misread it. A new contract set needs a new --datadir (RUNBOOK.md)", hash[:12], was)
		hash = prev.ContractsHash
	}
	d.f = DevnetFile{Deployment: d.ch.dep, Network: cfg.name, FaucetURL: faucetURL, Relay: cfg.relay,
		Operator: cfg.operator, Contact: cfg.contact, Notice: notice, ContractsHash: hash}
	if cfg.shared {
		d.f.Mode, d.f.Refuse = "shared", defaultSharedRefusals
		if prev != nil && prev.Refuse != nil { // a restart keeps the list the operator file holds (edited, perhaps)
			d.f.Refuse = prev.Refuse
		}
	}
	op := d.f
	op.Faucet, op.Admin = keyHex(key), "http://"+d.admin.Addr().String()+"/advance"
	if opFile != "" {
		if err := writeJSON(opFile, op); err != nil {
			d.Close()
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(cfg.net), 0o755); err != nil {
		d.Close()
		return nil, err
	}
	pb, _ := json.MarshalIndent(d.f, "", "  ")
	ob, _ := json.MarshalIndent(op, "", "  ")
	if err := os.WriteFile(cfg.net, pb, 0o644); err != nil {
		d.Close()
		return nil, err
	}
	if err := os.WriteFile(operatorPath(cfg.net), ob, 0o600); err != nil {
		d.Close()
		return nil, err
	}
	kept := "in memory: a restart is a new network"
	if cfg.dataDir != "" {
		kept = "on disk in " + cfg.dataDir
	}
	log.Printf("ternchain devnet: RPC %s (advertised %s), chain %s; public file %s (hand it out), operator file %s (keep it)",
		d.rpc, public, kept, cfg.net, operatorPath(cfg.net))
	return d, nil
}

func (d *devnet) Close() error {
	select {
	case <-d.stop:
		return nil
	default:
		close(d.stop)
	}
	d.mu <- struct{}{} // no block in flight
	defer func() { <-d.mu }()
	if d.admin != nil {
		d.admin.Close()
	}
	if d.cl != nil {
		d.cl.Close()
	}
	d.beacon.Stop()
	return d.stack.Close()
}

// openChain: geth's simulated beacon on a node, as ethclient/simulated does,
// but on a database that may be on disk, and without rewinding to genesis
// on start (which simulated.NewBackend does).
func openChain(dataDir string, alloc types.GenesisAlloc, handlers map[string]http.Handler,
	options ...func(*node.Config, *ethconfig.Config)) (*node.Node, *catalyst.SimulatedBeacon, ethdb.Database, error) {
	nc := node.DefaultConfig
	nc.DataDir, nc.IPCPath, nc.Name = dataDir, "", "ternchain"
	nc.P2P = p2p.Config{NoDiscovery: true}
	ec := ethconfig.Defaults
	ec.Genesis = &core.Genesis{Config: params.AllDevChainProtocolChanges, GasLimit: ethconfig.Defaults.Miner.GasCeil, Alloc: alloc}
	ec.SyncMode = ethconfig.FullSync
	ec.TxPool.NoLocals = true
	if dataDir != "" { // every block's state straight to disk: a crash loses at most the block being made
		ec.NoPruning, ec.StateScheme = true, rawdb.HashScheme
	}
	for _, o := range options {
		o(&nc, &ec)
	}
	stack, err := node.New(&nc)
	if err != nil {
		return nil, nil, nil, err
	}
	backend, err := eth.New(stack, &ec)
	if err != nil {
		stack.Close()
		return nil, nil, nil, err
	}
	fs := filters.NewFilterSystem(backend.APIBackend, filters.Config{})
	stack.RegisterAPIs([]rpc.API{{Namespace: "eth", Service: filters.NewFilterAPI(fs)}})
	for path, h := range handlers {
		stack.RegisterHandler(strings.Trim(path, "/"), path, h)
	}
	if err := stack.Start(); err != nil {
		stack.Close()
		return nil, nil, nil, err
	}
	beacon, err := catalyst.NewSimulatedBeacon(0, common.Address{}, backend)
	if err != nil {
		stack.Close()
		return nil, nil, nil, err
	}
	return stack, beacon, backend.ChainDb(), nil
}

// ---- the faucet: a few test coins per address a day, rate-limited

type faucetHandler struct {
	mu     sync.Mutex
	ch     *Chain
	key    *ecdsa.PrivateKey
	coins  *big.Int
	gas    *big.Int // a little more, for the first transactions' gas (faucet serve; nil: none)
	byAddr map[common.Address]int64
	byIP   map[string][]int64
	hour   []int64
	path   string
	notice string
	// limits (0: faucetPerHour, faucetPerIPDay)
	perHour, perIPDay int
}

func (f *faucetHandler) limits() (perHour, perIPDay int) {
	perHour, perIPDay = faucetPerHour, faucetPerIPDay
	if f.perHour > 0 {
		perHour = f.perHour
	}
	if f.perIPDay > 0 {
		perIPDay = f.perIPDay
	}
	return
}

type faucetLog struct {
	ByAddr map[common.Address]int64 `json:"by_address"`
	ByIP   map[string][]int64       `json:"by_ip"`
	Hour   []int64                  `json:"hour"`
}

func (f *faucetHandler) load() {
	var l faucetLog
	if readJSON(f.path, &l) == nil {
		if l.ByAddr != nil {
			f.byAddr = l.ByAddr
		}
		if l.ByIP != nil {
			f.byIP = l.ByIP
		}
		f.hour = l.Hour
	}
}

func (f *faucetHandler) save() {
	if f.path != "" {
		writeJSON(f.path, faucetLog{f.byAddr, f.byIP, f.hour})
	}
}

func recent(ts []int64, now, window int64) []int64 {
	var out []int64
	for _, t := range ts {
		if now-t < window {
			out = append(out, t)
		}
	}
	return out
}

func (f *faucetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `POST {"address": "0x…"}`, http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil || !common.IsHexAddress(req.Address) {
		http.Error(w, `POST {"address": "0x…"}`, http.StatusBadRequest)
		return
	}
	a, ips := common.HexToAddress(req.Address), netlimit.Buckets(netlimit.ClientIP(r)) // per IPv4 address; per IPv6 /64, /56 (×2), /48 (×4)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ch == nil {
		http.Error(w, "the network is starting; try again in a minute", http.StatusServiceUnavailable)
		return
	}
	now := time.Now().Unix()
	perHour, perIPDay := f.limits()
	over := false
	for ip, share := range ips {
		f.byIP[ip] = recent(f.byIP[ip], now, 86400)
		over = over || len(f.byIP[ip]) >= share*perIPDay
	}
	f.hour = recent(f.hour, now, 3600)
	switch {
	case now-f.byAddr[a] < 86400:
		http.Error(w, "this address got test coins in the last 24 hours", http.StatusTooManyRequests)
		return
	case over:
		http.Error(w, "too many requests from your address today; ask the operator", http.StatusTooManyRequests)
		return
	case len(f.hour) >= perHour:
		http.Error(w, "the faucet is busy; try again within the hour", http.StatusTooManyRequests)
		return
	}
	grant := new(big.Int).Set(f.coins)
	if f.gas != nil {
		grant.Add(grant, f.gas)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	_, berr := f.ch.b.BlockNumber(ctx)
	cancel()
	if berr != nil {
		http.Error(w, "the network's node is unreachable; try again in a few minutes", http.StatusServiceUnavailable)
		return
	}
	if err := f.ch.transfer(f.key, a, grant); err != nil {
		http.Error(w, "the faucet failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for ip := range ips {
		f.byIP[ip] = append(f.byIP[ip], now)
	}
	f.byAddr[a], f.hour = now, append(f.hour, now)
	f.save()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"funded": a.Hex(), "coins": fmtCoins(f.coins), "gas": fmtCoins(orZero(f.gas)), "note": "test coins: no value", "notice": f.notice})
}

// askFaucet: test coins for an address from a network's faucet URL.
func askFaucet(u string, a common.Address) error {
	b, _ := json.Marshal(map[string]string{"address": a.Hex()})
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Post(u, "application/json", strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

var coinUnit = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

// connect opens the network described by a net file; the operator's file
// beside it, if this machine has it, adds the faucet key and the admin URL.
func connect(netPath string) (*Chain, *DevnetFile, error) {
	var f DevnetFile
	if err := readJSON(netPath, &f); err != nil {
		return nil, nil, fmt.Errorf("no network file %s: set TERNCHAIN_NET to the net.json you were given (see START.md), or start your own network with ternchain-ops devnet: %w", netPath, err)
	}
	var op DevnetFile
	if readJSON(operatorPath(netPath), &op) == nil && op.Faucet != "" {
		f.Faucet, f.Admin = op.Faucet, op.Admin
	}
	ch, err := dialNet(&f, netPath)
	if err != nil {
		return nil, nil, err
	}
	if err := zkUseNet(netPath, f.Deployment); err != nil { // the zk keys net.json names (ceremony.go)
		return nil, nil, err
	}
	if _, err := ch.refresh(); err != nil {
		if strings.Contains(err.Error(), "no contract code") {
			return nil, nil, fmt.Errorf("the network at %s doesn't have this net.json's contracts: it was reset, or the file is for another network. "+
				"Get the current net.json; names and coins of the old network are gone (start again with new --dir folders)", f.RPC)
		}
		return nil, nil, fmt.Errorf("reading current contracts from governance: %w", err)
	}
	return ch, &f, nil
}

// dialNet: the first of net.json's RPCs (rpc, then rpcs in order) that
// answers as this network: its chain id, and block 0 when genesis_hash is set.
// The one taken becomes f.RPC (the client's websocket follows it too).
func dialNet(f *DevnetFile, netPath string) (*Chain, error) {
	var urls []string
	for _, u := range append([]string{f.RPC}, f.RPCs...) {
		if u = strings.TrimSpace(u); u != "" && !slices.Contains(urls, u) {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("%s names no rpc", netPath)
	}
	var errs []string
	for _, u := range urls {
		ch, err := dialOne(f, netPath, u)
		if err == nil {
			f.RPC, f.Deployment.RPC, ch.dep.RPC = u, u, u
			return ch, nil
		}
		if strings.Contains(err.Error(), "refusing to sign") && len(urls) == 1 {
			return nil, err // one RPC: its own words (checkChainID)
		}
		errs = append(errs, err.Error())
	}
	return nil, fmt.Errorf("no RPC of %s answers as this network (tried %d): %s", netPath, len(urls), strings.Join(errs, "; "))
}

func dialOne(f *DevnetFile, netPath, u string) (*Chain, error) {
	if f.ChainID == 0 && !localURL(u) {
		return nil, fmt.Errorf("%s has no chain_id: refusing to sign for whatever chain %s serves (get the network's net.json)", netPath, u)
	}
	allowPrivate = localURL(u)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cl, err := ethclient.DialContext(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", u, err)
	}
	dep := f.Deployment
	dep.RPC = u
	ch, err := newChain(cl, dep, nil)
	if err != nil {
		cl.Close()
		return nil, fmt.Errorf("can't reach the network at %s: %w", u, err)
	}
	if err := checkChainID(f.ChainID, ch.chainID.Int64(), netPath, u); err != nil {
		cl.Close()
		return nil, err
	}
	if f.GenesisHash != "" {
		g, err := genesisHashAt(ctx, cl)
		if err != nil || !strings.EqualFold(g, f.GenesisHash) {
			cl.Close()
			return nil, fmt.Errorf("%s serves another chain with the same id (block 0 %s, net.json says %s): refusing to sign", u, g, f.GenesisHash)
		}
	}
	return ch, nil
}

// checkChainID: replay protection between forks. Transactions are signed for
// the chain id the RPC reports; if that isn't the network net.json names, a
// signature would be valid on another chain (a fork, or a node pretending), so
// the client refuses to sign anything.
func checkChainID(want, got int64, netPath, rpc string) error {
	if want != 0 && want != got {
		return fmt.Errorf("%s is for chain %d, but the RPC %s serves chain %d: refusing to sign (a transaction signed for one fork "+
			"could be replayed on the other); get the net.json of the network you mean, or the right RPC", netPath, want, rpc, got)
	}
	return nil
}

// localURL: is this URL's host this machine or a private network?
func localURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	h := p.Hostname()
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

// withChainID: a network of its own id (EIP-155), so a transaction signed for
// one fork can't be replayed on another.
func withChainID(id int64) func(*node.Config, *ethconfig.Config) {
	return func(nc *node.Config, ec *ethconfig.Config) {
		cfg := *ec.Genesis.Config
		cfg.ChainID = big.NewInt(id)
		ec.Genesis.Config = &cfg
		ec.NetworkId = uint64(id)
	}
}

// withOsaka runs the chain with the Osaka fork: among others, the P256VERIFY
// precompile (EIP-7951), so a passkey signature costs a few thousand gas
// instead of ~300k in Solidity. A chain of our own would launch with it.
func withOsaka(nc *node.Config, ec *ethconfig.Config) {
	cfg := *ec.Genesis.Config
	zero := uint64(0)
	cfg.OsakaTime = &zero
	bs := *cfg.BlobScheduleConfig
	bs.Osaka = params.DefaultOsakaBlobConfig
	cfg.BlobScheduleConfig = &bs
	ec.Genesis.Config = &cfg
}

// adminGuard: the operator's admin port answers its own tools (curl, the
// test harness), not a web page the operator happens to open: a browser
// request carries Origin or Sec-Fetch-Site, and is refused (audit 4 DV-1, CSRF).
func adminGuard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" || (r.Header.Get("Sec-Fetch-Site") != "" && r.Header.Get("Sec-Fetch-Site") != "none") {
			http.Error(w, "the admin port takes no browser requests", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}
