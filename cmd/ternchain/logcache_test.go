package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

var (
	lcA = common.HexToAddress("0x0a") // a contract of the network (kept whole)
	lcB = common.HexToAddress("0x0b")
	lcX = common.HexToAddress("0xee") // someone else's (a cursor per query)
)

// logChain: head blocks, in each one log of A (topic 1 or 2, by parity), one
// of B and one of X; the fork number changes every log (a reorganisation or
// a reset chain), and the block hash the anchor reads.
type logChain struct {
	mu     sync.Mutex
	head   uint64
	fork   map[uint64]byte // block → its fork (0 by default)
	limit  uint64          // a wider query is refused, dRPC's way (0: none)
	calls  [][2]uint64
	addrs  [][]common.Address
	anchor byte
}

func (c *logChain) BlockNumber(context.Context) (uint64, error) { return c.head, nil }

func (c *logChain) FilterLogs(_ context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	from, to := q.FromBlock.Uint64(), c.head
	if q.ToBlock != nil {
		to = q.ToBlock.Uint64()
	}
	if c.limit > 0 && to-from+1 > c.limit {
		return nil, errors.New(`400 Bad Request: {"code":35,"message":"ranges over 10000 blocks are not supported on free plan"}`)
	}
	c.calls = append(c.calls, [2]uint64{from, to})
	c.addrs = append(c.addrs, q.Addresses)
	var out []types.Log
	for n := from; n <= min(to, c.head); n++ {
		f := c.fork[n]
		for i, a := range []common.Address{lcA, lcB, lcX} {
			l := types.Log{Address: a, Topics: []common.Hash{{byte(1 + n%2)}, {byte(n)}}, Data: []byte{f}, BlockNumber: n,
				TxHash: common.Hash{byte(n), byte(n >> 8), f}, Index: uint(i)}
			if (len(q.Addresses) == 0 || lcHas(q.Addresses, a)) && matchTopics(l, q.Topics) {
				out = append(out, l)
			}
		}
	}
	return out, nil
}

func lcHas(xs []common.Address, a common.Address) bool {
	for _, x := range xs {
		if x == a {
			return true
		}
	}
	return false
}

func (c *logChain) anchorFn(ctx context.Context) (logAnchorFile, error) {
	return logAnchorFile{Chain: 1, Block: 1, Hash: common.Hash{c.anchor}.Hex()}, nil
}

// run: a new run of the CLI over this chain (memory empty, the folder kept).
func (c *logChain) run(t *testing.T, dir string) func(q ethereum.FilterQuery) []types.Log {
	configureLogStore(dir, "", []common.Address{lcA, lcB}, 1, c.anchorFn)
	logStore.Lock()
	forgetLogs()
	logStore.Unlock()
	w := new(atomic.Uint64)
	w.Store(10_000)
	r := c
	fetch := func(ctx context.Context, q ethereum.FilterQuery, wide bool) ([]types.Log, error) {
		return filterLogsParallel(ctx, r, w, q)
	}
	c.calls, c.addrs = nil, nil
	return func(q ethereum.FilterQuery) []types.Log {
		t.Helper()
		logs, err := cachedFilterLogs(context.Background(), c, fetch, q)
		if err != nil {
			t.Fatal(err)
		}
		return logs
	}
}

func lq(from uint64, addrs []common.Address, topics ...[]common.Hash) ethereum.FilterQuery {
	return ethereum.FilterQuery{FromBlock: new(big.Int).SetUint64(from), Addresses: addrs, Topics: topics}
}

func sameLogs(t *testing.T, got, want []types.Log) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d logs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].BlockNumber != want[i].BlockNumber || got[i].TxHash != want[i].TxHash || got[i].Address != want[i].Address {
			t.Fatalf("log %d: %d %s %x, want %d %s %x", i, got[i].BlockNumber, got[i].Address, got[i].TxHash, want[i].BlockNumber, want[i].Address, want[i].TxHash)
		}
	}
}

