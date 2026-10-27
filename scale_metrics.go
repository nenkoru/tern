//go:build ops

// Measurements for the scale simulation (scale_sim.go): latencies and
// outcomes per operation, blocks (gas, fill, base fee, gas per method), the
// RPC load the clients put on the node, and the processes' CPU, memory and disk.
package main

import (
	"context"
	"encoding/hex"
	"math"
	"math/big"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// ---- operations: latency and outcome

type opStats struct {
	mu   sync.Mutex
	lat  map[string][]float64      // op => seconds (successes)
	ok   map[string]int            // op => successes
	fail map[string]map[string]int // op => error class => count
	ex   map[string]string         // op/class => an example message
	cnt  map[string]int            // free counters (mail sent, vouchers, …)
	val  map[string][]float64      // free samples (batch sizes, bytes, …)
}

func newOpStats() *opStats {
	return &opStats{lat: map[string][]float64{}, ok: map[string]int{}, fail: map[string]map[string]int{}, ex: map[string]string{},
		cnt: map[string]int{}, val: map[string][]float64{}}
}

var hexRe = regexp.MustCompile(`0x[0-9a-fA-F]{6,}|[0-9a-f]{16,}|\b\d+(\.\d+)?\b`)

// errClass: an error message without its numbers and hashes, cut short.
func errClass(err error) string {
	s := hexRe.ReplaceAllString(err.Error(), "N")
	if len(s) > 110 {
		s = s[:110]
	}
	return s
}

// time runs f as operation op and records how long it took and how it ended.
func (m *opStats) time(op string, f func() error) error {
	t0 := time.Now()
	err := f()
	m.record(op, time.Since(t0), err)
	return err
}

func (m *opStats) record(op string, d time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.ok[op]++
		m.lat[op] = append(m.lat[op], d.Seconds())
		return
	}
	cl := errClass(err)
	if m.fail[op] == nil {
		m.fail[op] = map[string]int{}
	}
	m.fail[op][cl]++
	if _, ok := m.ex[op+"|"+cl]; !ok {
		m.ex[op+"|"+cl] = err.Error()
	}
}

func (m *opStats) add(k string, n int) {
	m.mu.Lock()
	m.cnt[k] += n
	m.mu.Unlock()
}

