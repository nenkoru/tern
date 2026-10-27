//go:build ops

// ternchain simulate: a network at scale (sim/scale/). One process drives
// thousands of identities through the client library (no CLI processes)
// against a fresh devnet on disk and several data nodes, which run as their
// own processes so their load can be measured. Phases:
//
//	setup    a devnet (--datadir), data nodes, the population: owners, agents, services' cards, communities, citizens
//	days     simulated days of ordinary life (mail, hires, channels, crowdfunds, communities, reviews, pool, governance, prices), time skipped between days
//	stress   rising load until something gives: mail, payments, channel vouchers
//	serve    keep the world running for outside (LLM) agents: services answer mail and deliver, until ROOT/stop exists
//	report   results.json and results.md from what was measured
//
// All state is under --root (default sim/scale/run); phases resume on it.
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"math/rand/v2"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

type scaleCfg struct {
	root, phase                                  string
	owners, agents, services, communities, nodes int
	founders                                     int
	abuse                                        float64
	days, port, conc                             int
	rate                                         float64 // activity multiplier
	stressSec                                    int
	seed                                         uint64
	shareBatches                                 bool // readers in this process share fetched batches (the bytes a node would serve are still counted)
	tagIndex                                     bool // readers skip batches without their tags (the cheap fix)
	keep                                         bool // leave the devnet and nodes running at the end
	pprof                                        string
	fixes                                        bool // the cheap fixes (perfFixes)
	logIndex                                     bool // geth's log index on the devnet
	reportScaling                                bool
	stressKinds                                  string // mail,pay,voucher (default all)
	stressRates                                  string // steps instead of the default
	bucketBits                                   int    // tag buckets (bucket.go): 0 = every reader reads every batch
	reportWeights                                bool   // weighted reports (reports.go); false: the fixed 3
}

// ---- the world, as written to ROOT/world.json

type simPerson struct {
	Name    string   `json:"name"`
	Agents  []string `json:"agents"`
	Node    int      `json:"node"`
	Abuse   string   `json:"abuse,omitempty"` // spam | sybil | fakereview | inject
	Founder bool     `json:"founder,omitempty"`
}

type simService struct {
	Agent   string   `json:"agent"`
	Kind    string   `json:"kind"`
	Caps    []string `json:"caps"`
	Price   float64  `json:"price"`
	Quality float64  `json:"quality"` // 0..1: how good its work really is (hidden from clients)
	PerReq  float64  `json:"per_request,omitempty"`
	Crony   bool     `json:"crony,omitempty"` // a bad service the fake reviewers push
}

type simCommunity struct {
	Name         string   `json:"name"`
	Founder      string   `json:"founder"`
	CitizensOnly bool     `json:"citizens_only,omitempty"`
	Members      []string `json:"members"`
	Fee          float64  `json:"fee,omitempty"`
}

type scaleWorld struct {
	People      []*simPerson    `json:"people"`
	Services    []*simService   `json:"services"`
	Communities []*simCommunity `json:"communities"`
	Nodes       []string        `json:"nodes"` // data node URLs
	NodeNames   []string        `json:"node_names"`
	Arbiters    []string        `json:"arbiters"`
	Panelists   []string        `json:"panelists"`
	Day         int             `json:"day"`
	Citizens    []string        `json:"citizens"`
	Campaigns   []simCampaign   `json:"campaigns,omitempty"`
	Proposals   []simProposal   `json:"proposals,omitempty"`
	LLM         []string        `json:"llm,omitempty"` // owners left for outside agents
	SetupSec    float64         `json:"setup_s"`
}

type simCampaign struct {
	ID       uint64   `json:"id"`
	Creator  string   `json:"creator"`
	Deadline uint64   `json:"deadline"`
	Goal     float64  `json:"goal"`
	Backers  []string `json:"backers"`
	Done     bool     `json:"done"`
	Hopeless bool     `json:"hopeless"`
}

type simProposal struct {
	ID    string `json:"id"`
	By    string `json:"by"`
	Day   int    `json:"day"`
	State string `json:"state"`
}

