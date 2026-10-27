// Tag buckets and home data nodes: readers fetch their own share of the
// network's envelopes instead of downloading every batch and trying every
// envelope with their keys (sim/scale/REPORT.md: O(N²) bytes, O(N) CPU per reader).
//
//	bucket(epoch, V) = top k bits of SHA-256("tern-bucket" ‖ epoch ‖ V)
//
// V is a view public key (a family's, or a group's), k the network's
// BucketBits (net.json "bucket_bits"). A sender writes the recipient's bucket
// into the top k bits of the first two bytes of the envelope's tag (the other
// bits random), and its own family's into tag2: the envelope layout, the
// batches and the roots on chain are unchanged (the tag's last 6 bytes still
// find it, envelope.go). Buckets change every epoch.
//
// Privacy: anyone who knows a name's view key (it is public) can compute its
// bucket, so an envelope now says "to one of the identities in bucket b" —
// the anonymity set of a recipient is its bucket, about families/2^k of them,
// not the whole network; tag and tag2 together narrow a pair to two buckets.
// k is chosen so a bucket holds ~32 families or more (k = 5 for the 2000-agent
// simulation's ~1300; 11 at 100k agents, 15 at a million). Buckets change with
// the epoch, which does NOT widen the crowd: whoever links a reader's queries of
// two epochs intersects them (audit 5 BK-1). So a query names one epoch only,
// with bucketDecoys decoy buckets beside the real ones (the same all epoch).
//
// A data node indexes every committed batch it holds by bucket (its own, and
// with --mirror everyone's) and serves
//
//	GET /bucket/{ids}?bits=K&since=BLOCK[&epoch=E]
//
// ids: comma-separated bucket numbers. The answer is binary, one record per
// envelope in those buckets from batches committed at block ≥ since:
//
//	root [32] | block u64 | index u32 | n u32 | proof count u8 | proof [32]… | envelope
//
// with X-Bucket-Through (every batch committed up to that block is in the
// index) and X-Bucket-Complete (1: no gaps — the node holds every batch, so it
// can be a home). A reader checks each envelope against the chain: its hash,
// index and proof give the batch's leaf root (Inbox.leaves(root), what the
// node's receipts are proven against), the index is below the batch's paid
// count, and its hint is one of the buckets asked for. A node can't forge or
// misfile an envelope; it can leave one out, as a node could keep a batch to
// itself — which is why a family keeps two home nodes (identity "home", the
// card's "home"), and the old full read remains (any node, any batch).
//
// The node's memory follows buckets too: /pending and /stream take
// ?bucket=IDS&bits=K, and a node copies other nodes' pending envelopes
// (/pending?own=1), so a home has its families' mail before it is on chain,
// whichever node took it.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// ---- hints

func (ch *Chain) bucketBits() int {
	if ch == nil {
		return 0
	}
	return max(0, min(ch.dep.BucketBits, 16))
}

func bucketHash(epoch uint64, view []byte) uint16 {
	h := sha256.New()
	h.Write([]byte("tern-bucket"))
	binary.Write(h, binary.BigEndian, epoch)
	h.Write(view)
	s := h.Sum(nil)
	return binary.BigEndian.Uint16(s[:2])
}

// bucketOf: the bucket of view key `view` in epoch e, with k bits (0: the one bucket).
func bucketOf(e uint64, view []byte, bits int) uint16 {
	if bits <= 0 {
		return 0
	}
	return bucketHash(e, view) >> (16 - bits)
}

// bucketHint: the two bytes a sender puts in a tag: the bucket on top, random below.
func bucketHint(e uint64, view []byte, bits int) [2]byte {
	var r [2]byte
	randRead(r[:])
	if bits <= 0 {
		return r
	}
	low := uint16(1)<<(16-bits) - 1
	v := bucketOf(e, view, bits)<<(16-bits) | binary.BigEndian.Uint16(r[:])&low
	binary.BigEndian.PutUint16(r[:], v)
	return r
}

func hintBucket(t [8]byte, bits int) uint16 {
	if bits <= 0 {
		return 0
	}
	return binary.BigEndian.Uint16(t[:2]) >> (16 - bits)
}

