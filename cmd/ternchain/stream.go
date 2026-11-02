// Push: a data node hands accepted envelopes to recipients at once, from its
// memory, before their batch is on chain; clients follow new chain heads
// instead of polling. The chain stays the final word: an item that came
// early is "pending (not on chain yet)" until its batch lands, and if it never
// does by the node's deadline it is treated as undelivered (and the sender
// holds the node's signed receipt, which slashes the node: receipt.go).
//
// The node's side:
//
//	GET /pending              the envelopes it accepted and has not committed (JSON)
//	GET /stream[?tag=T,…]     the same, pushed as they come (server-sent events):
//	                          "envelope" {seq, hash, env, deadline} and
//	                          "committed" {root, block, envs}
//
// A tag is an envelope's own 8-byte view tag (hex). Tags are per envelope (an
// X25519 with its ephemeral key), so a node can't tell whose an envelope is:
// recipients take the whole stream and check tags themselves, as they do for
// batches on chain; ?tag= narrows it to envelopes one already knows of (a
// sender following its own). Nothing is disclosed that the chain won't show
// a block later.
package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// ---- the node's side

type push struct {
	pmu  sync.Mutex
	seq  uint64
	base int64                    // when this node's sequence started (a restart begins a new one)
	pend map[common.Hash]*wireEnv // accepted, not committed yet
	subs map[chan streamMsg]*streamFilter
}

// streamFilter: what a subscriber asked for: envelopes with these tags, or in
// these buckets (bucket.go); nil maps: everything.
type streamFilter struct {
	tags    map[[8]byte]bool
	buckets map[uint16]bool
	bits    int
	own     bool // only what this node accepted itself (not copies of other nodes' pending)
}

func (f *streamFilter) match(t *[2][8]byte, peer bool) bool {
	if f == nil || t == nil {
		return true
	}
	if f.own && peer {
		return false
	}
	if f.tags != nil && !f.tags[t[0]] && !f.tags[t[1]] {
		return false
	}
	return f.buckets == nil || f.buckets[hintBucket(t[0], f.bits)] || f.buckets[hintBucket(t[1], f.bits)]
}

func parseStreamFilter(req *http.Request, bits int) (*streamFilter, error) {
	q := req.URL.Query()
	tags, err := parseTags(q.Get("tag"))
	if err != nil {
		return nil, err
	}
	if b := q.Get("bits"); b != "" && b != strconv.Itoa(bits) {
		return nil, fmt.Errorf("this network's buckets have %d bits", bits)
	}
	bs, err := parseBuckets(q.Get("bucket"), bits)
	if err != nil {
		return nil, err
	}
	own := q.Get("own") == "1"
	if tags == nil && bs == nil && !own {
		return nil, nil
	}
	return &streamFilter{tags: tags, buckets: bs, bits: bits, own: own}, nil
}

// wireEnv: an envelope as the node pushes it.
type wireEnv struct {
	Seq      uint64 `json:"seq"`
	Hash     string `json:"hash"`
	Env      string `json:"env"`      // hex of the encoded envelope
	Deadline int64  `json:"deadline"` // the node commits it by then (chain time)
	// The receipt of the node that accepted it (its deadline is the one that
	// counts: BK-3, BK-4); copies of other nodes' pending carry the origin's.
	Node  string `json:"node,omitempty"`
	Claim string `json:"claim,omitempty"`
	Sig   string `json:"sig,omitempty"`
	tags  [2][8]byte
	peer  bool  // a copy of another node's pending envelope (bucket.go)
	got   int64 // when the copy came (unix)
}

type committedMsg struct {
	Root  string   `json:"root,omitempty"` // empty: posted in full
	Block uint64   `json:"block"`
	Envs  []string `json:"envs"`
}

type streamMsg struct {
	event string
	data  []byte
	tags  *[2][8]byte // envelope messages: its tags (for ?tag=, ?bucket=)
	peer  bool
}

type leafLoc struct {
	Root  common.Hash
	Index int
	At    int64
}

func (r *Relay) windowS() int64 {
	if r.window > 0 {
		return r.window
	}
	return receiptWindow
}

