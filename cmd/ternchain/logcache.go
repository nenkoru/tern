package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// The log history a one-shot command learned, kept for the next one. Every
// command used to read each history from the deployment block: hundreds of
// log windows per query on a public RPC. Now what was read is kept with the
// block it is complete up to (its cursor), and the next run asks only for the
// blocks after it.
//
//   - The network's own contracts are kept whole, by address, and read
//     together: one query for all of them, with no topic in it, answers every
//     topic query about them from disk (the RPC learns less than before, when
//     each query named the topics it wanted: a name, an account). Being the
//     network's public history, this is kept once per machine (sharedCacheDir),
//     so a new identity starts from it.
//   - Other contracts (the EntryPoint, a token, the shared ERC-8004
//     registries, whose histories are everyone's) keep a cursor per query in
//     the identity's cache/logs (logCacheDir): the same queries as before,
//     over fewer blocks.
//   - Nothing within reorgMargin of the head is kept: the next run reads those
//     blocks again.
//   - A folder belongs to one chain: chain id and the hash of the deployment
//     block (anchor.json, checked once a run). Another chain under the same
//     folder (a reset devnet, another network) empties it.
//
// Deleting either folder is always safe.
var (
	logCacheDir    string // the identity's queries ("": not kept)
	sharedCacheDir string // the network's contracts ("": in logCacheDir)
)

// reorgMargin: blocks below the head that every run reads again (Base does
// not reorganise this deep; an L1 of our own finalises in 2).
const reorgMargin = 64

// headMemo: a head read this recently answers "latest" (a command's queries
// come in bursts; a later one asks again).
const headMemo = time.Second

// logFetch: logs from the RPC; wide: a read of the network's whole
// contracts (no topic in it), which may go to another RPC (historyFor).
type logFetch func(ctx context.Context, q ethereum.FilterQuery, wide bool) ([]types.Log, error)

type logEntry struct {
	file     string
	Start    uint64      `json:"start"` // logs kept for [Start, Next)
	Next     uint64      `json:"next"`
	Logs     []types.Log `json:"logs"`
	tail     []types.Log // [Next, tailNext): read this run, too near the head to keep
	tailNext uint64
	dirty    bool
}

var logStore = struct {
	sync.Mutex
	e       map[string]*logEntry // by file
	checked map[string]bool      // folder → anchor.json checked this run, and usable
	mirror  []common.Address     // the network's own contracts, kept whole
	start   uint64               // the deployment block (kept histories start there)
	anchor  func(context.Context) (logAnchorFile, error)
	now     *logAnchorFile // this chain's anchor, read once a run
	head    uint64
	headAt  time.Time
}{e: map[string]*logEntry{}, checked: map[string]bool{}}

type logAnchorFile struct {
	Chain int64  `json:"chain_id"`
	Block uint64 `json:"block"`
	Hash  string `json:"hash"`
}

// sharedContracts: in net.json, but not the network's own: their logs are
// everyone's (every user operation on the chain, every ERC-8004 agent).
var sharedContracts = map[string]bool{"EntryPoint": true, "ERC8004IdentityRegistry": true, "ERC8004ReputationRegistry": true}

// setSharedCache: the machine's folder for the network's history (connect,
// before the first read). TERNCHAIN_CACHE: its root (default the user's
// cache folder); "off": each identity keeps its own copy.
func setSharedCache(ch *Chain, network string) {
	root := os.Getenv("TERNCHAIN_CACHE")
	if root == "off" {
		return
	}
	if root == "" {
		d, err := os.UserCacheDir()
		if err != nil {
			return
		}
		root = filepath.Join(d, "ternchain")
	}
	reg := ""
	if c := ch.c["Registry"]; c != nil {
		reg = strings.ToLower(c.Address().Hex()[2:10])
	}
	name := fmt.Sprintf("%d-%d-%s", ch.chainID.Int64(), ch.dep.Block, reg)
	if nameRE.MatchString(network) {
		name = network + "-" + name
	}
	configureLogCache(logCacheDir, filepath.Join(root, "logs", name), ch)
}

// setLogCache: the identity's folder (main, once --dir is known).
func setLogCache(dir string, ch *Chain) { configureLogCache(dir, sharedCacheDir, ch) }

