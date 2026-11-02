// Networks on a chain someone else runs (Base Sepolia): ternchain's contracts
// on an existing EVM chain. No validators of ours (no Validators contract, no
// genesis), the chain's canonical ERC-4337 EntryPoint, public RPCs that cap
// log queries, and user operations sent to a bundler over the standard
// ERC-4337 JSON-RPC (eth_sendUserOperation), which a data node serves too.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Base Sepolia and the standard contracts already on it (checked on chain).
const (
	baseSepoliaChainID = 84532
	// canonicalEntryPoint: ERC-4337 EntryPoint v0.8 at its deterministic
	// address on every chain that has it (the version the vendored one is).
	canonicalEntryPoint = "0x4337084D9E255Ff0702461CF8895CE9E3b5Ff108"
)

// rangedClient: log queries in windows of at most n blocks (public RPCs refuse
// wider ones), answered from the kept history where it can (logcache.go). An
// RPC that refuses a window gets narrower ones from then on (and the next run
// starts from the width that worked: limits.json).
type rangedClient struct {
	*ethclient.Client
	n     *atomic.Uint64
	key   string // the RPC, hashed (a URL may carry an API key)
	head  *atomic.Pointer[headAt]
	code  *sync.Map      // address → its code (deployed code stays)
	addrs *atomic.Uint64 // the most addresses a log query may name (0: no limit seen)
}

func newRangedClient(c *ethclient.Client, n uint64, url string) rangedClient {
	h := sha256.Sum256([]byte(url))
	r := rangedClient{c, new(atomic.Uint64), hex.EncodeToString(h[:8]), new(atomic.Pointer[headAt]), new(sync.Map), new(atomic.Uint64)}
	r.n.Store(n)
	return r
}

// BlockNumber: a reading under headMemo old (a command asks dozens of
// times in a second; each was a round trip to the RPC).
func (r rangedClient) BlockNumber(ctx context.Context) (uint64, error) {
	if v := r.head.Load(); v != nil && time.Since(v.at) < headMemo {
		return v.n, nil
	}
	n, err := r.Client.BlockNumber(ctx)
	if err == nil {
		r.head.Store(&headAt{n, time.Now()})
	}
	return n, err
}

// blockHash: block n's hash as the RPC reports it (a hash computed from
// the header it returns differs between providers that leave out fields).
func (r rangedClient) blockHash(ctx context.Context, n uint64) (common.Hash, error) {
	var b struct {
		Hash *common.Hash `json:"hash"`
	}
	if err := r.Client.Client().CallContext(ctx, &b, "eth_getBlockByNumber", hexutil.EncodeUint64(n), false); err != nil {
		return common.Hash{}, err
	}
	if b.Hash == nil {
		return common.Hash{}, fmt.Errorf("no block %d", n)
	}
	return *b.Hash, nil
}

func (r rangedClient) freshHead(ctx context.Context) (uint64, error) {
	n, err := r.Client.BlockNumber(ctx)
	if err == nil {
		r.head.Store(&headAt{n, time.Now()})
	}
	return n, err
}

// CodeAt: the latest code of an address that has some, asked once a run
// (deployed code stays; an address without code is asked again).
func (r rangedClient) CodeAt(ctx context.Context, a common.Address, block *big.Int) ([]byte, error) {
	if block == nil {
		if c, ok := r.code.Load(a); ok {
			return c.([]byte), nil
		}
	}
	c, err := r.Client.CodeAt(ctx, a, block)
	if err == nil && block == nil && len(c) > 0 {
		r.code.Store(a, c)
	}
	return c, err
}

var rangeLimitRE = regexp.MustCompile(`(?:limited to a|up to a|max(?:imum)? (?:block )?range(?: of)?|ranges over) (\d+)`)

// rangeErrRE: an RPC refusing a log query for its size, in the words
// providers use (a rate limit is not one: a narrower window wouldn't help).
var (
	rangeErrRE = regexp.MustCompile(`(?i)block range|range (?:is )?too (?:large|wide|big)|too many (?:blocks|results|logs)|more than \d+ (?:results|logs)|exceed|not supported|limit`)
	rateErrRE  = regexp.MustCompile(`(?i)429|too many requests|rate.?limit|capacity|throttl`)
)