// buckets: the two buckets an envelope says it is in (recipient's, sender family's).
func (e *Envelope) buckets(bits int) [2]uint16 {
	return [2]uint16{hintBucket(e.Tag, bits), hintBucket(e.Tag2, bits)}
}

// ---- the node's index

type bucketEntry struct {
	block uint64
	root  common.Hash
	idx   uint32
	n     uint32
	epoch uint64
	off   uint32 // the envelope's bytes in the batch
	size  uint32
}

type bucketIndex struct {
	mu      sync.Mutex
	bits    int
	entries []bucketEntry       // in block order
	by      map[uint16][]int32  // bucket => entries
	roots   map[common.Hash]int // batches indexed => their place in the order (dedup)
	hashes  map[common.Hash][]common.Hash
	envs    map[common.Hash]bool // envelopes indexed (peer pending skips them)
	next    uint64               // next block of Committed events to read
	through uint64               // every committed batch up to here is indexed (or skipped as lost)
	gaps    int                  // batches this node can't index (not a mirror, or unreachable for long)
	tries   map[common.Hash]int
	noLeaf  map[common.Hash]bool // committed with postRoot (no leaf root): readers can't check a record of it (BK-5)
}

func (r *Relay) bindex() *bucketIndex {
	r.bkOnce.Do(func() {
		r.bk = &bucketIndex{bits: r.ch.bucketBits(), by: map[uint16][]int32{}, roots: map[common.Hash]int{}, hashes: map[common.Hash][]common.Hash{},
			envs: map[common.Hash]bool{}, next: r.ch.dep.Block, tries: map[common.Hash]int{}, noLeaf: map[common.Hash]bool{}}
	})
	return r.bk
}

// indexBatch files a committed batch's first n envelopes under their buckets.
func (bk *bucketIndex) indexBatch(root common.Hash, block uint64, b []byte, n int) error {
	es, err := decodeBatch(b)
	if err != nil {
		return err
	}
	if n <= 0 || n > len(es) {
		n = len(es)
	}
	es = es[:n] // the reader rule: only what was paid for
	hs := envHashes(es)
	bk.mu.Lock()
	defer bk.mu.Unlock()
	if _, ok := bk.roots[root]; ok {
		return nil
	}
	bk.roots[root] = len(bk.entries)
	bk.hashes[root] = envHashes(decodeAll(b))
	off := uint32(0)
	for i := range es {
		size := uint32(len(es[i].encode()))
		at := int32(len(bk.entries))
		bk.entries = append(bk.entries, bucketEntry{block: block, root: root, idx: uint32(i), n: uint32(n), epoch: es[i].Epoch, off: off, size: size})
		bs := es[i].buckets(bk.bits)
		bk.by[bs[0]] = append(bk.by[bs[0]], at)
		if bs[1] != bs[0] {
			bk.by[bs[1]] = append(bk.by[bs[1]], at)
		}
		bk.envs[hs[i]] = true
		off += size
	}
	return nil
}

func decodeAll(b []byte) []Envelope {
	es, _ := decodeBatch(b)
	return es
}