func configureLogCache(dir, shared string, ch *Chain) {
	if ch == nil {
		configureLogStore(dir, shared, nil, 0, nil)
		return
	}
	var mirror []common.Address
	for n, c := range ch.c {
		if !sharedContracts[n] && c.Address() != (common.Address{}) {
			mirror = append(mirror, c.Address())
		}
	}
	id, block := ch.chainID.Int64(), ch.dep.Block
	hb, ok := ch.b.(interface {
		blockHash(context.Context, uint64) (common.Hash, error)
	})
	if !ok { // not a public RPC's client: nothing kept
		configureLogStore(dir, shared, nil, 0, nil)
		return
	}
	configureLogStore(dir, shared, mirror, block, func(ctx context.Context) (logAnchorFile, error) {
		h, err := hb.blockHash(ctx, block)
		if err != nil {
			return logAnchorFile{}, err
		}
		return logAnchorFile{id, block, h.Hex()}, nil
	})
}

func configureLogStore(dir, shared string, mirror []common.Address, start uint64, anchor func(context.Context) (logAnchorFile, error)) {
	logStore.Lock()
	defer logStore.Unlock()
	logCacheDir, sharedCacheDir = dir, shared
	sort.Slice(mirror, func(i, j int) bool { return mirror[i].Cmp(mirror[j]) < 0 })
	mirror = slicesCompact(mirror)
	same := logStore.start == start && logStore.anchor != nil && anchor != nil
	if !same { // another chain (setLogCache after setSharedCache keeps what was read)
		forgetLogs()
	}
	logStore.mirror, logStore.start, logStore.anchor = mirror, start, anchor
	limitsLoaded.Clear()
}

// forgetLogs: what this run read and checked, forgotten (as a new run starts).
// Called with logStore held.
func forgetLogs() {
	logStore.e, logStore.checked, logStore.now, logStore.head = map[string]*logEntry{}, map[string]bool{}, nil, 0
	keptTimes.Lock()
	keptTimes.loaded, keptTimes.dirty, keptTimes.m = false, false, nil
	keptTimes.Unlock()
}

func slicesCompact(xs []common.Address) []common.Address {
	var out []common.Address
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// mirrorDir: where the network's contracts are kept ("": nowhere).
func mirrorDir() string {
	if sharedCacheDir != "" {
		return sharedCacheDir
	}
	return logCacheDir
}

// usable: the folder is this chain's (else emptied), checked once a run.
// Called with logStore held.
func usable(ctx context.Context, dir string) bool {
	if dir == "" || logStore.anchor == nil {
		return false
	}
	if ok, done := logStore.checked[dir]; done {
		return ok
	}
	if logStore.now == nil {
		now, err := logStore.anchor(ctx)
		if err != nil {
			return false // asked again by the next query
		}
		logStore.now = &now
	}
	logStore.checked[dir] = false
	p := filepath.Join(dir, "anchor.json")
	var was logAnchorFile
	if readJSON(p, &was) != nil || was != *logStore.now {
		os.RemoveAll(dir) // another chain's (or an older client's windows)
		if os.MkdirAll(dir, 0o700) != nil || writeJSON(p, *logStore.now) != nil {
			return false
		}
	}
	logStore.checked[dir] = true
	return true
}

// cacheReady: the folder is this chain's and usable (for files kept beside the logs).
func cacheReady(dir string) bool {
	logStore.Lock()
	defer logStore.Unlock()
	return usable(context.Background(), dir)
}

// logQueryKey: the query without its block range.
func logQueryKey(q ethereum.FilterQuery) string {
	var b strings.Builder
	for _, a := range q.Addresses {
		b.WriteString(a.Hex())
	}
	for _, ts := range q.Topics {
		b.WriteString("|")
		for _, t := range ts {
			b.WriteString(t.Hex())
		}
	}
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:12])
}

// entry: loaded from disk once a run; a new one starts at start. Called with logStore held.
func entry(dir, name string, start uint64) *logEntry {
	file := filepath.Join(dir, name)
	if e := logStore.e[file]; e != nil {
		return e
	}
	e := &logEntry{}
	if readJSON(file, e) != nil || e.Next < e.Start {
		e = &logEntry{Start: start, Next: start}
	}
	e.file, e.tailNext = file, e.Next
	logStore.e[file] = e
	return e
}

func mirrorEntry(dir string, a common.Address) *logEntry {
	return entry(dir, "a-"+strings.ToLower(a.Hex()[2:])+".json", logStore.start)
}

// save: temp file and rename (a concurrent run sees the old file or the new
// one; the last writer wins, and what it lacks is read again).
func (e *logEntry) save() {
	if !e.dirty {
		return
	}
	e.dirty = false
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(e.file), "logs-*.tmp")
	if err != nil {
		return
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil && cerr == nil && os.Rename(tmp.Name(), e.file) == nil {
		return
	}
	os.Remove(tmp.Name())
}