// minLogWindow: the narrowest window tried before giving up.
const minLogWindow = 10

func (r rangedClient) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	return cachedFilterLogs(ctx, r, r.fetch, q)
}

func (r rangedClient) window() uint64 { return r.n.Load() }

// fetch: windows of this RPC; a read of the network's whole contracts
// (wide) through net.json's public RPCs instead when this one is a narrow
// home RPC (historyFor).
func (r rangedClient) fetch(ctx context.Context, q ethereum.FilterQuery, wide bool) ([]types.Log, error) {
	r.loadLimits()
	if wide {
		if h := historyFor(r); h != nil {
			return h.fetch(ctx, q, false)
		}
		if first, rest, ok := r.probe(ctx, q); ok { // the home RPC proved narrow: the rest from history
			if h := historyFor(r); h != nil {
				logs, err := h.fetch(ctx, rest, false)
				return append(first, logs...), err
			}
		}
	}
	heavy := false
	for {
		n, k := r.n.Load(), r.addrs.Load()
		logs, err := r.fetchGroups(ctx, q, k)
		if err != nil {
			if lim, ok := addrLimit(err, len(q.Addresses), k); ok {
				r.addrs.Store(lim)
				r.saveLimits()
				continue
			}
			if heavyErrRE.MatchString(err.Error()) { // too much for the RPC at once (dRPC's free plan: 408, 500): fewer addresses, then fewer blocks
				cur := uint64(len(q.Addresses))
				if k > 0 && k < cur {
					cur = k
				}
				if cur > 1 { // kept: the next run starts with groups this RPC can carry
					r.addrs.Store(cur / 2)
					r.saveLimits()
					continue
				}
				if n > minLogWindow && r.n.CompareAndSwap(n, max(n/2, minLogWindow)) {
					heavy = true
					continue
				}
			}
		}
		if r.n.Load() < n && !heavy { // a slow moment is not a limit: not kept
			r.saveLimits()
		}
		return logs, err
	}
}

// probe: a wide read's first window alone, from an RPC whose width is not
// known yet (a narrow one shows it there, not over the whole history); ok:
// the window narrowed, first is its logs and rest the remaining range.
func (r rangedClient) probe(ctx context.Context, q ethereum.FilterQuery) (first []types.Log, rest ethereum.FilterQuery, ok bool) {
	if homeRPC == "" || q.FromBlock == nil || q.ToBlock == nil {
		return nil, q, false
	}
	if _, known := probed.LoadOrStore(r.key, true); known {
		return nil, q, false
	}
	n := r.n.Load()
	from, to := q.FromBlock.Uint64(), q.ToBlock.Uint64()
	if to-from < n {
		return nil, q, false
	}
	for w := n; ; { // one window at a time, narrowed until the RPC answers
		p := q
		p.ToBlock = new(big.Int).SetUint64(from + w - 1)
		logs, err := r.oneQuery(ctx, p)
		if err == nil {
			if w >= n {
				return nil, q, false // as wide as said
			}
			r.saveLimits()
			historyLearned = true
			rest = q
			rest.FromBlock = new(big.Int).SetUint64(from + w)
			return logs, rest, true
		}
		if lim, ok := addrLimit(err, len(q.Addresses), r.addrs.Load()); ok {
			r.addrs.Store(lim)
			continue
		}
		if heavyErrRE.MatchString(err.Error()) { // too much at once: fewer addresses first
			cur := uint64(len(q.Addresses))
			if k := r.addrs.Load(); k > 0 && k < cur {
				cur = k
			}
			if cur > 1 {
				r.addrs.Store(cur / 2)
				continue
			}
		}
		next, ok := narrower(err, w)
		if !ok && heavyErrRE.MatchString(err.Error()) && w/2 >= minLogWindow {
			next, ok = w/2, true
		}
		if !ok {
			return nil, q, false // the whole read shows the error
		}
		w = next
		r.n.Store(w)
	}
}

var probed sync.Map // rpc key → probed this run