// indexBuckets: committed batches, in chain order, into the index. A mirror
// fetches the batches it doesn't hold; any node indexes what it has. A batch
// nobody serves holds the index back for a while (readers don't skip past
// it), then is counted as a gap.
func (r *Relay) indexBuckets() error {
	bk := r.bindex()
	head, err := r.ch.b.BlockNumber(ctxb())
	if err != nil {
		return err
	}
	if head < bk.next {
		return nil
	}
	evs, err := r.ch.eventsFrom(bk.next, "Inbox", "Committed")
	if err != nil {
		return err
	}
	me := crypto.PubkeyToAddress(r.key.PublicKey)
	var urls map[common.Address]string
	for _, ev := range evs {
		if ev.Block > head {
			break
		}
		root := common.Hash(ev.Args["root"].([32]byte))
		bk.mu.Lock()
		_, done := bk.roots[root]
		bk.mu.Unlock()
		if done {
			continue
		}
		poster := ev.Args["poster"].(common.Address)
		n := int(ev.Args["n"].(*big.Int).Int64())
		b, ok := r.get(root)
		if !ok && r.mirrors && poster != me {
			if urls == nil {
				urls = stakedURLs(r.ch)
			}
			if x, err := fetchBatch(urls[poster], root); err == nil {
				if r.put(root, x) == nil {
					b, ok = x, true
				}
			}
		}
		if !ok {
			if poster != me && !r.mirrors { // not ours to keep: a gap in this node's index, not a wait
				r.skipBatch(bk, root, "a non-mirror node")
				continue
			}
			bk.mu.Lock()
			bk.tries[root]++
			t := bk.tries[root]
			bk.mu.Unlock()
			if st, err := r.ch.call("Inbox", "commitment", root); err == nil && st[2].(uint8) == 4 {
				r.skipBatch(bk, root, "lost on chain")
				continue
			}
			if t < 60 { // about a minute of tries: readers wait for it
				bk.mu.Lock()
				bk.next, bk.through = ev.Block, max(bk.through, ev.Block-1)
				bk.mu.Unlock()
				return nil
			}
			r.skipBatch(bk, root, "no node serves it")
			continue
		}
		if err := bk.indexBatch(root, ev.Block, b, n); err != nil {
			log.Printf("relay: indexing batch %s: %v", root.Hex()[:10], err)
			r.skipBatch(bk, root, "undecodable")
			continue
		}
		if lr, err := r.ch.call("Inbox", "leaves", root); err == nil && common.Hash(lr[0].([32]byte)) == (common.Hash{}) {
			bk.mu.Lock()
			bk.noLeaf[root] = true // served as incomplete: the reader reads it whole instead (BK-5)
			bk.mu.Unlock()
		}
		r.unpendPeers(bk.hashes[root])
	}
	bk.mu.Lock()
	bk.next, bk.through = head+1, head
	bk.mu.Unlock()
	return nil
}

func (r *Relay) skipBatch(bk *bucketIndex, root common.Hash, why string) {
	bk.mu.Lock()
	defer bk.mu.Unlock()
	if _, ok := bk.roots[root]; ok {
		return
	}
	bk.roots[root] = -1
	bk.gaps++
	if why != "a non-mirror node" {
		log.Printf("relay: bucket index skips batch %s: %s", root.Hex()[:10], why)
	}
}

// parseBuckets: "3,17" with bits k.
func parseBuckets(s string, bits int) (map[uint16]bool, error) {
	if s == "" {
		return nil, nil
	}
	out := map[uint16]bool{}
	for _, x := range strings.Split(s, ",") {
		v, err := strconv.ParseUint(strings.TrimSpace(x), 10, 16)
		if err != nil || bits < 16 && v >= 1<<bits {
			return nil, fmt.Errorf("bucket %q: a number below 2^%d", x, bits)
		}
		out[uint16(v)] = true
	}
	if len(out) > 64 {
		return nil, fmt.Errorf("at most 64 buckets")
	}
	return out, nil
}

const bucketMaxRecords = 8192

