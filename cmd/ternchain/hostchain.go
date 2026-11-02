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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
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
// wider ones). An RPC that says its own limit ("limited to a 200 range",
// sepolia.base.org) gets windows that size from then on.
type rangedClient struct {
	*ethclient.Client
	n *atomic.Uint64
}

func newRangedClient(c *ethclient.Client, n uint64) rangedClient {
	r := rangedClient{c, new(atomic.Uint64)}
	r.n.Store(n)
	return r
}

var rangeLimitRE = regexp.MustCompile(`(?:limited to a|up to a|max(?:imum)? (?:block )?range(?: of)?|ranges over) (\d+)`)

func (r rangedClient) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	return filterLogsAdaptive(ctx, r.Client, r.n, q)
}

// filterLogsAdaptive: windows of n blocks, narrowed to the RPC's own limit
// when it names one (and kept narrowed for the next queries).
func filterLogsAdaptive(ctx context.Context, b logFilterer, w *atomic.Uint64, q ethereum.FilterQuery) ([]types.Log, error) {
	for {
		n := w.Load()
		logs, err := filterLogsRanged(ctx, b, n, q)
		if err == nil {
			return logs, nil
		}
		m := rangeLimitRE.FindStringSubmatch(err.Error())
		if m == nil {
			return nil, err
		}
		lim, _ := strconv.ParseUint(m[1], 10, 64)
		if lim <= 1 || lim-1 >= n {
			return nil, err
		}
		w.CompareAndSwap(n, lim-1) // the RPC's limit counts both ends
	}
}

type logFilterer interface {
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
	BlockNumber(ctx context.Context) (uint64, error)
}

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
	// Windows deep below the head never change: kept between runs (logcache.go).
	key, safe := "", uint64(0)
	if logCacheDir != "" {
		head := to
		if q.ToBlock != nil && q.ToBlock.Sign() >= 0 {
			head, _ = b.BlockNumber(ctx)
		}
		if head > logFinality {
			key, safe = logQueryKey(q), head-logFinality
		}
	}
	added := false
	if key != "" {
		defer func() { // also after an error: what was read stays read
			if added {
				logCache.Lock()
				saveWindows(key, cachedWindows(key))
				logCache.Unlock()
			}
		}()
	}
	var out []types.Log
	for lo := from; lo <= to; lo += n {
		hi := min(lo+n-1, to)
		if key != "" {
			logCache.Lock()
			logs, ok := cachedWindows(key)[logWindow(lo, hi)]
			logCache.Unlock()
			if ok {
				out = append(out, logs...)
				continue
			}
		}
		w := q
		w.FromBlock, w.ToBlock = new(big.Int).SetUint64(lo), new(big.Int).SetUint64(hi)
		logs, err := b.FilterLogs(ctx, w)
		if err != nil {
			return nil, err
		}
		if key != "" && hi <= safe {
			logCache.Lock()
			cachedWindows(key)[logWindow(lo, hi)] = logs
			logCache.Unlock()
			added = true
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

func (retryRPC) RoundTrip(req *http.Request) (*http.Response, error) {
	for try := 0; ; try++ {
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
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
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
	c, err := rpc.DialContext(ctx, ch.bundlerURL)
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
	c, err := rpc.DialContext(ctx, ch.bundlerURL)
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
	c, err := rpc.DialContext(ctx, ch.bundlerURL)
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