type scaleSim struct {
	cfg                scaleCfg
	ch                 *Chain
	cl                 *ethclient.Client
	df                 DevnetFile
	faucet             *ecdsa.PrivateKey
	rpcT               *countingTransport
	nodeT              *nodeTransport
	w                  *scaleWorld
	wmu                sync.Mutex
	clients            sync.Map // name => *simC
	m                  *opStats
	bw                 *blockWatch
	pw                 *procWatch
	procs              []*exec.Cmd
	ctx                context.Context
	stop               context.CancelFunc
	days               []dayReport
	fmu                sync.Mutex // the faucet's nonces
	people             map[string]*simPerson
	svc                map[string]*simService
	t0                 time.Time
	st                 *simState
	plain, probeAgents []string
	cands              []string
}

type simC struct {
	mu sync.Mutex
	c  *Client
}

func (s *scaleSim) dir(name string) string { return filepath.Join(s.cfg.root, "ids", name) }

// client: the identity's client, opened once and kept (a long-lived agent).
func (s *scaleSim) client(name string) *simC {
	if v, ok := s.clients.Load(name); ok {
		return v.(*simC)
	}
	c, err := openClient(s.dir(name), s.ch)
	if err != nil {
		log.Printf("open %s: %v", name, err)
		return nil
	}
	if p := s.people[ownerName(name)]; p != nil && len(s.w.Nodes) > 0 {
		c.relay = s.w.Nodes[p.Node%len(s.w.Nodes)]
		c.from = simIP(ownerName(name))
		c.home = []string{c.relay, s.w.Nodes[(p.Node+1)%len(s.w.Nodes)]} // two homes: its own node and the next (all mirror)
	}
	v, _ := s.clients.LoadOrStore(name, &simC{c: c})
	return v.(*simC)
}