func (r *Relay) serveBucket(w http.ResponseWriter, req *http.Request) {
	if !r.da {
		http.Error(w, "not a data node", 404)
		return
	}
	bk := r.bindex()
	q := req.URL.Query()
	if b, err := strconv.Atoi(q.Get("bits")); q.Get("bits") != "" && (err != nil || b != bk.bits) {
		http.Error(w, fmt.Sprintf("this network's buckets have %d bits", bk.bits), 400)
		return
	}
	ids, err := parseBuckets(req.PathValue("ids"), bk.bits)
	if err != nil || len(ids) == 0 {
		http.Error(w, fmt.Sprintf("buckets: %v", err), 400)
		return
	}
	since, _ := strconv.ParseUint(q.Get("since"), 10, 64)
	epoch, ep := uint64(0), q.Get("epoch") != ""
	if ep {
		epoch, _ = strconv.ParseUint(q.Get("epoch"), 10, 64)
	}
	bk.mu.Lock()
	through, complete := bk.through, bk.gaps == 0
	var picked []bucketEntry
	seen := map[int32]bool{}
	for id := range ids {
		list := bk.by[id]
		i := sort.Search(len(list), func(i int) bool { return bk.entries[list[i]].block >= since })
		for _, at := range list[i:] {
			if e := bk.entries[at]; !seen[at] && (!ep || e.epoch == epoch) {
				seen[at] = true
				picked = append(picked, e)
				if bk.noLeaf[e.root] {
					complete = false // a reader can't check it: it must read that batch whole (BK-5)
				}
			}
		}
	}
	bk.mu.Unlock()
	sort.Slice(picked, func(i, j int) bool {
		if picked[i].block != picked[j].block {
			return picked[i].block < picked[j].block
		}
		return picked[i].idx < picked[j].idx
	})
	if len(picked) > bucketMaxRecords { // the rest next time: cut at a block boundary
		cut := bucketMaxRecords
		for cut > 0 && picked[cut].block == picked[cut-1].block {
			cut--
		}
		if cut > 0 {
			through = picked[cut].block - 1
			picked = picked[:cut]
		}
	}
	var out bytes.Buffer
	batches := map[common.Hash][]byte{}
	for _, e := range picked {
		b, ok := batches[e.root]
		if !ok {
			if b, ok = r.get(e.root); !ok {
				complete = false // the index has it, the store doesn't (deleted): not a home for this
				continue
			}
			batches[e.root] = b
		}
		if int(e.off+e.size) > len(b) {
			complete = false
			continue
		}
		bk.mu.Lock()
		hs := bk.hashes[e.root]
		bk.mu.Unlock()
		proof := leafProof(hs, int(e.idx))
		out.Write(e.root[:])
		binary.Write(&out, binary.BigEndian, e.block)
		binary.Write(&out, binary.BigEndian, e.idx)
		binary.Write(&out, binary.BigEndian, e.n)
		out.WriteByte(byte(len(proof)))
		for _, p := range proof {
			out.Write(p[:])
		}
		out.Write(b[e.off : e.off+e.size])
	}
	c := "1"
	if !complete {
		c = "0"
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Bucket-Through", fmt.Sprint(through))
	w.Header().Set("X-Bucket-Complete", c)
	w.Header().Set("X-Bucket-Bits", fmt.Sprint(bk.bits))
	w.Write(out.Bytes())
}

// ---- other nodes' pending envelopes, for the families this node is home to

// Copying other nodes' pending (BK-3): only envelopes the node itself signed
// a receipt for (its own, by a near deadline: the deadline is the receipt's,
// BK-4), at most peerPendMax held from one node and peerPendTotal in all, one
// poll at most peerPendBytes; a mirror stops copying a node once peerBadMax of
// its copies never landed by their deadline.
const (
	peerPendMax   = 2048
	peerPendTotal = 16384
	peerPendBytes = 16 << 20
	peerBadMax    = 256
)

// pollPeers copies what other data nodes hold in memory (their own, not what
// they copied in turn) into this node's pending set.
func (r *Relay) pollPeers() {
	me := strings.TrimRight(r.publicURL, "/")
	if r.peerCur == nil {
		r.peerCur = map[string]pendCursor{}
	}
	if r.peerBad == nil {
		r.peerBad = map[common.Address]int{}
	}
	now := chainTime(r.ch)
	held, total := r.peerHeld(now)
	for a, u := range stakedURLs(r.ch) {
		u = strings.TrimRight(u, "/")
		if u == "" || u == me || r.peerBad[a] >= peerBadMax {
			continue
		}
		cur := r.peerCur[u]
		ws, base, seq, err := fetchPendingLim(u, cur.seq, cur.base, "own=1", peerPendBytes)
		if err != nil {
			continue
		}
		r.peerCur[u] = pendCursor{base: base, seq: seq}
		bk := r.bindex()
		for i := range ws {
			if held[a] >= peerPendMax || total >= peerPendTotal {
				break
			}
			e, err := ws[i].envelope()
			if err != nil {
				continue
			}
			h := e.hash()
			rc := Receipt{Env: h.Hex(), Node: ws[i].Node, Deadline: ws[i].Deadline, Claim: ws[i].Claim, Sig: ws[i].Sig}
			if ws[i].Sig == "" || common.HexToAddress(rc.Node) != a || rc.verify(r.ch, h) != nil ||
				rc.Deadline > now+receiptMaxAhead || rc.Deadline+pendingGrace < now {
				continue // no promise of that node's to commit it soon: not ours to spread
			}
			bk.mu.Lock()
			done := bk.envs[h]
			bk.mu.Unlock()
			if done {
				continue
			}
			r.addPending(e, rc.Deadline, true, &rc)
			held[a]++
			total++
		}
	}
}

// peerHeld: copies held per origin node, and in all; copies past their
// receipt's deadline leave (a mirror counts them against their node).
func (r *Relay) peerHeld(now int64) (map[common.Address]int, int) {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	held, total := map[common.Address]int{}, 0
	for h, w := range r.pend {
		if !w.peer {
			continue
		}
		a := common.HexToAddress(w.Node)
		if w.Deadline+pendingGrace < now {
			delete(r.pend, h)
			if r.mirrors { // a mirror indexes every batch, so a copy that landed has left already
				r.peerBad[a]++
			}
			continue
		}
		held[a]++
		total++
	}
	return held, total
}

// unpendPeers: copied envelopes that are now in an indexed batch leave the pending set.
func (r *Relay) unpendPeers(hs []common.Hash) {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	for _, h := range hs {
		if w, ok := r.pend[h]; ok && w.peer {
			delete(r.pend, h)
		}
	}
	now := time.Now().Unix()
	if len(r.pend) > 0 && now%60 == 0 { // copies whose node never committed them: gone after their deadline (wall clock is close enough here)
		for h, w := range r.pend {
			if w.peer && w.got > 0 && now-w.got > 2*receiptWindow+pendingGrace {
				delete(r.pend, h)
			}
		}
	}
}

// ---- the reader's side

// bucketsOff: read as before (every batch), even on a network with buckets.
var bucketsOff = false

func (c *Client) bucketMode() bool { return !bucketsOff && c.ch.bucketBits() > 0 && c.view != nil }

// homeNodes: the data nodes that keep this family's buckets: set by the
// process (a simulation), the identity's "home", or its relay.
func (c *Client) homeNodes() []string {
	var out []string
	add := func(u string) {
		u = strings.TrimRight(u, "/")
		for _, x := range out {
			if x == u {
				return
			}
		}
		if u != "" {
			out = append(out, u)
		}
	}
	for _, u := range c.home {
		add(u)
	}
	for _, u := range c.f.Home {
		add(u)
	}
	add(c.relay)
	return out
}

// myBuckets: the buckets this identity reads in epoch e: its family's and its groups'.
func (c *Client) myBuckets(e uint64) map[uint16]bool {
	bits := c.ch.bucketBits()
	if c.bkMemo == nil || c.bkGroups != len(c.st.Groups) || len(c.bkMemo) > 64 {
		c.bkMemo, c.bkGroups = map[uint64]map[uint16]bool{}, len(c.st.Groups)
	}
	if m, ok := c.bkMemo[e]; ok {
		return m
	}
	m := map[uint16]bool{}
	if c.view != nil {
		m[bucketOf(e, c.view.PublicKey().Bytes(), bits)] = true
	}
	for _, g := range c.st.Groups {
		if v, err := g.view(); err == nil {
			m[bucketOf(e, v.PublicKey().Bytes(), bits)] = true
		}
	}
	c.bkMemo[e] = m
	return m
}

// inMyBuckets: could this envelope be ours? (No trial decryption otherwise.)
func (c *Client) inMyBuckets(e *Envelope) bool {
	if !c.bucketMode() {
		return true
	}
	bs, mine := e.buckets(c.ch.bucketBits()), c.myBuckets(e.Epoch)
	return mine[bs[0]] || mine[bs[1]]
}

func bucketList(m map[uint16]bool) string {
	var xs []int
	for b := range m {
		xs = append(xs, int(b))
	}
	sort.Ints(xs)
	var s []string
	for _, x := range xs {
		s = append(s, strconv.Itoa(x))
	}
	return strings.Join(s, ",")
}

// bucketQuery: ?bucket=…&bits=… for this epoch only (pending, streams). The
// buckets of e-1, e and e+1 together named one family among 100k (BK-1).
func (c *Client) bucketQuery() string {
	if !c.bucketMode() {
		return ""
	}
	e, err := c.ch.epoch()
	if err != nil {
		return ""
	}
	return c.bucketQueryAt(e)
}

func (c *Client) bucketQueryAt(e uint64) string {
	return "bucket=" + bucketList(c.queryBuckets(e)) + "&bits=" + strconv.Itoa(c.ch.bucketBits())
}

// bucketDecoys: buckets asked for beside our own in each query (BK-1).
var bucketDecoys = 3

// queryBuckets: what we ask a node for in epoch e: our buckets and
// bucketDecoys others, the same all epoch (fresh ones per request would give
// the real ones away to whoever compares two requests).
func (c *Client) queryBuckets(e uint64) map[uint16]bool {
	mine, bits := c.myBuckets(e), c.ch.bucketBits()
	m := make(map[uint16]bool, len(mine)+bucketDecoys)
	for b := range mine {
		m[b] = true
	}
	if bits <= 0 || c.view == nil {
		return m
	}
	want := min(len(mine)+bucketDecoys, 64, 1<<bits)
	seed := sha256.Sum256(append([]byte("tern-decoy"), c.view.Bytes()...))
	for i := uint64(0); len(m) < want && i < 256; i++ {
		h := sha256.New()
		h.Write(seed[:])
		binary.Write(h, binary.BigEndian, e)
		binary.Write(h, binary.BigEndian, i)
		m[binary.BigEndian.Uint16(h.Sum(nil)[:2])>>(16-bits)] = true
	}
	return m
}

// bucketHook: what a reader downloaded from home nodes; bucketFallbackHook: a
// scan that found no home and read every batch (a simulation counts them).
var (
	bucketHook         func(reader string, bytes int)
	bucketFallbackHook func()
)

type bucketRec struct {
	root  common.Hash
	block uint64
	idx   uint32
	n     uint32
	proof []common.Hash
	env   Envelope
	raw   []byte
}

func decodeBucket(b []byte) ([]bucketRec, error) {
	var out []bucketRec
	for len(b) > 0 {
		if len(b) < 32+8+4+4+1 {
			return out, fmt.Errorf("short bucket record")
		}
		var r bucketRec
		copy(r.root[:], b[:32])
		r.block = binary.BigEndian.Uint64(b[32:])
		r.idx = binary.BigEndian.Uint32(b[40:])
		r.n = binary.BigEndian.Uint32(b[44:])
		np := int(b[48])
		b = b[49:]
		if len(b) < 32*np+63 {
			return out, fmt.Errorf("short bucket record")
		}
		for i := 0; i < np; i++ {
			r.proof = append(r.proof, common.BytesToHash(b[:32]))
			b = b[32:]
		}
		size := 63 + int(binary.BigEndian.Uint32(b[59:63]))
		if size > len(b) {
			return out, fmt.Errorf("short envelope in bucket record")
		}
		es, err := decodeBatch(b[:size])
		if err != nil || len(es) != 1 {
			return out, fmt.Errorf("bad envelope in bucket record")
		}
		r.env, r.raw = es[0], b[:size]
		b = b[size:]
		out = append(out, r)
	}
	return out, nil
}

// proofRoot: the leaf root that leaf at index i with this proof hashes to
// (as leafProof makes it: the sibling's side follows from the index and the
// level's width, which the batch's count gives).
func proofRoot(leaf common.Hash, i, n int, proof []common.Hash) (common.Hash, bool) {
	h, k := leaf, 0
	for width := n; width > 1; width = (width + 1) / 2 {
		if i%2 == 1 {
			if k >= len(proof) {
				return h, false
			}
			h = merkleNode(proof[k], h)
			k++
		} else if i+1 < width {
			if k >= len(proof) {
				return h, false
			}
			h = merkleNode(h, proof[k])
			k++
		}
		i /= 2
	}
	return h, k == len(proof)
}

// leafRootOn: the batch's leaf root as the Inbox keeps it (once per root: it never changes).
func (c *Client) leafRootOn(root common.Hash) (common.Hash, error) {
	if v, ok := c.ch.leaves.Load(root); ok {
		return v.(common.Hash), nil
	}
	out, err := c.ch.call("Inbox", "leaves", root)
	if err != nil {
		return common.Hash{}, err
	}
	l := common.Hash(out[0].([32]byte))
	if l != (common.Hash{}) {
		c.ch.leaves.Store(root, l)
	}
	return l, nil
}

var bucketHTTP = &http.Client{Timeout: 10 * time.Second}

// bucketScan: the batches a scan reads from block `from` on a network with
// buckets: those posted in full or resolved on chain (filtered by bucket in
// the scan), and our buckets' envelopes from a home node; and the next block
// to scan from. Without a home that answers, every batch, as before.
func (c *Client) bucketScan(from, head uint64) ([]rawBatch, uint64, error) {
	evs, err := c.ch.eventsMulti(from, "Inbox", "Posted", "Resolved", "Committed")
	if err != nil {
		return nil, 0, err
	}
	var out []rawBatch
	committed := map[common.Hash]event{}
	for _, e := range evs {
		switch e.Name {
		case "Posted":
			out = append(out, rawBatch{Block: e.Block, Data: e.Args["batch"].([]byte)})
		case "Resolved":
			root := common.Hash(e.Args["root"].([32]byte))
			out = append(out, rawBatch{e.Block, e.Args["batch"].([]byte), root.Hex(), "on chain (resolved after a challenge)"})
		case "Committed":
			committed[common.Hash(e.Args["root"].([32]byte))] = e
		}
	}
	bk, through, ok := c.bucketBatches(from, head, committed)
	if !ok {
		if bucketFallbackHook != nil {
			bucketFallbackHook()
		}
		all, err := c.batchesSince(from)
		return all, head + 1, err
	}
	return append(out, bk...), through + 1, nil
}

// bucketBatches: what our buckets hold from block `from`, from every home
// node that answers completely, each envelope checked against the chain, and
// the union of them (BK-2: one home may leave mail out; a home that left out
// what another served is an alert); and the block up to which that is
// everything. committed: the Committed events of the range (root => event).
// ok false: no home could answer (the caller reads every batch instead).
func (c *Client) bucketBatches(from, head uint64, committed map[common.Hash]event) ([]rawBatch, uint64, bool) {
	bits := c.ch.bucketBits()
	e1, err := c.ch.epoch()
	if err != nil {
		return nil, 0, false
	}
	e0 := e1
	if bt, err := c.ch.blockTime(min(from, head)); err == nil {
		if e, err := epochOf(c, bt); err == nil && e <= e1 {
			e0 = e
		}
	}
	if e1-e0 > 30 {
		e0 = e1 - 30 // keys older than a month are gone anyway
	}
	type answer struct {
		u    string
		recs map[bucketKey]bool
	}
	var good []answer
	union := map[bucketKey]rawBatch{}
	through := head
	for _, u := range c.homeNodes() {
		recs, t, ok := c.bucketHome(u, from, head, e0, e1, bits, committed)
		if !ok {
			continue
		}
		a := answer{u, map[bucketKey]bool{}}
		for k, x := range recs {
			a.recs[k] = true
			if _, ok := union[k]; !ok {
				union[k] = x
			}
		}
		good = append(good, a)
		through = min(through, t)
	}
	if len(good) == 0 {
		return nil, 0, false
	}
	var keys []bucketKey
	for k, x := range union {
		if x.Block <= through {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if union[keys[i]].Block != union[keys[j]].Block {
			return union[keys[i]].Block < union[keys[j]].Block
		}
		if keys[i].root != keys[j].root {
			return keys[i].root.Hex() < keys[j].root.Hex()
		}
		return keys[i].idx < keys[j].idx
	})
	out := make([]rawBatch, 0, len(keys))
	for _, k := range keys {
		out = append(out, union[k])
	}
	for _, a := range good {
		missing := 0
		for _, k := range keys {
			if !a.recs[k] {
				missing++
			}
		}
		if missing > 0 {
			c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("home data node %s said it had everything in your buckets but left out %d envelope(s) another home served (checked against the chain): it may be hiding your mail; replace it as a home",
				a.u, missing))
		}
	}
	return out, through, true
}