// storeHead: the head, read again when the last reading is older than headMemo.
func storeHead(ctx context.Context, b logFilterer, fresh bool) (uint64, error) {
	if !fresh && logStore.head != 0 && time.Since(logStore.headAt) < headMemo {
		return logStore.head, nil
	}
	h, err := b.BlockNumber(ctx)
	if err == nil {
		logStore.head, logStore.headAt = h, time.Now()
	}
	return h, err
}

// cachedFilterLogs: q from what is kept, the RPC (fetch) for the rest.
func cachedFilterLogs(ctx context.Context, b logFilterer, fetch logFetch, q ethereum.FilterQuery) ([]types.Log, error) {
	if q.BlockHash != nil || len(q.Addresses) == 0 || q.FromBlock != nil && q.FromBlock.Sign() < 0 {
		return fetch(ctx, q, false)
	}
	logStore.Lock()
	defer logStore.Unlock()
	mirrored := len(logStore.mirror) > 0
	for _, a := range q.Addresses {
		_, found := sort.Find(len(logStore.mirror), func(i int) int { return a.Cmp(logStore.mirror[i]) })
		mirrored = mirrored && found
	}
	dir := logCacheDir
	if mirrored {
		dir = mirrorDir()
	}
	if !usable(ctx, dir) {
		return fetch(ctx, q, false)
	}
	head, err := storeHead(ctx, b, false)
	if err != nil {
		return nil, err
	}
	var from uint64
	if q.FromBlock != nil {
		from = q.FromBlock.Uint64()
	}
	to := head
	if q.ToBlock != nil && q.ToBlock.Sign() >= 0 {
		to = q.ToBlock.Uint64()
	}
	if to > head { // past the head this run knows: read it now
		if f, ok := b.(interface {
			freshHead(context.Context) (uint64, error)
		}); ok {
			head, err = f.freshHead(ctx)
			logStore.head, logStore.headAt = head, time.Now()
		} else {
			head, err = storeHead(ctx, b, true)
		}
		if err != nil {
			return nil, err
		}
	}
	if to < from {
		return nil, nil
	}
	stable := uint64(0)
	if head > reorgMargin {
		stable = head - reorgMargin
	}
	var es, want []*logEntry
	var sq ethereum.FilterQuery
	if mirrored { // all of them in one read; the answer from the ones asked for
		from = max(from, logStore.start)
		for _, a := range logStore.mirror {
			e := mirrorEntry(dir, a)
			es = append(es, e)
			if containsAddress(q.Addresses, a) {
				want = append(want, e)
			}
		}
		sq.Addresses = logStore.mirror
	} else {
		e := entry(dir, "q-"+logQueryKey(q)+".json", from)
		if from < e.Start { // asked from earlier than kept: keep what's before too
			if e.Next == e.Start {
				e.Start, e.Next, e.tailNext, e.tail = from, from, from, nil
			} else {
				w := q
				w.FromBlock, w.ToBlock = new(big.Int).SetUint64(from), new(big.Int).SetUint64(e.Start-1)
				logs, err := fetch(ctx, w, false)
				if err != nil {
					return nil, err
				}
				e.Logs, e.Start, e.dirty = append(live(logs), e.Logs...), from, true
			}
		}
		es, want = []*logEntry{e}, []*logEntry{e}
		sq.Addresses, sq.Topics = q.Addresses, q.Topics
	}
	defer func() { // also after an error: what was read stays read
		for _, e := range es {
			e.save()
		}
	}()
	if err := syncEntries(ctx, fetch, sq, es, mirrored, to, stable); err != nil {
		return nil, err
	}
	var out []types.Log
	for _, e := range want {
		for _, part := range [][]types.Log{e.Logs, e.tail} {
			i := sort.Search(len(part), func(i int) bool { return part[i].BlockNumber >= from })
			for ; i < len(part) && part[i].BlockNumber <= to; i++ {
				if matchTopics(part[i], q.Topics) {
					out = append(out, part[i])
				}
			}
		}
	}
	if len(want) > 1 {
		sortLogs(out)
	}
	return out, nil
}

func containsAddress(xs []common.Address, a common.Address) bool {
	for _, x := range xs {
		if x == a {
			return true
		}
	}
	return false
}