// with runs f on the identity's client, one action at a time per identity.
func (s *scaleSim) with(name string, f func(c *Client) error) error {
	sc := s.client(name)
	if sc == nil {
		return fmt.Errorf("no client %s", name)
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return f(sc.c)
}

// simIP: each owner's machine appears from its own address (the data nodes
// limit senders per address, and in this simulation everyone is on loopback).
func simIP(owner string) string {
	h := crypto.Keccak256([]byte(owner))
	return fmt.Sprintf("10.%d.%d.%d", h[0], h[1], h[2]|1)
}

// simCoins: f coins in wei, through its decimal form (a float product can land a wei above what an approval of "1.249000" allows).
func simCoins(f float64) *big.Int {
	v, err := parseCoins(strconv.FormatFloat(f, 'f', 9, 64))
	if err != nil {
		return new(big.Int)
	}
	return v
}

func runSimulate(args []string) error {
	fs := flag.NewFlagSet("simulate", flag.ContinueOnError)
	var cfg scaleCfg
	fs.StringVar(&cfg.root, "root", "sim/scale/run", "state directory")
	fs.StringVar(&cfg.phase, "phase", "all", "setup | days | stress | serve | report | all (setup+days+stress+report) | stop")
	fs.IntVar(&cfg.owners, "owners", 1200, "people (owners)")
	fs.IntVar(&cfg.agents, "agents", 2000, "agents in all (1–3 per owner)")
	fs.IntVar(&cfg.services, "services", 300, "services (agents with cards, capabilities and prices)")
	fs.IntVar(&cfg.communities, "communities", 40, "communities")
	fs.IntVar(&cfg.founders, "founders", 40, "founding citizens")
	fs.IntVar(&cfg.nodes, "nodes", 4, "data nodes")
	fs.Float64Var(&cfg.abuse, "abuse", 0.05, "share of abusive agents")
	fs.IntVar(&cfg.days, "days", 5, "simulated days")
	fs.IntVar(&cfg.port, "port", 19545, "devnet RPC port (admin +1, data nodes +10…)")
	fs.IntVar(&cfg.conc, "conc", 96, "actions in flight")
	fs.Float64Var(&cfg.rate, "rate", 1, "activity multiplier")
	fs.IntVar(&cfg.stressSec, "stress", 60, "seconds per stress step")
	fs.StringVar(&cfg.stressKinds, "stress-kinds", "", "stress only these kinds (mail,pay,voucher)")
	fs.StringVar(&cfg.stressRates, "stress-rates", "", "stress steps instead of the default, e.g. pay=500,1000;mail=800")
	fs.Uint64Var(&cfg.seed, "seed", 7, "random seed")
	fs.BoolVar(&cfg.shareBatches, "share-batches", true, "readers in this process share fetched batches")
	fs.BoolVar(&cfg.tagIndex, "tag-index", false, "readers fetch only batches holding their tags")
	fs.BoolVar(&cfg.keep, "keep", false, "leave the devnet and data nodes running")
	fs.StringVar(&cfg.pprof, "pprof", "", "serve net/http/pprof on this address")
	fs.BoolVar(&cfg.logIndex, "log-index", false, "keep geth's log search index on the devnet (it makes log queries wait for it under load)")
	fs.BoolVar(&cfg.reportScaling, "report-scaling", false, "hide a community post at one report per 50 members (at least 3) instead of 3")
	fs.IntVar(&cfg.bucketBits, "bucket-bits", 5, "tag buckets: 2^N buckets (~families/32); 0: none, every reader reads every batch")
	fs.BoolVar(&cfg.reportWeights, "report-weights", true, "weigh community reports by the reporter's standing, against the community's size (false: 3 reports hide a post)")
	fs.BoolVar(&cfg.fixes, "fixes", true, "the cheap fixes this simulation led to (claims cache, block times, pending cursor, item index, node info cache)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := filepath.Abs(cfg.root)
	if err != nil {
		return err
	}
	cfg.root = root
	os.MkdirAll(root, 0o755)
	s := &scaleSim{cfg: cfg, m: newOpStats(), pw: newProcWatch(), t0: time.Now(), people: map[string]*simPerson{}, svc: map[string]*simService{}}
	s.ctx, s.stop = context.WithCancel(context.Background())
	defer s.stop()
	if cfg.phase == "stop" {
		s.killOld()
		return nil
	}
	if cfg.phase == "report" {
		return s.writeReport()
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		log.Printf("simulate: interrupted; stopping the devnet and nodes")
		if s.bw != nil {
			s.saveMetrics("interrupted")
		}
		s.shutdown()
		os.Exit(1)
	}()
	defer s.shutdown()
	tuneHTTP(s)
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(12 << 30) // thousands of clients in one process: collect harder rather than grow past what the machine has
	}
	if cfg.shareBatches {
		var bc sync.Map
		fetchBatchFn = func(url string, root common.Hash) ([]byte, error) {
			if v, ok := bc.Load(root); ok {
				b := v.([]byte)
				s.m.add("batch.fetch_virtual", 1)
				s.m.add("batch.bytes_virtual", len(b))
				return b, nil
			}
			b, err := fetchBatch(url, root)
			if err == nil {
				bc.Store(root, b)
				s.m.add("batch.fetch_virtual", 1)
				s.m.add("batch.bytes_virtual", len(b))
			}
			return b, err
		}
	}
	perfFixes, daHeads = cfg.fixes, cfg.tagIndex
	callHook = func(contract, method string) { s.m.add("call."+contract+"."+method, 1) }
	longLived = cfg.fixes
	reportScaling = cfg.reportScaling
	reportWeights = cfg.reportWeights
	bucketHook = func(reader string, n int) {
		s.m.add("bucket.bytes", n)
		s.m.add("bucket.requests", 1)
		if fromAgent(reader) {
			s.m.add("bucket.bytes_agent", n)
		} else {
			s.m.add("bucket.bytes_owner", n)
		}
	}
	bucketFallbackHook = func() { s.m.add("bucket.fallback", 1) }
	pendingHook = func(_ string, n int64) {
		if n > 0 {
			s.m.add("pending.bytes", int(n))
		}
		s.m.add("pending.requests", 1)
	}
	if cfg.shareBatches {
		var hc sync.Map
		fetchHeadsFn = func(url string, root common.Hash) ([]byte, error) {
			b, ok := hc.Load(root)
			if !ok {
				x, err := fetchHeads(url, root)
				if err != nil {
					return nil, err
				}
				hc.Store(root, x)
				b = x
			}
			s.m.add("heads.fetch_virtual", 1)
			s.m.add("heads.bytes_virtual", len(b.([]byte)))
			return b.([]byte), nil
		}
	}
	if cfg.pprof != "" {
		go http.ListenAndServe(cfg.pprof, nil)
	}
	fresh := cfg.phase == "all" || cfg.phase == "setup"
	if fresh {
		s.killOld()
		for _, d := range []string{"chain", "ids", "nodes", "metrics"} {
			X := filepath.Join(root, d)
			os.RemoveAll(X)
		}
		os.Remove(filepath.Join(root, "world.json"))
	}
	if err := s.start(fresh); err != nil {
		return err
	}
	switch cfg.phase {
	case "all":
		if err := s.setup(); err != nil {
			return err
		}
		if err := s.runDays(); err != nil {
			return err
		}
		s.stress()
	case "setup":
		return s.setup()
	case "days":
		return s.runDays()
	case "stress":
		s.stress()
	case "serve":
		return s.serve()
	case "ratings":
		s.ratingQuality()
		return nil
	default:
		return fmt.Errorf("unknown phase %q", cfg.phase)
	}
	s.saveMetrics("final")
	return s.writeReport()
}

// tuneHTTP: many identities share this process: keep connections alive, count
// RPC and data-node requests, and let each owner's family appear from its own address.
func tuneHTTP(s *scaleSim) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns, t.MaxIdleConnsPerHost, t.MaxConnsPerHost = 2048, 1024, 0
	s.rpcT = &countingTransport{base: t, lat: func(m string, d time.Duration, err error) { s.m.record(m, d, err) }}
	t2 := t.Clone()
	s.nodeT = &nodeTransport{base: t2, reqs: map[string]int{}, resB: map[string]int64{}, fail: map[string]int{}}
	http.DefaultClient.Transport = s.nodeT
	daHTTP.Transport = s.nodeT
	pushHTTP.Transport = s.nodeT
	httpc.Transport = s.nodeT
	pushHTTP.Timeout = 10 * time.Second
}