// accepted: an envelope is in the queue. A data node signs a receipt for it
// (to the sender's claim; none without one, and none while its posts fail:
// IP-5); subscribers get it now.
func (r *Relay) accepted(e Envelope, claim common.Hash) *Receipt {
	h := e.hash()
	deadline := chainTime(r.ch) + r.windowS()
	var rc *Receipt
	if r.da && claim != (common.Hash{}) && !r.stalled() {
		if x, err := signReceipt(r.ch, r.key, h, deadline, claim); err == nil {
			rc = &x
		} else {
			log.Printf("relay: signing a receipt: %v", err)
		}
	}
	r.addPending(e, deadline, false, rc)
	return rc
}

// stalled: its last post failed (gas money gone, the RPC down): it promises
// nothing new until a post goes through (IP-5). Envelopes are still taken,
// without a receipt: their senders watch them as handed to a plain relay.
func (r *Relay) stalled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.postErr.IsZero()
}

// addPending: into the pending set, and to subscribers. peer: a copy of
// another node's (it has no receipt of ours; it leaves when its batch is indexed).
// rc: the receipt of the node that accepted it (ours, or the origin's for a copy).
func (r *Relay) addPending(e Envelope, deadline int64, peer bool, rc *Receipt) {
	h := e.hash()
	r.pmu.Lock()
	defer r.pmu.Unlock()
	if r.pend == nil {
		r.pend = map[common.Hash]*wireEnv{}
	}
	if _, ok := r.pend[h]; ok && peer {
		return
	}
	r.seq++
	w := &wireEnv{Seq: r.seq, Hash: h.Hex(), Env: hex.EncodeToString(e.encode()), Deadline: deadline, tags: [2][8]byte{e.Tag, e.Tag2}, peer: peer}
	if rc != nil {
		w.Node, w.Claim, w.Sig = rc.Node, rc.Claim, rc.Sig
	}
	if peer {
		w.got = time.Now().Unix()
	}
	r.pend[h] = w
	b, _ := json.Marshal(w)
	r.broadcastLocked(streamMsg{"envelope", b, &w.tags, peer})
}

// committed: a batch is on chain; its envelopes leave the pending set.
func (r *Relay) committed(root common.Hash, hs []common.Hash, block uint64) {
	now := time.Now().Unix()
	m := committedMsg{Block: block}
	if root != (common.Hash{}) {
		m.Root = root.Hex()
	}
	r.mu.Lock()
	if r.where == nil {
		r.where = map[common.Hash]leafLoc{}
	}
	for i, h := range hs {
		if root != (common.Hash{}) {
			r.where[h] = leafLoc{root, i, now}
		}
		m.Envs = append(m.Envs, h.Hex())
	}
	r.mu.Unlock()
	b, _ := json.Marshal(m)
	r.pmu.Lock()
	for _, h := range hs {
		delete(r.pend, h)
	}
	r.broadcastLocked(streamMsg{event: "committed", data: b})
	r.pmu.Unlock()
}

// broadcastLocked: to every subscriber; one that can't keep up is dropped
// (it reconnects and gets the pending set again).
func (r *Relay) broadcastLocked(m streamMsg) {
	for ch := range r.subs {
		select {
		case ch <- m:
		default:
			delete(r.subs, ch)
			close(ch)
		}
	}
}

func (r *Relay) pendingList() []*wireEnv {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	out := make([]*wireEnv, 0, len(r.pend))
	for _, w := range r.pend {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// servePending: everything pending, or with ?since=SEQ only what came after
// (a poller takes each envelope once instead of the whole set every time);
// ?bucket=IDS&bits=K only those buckets, ?own=1 only what this node accepted.
func (r *Relay) servePending(w http.ResponseWriter, req *http.Request) {
	filter, err := parseStreamFilter(req, r.ch.bucketBits())
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	r.pmu.Lock()
	if r.base == 0 {
		r.base = time.Now().UnixNano()
	}
	w.Header().Set("X-Pending-Base", fmt.Sprint(r.base))
	w.Header().Set("X-Pending-Seq", fmt.Sprint(r.seq))
	r.pmu.Unlock()
	list := r.pendingList()
	if since, err := strconv.ParseUint(req.URL.Query().Get("since"), 10, 64); err == nil {
		i := sort.Search(len(list), func(i int) bool { return list[i].Seq > since })
		list = list[i:]
	}
	if filter != nil {
		keep := list[:0:0]
		for _, x := range list {
			if filter.match(&x.tags, x.peer) {
				keep = append(keep, x)
			}
		}
		list = keep
	}
	json.NewEncoder(w).Encode(list)
}

func parseTags(s string) (map[[8]byte]bool, error) {
	if s == "" {
		return nil, nil
	}
	out := map[[8]byte]bool{}
	for _, t := range strings.Split(s, ",") {
		b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(t), "0x"))
		if err != nil || len(b) != 8 {
			return nil, fmt.Errorf("tag %q: 8 bytes in hex", t)
		}
		out[[8]byte(b)] = true
	}
	if len(out) > 256 {
		return nil, errors.New("at most 256 tags")
	}
	return out, nil
}