func (m *opStats) sample(k string, v float64) {
	m.mu.Lock()
	m.val[k] = append(m.val[k], v)
	m.mu.Unlock()
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(math.Ceil(p/100*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	t := 0.0
	for _, x := range xs {
		t += x
	}
	return t / float64(len(xs))
}

type opSummary struct {
	Op       string         `json:"op"`
	OK       int            `json:"ok"`
	Failed   int            `json:"failed"`
	FailRate float64        `json:"fail_rate"`
	P50      float64        `json:"p50_s"`
	P95      float64        `json:"p95_s"`
	P99      float64        `json:"p99_s"`
	Max      float64        `json:"max_s"`
	Errors   map[string]int `json:"errors,omitempty"`
	Examples []string       `json:"examples,omitempty"`
}

func r3(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return -1
	}
	return math.Round(f*1000) / 1000
}

func (m *opStats) summary() []opSummary {
	m.mu.Lock()
	defer m.mu.Unlock()
	ops := map[string]bool{}
	for k := range m.ok {
		ops[k] = true
	}
	for k := range m.fail {
		ops[k] = true
	}
	var out []opSummary
	for op := range ops {
		nf := 0
		for _, n := range m.fail[op] {
			nf += n
		}
		l := m.lat[op]
		s := opSummary{Op: op, OK: m.ok[op], Failed: nf, P50: r3(pct(l, 50)), P95: r3(pct(l, 95)), P99: r3(pct(l, 99)), Max: r3(pct(l, 100)), Errors: m.fail[op]}
		if m.ok[op]+nf > 0 {
			s.FailRate = r3(float64(nf) / float64(m.ok[op]+nf))
		}
		for cl := range m.fail[op] {
			if len(s.Examples) < 3 {
				s.Examples = append(s.Examples, m.ex[op+"|"+cl])
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Op < out[j].Op })
	return out
}

// snapshot copies the counters (for per-day differences).
func (m *opStats) snapshot() (map[string]int, map[string]int, map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ok, cnt, fl := map[string]int{}, map[string]int{}, map[string]int{}
	for k, v := range m.ok {
		ok[k] = v
	}
	for k, v := range m.cnt {
		cnt[k] = v
	}
	for k, v := range m.fail {
		for _, n := range v {
			fl[k] += n
		}
	}
	return ok, cnt, fl
}

// ---- RPC load: every JSON-RPC request the clients make, counted

type countingTransport struct {
	base          http.RoundTripper
	reqs, inBytes atomic.Int64
	methods       sync.Map // method => *atomic.Int64
	lat           func(method string, d time.Duration, err error)
}

var rpcMethodRe = regexp.MustCompile(`"method":"([a-zA-Z_]+)"`)

func (t *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.reqs.Add(1)
	method := ""
	t0 := time.Now()
	if r.GetBody != nil {
		if b, err := r.GetBody(); err == nil {
			buf := make([]byte, 256)
			n, _ := b.Read(buf)
			b.Close()
			if m := rpcMethodRe.FindSubmatch(buf[:n]); m != nil {
				method = string(m[1])
				v, _ := t.methods.LoadOrStore(string(m[1]), new(atomic.Int64))
				v.(*atomic.Int64).Add(1)
			}
		}
	}
	resp, err := t.base.RoundTrip(r)
	if err == nil && resp.ContentLength > 0 {
		t.inBytes.Add(resp.ContentLength)
	}
	if t.lat != nil && method != "" && rand.IntN(20) == 0 { // a 5% sample: time to the response's headers
		t.lat("rpc."+method, time.Since(t0), err)
	}
	return resp, err
}

func (t *countingTransport) byMethod() map[string]int64 {
	out := map[string]int64{}
	t.methods.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}

// ---- data node HTTP: what the clients ask of the nodes (posts, batch fetches, pending)

type nodeTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	reqs map[string]int   // path kind => requests
	resB map[string]int64 // path kind => response bytes
	fail map[string]int
}

func nodePathKind(p string) string {
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	return p
}

func (t *nodeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	k := nodePathKind(r.URL.Path)
	resp, err := t.base.RoundTrip(r)
	t.mu.Lock()
	t.reqs[k]++
	if err != nil {
		t.fail[k]++
	} else {
		if resp.StatusCode >= 400 {
			t.fail[k+" "+strconv.Itoa(resp.StatusCode)]++
		}
		if resp.ContentLength > 0 {
			t.resB[k] += resp.ContentLength
		}
	}
	t.mu.Unlock()
	return resp, err
}

// ---- blocks: gas, fill, base fee, transactions by method

type blockRec struct {
	N       uint64  `json:"n"`
	Time    uint64  `json:"time"`
	Wall    float64 `json:"wall"` // unix seconds when seen
	GasUsed uint64  `json:"gas_used"`
	Limit   uint64  `json:"gas_limit"`
	Txs     int     `json:"txs"`
	BaseFee float64 `json:"base_fee_gwei"`
	Day     int     `json:"day"`
}

type blockWatch struct {
	mu       sync.Mutex
	blocks   []blockRec
	byMethod map[string][2]float64 // "Contract.method" => {count, gas}
	failedTx map[string]int        // reverted transactions by method
	sel      map[string]string     // 4-byte selector => Contract.method
	addrName map[common.Address]string
	day      atomic.Int64
	last     uint64
}

func newBlockWatch(ch *Chain) *blockWatch {
	w := &blockWatch{byMethod: map[string][2]float64{}, failedTx: map[string]int{}, sel: map[string]string{}, addrName: map[common.Address]string{}}
	for name, a := range ch.abis {
		for _, m := range a.Methods {
			w.sel[hex.EncodeToString(m.ID)] = name + "." + m.Name
		}
	}
	for name, addr := range ch.dep.Contracts {
		w.addrName[common.HexToAddress(addr)] = name
	}
	return w
}

func (w *blockWatch) method(tx *types.Transaction) string {
	if tx.To() == nil {
		return "create"
	}
	d := tx.Data()
	if len(d) < 4 {
		return "transfer"
	}
	if m, ok := w.sel[hex.EncodeToString(d[:4])]; ok {
		if n, ok := w.addrName[*tx.To()]; ok && !strings.HasPrefix(m, n+".") { // the selector of another contract with the same method
			if _, ok2 := w.sel[hex.EncodeToString(d[:4])]; ok2 {
				return n + "." + strings.SplitN(m, ".", 2)[1]
			}
		}
		return m
	}
	if n, ok := w.addrName[*tx.To()]; ok {
		return n + ".?"
	}
	return "other"
}

// run follows new blocks until ctx ends.
func (w *blockWatch) run(ctx context.Context, cl *ethclient.Client) {
	if h, err := cl.BlockNumber(ctx); err == nil {
		w.last = h
	}
	for ctx.Err() == nil {
		head, err := cl.BlockNumber(ctx)
		if err != nil || head <= w.last {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		for n := w.last + 1; n <= head && ctx.Err() == nil; n++ {
			b, err := cl.BlockByNumber(ctx, new(big.Int).SetUint64(n))
			if err != nil {
				break
			}
			rec := blockRec{N: n, Time: b.Time(), Wall: float64(time.Now().UnixMilli()) / 1000, GasUsed: b.GasUsed(), Limit: b.GasLimit(), Txs: len(b.Transactions()), Day: int(w.day.Load())}
			if bf := b.BaseFee(); bf != nil {
				f, _ := new(big.Float).Quo(new(big.Float).SetInt(bf), big.NewFloat(1e9)).Float64()
				rec.BaseFee = f
			}
			var rcpts []*types.Receipt
			if len(b.Transactions()) > 0 {
				rcpts, _ = cl.BlockReceipts(ctx, rpc.BlockNumberOrHashWithNumber(rpc.BlockNumber(n)))
			}
			w.mu.Lock()
			for i, tx := range b.Transactions() {
				m := w.method(tx)
				v := w.byMethod[m]
				v[0]++
				if i < len(rcpts) {
					v[1] += float64(rcpts[i].GasUsed)
					if rcpts[i].Status == 0 {
						w.failedTx[m]++
					}
				}
				w.byMethod[m] = v
			}
			w.blocks = append(w.blocks, rec)
			w.mu.Unlock()
			w.last = n
		}
	}
}

// ---- processes: CPU seconds and memory of the devnet and the data nodes

type procSample struct {
	Wall   float64 `json:"wall"`
	RSSMB  float64 `json:"rss_mb"`
	CPUSec float64 `json:"cpu_s"`
}

type procWatch struct {
	mu      sync.Mutex
	pids    map[string]int
	samples map[string][]procSample
}

func newProcWatch() *procWatch {
	return &procWatch{pids: map[string]int{}, samples: map[string][]procSample{}}
}

func (p *procWatch) add(name string, pid int) {
	p.mu.Lock()
	p.pids[name] = pid
	p.mu.Unlock()
}

// psTime: "[[dd-]hh:]mm:ss.ss" => seconds.
func psTime(s string) float64 {
	days := 0.0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days, _ = strconv.ParseFloat(d, 64)
		s = rest
	}
	parts := strings.Split(s, ":")
	t := 0.0
	for _, x := range parts {
		v, _ := strconv.ParseFloat(x, 64)
		t = t*60 + v
	}
	return days*86400 + t
}

func (p *procWatch) sampleOnce() {
	p.mu.Lock()
	pids := map[string]int{}
	for k, v := range p.pids {
		pids[k] = v
	}
	p.mu.Unlock()
	if len(pids) == 0 {
		return
	}
	var args []string
	for _, pid := range pids {
		args = append(args, strconv.Itoa(pid))
	}
	out, err := exec.Command("ps", "-o", "pid=,rss=,time=", "-p", strings.Join(args, ",")).Output()
	if err != nil && len(out) == 0 {
		return
	}
	now := float64(time.Now().UnixMilli()) / 1000
	byPid := map[int]procSample{}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		rss, _ := strconv.ParseFloat(f[1], 64)
		byPid[pid] = procSample{Wall: now, RSSMB: rss / 1024, CPUSec: psTime(f[2])}
	}
	p.mu.Lock()
	for name, pid := range pids {
		if s, ok := byPid[pid]; ok {
			p.samples[name] = append(p.samples[name], s)
		}
	}
	p.mu.Unlock()
}

func (p *procWatch) run(ctx context.Context) {
	for ctx.Err() == nil {
		p.sampleOnce()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// between: CPU seconds used and the largest RSS seen in [t0, t1].
func (p *procWatch) between(name string, t0, t1 float64) (cpu, maxRSS float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var first, last *procSample
	for i := range p.samples[name] {
		s := &p.samples[name][i]
		if s.Wall < t0 || s.Wall > t1 {
			continue
		}
		if first == nil {
			first = s
		}
		last = s
		maxRSS = math.Max(maxRSS, s.RSSMB)
	}
	if first != nil {
		cpu = last.CPUSec - first.CPUSec
	}
	return
}

// dirSize in bytes (du -sk).
func dirSize(dir string) int64 {
	out, err := exec.Command("du", "-sk", dir).Output()
	if err != nil {
		var n int64
		filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				n += fi.Size()
			}
			return nil
		})
		return n
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return 0
	}
	kb, _ := strconv.ParseInt(f[0], 10, 64)
	return kb * 1024
}

func mb(b int64) float64 { return math.Round(float64(b)/(1<<20)*10) / 10 }