// bucketKey: an envelope's place on chain.
type bucketKey struct {
	root common.Hash
	idx  uint32
}

// bucketHome: what one home serves of our buckets, epoch by epoch (one epoch
// per query, with decoys: BK-1), checked; and the block it is complete up to.
func (c *Client) bucketHome(u string, from, head, e0, e1 uint64, bits int, committed map[common.Hash]event) (map[bucketKey]rawBatch, uint64, bool) {
	out := map[bucketKey]rawBatch{}
	through := head
	for e := e0; e <= e1; e++ {
		ids, mine := c.queryBuckets(e), c.myBuckets(e)
		url := fmt.Sprintf("%s/bucket/%s?bits=%d&since=%d&epoch=%d", u, bucketList(ids), bits, from, e)
		resp, err := bucketHTTP.Get(url)
		if err != nil {
			return nil, 0, false
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if bucketHook != nil {
			bucketHook(c.f.Name, len(b))
		}
		if err != nil || resp.StatusCode != 200 || resp.Header.Get("X-Bucket-Complete") != "1" {
			return nil, 0, false
		}
		if t, err := strconv.ParseUint(resp.Header.Get("X-Bucket-Through"), 10, 64); err == nil && t < through {
			through = t
		}
		recs, err := decodeBucket(b)
		if err != nil {
			return nil, 0, false
		}
		for _, r := range recs {
			if r.block > through || r.block < from {
				continue // past what we read now (next scan), or not asked for
			}
			x, ok := c.checkBucketRec(r, e, ids, bits, committed)
			if !ok {
				c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("data node %s served envelope %s of batch %s that doesn't check against the chain: dropped",
					u, r.env.hash().Hex()[:10], r.root.Hex()[:10]))
				continue
			}
			if bs := r.env.buckets(bits); !mine[bs[0]] && !mine[bs[1]] {
				continue // a decoy's
			}
			x.URL = u
			out[bucketKey{r.root, r.idx}] = x
		}
	}
	if through < from {
		through = from - 1
	}
	for k, x := range out {
		if x.Block > through {
			delete(out, k)
		}
	}
	return out, through, true
}

// checkBucketRec: the envelope is in a batch committed on chain (leaf root,
// index below the paid count), in one of the buckets we asked for, of epoch e.
func (c *Client) checkBucketRec(r bucketRec, e uint64, ids map[uint16]bool, bits int, committed map[common.Hash]event) (rawBatch, bool) {
	bs := r.env.buckets(bits)
	if r.env.Epoch != e || !ids[bs[0]] && !ids[bs[1]] {
		return rawBatch{}, false
	}
	ev, ok := committed[r.root]
	if !ok || ev.Block != r.block {
		return rawBatch{}, false
	}
	n := ev.Args["n"].(*big.Int).Int64()
	if int64(r.idx) >= n || int64(r.n) != n {
		return rawBatch{}, false
	}
	lr, err := c.leafRootOn(r.root)
	if err != nil || lr == (common.Hash{}) {
		return rawBatch{}, false // committed without a leaf root: only the whole batch proves it
	}
	got, ok := proofRoot(crypto.Keccak256Hash(r.raw), int(r.idx), int(n), r.proof)
	if !ok || got != lr {
		return rawBatch{}, false
	}
	return rawBatch{Block: r.block, Data: r.raw, Root: r.root.Hex()}, true
}