func (r *Relay) serveStream(w http.ResponseWriter, req *http.Request) {
	filter, err := parseStreamFilter(req, r.ch.bucketBits())
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming here", 500)
		return
	}
	rc := http.NewResponseController(w) // a stream outlives the server's read and write timeouts
	rc.SetReadDeadline(time.Time{})
	rc.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan streamMsg, 512)
	r.pmu.Lock()
	if r.subs == nil {
		r.subs = map[chan streamMsg]*streamFilter{}
	}
	if len(r.subs) >= 1000 {
		r.pmu.Unlock()
		http.Error(w, "too many subscribers; try again later", http.StatusServiceUnavailable)
		return
	}
	r.subs[ch] = filter
	snap := make([]*wireEnv, 0, len(r.pend))
	for _, x := range r.pend {
		snap = append(snap, x)
	}
	r.pmu.Unlock()
	defer func() {
		r.pmu.Lock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
		r.pmu.Unlock()
	}()
	sort.Slice(snap, func(i, j int) bool { return snap[i].Seq < snap[j].Seq })
	fmt.Fprint(w, ": tern stream\n\n")
	for _, x := range snap {
		if filter.match(&x.tags, x.peer) {
			b, _ := json.Marshal(x)
			fmt.Fprintf(w, "event: envelope\ndata: %s\n\n", b)
		}
	}
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return
			}
			if m.event == "envelope" && !filter.match(m.tags, m.peer) {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.event, m.data); err != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case <-req.Context().Done():
			return
		}
	}
}