// oneQuery: q as one query per group of addresses, not windowed.
func (r rangedClient) oneQuery(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	k := int(r.addrs.Load())
	if k == 0 || len(q.Addresses) <= k {
		return r.Client.FilterLogs(ctx, q)
	}
	var out []types.Log
	for i := 0; i < len(q.Addresses); i += k {
		g := q
		g.Addresses = q.Addresses[i:min(i+k, len(q.Addresses))]
		logs, err := r.Client.FilterLogs(ctx, g)
		if err != nil {
			return nil, err
		}
		out = append(out, logs...)
	}
	sortLogs(out)
	return out, nil
}

// fetchGroups: q's addresses k at a time (0: all at once), oldest first.
func (r rangedClient) fetchGroups(ctx context.Context, q ethereum.FilterQuery, k uint64) ([]types.Log, error) {
	if k == 0 || uint64(len(q.Addresses)) <= k {
		return filterLogsParallel(ctx, r.Client, r.n, q)
	}
	var out []types.Log
	for i := 0; i < len(q.Addresses); i += int(k) {
		g := q
		g.Addresses = q.Addresses[i:min(i+int(k), len(q.Addresses))]
		logs, err := filterLogsParallel(ctx, r.Client, r.n, g)
		if err != nil {
			return nil, err
		}
		out = append(out, logs...)
	}
	sortLogs(out)
	return out, nil
}

var (
	heavyErrRE  = regexp.MustCompile(`(?i)\b408\b|request timeout|query timeout|timed out|temporary internal error`)
	addrErrRE   = regexp.MustCompile(`(?i)address`)
	addrLimitRE = regexp.MustCompile(`(?i)(\d+) addresses`)
)

// addrLimit: fewer addresses a query after one was refused for naming too
// many (publicnode: "A maximum of 10 addresses is allowed"): the number it
// names, else half.
func addrLimit(err error, n int, k uint64) (uint64, bool) {
	s := err.Error()
	if n < 2 || !addrErrRE.MatchString(s) || rateErrRE.MatchString(s) {
		return 0, false
	}
	cur := uint64(n)
	if k > 0 && k < cur {
		cur = k
	}
	if m := addrLimitRE.FindStringSubmatch(s); m != nil {
		if lim, _ := strconv.ParseUint(m[1], 10, 64); lim >= 1 && lim < cur {
			return lim, true
		}
	}
	if cur < 2 {
		return 0, false
	}
	return cur / 2, true
}

// limits.json: the window and the addresses a query each RPC (by hash)
// answered, kept with the network's logs for the next run.
type rpcLimits struct {
	Window uint64 `json:"window"`
	Addrs  uint64 `json:"addresses,omitempty"`
}

var limitsLoaded sync.Map // key → true: read this run

func (r rangedClient) loadLimits() {
	d := mirrorDir()
	if d == "" {
		return
	}
	if _, done := limitsLoaded.LoadOrStore(r.key, true); done {
		return
	}
	var m map[string]rpcLimits
	if readJSON(filepath.Join(d, "limits.json"), &m) == nil {
		l := m[r.key]
		if l.Window >= minLogWindow && l.Window < r.n.Load() {
			r.n.Store(l.Window)
		}
		if l.Addrs > 0 {
			r.addrs.Store(l.Addrs)
		}
	}
}

func (r rangedClient) saveLimits() {
	d := mirrorDir()
	if _, err := os.Stat(filepath.Join(d, "anchor.json")); d == "" || err != nil { // not a folder of this chain's (yet)
		return
	}
	p := filepath.Join(d, "limits.json")
	m := map[string]rpcLimits{}
	readJSON(p, &m)
	m[r.key] = rpcLimits{r.n.Load(), r.addrs.Load()}
	writeJSON(p, m)
}