// ---- processes

func (s *scaleSim) logFile(name string) *os.File {
	os.MkdirAll(filepath.Join(s.cfg.root, "logs"), 0o755)
	f, _ := os.OpenFile(filepath.Join(s.cfg.root, "logs", name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	return f
}

func (s *scaleSim) spawn(name string, args ...string) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, args...)
	lf := s.logFile(name)
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s.procs = append(s.procs, cmd)
	s.pw.add(name, cmd.Process.Pid)
	os.WriteFile(filepath.Join(s.cfg.root, "logs", name+".pid"), []byte(fmt.Sprint(cmd.Process.Pid)), 0o644)
	return cmd, nil
}

// killOld: processes a previous run left (their pid files).
func (s *scaleSim) killOld() {
	ms, _ := filepath.Glob(filepath.Join(s.cfg.root, "logs", "*.pid"))
	for _, m := range ms {
		b, _ := os.ReadFile(m)
		var pid int
		fmt.Sscan(string(b), &pid)
		if pid > 0 {
			syscall.Kill(pid, syscall.SIGTERM)
		}
		os.Remove(m)
	}
	if len(ms) > 0 {
		time.Sleep(2 * time.Second)
	}
}

func (s *scaleSim) shutdown() {
	if s.cfg.keep {
		return
	}
	for i := len(s.procs) - 1; i >= 0; i-- {
		if p := s.procs[i].Process; p != nil {
			p.Signal(syscall.SIGTERM)
		}
	}
	for _, c := range s.procs {
		done := make(chan struct{})
		go func() { c.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			c.Process.Kill()
		}
	}
	s.procs = nil
	ms, _ := filepath.Glob(filepath.Join(s.cfg.root, "logs", "*.pid"))
	for _, m := range ms {
		os.Remove(m)
	}
}

func (s *scaleSim) netPath() string { return filepath.Join(s.cfg.root, "net.json") }