// loadWhere: which committed batch (of this node) holds each envelope, from
// the batches on disk: what rebutting a missed-receipt claim needs.
func (r *Relay) loadWhere() {
	if r.dir == "" {
		return
	}
	me := crypto.PubkeyToAddress(r.key.PublicKey)
	files, _ := filepath.Glob(filepath.Join(r.dir, "*.batch"))
	r.where = map[common.Hash]leafLoc{}
	for _, f := range files {
		root := common.HexToHash(strings.TrimSuffix(filepath.Base(f), ".batch"))
		st, err := r.ch.call("Inbox", "commitment", root)
		if err != nil || st[0].(common.Address) != me || st[2].(uint8) == 0 {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		es, _ := decodeBatch(b)
		for i, h := range envHashes(es) {
			r.where[h] = leafLoc{root, i, int64(st[1].(uint64))}
		}
	}
}

// openClaim: a claim on chain that this node missed a receipt, not settled yet.
type openClaim struct {
	Env    common.Hash `json:"env"`
	Claim  common.Hash `json:"claim"`
	Warned bool        `json:"warned,omitempty"` // logged once: in no batch of ours
}

// claimsFile: the open claims and the next block to read claims from, kept
// across restarts (IP-3): a claim is dropped only once the chain settled it.
type claimsFile struct {
	Seen uint64      `json:"seen"`
	Open []openClaim `json:"open"`
}

// loadClaims: the open claims a previous run kept, and where it read up to;
// without a file, claims are read again from the deployment block.
func (r *Relay) loadClaims() {
	r.claims = map[[2]common.Hash]*openClaim{}
	r.missSeen = r.ch.dep.Block
	if r.dir == "" {
		return
	}
	b, err := os.ReadFile(filepath.Join(r.dir, "claims.json"))
	if err != nil {
		return
	}
	var f claimsFile
	if json.Unmarshal(b, &f) != nil {
		return
	}
	r.missSeen = max(f.Seen, r.ch.dep.Block)
	for i := range f.Open {
		x := f.Open[i]
		r.claims[[2]common.Hash{x.Env, x.Claim}] = &x
	}
}

// saveClaimsLocked (r.mu held).
func (r *Relay) saveClaimsLocked() {
	if r.dir == "" {
		return
	}
	f := claimsFile{Seen: r.missSeen}
	for _, x := range r.claims {
		f.Open = append(f.Open, *x)
	}
	sort.Slice(f.Open, func(i, j int) bool {
		return f.Open[i].Env.Hex()+f.Open[i].Claim.Hex() < f.Open[j].Env.Hex()+f.Open[j].Claim.Hex()
	})
	b, _ := json.Marshal(f)
	p := filepath.Join(r.dir, "claims.json")
	if os.WriteFile(p+".tmp", b, 0o600) == nil {
		os.Rename(p+".tmp", p)
	}
}

// rebutMisses answers claims that this node missed a receipt: the batch root,
// the Merkle proof and the envelope itself. New claims join the open set
// (kept on disk) before the read cursor moves; a claim leaves it only once the
// chain says it is settled, so a failed rebuttal is tried again next time (IP-3).
func (r *Relay) rebutMisses() error {
	me := crypto.PubkeyToAddress(r.key.PublicKey)
	head, err := r.ch.b.BlockNumber(context.Background())
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.claims == nil {
		r.claims = map[[2]common.Hash]*openClaim{}
	}
	from := r.missSeen
	r.mu.Unlock()
	evs, err := r.ch.eventsFrom(from, "Inbox", "Missed", nil, []common.Hash{common.BytesToHash(me.Bytes())})
	if err != nil {
		return err
	}
	r.mu.Lock()
	for _, ev := range evs {
		env, claim := common.Hash(ev.Args["env"].([32]byte)), common.Hash(ev.Args["claim"].([32]byte))
		if k := [2]common.Hash{env, claim}; r.claims[k] == nil {
			r.claims[k] = &openClaim{Env: env, Claim: claim}
		}
	}
	if head+1 > r.missSeen {
		r.missSeen = head + 1
	}
	todo := make([]*openClaim, 0, len(r.claims))
	for _, x := range r.claims {
		todo = append(todo, x)
	}
	r.saveClaimsLocked()
	r.mu.Unlock()
	var errs []error
	settled := map[[2]common.Hash]bool{}
	for _, x := range todo {
		k := [2]common.Hash{x.Env, x.Claim}
		st, err := r.ch.call("Inbox", "missOf", x.Env, me, x.Claim)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if st[3].(uint8) != 1 {
			settled[k] = true
			continue
		}
		r.mu.Lock()
		loc, ok := r.where[x.Env]
		r.mu.Unlock()
		if !ok {
			if !x.Warned {
				log.Printf("relay: a claim that envelope %s missed its receipt, and it is in no batch of ours: it will be slashed", x.Env.Hex()[:10])
				x.Warned = true
			}
			continue
		}
		b, ok := r.get(loc.Root)
		if !ok {
			errs = append(errs, fmt.Errorf("batch %s is gone: can't prove %s", loc.Root.Hex()[:10], x.Env.Hex()[:10]))
			continue
		}
		es, _ := decodeBatch(b)
		if loc.Index >= len(es) {
			errs = append(errs, fmt.Errorf("batch %s has no envelope %d", loc.Root.Hex()[:10], loc.Index))
			continue
		}
		var proof [][32]byte
		for _, p := range leafProof(envHashes(es), loc.Index) {
			proof = append(proof, p)
		}
		if _, err := r.ch.send(r.key, nil, "Inbox", "rebutMissed", x.Env, me, x.Claim, loc.Root, big.NewInt(int64(loc.Index)), proof, es[loc.Index].encode()); err != nil {
			errs = append(errs, err)
			continue
		}
		settled[k] = true
		log.Printf("relay: rebutted a missed-receipt claim for %s (batch %s)", x.Env.Hex()[:10], loc.Root.Hex()[:10])
	}
	r.mu.Lock()
	for k := range settled {
		delete(r.claims, k)
	}
	r.saveClaimsLocked()
	r.mu.Unlock()
	return errors.Join(errs...)
}

// ---- the client's side

func (w wireEnv) envelope() (Envelope, error) {
	b, err := hex.DecodeString(w.Env)
	if err != nil {
		return Envelope{}, err
	}
	es, err := decodeBatch(b)
	if err != nil || len(es) != 1 {
		return Envelope{}, errors.New("not one envelope")
	}
	return es[0], nil
}

var pushHTTP = &http.Client{Timeout: 2 * time.Second}

// fetchPending: what a node holds in memory now.
func fetchPending(u string) ([]wireEnv, error) {
	out, _, _, err := fetchPendingSince(u, 0, "")
	return out, err
}

// fetchPendingSince: what came after seq in the node's sequence `base` (all of
// it when base is not the node's current one); and the node's base and seq now.
func fetchPendingSince(u string, since uint64, base string) ([]wireEnv, string, uint64, error) {
	return fetchPendingQ(u, since, base, "")
}

// fetchPendingQ: with more of a query (bucket=…&bits=…, own=1).
func fetchPendingQ(u string, since uint64, base, extra string) ([]wireEnv, string, uint64, error) {
	return fetchPendingLim(u, since, base, extra, 64<<20)
}

// fetchPendingLim: reading at most limit bytes (a peer's poll: BK-3).
func fetchPendingLim(u string, since uint64, base, extra string, limit int64) ([]wireEnv, string, uint64, error) {
	q := ""
	if base != "" && since > 0 {
		q = fmt.Sprintf("?since=%d", since)
	}
	if extra != "" {
		if q == "" {
			q = "?" + extra
		} else {
			q += "&" + extra
		}
	}
	resp, err := pushHTTP.Get(strings.TrimRight(u, "/") + "/pending" + q)
	if err != nil {
		return nil, "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", 0, statusErr{resp.Status}
	}
	nb := resp.Header.Get("X-Pending-Base")
	seq, _ := strconv.ParseUint(resp.Header.Get("X-Pending-Seq"), 10, 64)
	var out []wireEnv
	if err := json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(&out); err != nil {
		return nil, "", 0, err
	}
	if since > 0 && base != "" && nb != base { // the node restarted: its sequence began again; read all of it
		return fetchPendingLim(u, 0, "", extra, limit)
	}
	if pendingHook != nil {
		pendingHook(u, resp.ContentLength)
	}
	return out, nb, seq, nil
}

const (
	onchainPending   = "pending (not on chain yet)"
	onchainCommitted = "committed"
	onchainNever     = "never committed: undelivered"
	pendingGrace     = 30 // seconds past a node's deadline before an early item counts as undelivered
)

// pendingHook: bytes of a pending read (a simulation counts them).
var pendingHook func(url string, n int64)

// pushNodes: the data nodes whose memory this client reads: its relay, and
// on a network with buckets its home nodes (which copy other nodes' pending).
func (c *Client) pushNodes() []string {
	if c.bucketMode() {
		return c.homeNodes()
	}
	if c.relay == "" {
		return nil
	}
	return []string{strings.TrimRight(c.relay, "/")}
}

// pullPending reads what the nodes hold and takes our envelopes from it.
func (c *Client) pullPending() []*Item {
	var got []*Item
	for _, u := range c.pushNodes() {
		if !perfFixes {
			ws, err := fetchPending(u)
			if err != nil {
				continue
			}
			got = append(got, c.takePending(ws, u)...)
			continue
		}
		if c.pendCur == nil {
			c.pendCur = map[string]pendCursor{}
		}
		cur := c.pendCur[u]
		ws, base, seq, err := fetchPendingQ(u, cur.seq, cur.base, c.bucketQuery())
		if err != nil {
			continue
		}
		got = append(got, c.takePending(ws, u)...)
		c.pendCur[u] = pendCursor{base: base, seq: seq} // every home: one may leave mail out (BK-2)
	}
	return got
}

// pendCursor: where this process last read a node's pending sequence.
type pendCursor struct {
	base string
	seq  uint64
}

// takePending: envelopes from a node's memory, checked like any other (the
// grant as of the current head); kept as pending until their batch lands.
func (c *Client) takePending(ws []wireEnv, u string) []*Item {
	if c.view == nil || len(ws) == 0 {
		return nil
	}
	head, err := c.ch.b.BlockNumber(context.Background())
	if err != nil {
		return nil
	}
	now := c.chainNow()
	ids := c.identities()
	var hidden map[common.Hash][]string
	hid := func() map[common.Hash][]string {
		if hidden == nil {
			if hidden, _ = c.hiddenPosts(); hidden == nil {
				hidden = map[common.Hash][]string{}
			}
		}
		return hidden
	}
	var got []*Item
	for _, w := range ws {
		e, err := w.envelope()
		if err != nil || !c.inMyBuckets(&e) || c.item(e.hash().Hex()) != nil {
			continue
		}
		it := c.open1(e, head, now, ids, hid)
		if it == nil {
			continue
		}
		it.Onchain, it.Deadline, it.Node = onchainPending, pendingDeadline(c.ch, w, e.hash(), now), u
		it.Verified += " (early, from data node memory; not on chain yet)"
		accepted, err := c.acceptIncoming(it)
		if err != nil || !accepted {
			continue
		}
		got = append(got, it)
	}
	if len(got) > 0 {
		c.afterItems(got)
		c.save()
	}
	return got
}

// pendingDeadline: when an early envelope must be on chain: its accepting
// node's signed receipt says so (BK-4: not whatever a home copying it says);
// without one, at least receiptWindow from now; never past receiptMaxAhead.
func pendingDeadline(ch *Chain, w wireEnv, h common.Hash, now int64) int64 {
	rc := Receipt{Env: h.Hex(), Node: w.Node, Deadline: w.Deadline, Claim: w.Claim, Sig: w.Sig}
	dl := w.Deadline
	if w.Sig == "" || rc.verify(ch, h) != nil {
		dl = max(dl, now+receiptWindow)
	}
	return min(dl, now+receiptMaxAhead)
}

// commitItem: an item that came early has landed on chain.
func (c *Client) commitItem(it *Item, b rawBatch, bt int64) {
	if it.Onchain != onchainPending && it.Onchain != onchainNever {
		return
	}
	if it.Onchain == onchainNever && it.Was != "" {
		it.Status, it.Was = it.Was, ""
	}
	it.Block, it.Time, it.Batch, it.Node, it.Onchain = b.Block, bt, b.Root, b.URL, onchainCommitted
	it.Verified = strings.TrimSuffix(it.Verified, " (early, from data node memory; not on chain yet)")
	c.flipped = append(c.flipped, it)
}

// checkPending: an early item whose batch never landed by the node's deadline
// is undelivered (an alert, never silence).
func (c *Client) checkPending() {
	now := c.chainNow()
	for _, it := range c.st.Items {
		if it.Onchain != onchainPending || it.Deadline == 0 || now <= it.Deadline+pendingGrace {
			continue
		}
		it.Was, it.Status, it.Onchain = it.Status, "undelivered", onchainNever
		note := ""
		if it.Type == tApprove {
			if a, err := approvalOf(it); err == nil && c.st.UsedApprovals[a.Nonce] {
				note = " (an approval that was already used: the action stands, its record is not on chain)"
			}
		}
		c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("%s %s from %s came early from data node %s but was not committed by its deadline: treated as undelivered%s",
			it.Type, it.ID[:18], it.From, it.Node, note))
		c.undelivered = append(c.undelivered, it)
		c.markEarlyMissed(it) // a batch its node committed and doesn't serve concerns us (dawithhold.go)
	}
}