// filterLogsParallel: a long range in windows, a few at a time (each in
// order of the chain); a short one as filterLogsAdaptive.
func filterLogsParallel(ctx context.Context, b logFilterer, w *atomic.Uint64, q ethereum.FilterQuery) ([]types.Log, error) {
	const par = 4
	n := w.Load()
	if q.BlockHash != nil || q.FromBlock == nil || q.ToBlock == nil || q.FromBlock.Sign() < 0 || q.ToBlock.Sign() < 0 ||
		q.ToBlock.Uint64() < q.FromBlock.Uint64() || q.ToBlock.Uint64()-q.FromBlock.Uint64() < 2*n {
		return filterLogsAdaptive(ctx, b, w, q)
	}
	from, to := q.FromBlock.Uint64(), q.ToBlock.Uint64()
	var chunks [][2]uint64
	for lo := from; lo <= to; lo += n {
		chunks = append(chunks, [2]uint64{lo, min(lo+n-1, to)})
		if lo+n < lo {
			break
		}
	}
	res := make([][]types.Log, len(chunks))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg      sync.WaitGroup
		next    atomic.Int64
		errOnce sync.Once
		first   error
	)
	for range min(par, len(chunks)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(chunks) || ctx.Err() != nil {
					return
				}
				c := q
				c.FromBlock, c.ToBlock = new(big.Int).SetUint64(chunks[i][0]), new(big.Int).SetUint64(chunks[i][1])
				logs, err := filterLogsAdaptive(ctx, b, w, c)
				if err != nil {
					errOnce.Do(func() { first = err; cancel() })
					return
				}
				res[i] = logs
			}
		}()
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	var out []types.Log
	for _, r := range res {
		out = append(out, r...)
	}
	return out, nil
}

// filterLogsAdaptive: windows of n blocks, narrowed when the RPC refuses
// one for its size (and kept narrowed for the next queries).
func filterLogsAdaptive(ctx context.Context, b logFilterer, w *atomic.Uint64, q ethereum.FilterQuery) ([]types.Log, error) {
	for {
		n := w.Load()
		logs, err := filterLogsRanged(ctx, b, n, q)
		if err == nil {
			return logs, nil
		}
		next, ok := narrower(err, n)
		if !ok {
			return nil, err
		}
		w.CompareAndSwap(n, next)
	}
}

// narrower: the window after one was refused for its size: the RPC's own
// limit when it names a smaller one (counted both ends), else half: some name
// a limit they don't keep (dRPC's free plan says "ranges over 10000 blocks
// are not supported" and answers about 100).
func narrower(err error, n uint64) (uint64, bool) {
	s := err.Error()
	if n <= minLogWindow || rateErrRE.MatchString(s) || addrErrRE.MatchString(s) || heavyErrRE.MatchString(s) {
		return 0, false
	}
	if m := rangeLimitRE.FindStringSubmatch(s); m != nil {
		if lim, _ := strconv.ParseUint(m[1], 10, 64); lim > 1 && lim-1 < n {
			return max(lim-1, minLogWindow), true
		}
		return max(n/2, minLogWindow), true
	}
	if rangeErrRE.MatchString(s) {
		return max(n/2, minLogWindow), true
	}
	return 0, false
}

type logFilterer interface {
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
	BlockNumber(ctx context.Context) (uint64, error)
}

// filterLogsRanged: q in windows of n blocks, in order.
func filterLogsRanged(ctx context.Context, b logFilterer, n uint64, q ethereum.FilterQuery) ([]types.Log, error) {
	if n == 0 || q.BlockHash != nil || (q.FromBlock != nil && q.FromBlock.Sign() < 0) {
		return b.FilterLogs(ctx, q)
	}
	var from, to uint64
	if q.FromBlock != nil {
		from = q.FromBlock.Uint64()
	}
	if q.ToBlock == nil || q.ToBlock.Sign() < 0 { // latest (or a tag): the head now
		h, err := b.BlockNumber(ctx)
		if err != nil {
			return nil, err
		}
		to = h
	} else {
		to = q.ToBlock.Uint64()
	}
	if to < from || to-from < n {
		return b.FilterLogs(ctx, q)
	}
	var out []types.Log
	for lo := from; lo <= to; lo += n {
		hi := min(lo+n-1, to)
		w := q
		w.FromBlock, w.ToBlock = new(big.Int).SetUint64(lo), new(big.Int).SetUint64(hi)
		logs, err := b.FilterLogs(ctx, w)
		if err != nil {
			return nil, err
		}
		out = append(out, logs...)
	}
	return out, nil
}

// retryRPC: public RPCs answer bursts with 429 (or a 5xx); one such answer
// among the hundreds of log windows a scan from the deployment block needs
// used to fail the whole command. Retried with backoff (Retry-After when
// given), the request body replayed; the default transport at call time,
// so --proxy still applies.
type retryRPC struct{}