// The RPC's answer to q, without any cache.
func direct(t *testing.T, c *logChain, qq ethereum.FilterQuery) []types.Log {
	t.Helper()
	qq.ToBlock = new(big.Int).SetUint64(c.head)
	calls, addrs := c.calls, c.addrs
	defer func() { c.calls, c.addrs = calls, addrs }()
	logs, err := c.FilterLogs(context.Background(), qq)
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

// A run after the first reads only from its cursor (the last reorgMargin
// blocks again), and answers topic queries of the network's contracts from
// what it keeps, the same as the RPC would.
func TestLogCursorResume(t *testing.T) {
	defer configureLogStore("", "", nil, 0, nil)
	dir := t.TempDir()
	c := &logChain{head: 25_000, fork: map[uint64]byte{}}
	get := c.run(t, dir)
	mine := []common.Hash{{byte(7)}}
	topical := lq(1, []common.Address{lcA}, nil, mine) // a user's topic
	sameLogs(t, get(topical), direct(t, c, topical))
	if len(c.calls) != 3 {
		t.Fatalf("first run: %d queries (%v), want 3 windows", len(c.calls), c.calls)
	}
	for _, as := range c.addrs { // the network's contracts read whole, together: no topic of the user's went out
		if len(as) != 2 || as[0] != lcA || as[1] != lcB {
			t.Fatalf("query addresses %v", as)
		}
	}
	// same run: other topics, other contracts of the network: nothing to read
	c.calls, c.addrs = nil, nil
	sameLogs(t, get(lq(1, []common.Address{lcA}, []common.Hash{{2}})), direct(t, c, lq(1, []common.Address{lcA}, []common.Hash{{2}})))
	if len(c.calls) != 0 {
		t.Fatalf("same run, same head: %v", c.calls)
	}
	both := lq(100, []common.Address{lcB, lcA})
	sameLogs(t, get(both), direct(t, c, both))
	if len(c.calls) != 0 {
		t.Fatalf("B read again: %v %v", c.calls, c.addrs)
	}
	// next run, 500 blocks later: one query for both from the cursor (head-64+1 of the last run)
	c.head = 25_500
	get = c.run(t, dir)
	sameLogs(t, get(both), direct(t, c, both))
	if len(c.calls) != 1 || c.calls[0] != [2]uint64{25_000 - reorgMargin + 1, 25_500} || len(c.addrs[0]) != 2 {
		t.Fatalf("resumed: %v %v", c.calls, c.addrs)
	}
	sameLogs(t, get(topical), direct(t, c, topical))
	// a cursor per query for someone else's contract: the same query, fewer blocks
	other := lq(10_000, []common.Address{lcX}, nil, mine)
	c.calls, c.addrs = nil, nil
	sameLogs(t, get(other), direct(t, c, other))
	if len(c.calls) != 2 || c.addrs[0][0] != lcX {
		t.Fatalf("other contract: %v", c.calls)
	}
	c.head = 25_600
	get = c.run(t, dir)
	sameLogs(t, get(other), direct(t, c, other))
	if len(c.calls) != 1 || c.calls[0][0] != 25_500-reorgMargin+1 {
		t.Fatalf("other contract resumed: %v", c.calls)
	}
	// asked from earlier than kept: the blocks before are read once, then kept
	earlier := lq(5_000, []common.Address{lcX}, nil, mine)
	c.calls, c.addrs = nil, nil
	sameLogs(t, get(earlier), direct(t, c, earlier))
	if len(c.calls) != 1 || c.calls[0] != [2]uint64{5_000, 9_999} {
		t.Fatalf("earlier: %v", c.calls)
	}
	// "to" bounds hold
	part := ethereum.FilterQuery{FromBlock: big.NewInt(300), ToBlock: big.NewInt(400), Addresses: []common.Address{lcA}}
	var want []types.Log
	for _, l := range direct(t, c, lq(300, []common.Address{lcA})) {
		if l.BlockNumber <= 400 {
			want = append(want, l)
		}
	}
	sameLogs(t, get(part), want)
}

// Logs near the head are read again by the next run: a reorganisation there
// shows; deeper than the margin, what was kept stands.
func TestLogCursorReorgMargin(t *testing.T) {
	defer configureLogStore("", "", nil, 0, nil)
	dir := t.TempDir()
	c := &logChain{head: 1000, fork: map[uint64]byte{}}
	all := lq(1, []common.Address{lcA})
	get := c.run(t, dir)
	get(all)
	var kept logEntry
	if err := readJSON(filepath.Join(dir, "a-"+fmt.Sprintf("%040x", lcA.Bytes())+".json"), &kept); err != nil {
		t.Fatal(err)
	}
	if kept.Next != 1000-reorgMargin+1 || len(kept.Logs) != int(1000-reorgMargin) {
		t.Fatalf("kept up to %d (%d logs), want %d", kept.Next, len(kept.Logs), 1000-reorgMargin+1)
	}
	for n := uint64(1000 - reorgMargin + 1); n <= 1000; n++ { // the near-head blocks change
		c.fork[n] = 1
	}
	get = c.run(t, dir)
	sameLogs(t, get(all), direct(t, c, all))
}

// Another chain under the same folder (a reset devnet: another deployment
// block hash) empties it; a folder of no chain is not used.
func TestLogCursorChainReset(t *testing.T) {
	defer configureLogStore("", "", nil, 0, nil)
	dir := t.TempDir()
	c := &logChain{head: 30_000, fork: map[uint64]byte{}}
	all := lq(1, []common.Address{lcA})
	c.run(t, dir)(all)
	os.WriteFile(filepath.Join(dir, "logs-0123.json"), []byte("{}"), 0o600) // an older client's windows
	// the chain starts again: every block different, the same addresses
	for n := uint64(0); n <= 30_000; n++ {
		c.fork[n] = 9
	}
	c.anchor, c.head = 1, 20_000
	get := c.run(t, dir)
	sameLogs(t, get(all), direct(t, c, all))
	if len(c.calls) != 2 || c.calls[0][0] != 1 {
		t.Fatalf("after a reset: %v (want everything read again)", c.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs-0123.json")); err == nil {
		t.Fatal("the old folder's files stayed")
	}
	// the anchor can't be read: no cache, the RPC answers
	configureLogStore(dir, "", []common.Address{lcA}, 1, func(context.Context) (logAnchorFile, error) { return logAnchorFile{}, errors.New("down") })
	logStore.Lock()
	forgetLogs()
	logStore.Unlock()
	w := new(atomic.Uint64)
	w.Store(10_000)
	c.calls, c.addrs = nil, nil
	logs, err := cachedFilterLogs(context.Background(), c, func(ctx context.Context, qq ethereum.FilterQuery, _ bool) ([]types.Log, error) {
		return filterLogsRanged(ctx, c, w.Load(), qq)
	}, all)
	if err != nil || len(logs) != 20_000 || len(c.calls) != 2 {
		t.Fatalf("no anchor: %d logs, %v, %v", len(logs), c.calls, err)
	}
}

// An RPC that refuses wide queries with a limit it doesn't keep (dRPC's free
// plan: "ranges over 10000 blocks", answers ~100): halved until it answers,
// and the width kept for the next run.
func TestRangeHalvedPastWrongLimit(t *testing.T) {
	c := &logChain{head: 5_000, limit: 100, fork: map[uint64]byte{}}
	w := new(atomic.Uint64)
	w.Store(10_000)
	logs, err := filterLogsParallel(context.Background(), c, w, ethereum.FilterQuery{FromBlock: big.NewInt(1), ToBlock: big.NewInt(5_000), Addresses: []common.Address{lcA}})
	if err != nil || len(logs) != 5_000 {
		t.Fatalf("%d logs, %v", len(logs), err)
	}
	if w.Load() > 100 || w.Load() < 50 {
		t.Fatalf("window %d", w.Load())
	}
	for i := 1; i < len(logs); i++ {
		if logs[i].BlockNumber != logs[i-1].BlockNumber+1 {
			t.Fatalf("out of order at %d", i)
		}
	}
	// a rate limit is not a range limit
	if _, ok := narrower(errors.New("429 Too Many Requests: rate limit exceeded"), 10_000); ok {
		t.Fatal("narrowed on a 429")
	}
	// the width that worked, for the next run
	defer configureLogStore("", "", nil, 0, nil)
	c2 := &logChain{}
	d := t.TempDir()
	configureLogStore(d, "", nil, 0, c2.anchorFn)
	cacheReady(d)
	r := rangedClient{n: new(atomic.Uint64), key: "k", addrs: new(atomic.Uint64)}
	r.n.Store(78)
	r.addrs.Store(10)
	r.saveLimits()
	limitsLoaded.Clear()
	r2 := rangedClient{n: new(atomic.Uint64), key: "k", addrs: new(atomic.Uint64)}
	r2.n.Store(10_000)
	r2.loadLimits()
	if r2.window() != 78 || r2.addrs.Load() != 10 {
		t.Fatalf("next run's window %d, addresses %d", r2.window(), r2.addrs.Load())
	}
	// publicnode's address limit is not a range limit; it is learned
	pn := errors.New(`403 Forbidden: {"code":-32602,"message":"A maximum of 10 addresses is allowed. No limit on a dedicated node"}`)
	if _, ok := narrower(pn, 10_000); ok {
		t.Fatal("narrowed the window on an address limit")
	}
	if k, ok := addrLimit(pn, 44, 0); !ok || k != 10 {
		t.Fatalf("address limit %d %v", k, ok)
	}
}

// Mail put on chain from the user's own account says so, once a command.
func TestOwnAccountNoteOnce(t *testing.T) {
	var b bytes.Buffer
	old := outW
	outW = &b
	defer func() { outW, postedFromOwnAccount = old, false }()
	postedFromOwnAccount = true
	out(map[string]any{"sent": "0x01"})
	out(map[string]any{"sent": "0x02"})
	if n := strings.Count(b.String(), ownAccountNote); n != 1 {
		t.Fatalf("note %d times:\n%s", n, b.String())
	}
}

// A public RPC over HTTP the way providers answer: a 10-address limit
// (publicnode), a range limit it misstates (dRPC's free plan), block hashes
// as reported. The client learns both limits, keeps the network's history,
// and the next run asks only for new blocks.
func TestRangedClientOverHTTP(t *testing.T) {
	defer configureLogStore("", "", nil, 0, nil)
	var mirror []common.Address
	for i := 1; i <= 12; i++ {
		mirror = append(mirror, common.BigToAddress(big.NewInt(int64(i))))
	}
	var head atomic.Uint64
	head.Store(3_000)
	var getLogs atomic.Int64
	srv := fakeRPC(true, &head, &getLogs)
	defer srv.Close()
	dir := t.TempDir()
	run := func() (rangedClient, func(ethereum.FilterQuery) []types.Log) {
		rc, err := rpc.DialOptions(context.Background(), srv.URL, rpc.WithHTTPClient(&http.Client{Transport: retryRPC{}}))
		if err != nil {
			t.Fatal(err)
		}
		r := newRangedClient(ethclient.NewClient(rc), 10_000, srv.URL)
		configureLogStore("", dir, mirror, 1, func(ctx context.Context) (logAnchorFile, error) {
			h, err := r.blockHash(ctx, 1)
			return logAnchorFile{1, 1, h.Hex()}, err
		})
		logStore.Lock()
		forgetLogs()
		logStore.Unlock()
		return r, func(q ethereum.FilterQuery) []types.Log {
			t.Helper()
			done := make(chan []types.Log)
			go func() {
				logs, err := r.FilterLogs(context.Background(), q)
				if err != nil {
					t.Error(err)
				}
				done <- logs
			}()
			select {
			case l := <-done:
				return l
			case <-time.After(20 * time.Second):
				t.Fatal("FilterLogs hangs")
			}
			return nil
		}
	}
	r, get := run()
	logs := get(ethereum.FilterQuery{FromBlock: big.NewInt(1), Addresses: mirror[3:4]})
	if len(logs) != 60 { // blocks 1, 51, …, 2951
		t.Fatalf("%d logs", len(logs))
	}
	if r.window() > 100 || r.addrs.Load() != 10 {
		t.Fatalf("learned window %d, addresses %d", r.window(), r.addrs.Load())
	}
	// next run: limits from limits.json, only the blocks after the cursor
	head.Store(3_200)
	getLogs.Store(0)
	r, get = run()
	logs = get(ethereum.FilterQuery{FromBlock: big.NewInt(1), Addresses: mirror[11:12]})
	if len(logs) != 64 {
		t.Fatalf("next run: %d logs", len(logs))
	}
	if r.window() > 100 || r.addrs.Load() != 10 || getLogs.Load() > 8 { // (3200-2937+1) blocks: 4 windows of 78, 2 address groups
		t.Fatalf("next run: window %d, addresses %d, %d getLogs", r.window(), r.addrs.Load(), getLogs.Load())
	}
}

// fakeRPC: a provider's JSON-RPC: publicnode's 10-address limit and, narrow,
// dRPC's free plan (refuses over 100 blocks, naming 10000); a log every 50
// blocks from each address.
func fakeRPC(narrow bool, head *atomic.Uint64, getLogs *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		reply := func(v any) {
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": v})
		}
		switch req.Method {
		case "eth_chainId":
			reply("0x14a34")
		case "eth_blockNumber":
			reply(hexutil.EncodeUint64(head.Load()))
		case "eth_getBlockByNumber":
			reply(map[string]any{"hash": common.Hash{0xab}.Hex(), "number": "0x1"})
		case "eth_getLogs":
			getLogs.Add(1)
			var f struct {
				From    hexutil.Uint64   `json:"fromBlock"`
				To      hexutil.Uint64   `json:"toBlock"`
				Address []common.Address `json:"address"`
			}
			json.Unmarshal(req.Params[0], &f)
			if len(f.Address) > 10 {
				w.WriteHeader(403)
				fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"code":-32602,"message":"A maximum of 10 addresses is allowed. No limit on a dedicated node"},"id":1}`)
				return
			}
			if narrow && f.To-f.From+1 > 100 {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"code":35,"message":"ranges over 10000 blocks are not supported on free plan"},"id":1}`)
				return
			}
			var out []map[string]any
			for n := uint64(f.From); n <= uint64(f.To); n++ { // a log every 50 blocks, from each contract
				for _, a := range f.Address {
					if n%50 != 1 {
						continue
					}
					out = append(out, map[string]any{"address": a, "topics": []common.Hash{{1}}, "data": "0x", "blockNumber": hexutil.EncodeUint64(n),
						"transactionHash": common.Hash{byte(n), byte(n >> 8)}, "transactionIndex": "0x0", "blockHash": common.Hash{2}, "logIndex": "0x0", "removed": false})
				}
			}
			if out == nil {
				out = []map[string]any{}
			}
			reply(out)
		default:
			w.WriteHeader(400)
		}
	}))
}