func approvalOf(it *Item) (Approval, error) {
	var a Approval
	return a, json.Unmarshal(it.Body, &a)
}

// ---- subscriptions: node streams and chain heads

type nodeMsg struct {
	url       string
	env       *wireEnv
	committed *committedMsg
}

// sse follows one node's stream until ctx ends, reconnecting.
func sse(ctx context.Context, u, tags string, out chan<- nodeMsg) {
	sseQ(ctx, u, func() string { return tags }, out)
}

// sseQ: the query asked again at each connection, and the connection made
// again when the query changes (a bucket query is one epoch's: BK-1).
func sseQ(ctx context.Context, u string, query func() string, out chan<- nodeMsg) {
	cl := &http.Client{} // no timeout: a stream stays open
	backoff := time.Second
	for ctx.Err() == nil {
		tags := query()
		cctx, cancel := context.WithCancel(ctx)
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-cctx.Done():
					return
				case <-t.C:
					if query() != tags {
						cancel()
						return
					}
				}
			}
		}()
		q := ""
		if strings.HasPrefix(tags, "bucket=") { // a bucket query as is (bucket.go)
			q = "?" + tags
		} else if tags != "" {
			q = "?tag=" + url.QueryEscape(tags)
		}
		req, _ := http.NewRequestWithContext(cctx, "GET", u+"/stream"+q, nil)
		resp, err := cl.Do(req)
		if err == nil && resp.StatusCode == 200 {
			backoff = time.Second
			readSSE(cctx, resp.Body, u, out)
		}
		if resp != nil {
			resp.Body.Close()
		}
		changed := cctx.Err() != nil && ctx.Err() == nil
		cancel()
		if changed {
			continue // the epoch turned: the new epoch's buckets at once
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

func readSSE(ctx context.Context, body io.Reader, u string, out chan<- nodeMsg) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	ev, data := "", ""
	for sc.Scan() {
		l := sc.Text()
		switch {
		case l == "":
			m := nodeMsg{url: u}
			switch ev {
			case "envelope":
				var w wireEnv
				if json.Unmarshal([]byte(data), &w) == nil {
					m.env = &w
				}
			case "committed":
				var cm committedMsg
				if json.Unmarshal([]byte(data), &cm) == nil {
					m.committed = &cm
				}
			}
			if m.env != nil || m.committed != nil {
				select {
				case out <- m:
				case <-ctx.Done():
					return
				}
			}
			ev, data = "", ""
		case strings.HasPrefix(l, "event:"):
			ev = strings.TrimSpace(l[6:])
		case strings.HasPrefix(l, "data:"):
			data += strings.TrimSpace(l[5:])
		}
	}
}