func (retryRPC) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	if rpcStatsMode != "" { // TERNCHAIN_RPC_STATS (rpcstats.go)
		body, t, try := reqBody(req), time.Now(), 0
		defer func() { countRPC(body, time.Since(t), try) }()
		return retryRT(req, &try)
	}
	return retryRT(req, new(int))
}

var retrySaysRE = regexp.MustCompile(`(?i)retry|temporar`)

func retryRT(req *http.Request, tries *int) (*http.Response, error) {
	for try := 0; ; try++ {
		*tries = try
		if try > 0 && req.GetBody != nil {
			b, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = b
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil || try >= 5 || req.GetBody == nil {
			return resp, err
		}
		switch resp.StatusCode {
		case http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		case http.StatusInternalServerError: // only when the RPC says to retry (dRPC: "Temporary internal error. Please retry")
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if !retrySaysRE.Match(b) || try >= 2 { // and twice at most: then it's the query (fewer addresses: rangedClient.fetch)
				resp.Body = io.NopCloser(bytes.NewReader(b))
				return resp, nil
			}
			resp.Body = io.NopCloser(bytes.NewReader(nil))
		default:
			return resp, nil
		}
		wait := time.Duration(1<<try) * time.Second
		if s, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && s > 0 && s <= 30 {
			wait = time.Duration(s) * time.Second
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(wait):
		}
	}
}

// ---- ERC-4337 JSON-RPC (v0.7/v0.8 user operations, unpacked)

type rpcUserOp struct {
	Sender                        common.Address  `json:"sender"`
	Nonce                         *hexutil.Big    `json:"nonce"`
	Factory                       *common.Address `json:"factory,omitempty"`
	FactoryData                   hexutil.Bytes   `json:"factoryData,omitempty"`
	CallData                      hexutil.Bytes   `json:"callData"`
	CallGasLimit                  *hexutil.Big    `json:"callGasLimit"`
	VerificationGasLimit          *hexutil.Big    `json:"verificationGasLimit"`
	PreVerificationGas            *hexutil.Big    `json:"preVerificationGas"`
	MaxFeePerGas                  *hexutil.Big    `json:"maxFeePerGas"`
	MaxPriorityFeePerGas          *hexutil.Big    `json:"maxPriorityFeePerGas"`
	Paymaster                     *common.Address `json:"paymaster,omitempty"`
	PaymasterVerificationGasLimit *hexutil.Big    `json:"paymasterVerificationGasLimit,omitempty"`
	PaymasterPostOpGasLimit       *hexutil.Big    `json:"paymasterPostOpGasLimit,omitempty"`
	PaymasterData                 hexutil.Bytes   `json:"paymasterData,omitempty"`
	Signature                     hexutil.Bytes   `json:"signature"`
}

func unpack128(b [32]byte) (hi, lo *big.Int) {
	return new(big.Int).SetBytes(b[:16]), new(big.Int).SetBytes(b[16:])
}

func pack128Big(hi, lo *big.Int) (out [32]byte) {
	if hi != nil {
		hi.FillBytes(out[0:16])
	}
	if lo != nil {
		lo.FillBytes(out[16:32])
	}
	return
}

func hb(x *big.Int) *hexutil.Big { return (*hexutil.Big)(new(big.Int).Set(x)) }

func bigOf(x *hexutil.Big) *big.Int {
	if x == nil {
		return new(big.Int)
	}
	return x.ToInt()
}

func toRPCOp(op packedUserOp) rpcUserOp {
	vgas, cgas := unpack128(op.AccountGasLimits)
	tip, maxFee := unpack128(op.GasFees)
	r := rpcUserOp{Sender: op.Sender, Nonce: hb(op.Nonce), CallData: op.CallData, CallGasLimit: hb(cgas), VerificationGasLimit: hb(vgas),
		PreVerificationGas: hb(op.PreVerificationGas), MaxFeePerGas: hb(maxFee), MaxPriorityFeePerGas: hb(tip), Signature: op.Signature}
	if len(op.InitCode) >= 20 {
		f := common.BytesToAddress(op.InitCode[:20])
		r.Factory, r.FactoryData = &f, op.InitCode[20:]
	}
	if len(op.PaymasterAndData) >= 52 {
		pm := common.BytesToAddress(op.PaymasterAndData[:20])
		r.Paymaster = &pm
		r.PaymasterVerificationGasLimit = hb(new(big.Int).SetBytes(op.PaymasterAndData[20:36]))
		r.PaymasterPostOpGasLimit = hb(new(big.Int).SetBytes(op.PaymasterAndData[36:52]))
		r.PaymasterData = op.PaymasterAndData[52:]
	}
	return r
}

func fromRPCOp(r rpcUserOp) packedUserOp {
	op := packedUserOp{Sender: r.Sender, Nonce: bigOf(r.Nonce), CallData: r.CallData, PreVerificationGas: bigOf(r.PreVerificationGas),
		AccountGasLimits: pack128Big(bigOf(r.VerificationGasLimit), bigOf(r.CallGasLimit)),
		GasFees:          pack128Big(bigOf(r.MaxPriorityFeePerGas), bigOf(r.MaxFeePerGas)), Signature: r.Signature}
	if r.Factory != nil {
		op.InitCode = append(r.Factory.Bytes(), r.FactoryData...)
	}
	if r.Paymaster != nil {
		gl := pack128Big(bigOf(r.PaymasterVerificationGasLimit), bigOf(r.PaymasterPostOpGasLimit))
		op.PaymasterAndData = append(append(r.Paymaster.Bytes(), gl[:]...), r.PaymasterData...)
	}
	if op.Nonce == nil {
		op.Nonce = new(big.Int)
	}
	return op
}

type rpcOpReceipt struct {
	UserOpHash common.Hash `json:"userOpHash"`
	Success    bool        `json:"success"`
	Reason     string      `json:"reason,omitempty"`
	Receipt    struct {
		TransactionHash common.Hash `json:"transactionHash"`
	} `json:"receipt"`
}

// submitRemote: one user operation through the network's bundler (net.json
// "bundler"), then its outcome from the transaction that carried it.
func (ch *Chain) submitRemote(op packedUserOp) (*types.Receipt, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, err := rpc.DialOptions(ctx, ch.bundlerURL, rpc.WithHTTPClient(rpcHTTP()))
	if err != nil {
		return nil, fmt.Errorf("bundler %s: %w", ch.bundlerURL, err)
	}
	defer c.Close()
	var h common.Hash
	if err := c.CallContext(ctx, &h, "eth_sendUserOperation", toRPCOp(op), ch.addr("EntryPoint")); err != nil {
		return nil, ch.entryPointError(err)
	}
	if ch.mine != nil {
		ch.mine()
	}
	delay := 200 * time.Millisecond
	for {
		var r *rpcOpReceipt
		if err := c.CallContext(ctx, &r, "eth_getUserOperationReceipt", h); err == nil && r != nil {
			rcpt, err := ch.b.TransactionReceipt(ctx, r.Receipt.TransactionHash)
			if err != nil {
				return nil, fmt.Errorf("user operation %s in %s: %w", h.Hex(), r.Receipt.TransactionHash.Hex(), err)
			}
			if err := ch.opOutcome(rcpt, h); err != nil {
				return rcpt, err
			}
			if !r.Success {
				return rcpt, errors.New("the operation reverted")
			}
			return rcpt, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("user operation %s: not on chain within 90 s (bundler %s)", h.Hex(), ch.bundlerURL)
		case <-time.After(delay):
		}
		delay = min(delay*2, 2*time.Second)
	}
}

// opOutcome: the operation's failure in a bundle's receipt (the bundle may
// carry others' operations: only events for this hash count; zero hash: any).
func (ch *Chain) opOutcome(rcpt *types.Receipt, h common.Hash) error {
	ep := ch.addr("EntryPoint")
	evs := ch.abis["EntryPoint"].Events
	for _, l := range rcpt.Logs {
		if len(l.Topics) < 2 || l.Address != ep || (h != (common.Hash{}) && l.Topics[1] != h) {
			continue
		}
		switch l.Topics[0] {
		case evs["UserOperationRevertReason"].ID:
			v, err := evs["UserOperationRevertReason"].Inputs.NonIndexed().Unpack(l.Data)
			if err == nil && len(v) == 2 {
				return errors.New(decodeRevert(v[1].([]byte)))
			}
		case evs["UserOperationEvent"].ID:
			v, err := evs["UserOperationEvent"].Inputs.NonIndexed().Unpack(l.Data)
			if err == nil && len(v) >= 2 && !v[1].(bool) {
				return errors.New("the operation reverted")
			}
		}
	}
	return nil
}

// ---- the bundler a data node serves (relay serve --bundler): POST /bundler

type bundlerServer struct {
	ch    *Chain
	key   *ecdsa.PrivateKey
	admit func(*http.Request) string // rate limit ("" = go on)
	mu    sync.Mutex
	done  map[common.Hash]rpcOpReceipt
	order []common.Hash
}

type rpcReq struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      json.RawMessage   `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *bundlerServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "ERC-4337 bundler: POST JSON-RPC (eth_sendUserOperation, eth_getUserOperationReceipt, eth_supportedEntryPoints, eth_chainId)", 405)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(req.Body, 256<<10))
	var r rpcReq
	if err := json.Unmarshal(body, &r); err != nil {
		writeRPC(w, nil, nil, &rpcErr{-32700, "parse error (one request per call; no batches)"})
		return
	}
	res, e := s.call(req, r)
	writeRPC(w, r.ID, res, e)
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, res any, e *rpcErr) {
	if id == nil {
		id = json.RawMessage("null")
	}
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		out["error"] = e
	} else {
		out["result"] = res
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *bundlerServer) call(req *http.Request, r rpcReq) (any, *rpcErr) {
	ep := s.ch.addr("EntryPoint")
	switch r.Method {
	case "eth_chainId":
		return hb(s.ch.chainID), nil
	case "eth_supportedEntryPoints":
		return []common.Address{ep}, nil
	case "eth_getUserOperationReceipt":
		var h common.Hash
		if len(r.Params) < 1 || json.Unmarshal(r.Params[0], &h) != nil {
			return nil, &rpcErr{-32602, "params: [userOpHash]"}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if v, ok := s.done[h]; ok {
			return v, nil
		}
		return nil, nil
	case "eth_sendUserOperation":
		var op rpcUserOp
		var want common.Address
		if len(r.Params) < 2 || json.Unmarshal(r.Params[0], &op) != nil || json.Unmarshal(r.Params[1], &want) != nil {
			return nil, &rpcErr{-32602, "params: [userOperation, entryPoint]"}
		}
		if want != ep {
			return nil, &rpcErr{-32602, "this bundler serves EntryPoint " + ep.Hex()}
		}
		if msg := s.admit(req); msg != "" {
			return nil, &rpcErr{-32504, msg}
		}
		p := fromRPCOp(op)
		hv, err := s.ch.call("EntryPoint", "getUserOpHash", p)
		if err != nil {
			return nil, &rpcErr{-32602, err.Error()}
		}
		h := common.Hash(hv[0].([32]byte))
		rcpt, err := s.ch.submitOpsWith(s.key, []packedUserOp{p}) // simulated first: a failing validation costs the bundler nothing
		if err != nil && rcpt == nil {
			return nil, &rpcErr{-32500, err.Error()}
		}
		v := rpcOpReceipt{UserOpHash: h, Success: err == nil}
		if err != nil {
			v.Reason = err.Error()
		}
		v.Receipt.TransactionHash = rcpt.TxHash
		s.mu.Lock()
		s.done[h] = v
		s.order = append(s.order, h)
		if len(s.order) > 10000 { // receipts are asked for right after: keep the last ones
			delete(s.done, s.order[0])
			s.order = s.order[1:]
		}
		s.mu.Unlock()
		return h, nil
	}
	return nil, &rpcErr{-32601, "method not supported: " + r.Method}
}

func newBundlerServer(ch *Chain, key *ecdsa.PrivateKey, admit func(*http.Request) string) *bundlerServer {
	return &bundlerServer{ch: ch, key: key, admit: admit, done: map[common.Hash]rpcOpReceipt{}}
}

// tipCap: what a transaction or user operation offers its block producer.
// A chain of our own: 1 gwei (its base fee decays to a few wei). A chain
// someone else runs: what it suggests (Base: about 0.001 gwei, where a 1 gwei
// tip would cost a thousand times the going rate).
func (ch *Chain) tipCap() uint64 {
	const ownTip = 1_000_000_000
	if !ch.hosted {
		return ownTip
	}
	if t, err := ch.b.SuggestGasTipCap(ctxb()); err == nil && t.IsUint64() {
		return max(t.Uint64(), 1)
	}
	return ownTip
}

// checkEntryPoint: an existing EntryPoint for deploy --entry-point: code there,
// and a v0.8 one (its user operation hash is EIP-712, domain "ERC4337" "1",
// which the client's signatures and the accounts expect).
func checkEntryPoint(b Backend, a common.Address) error {
	code, err := b.CodeAt(ctxb(), a, nil)
	if err != nil {
		return err
	}
	if len(code) == 0 {
		return fmt.Errorf("--entry-point %s: no code there", a.Hex())
	}
	ea, err := loadArtifact("EntryPoint")
	if err != nil {
		return err
	}
	data, _ := ea.abi.Pack("eip712Domain")
	out, err := b.CallContract(ctxb(), ethereum.CallMsg{To: &a, Data: data}, nil)
	if err != nil {
		return fmt.Errorf("--entry-point %s: not an EntryPoint v0.8 (no eip712Domain): %w", a.Hex(), err)
	}
	v, err := ea.abi.Unpack("eip712Domain", out)
	if err != nil || len(v) < 3 || v[1] != "ERC4337" || v[2] != "1" {
		return fmt.Errorf("--entry-point %s: not an EntryPoint v0.8 (EIP-712 domain %v)", a.Hex(), v)
	}
	return nil
}

// bundlerGasPrice: a remote bundler's own gas price (Pimlico's "standard"),
// when this process sends through one that has it.
func (ch *Chain) bundlerGasPrice() (tip, maxFee uint64, ok bool) {
	if ch.bundler != nil || ch.bundlerURL == "" {
		return 0, 0, false
	}
	var r struct {
		Standard struct {
			MaxFeePerGas         *hexutil.Big `json:"maxFeePerGas"`
			MaxPriorityFeePerGas *hexutil.Big `json:"maxPriorityFeePerGas"`
		} `json:"standard"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := rpc.DialOptions(ctx, ch.bundlerURL, rpc.WithHTTPClient(rpcHTTP()))
	if err != nil {
		return 0, 0, false
	}
	defer c.Close()
	if err := c.CallContext(ctx, &r, "pimlico_getUserOperationGasPrice"); err != nil || r.Standard.MaxFeePerGas == nil || r.Standard.MaxPriorityFeePerGas == nil {
		return 0, 0, false
	}
	return r.Standard.MaxPriorityFeePerGas.ToInt().Uint64(), r.Standard.MaxFeePerGas.ToInt().Uint64(), true
}