// start: the devnet (and, once the world has them, the data nodes) as processes.
func (s *scaleSim) start(fresh bool) error {
	r := s.cfg.root
	if !fresh {
		if err := readJSON(filepath.Join(r, "world.json"), &s.w); err != nil {
			return fmt.Errorf("no world in %s (run --phase setup first): %w", r, err)
		}
		s.index()
	} else {
		s.w = &scaleWorld{}
	}
	args := []string{"devnet", "--datadir", filepath.Join(r, "chain"), "--net", s.netPath(),
		"--listen", fmt.Sprintf("127.0.0.1:%d", s.cfg.port), "--admin", fmt.Sprintf("127.0.0.1:%d", s.cfg.port+1),
		"--network", "scale-sim", "--faucet-coins", "20", fmt.Sprintf("--log-index=%v", s.cfg.logIndex), fmt.Sprintf("--bucket-bits=%d", s.cfg.bucketBits)}
	if fresh {
		gen, err := s.makeFounders()
		if err != nil {
			return err
		}
		args = append(args, "--genesis", gen)
	}
	if _, err := s.spawn("devnet", args...); err != nil {
		return err
	}
	var err error
	for i := 0; i < 240; i++ {
		time.Sleep(500 * time.Millisecond)
		if err = s.connect(); err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("devnet did not come up: %w (see %s/logs/devnet.log)", err, r)
	}
	s.bw = newBlockWatch(s.ch)
	s.bw.day.Store(int64(s.w.Day))
	go s.bw.run(s.ctx, s.cl)
	go s.pw.run(s.ctx)
	if !fresh {
		return s.startNodes()
	}
	return nil
}

func (s *scaleSim) connect() error {
	np := s.netPath()
	var f, op DevnetFile
	if err := readJSON(np, &f); err != nil {
		return err
	}
	if err := readJSON(operatorPath(np), &op); err != nil {
		return err
	}
	rc, err := rpc.DialOptions(s.ctx, f.RPC, rpc.WithHTTPClient(&http.Client{Transport: s.rpcT}))
	if err != nil {
		return err
	}
	cl := ethclient.NewClient(rc)
	ch, err := newChain(cl, f.Deployment, nil)
	if err != nil {
		return err
	}
	if _, err := ch.refresh(); err != nil {
		return err
	}
	k, err := crypto.HexToECDSA(op.Faucet)
	if err != nil {
		return err
	}
	allowPrivate = true
	f.Faucet, f.Admin = op.Faucet, op.Admin
	s.ch, s.cl, s.df, s.faucet = ch, cl, f, k
	return nil
}

// advance: chain time jumps (the devnet's admin endpoint).
func (s *scaleSim) advance(d time.Duration) error {
	var last error
	for i := 0; i < 20; i++ {
		resp, err := http.Get(fmt.Sprintf("%s?seconds=%d", s.df.Admin, int(d.Seconds())))
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			return nil
		}
		last = fmt.Errorf("advance: %s", strings.TrimSpace(string(b)))
		time.Sleep(200 * time.Millisecond)
	}
	s.m.record("devnet.advance", 0, last)
	log.Printf("%v", last)
	return last
}

func (s *scaleSim) now() int64 { return chainTime(s.ch) }

// ---- funding: the faucet pays many addresses without waiting for each

func (s *scaleSim) fund(to map[common.Address]*big.Int) error {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	addrs := make([]common.Address, 0, len(to))
	for a := range to {
		addrs = append(addrs, a)
	}
	sort.Slice(addrs, func(i, j int) bool { return addrs[i].Hex() < addrs[j].Hex() })
	from := crypto.PubkeyToAddress(s.faucet.PublicKey)
	ctx := context.Background()
	for len(addrs) > 0 {
		n := min(len(addrs), 400)
		chunk := addrs[:n]
		addrs = addrs[n:]
		nonce, err := s.cl.PendingNonceAt(ctx, from)
		if err != nil {
			return err
		}
		head, err := s.cl.HeaderByNumber(ctx, nil)
		if err != nil {
			return err
		}
		tip := big.NewInt(1e9)
		fee := new(big.Int).Add(tip, new(big.Int).Mul(head.BaseFee, big.NewInt(3)))
		var last *types.Transaction
		for _, a := range chunk {
			a := a
			tx := types.NewTx(&types.DynamicFeeTx{ChainID: s.ch.chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: fee, Gas: 21000, To: &a, Value: to[a]})
			signed, err := types.SignTx(tx, types.LatestSignerForChainID(s.ch.chainID), s.faucet)
			if err != nil {
				return err
			}
			if err := s.cl.SendTransaction(ctx, signed); err != nil {
				return fmt.Errorf("funding %s: %w", a.Hex(), err)
			}
			nonce++
			last = signed
		}
		if last != nil {
			c2, cancel := context.WithTimeout(ctx, 120*time.Second)
			_, err := waitMined(c2, s.cl, last.Hash())
			cancel()
			if err != nil {
				return fmt.Errorf("funding: %w", err)
			}
		}
	}
	return nil
}