type headSubscriber interface {
	SubscribeNewHead(ctx context.Context, ch chan<- *types.Header) (ethereum.Subscription, error)
}

// wsURL: the websocket endpoint beside an HTTP JSON-RPC one (the devnet serves both on one port).
func wsURL(rpc string) string {
	switch {
	case strings.HasPrefix(rpc, "http://"):
		return "ws://" + rpc[7:]
	case strings.HasPrefix(rpc, "https://"):
		return "wss://" + rpc[8:]
	}
	return rpc
}

// subscribeHeads: new chain heads by eth_subscribe (on the connection itself,
// or a websocket beside its HTTP endpoint). false: polling it is.
func (c *Client) subscribeHeads(ctx context.Context, out chan<- uint64) bool {
	hc := make(chan *types.Header, 16)
	var sub ethereum.Subscription
	if hs, ok := c.ch.b.(headSubscriber); ok {
		sub, _ = hs.SubscribeNewHead(ctx, hc)
	}
	if sub == nil && c.ch.dep.RPC != "" {
		if cl, err := ethclient.DialContext(ctx, wsURL(c.ch.dep.RPC)); err == nil {
			if s, err := cl.SubscribeNewHead(ctx, hc); err == nil {
				sub = s
				go func() { <-ctx.Done(); cl.Close() }()
			} else {
				cl.Close()
			}
		}
	}
	if sub == nil {
		return false
	}
	go func() {
		defer sub.Unsubscribe()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sub.Err():
				close(out)
				return
			case h := <-hc:
				select {
				case out <- h.Number.Uint64():
				default: // a head is pending already: one is enough
				}
			}
		}
	}()
	return true
}