// A narrow home RPC (--home-rpc on a free plan): the first window shows it,
// and the rest of a cold history read comes from net.json's RPCs; recent
// blocks and the next runs stay on the home RPC.
func TestNarrowHomeHistoryFromPublic(t *testing.T) {
	defer configureLogStore("", "", nil, 0, nil)
	oldHome, oldRPCs, oldNet := homeRPC, historyRPCs, historyNet
	defer func() {
		homeRPC, historyRPCs, historyNet, historyDone, historyVia, historyHost, historyLearned = oldHome, oldRPCs, oldNet, false, nil, "", false
		probed.Clear()
	}()
	var homeN, pubN atomic.Int64
	var head atomic.Uint64
	head.Store(50_000)
	srv, pub := fakeRPC(true, &head, &homeN), fakeRPC(false, &head, &pubN)
	defer srv.Close()
	defer pub.Close()
	homeRPC, historyRPCs, historyNet, historyDone, historyVia = srv.URL, []string{pub.URL}, func() *DevnetFile { f := &DevnetFile{LogRange: 10_000}; f.ChainID = 84532; return f }(), false, nil
	t.Setenv("TERNCHAIN_QUIET", "1")
	mirror := []common.Address{common.BigToAddress(big.NewInt(1))}
	rc, err := rpc.DialOptions(context.Background(), srv.URL, rpc.WithHTTPClient(&http.Client{Transport: retryRPC{}}))
	if err != nil {
		t.Fatal(err)
	}
	r := newRangedClient(ethclient.NewClient(rc), 10_000, srv.URL)
	configureLogStore("", t.TempDir(), mirror, 1, func(ctx context.Context) (logAnchorFile, error) {
		h, err := r.blockHash(ctx, 1)
		return logAnchorFile{1, 1, h.Hex()}, err
	})
	logStore.Lock()
	forgetLogs()
	logStore.Unlock()
	logs, err := r.FilterLogs(context.Background(), ethereum.FilterQuery{FromBlock: big.NewInt(1), Addresses: mirror})
	if err != nil || len(logs) != 1_000 { // 50,000 blocks, a log every 50
		t.Fatalf("%d logs, %v", len(logs), err)
	}
	for i := 1; i < len(logs); i++ {
		if logs[i].BlockNumber <= logs[i-1].BlockNumber {
			t.Fatalf("out of order at %d", i)
		}
	}
	if homeN.Load() > 40 || pubN.Load() == 0 {
		t.Fatalf("home %d queries, public %d", homeN.Load(), pubN.Load())
	}
}

// A 500 that asks for a retry is retried; another 500 is the answer.
func TestRetry500OnlyWhenAsked(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.WriteHeader(500)
			fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"message":"Temporary internal error. Please retry","code":19},"id":1}`)
		case 2:
			fmt.Fprint(w, `{"jsonrpc":"2.0","result":"0x10","id":1}`)
		default:
			w.WriteHeader(500)
			fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"message":"execution failed","code":-32000},"id":1}`)
		}
	}))
	defer srv.Close()
	rc, err := rpc.DialOptions(context.Background(), srv.URL, rpc.WithHTTPClient(&http.Client{Transport: retryRPC{}}))
	if err != nil {
		t.Fatal(err)
	}
	h, err := ethclient.NewClient(rc).BlockNumber(context.Background())
	if err != nil || h != 16 || n.Load() != 2 {
		t.Fatalf("head %d, %v, %d requests", h, err, n.Load())
	}
	if _, err := ethclient.NewClient(rc).BlockNumber(context.Background()); err == nil || n.Load() != 3 {
		t.Fatalf("a plain 500: %v, %d requests", err, n.Load())
	}
}