// ---- parallel work

// par runs f(i) for i in [0,n) with at most conc in flight.
func (s *scaleSim) par(n int, f func(i int)) {
	s.parN(n, s.cfg.conc, f)
}

func (s *scaleSim) parN(n, conc int, f func(i int)) {
	sem := make(chan struct{}, max(conc, 1))
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			defer func() {
				if r := recover(); r != nil {
					s.m.record("panic", 0, fmt.Errorf("%v", r))
					buf := make([]byte, 4096)
					buf = buf[:runtime.Stack(buf, false)]
					log.Printf("panic: %v\n%s", r, buf)
				}
			}()
			f(i)
		}(i)
	}
	wg.Wait()
}

func (s *scaleSim) saveWorld() {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	writeJSON(filepath.Join(s.cfg.root, "world.json"), s.w)
}

func (s *scaleSim) index() {
	for _, p := range s.w.People {
		s.people[p.Name] = p
	}
	for _, v := range s.w.Services {
		s.svc[v.Agent] = v
	}
}

// pick: a random element.
func pick[T any](xs []T) T { return xs[rand.IntN(len(xs))] }

func chance(p float64) bool { return rand.Float64() < p }

// saveMetrics: what was measured so far, to ROOT/metrics/NAME.json.
func (s *scaleSim) saveMetrics(name string) {
	os.MkdirAll(filepath.Join(s.cfg.root, "metrics"), 0o755)
	s.bw.mu.Lock()
	blocks := append([]blockRec(nil), s.bw.blocks...)
	bym := map[string][2]float64{}
	for k, v := range s.bw.byMethod {
		bym[k] = v
	}
	ftx := map[string]int{}
	for k, v := range s.bw.failedTx {
		ftx[k] = v
	}
	s.bw.mu.Unlock()
	s.nodeT.mu.Lock()
	nreq, nres, nfail := map[string]int{}, map[string]int64{}, map[string]int{}
	for k, v := range s.nodeT.reqs {
		nreq[k] = v
	}
	for k, v := range s.nodeT.resB {
		nres[k] = v
	}
	for k, v := range s.nodeT.fail {
		nfail[k] = v
	}
	s.nodeT.mu.Unlock()
	s.m.mu.Lock()
	cnt := map[string]int{}
	for k, v := range s.m.cnt {
		cnt[k] = v
	}
	vals := map[string][]float64{}
	for k, v := range s.m.val {
		vals[k] = append([]float64(nil), v...)
	}
	s.m.mu.Unlock()
	s.pw.mu.Lock()
	procs := map[string][]procSample{}
	for k, v := range s.pw.samples {
		procs[k] = append([]procSample(nil), v...)
	}
	s.pw.mu.Unlock()
	out := map[string]any{"ops": s.m.summary(), "counters": cnt, "samples": vals, "blocks": blocks, "gas_by_method": bym, "reverted_by_method": ftx,
		"rpc_requests": s.rpcT.reqs.Load(), "rpc_bytes_in": s.rpcT.inBytes.Load(), "rpc_by_method": s.rpcT.byMethod(),
		"node_requests": nreq, "node_bytes": nres, "node_failures": nfail, "procs": procs, "days": s.days, "cfg": map[string]any{
			"owners": s.cfg.owners, "agents": s.cfg.agents, "services": s.cfg.services, "communities": s.cfg.communities, "nodes": s.cfg.nodes,
			"abuse": s.cfg.abuse, "days": s.cfg.days, "conc": s.cfg.conc, "rate": s.cfg.rate, "share_batches": s.cfg.shareBatches, "tag_index": s.cfg.tagIndex, "fixes": s.cfg.fixes,
			"bucket_bits": s.ch.bucketBits(), "report_weights": s.cfg.reportWeights},
		"wall_s": time.Since(s.t0).Seconds()}
	b, _ := json.Marshal(out)
	os.WriteFile(filepath.Join(s.cfg.root, "metrics", name+".json"), b, 0o644)
}

var errSkip = errors.New("skipped")

func init() {
}