// pusher: what wakes a watcher.
type pusher struct {
	msgs   chan nodeMsg
	heads  chan uint64
	live   bool // heads come by subscription
	cancel context.CancelFunc
}

func (c *Client) startPush() *pusher {
	ctx, cancel := context.WithCancel(context.Background())
	p := &pusher{msgs: make(chan nodeMsg, 1024), heads: make(chan uint64, 1), cancel: cancel}
	query := func() string { return "" }
	if c.bucketMode() { // this epoch's buckets, and the next's ready (made here: myBuckets is not for other goroutines)
		if e, err := c.ch.epoch(); err == nil {
			q0, q1 := c.bucketQueryAt(e), c.bucketQueryAt(e+1)
			query = func() string {
				if n, err := c.ch.epoch(); err == nil && n > e {
					return q1
				}
				return q0
			}
		}
	}
	for _, u := range c.pushNodes() {
		go sseQ(ctx, u, query, p.msgs)
	}
	p.live = c.subscribeHeads(ctx, p.heads)
	return p
}

func (p *pusher) stop() { p.cancel() }

// next waits up to d for something: envelopes from a node ("envs"), a batch
// that landed ("committed"), a new head ("head"), the polling tick when no
// head subscription works ("tick"), or nothing ("timeout").
func (p *pusher) next(d time.Duration) (string, []nodeMsg) {
	var tick <-chan time.Time
	if !p.live {
		tick = time.After(time.Second)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case m := <-p.msgs:
		ms := []nodeMsg{m}
		for len(p.msgs) > 0 && len(ms) < 256 {
			ms = append(ms, <-p.msgs)
		}
		kind := "committed"
		for _, x := range ms {
			if x.env != nil {
				kind = "envs"
			}
		}
		return kind, ms
	case _, ok := <-p.heads:
		if !ok { // the subscription died: poll from now on
			p.live, p.heads = false, make(chan uint64, 1)
		}
		return "head", nil
	case <-tick:
		return "tick", nil
	case <-t.C:
		return "timeout", nil
	}
}