// syncEntries: every entry known through `to`. Entries read from the same
// block on go in one query (the network's contracts: one for all of them). A
// read starts at the cursor when blocks past it have become deep enough to
// keep (the near-head ones are read again), else after what this run read.
// For mirrored entries q names their addresses, es[i] the i-th.
func syncEntries(ctx context.Context, fetch logFetch, q ethereum.FilterQuery, es []*logEntry, mirrored bool, to, stable uint64) error {
	groups := map[uint64][]int{}
	for i, e := range es {
		if to < e.tailNext {
			continue
		}
		at := e.tailNext
		if e.Next <= stable {
			at = e.Next
		}
		groups[at] = append(groups[at], i)
	}
	ats := make([]uint64, 0, len(groups))
	for at := range groups {
		ats = append(ats, at)
	}
	sort.Slice(ats, func(i, j int) bool { return ats[i] < ats[j] })
	for _, at := range ats {
		idx := groups[at]
		w := ethereum.FilterQuery{FromBlock: new(big.Int).SetUint64(at), ToBlock: new(big.Int).SetUint64(to), Addresses: q.Addresses, Topics: q.Topics}
		if mirrored {
			w.Addresses = nil
			for _, i := range idx {
				w.Addresses = append(w.Addresses, q.Addresses[i])
			}
		}
		logs, err := fetch(ctx, w, mirrored) // wide: nothing of the user's in it
		if err != nil {
			return err
		}
		logs = live(logs)
		for _, i := range idx {
			e := es[i]
			var mine []types.Log
			for _, l := range logs {
				if !mirrored || l.Address == q.Addresses[i] {
					mine = append(mine, l)
				}
			}
			if at == e.Next { // from the cursor: what is deep enough now is kept
				keep := max(min(to, stable)+1, at)
				k := sort.Search(len(mine), func(j int) bool { return mine[j].BlockNumber >= keep })
				if k > 0 || keep > e.Next {
					e.Logs, e.Next, e.dirty = append(e.Logs, mine[:k]...), keep, true
				}
				e.tail = append([]types.Log(nil), mine[k:]...)
			} else {
				e.tail = append(e.tail, mine...)
			}
			e.tailNext = to + 1
		}
	}
	return nil
}

// live: logs without the ones a reorganisation removed, oldest first.
func live(logs []types.Log) []types.Log {
	out := logs[:0:0]
	for _, l := range logs {
		if !l.Removed {
			out = append(out, l)
		}
	}
	sortLogs(out)
	return out
}

func sortLogs(ls []types.Log) {
	sort.SliceStable(ls, func(i, j int) bool {
		return ls[i].BlockNumber < ls[j].BlockNumber || ls[i].BlockNumber == ls[j].BlockNumber && ls[i].Index < ls[j].Index
	})
}

// matchTopics: a log as an RPC filter would match it (empty position: any).
func matchTopics(l types.Log, topics [][]common.Hash) bool {
	for i, ts := range topics {
		if len(ts) == 0 {
			continue
		}
		if i >= len(l.Topics) {
			return false
		}
		ok := false
		for _, t := range ts {
			ok = ok || t == l.Topics[i]
		}
		if !ok {
			return false
		}
	}
	return true
}

// Block times, kept beside the network's logs (an inbox shows a time for
// every letter: a header read each, every run). Only blocks deeper than
// reorgMargin are kept.
var keptTimes = struct {
	sync.Mutex
	loaded, dirty bool
	m             map[uint64]int64
}{}

// keptTime: block n's time from the folder (loaded once a run).
func keptTime(n uint64) (int64, bool) {
	d := mirrorDir()
	if d == "" {
		return 0, false
	}
	keptTimes.Lock()
	defer keptTimes.Unlock()
	if !keptTimes.loaded {
		keptTimes.loaded, keptTimes.m = true, map[uint64]int64{}
		if cacheReady(d) {
			readJSON(filepath.Join(d, "times.json"), &keptTimes.m)
		}
	}
	t, ok := keptTimes.m[n]
	return t, ok
}

func keepTime(n uint64, t int64) {
	logStore.Lock()
	deep := logStore.head > n+reorgMargin
	logStore.Unlock()
	if !deep || mirrorDir() == "" {
		return
	}
	keptTimes.Lock()
	if keptTimes.m != nil {
		keptTimes.m[n], keptTimes.dirty = t, true
	}
	keptTimes.Unlock()
}

// flushKept: what this run added, to the folder (main, at the end of a command).
func flushKept() {
	d := mirrorDir()
	keptTimes.Lock()
	defer keptTimes.Unlock()
	if !keptTimes.dirty || d == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(d, "anchor.json")); err == nil {
		writeJSON(filepath.Join(d, "times.json"), keptTimes.m)
	}
	keptTimes.dirty = false
}