type rpcGasEstimate struct {
	PreVerificationGas            *hexutil.Big `json:"preVerificationGas"`
	VerificationGasLimit          *hexutil.Big `json:"verificationGasLimit"`
	CallGasLimit                  *hexutil.Big `json:"callGasLimit"`
	PaymasterVerificationGasLimit *hexutil.Big `json:"paymasterVerificationGasLimit"`
	PaymasterPostOpGasLimit       *hexutil.Big `json:"paymasterPostOpGasLimit"`
}

func bigOrNil(x *hexutil.Big) *big.Int {
	if x == nil {
		return nil
	}
	return x.ToInt()
}

// dummySignature: a well-formed ECDSA signature that recovers to nobody: an
// estimate needs one (validation fails softly, not by reverting).
var dummySignature = append(bytes.Repeat([]byte{0xff}, 64), 0x1c)

// bundlerEstimate: a remote bundler's eth_estimateUserOperationGas for op.
func (ch *Chain) bundlerEstimate(op packedUserOp) (*rpcGasEstimate, bool) {
	if ch.bundler != nil || ch.bundlerURL == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := rpc.DialOptions(ctx, ch.bundlerURL, rpc.WithHTTPClient(rpcHTTP()))
	if err != nil {
		return nil, false
	}
	defer c.Close()
	op.Signature = dummySignature
	var est rpcGasEstimate
	if err := c.CallContext(ctx, &est, "eth_estimateUserOperationGas", toRPCOp(op), ch.addr("EntryPoint")); err != nil || est.PreVerificationGas == nil {
		return nil, false
	}
	return &est, true
}
