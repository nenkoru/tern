// A relayer posts other people's envelopes and markers from its own
// address and pays the fees, so senders never appear on chain. It sees
// only sealed envelopes and random-looking markers. Anyone can run one;
// a production relayer would take private payments for its fees.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern/internal/netlimit"
)

type Relay struct {
	ch       *Chain
	key      *ecdsa.PrivateKey
	mu       sync.Mutex
	envs     []Envelope
	inflight []Envelope // the chunk being posted: still on disk until it is on chain
	fails    int        // failed posts of the current chunk in a row
	chunkMax int        // envelopes per chunk now (halved while a chunk keeps failing)
	markers  []common.Hash
	senders  map[string][]int64 // per sender address: when its last envelopes and markers came
	// As a data node: batches are kept here and only their hashes go on chain.
	da       bool
	store    map[common.Hash][]byte
	dir      string // where batches and earned vouchers are kept (empty: memory only)
	watched  uint64 // next block to check for challenges
	mirrored uint64 // next block to mirror other nodes' batches from
	mirrors  bool   // copy other nodes' batches
	// What senders pay this node per envelope, to its Tern name, by voucher.
	name     string
	price    *big.Int
	vouchers map[string]voucherState
	// Push: what it accepted, before it is on chain (stream.go).
	push
	vdirty      bool                          // vouchers changed since they were written (perfFixes)
	qmu         sync.Mutex                    // the queue file: appends and rewrites
	queuedSince time.Time                     // when the oldest envelope not yet posted came (flushDue)
	lockInfo    sync.Map                      // channel lock => *chanInfo (acceptVoucher)
	claiming    atomic.Bool                   // a claim of earnings is under way
	window      int64                         // seconds its receipts promise to commit within (0: receiptWindow)
	where       map[common.Hash]leafLoc       // envelope => the committed batch it is in (to rebut missed receipts)
	missSeen    uint64                        // next block to check for claims against its receipts
	claims      map[[2]common.Hash]*openClaim // (envelope, claim) => a claim not settled on chain yet (IP-3; claims.json)
	postErr     time.Time                     // when a post last failed (zero: the last one went through): no receipts meanwhile (IP-5)
	poolFee     *big.Int                      // its fee per pool withdrawal (nil: poolRelayFee)
	// Tag buckets (bucket.go): the index of committed envelopes by bucket, and
	// where it read other nodes' pending sets.
	bk        *bucketIndex
	bkOnce    sync.Once
	publicURL string
	peerCur   map[string]pendCursor
	peerBad   map[common.Address]int // copies a peer's pending sent that never landed by their deadline (BK-3)
}

func (r *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(r.info())
	})
	mux.HandleFunc("POST /envelope", func(w http.ResponseWriter, req *http.Request) {
		// A node that charges limits each paying channel (perPaidMin a minute)
		// rather than each address: one client paying for 3 requests a second
		// was refused by the 120-a-minute limit meant for free relays, while a
		// farm behind many addresses was not. A free relay limits each address.
		paid := perfFixes && r.price != nil && r.price.Sign() > 0 && req.Header.Get("X-Voucher-Lock") != ""
		if paid {
			if msg := r.admitKey("lock:"+req.Header.Get("X-Voucher-Lock"), perPaidMin); msg != "" {
				http.Error(w, msg, http.StatusTooManyRequests)
				return
			}
		} else if msg := r.admit(req, false); msg != "" {
			http.Error(w, msg, http.StatusTooManyRequests)
			return
		}
		if err := r.acceptVoucher(req.Header.Get("X-Voucher-Lock"), req.Header.Get("X-Voucher-Cumulative"), req.Header.Get("X-Voucher-Sig")); err != nil {
			code := 400
			if err == errPaymentRequired {
				code = 402
			}
			http.Error(w, err.Error(), code)
			return
		}
		b, _ := io.ReadAll(io.LimitReader(req.Body, 300<<10))
		es, err := decodeBatch(b)
		if err != nil || len(es) != 1 {
			http.Error(w, "one envelope", 400)
			return
		}
		if len(b) > maxPostBytes { // it could never be posted (or resolved), and would block the queue
			http.Error(w, fmt.Sprintf("envelope of %d bytes: a relay posts at most %d bytes per batch", len(b), maxPostBytes), http.StatusRequestEntityTooLarge)
			return
		}
		if perfFixes { // accepted means kept: appended to the queue file (not the whole queue rewritten per envelope)
			r.qmu.Lock()
			r.mu.Lock()
			r.envs = append(r.envs, es[0])
			r.mu.Unlock()
			r.appendQueue(es[0])
			r.qmu.Unlock()
		} else {
			r.mu.Lock()
			r.envs = append(r.envs, es[0])
			r.mu.Unlock()
			r.saveQueue() // accepted means kept: a restart must not lose it
		}
		rc := r.accepted(es[0], common.HexToHash(req.Header.Get("X-Receipt-Claim"))) // no claim: no receipt
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		if rc != nil {
			json.NewEncoder(w).Encode(rc)
		}
	})
	mux.HandleFunc("GET /pending", r.servePending)
	mux.HandleFunc("GET /bucket/{ids}", r.serveBucket)
	mux.HandleFunc("GET /stream", r.serveStream)
	mux.HandleFunc("GET /batch/{root}", func(w http.ResponseWriter, req *http.Request) {
		b, ok := r.get(common.HexToHash(req.PathValue("root")))
		if !ok {
			http.Error(w, "not here", 404)
			return
		}
		w.Write(b)
	})
	// GET /heads/ROOT: each envelope's epoch, ephemeral key and tags (56 bytes
	// instead of ~650): a reader checks its tags and fetches the batch only if
	// something in it is its own (daHeads in da.go).
	mux.HandleFunc("GET /heads/{root}", func(w http.ResponseWriter, req *http.Request) {
		b, ok := r.get(common.HexToHash(req.PathValue("root")))
		if !ok {
			http.Error(w, "not here", 404)
			return
		}
		es, err := decodeBatch(b)
		if err != nil {
			http.Error(w, "bad batch", 500)
			return
		}
		w.Write(encodeHeads(es))
	})
	mux.HandleFunc("POST /marker", func(w http.ResponseWriter, req *http.Request) {
		if msg := r.admit(req, true); msg != "" {
			http.Error(w, msg, http.StatusTooManyRequests)
			return
		}
		b, _ := io.ReadAll(io.LimitReader(req.Body, 64))
		if len(b) != 32 {
			http.Error(w, "32 bytes", 400)
			return
		}
		r.mu.Lock()
		r.markers = append(r.markers, common.BytesToHash(b))
		r.mu.Unlock()
		w.WriteHeader(202)
	})
	r.privateRoutes(mux) // pool withdrawals and secret votes (relay_private.go)
	return mux
}

// What one transaction may carry (Inbox.post: 256 envelopes and 128 KiB;
// postRoot: 4096 envelopes and 4 MiB), what the queue holds at most, and what
// one sender may hand over a minute.
const (
	maxPostEnvs   = 256
	maxPostBytes  = 120 << 10 // under geth's 128 KiB transaction limit, with the call's encoding
	maxRootEnvs   = 4096
	maxRootBytes  = 120 << 10 // the Inbox takes 4 MiB, but resolve and proveFraud must carry the batch in one transaction (geth: 128 KiB)
	maxQueue      = 20000
	maxMarkers    = 256
	perSenderMin  = 120
	perPaidMin    = 1200 // per paying channel, at a node that charges
	chunksPerTick = 8
)

// flushDue: is it time to post what is queued? A batch costs about 120,000
// gas whatever it holds; posting every second at a few envelopes a second
// made batches of ~6 envelopes, ~19,000 gas each (the scale simulation). With
// perfFixes a data node waits for flushMin envelopes or flushWait seconds:
// recipients have them at once from its memory anyway, and its receipts
// promise the chain within receiptWindow (120 s).
func (r *Relay) flushDue() bool {
	if !perfFixes || !r.da {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.inflight) > 0 || len(r.envs) >= flushMin || len(r.markers) > 0 {
		return true
	}
	if len(r.envs) == 0 {
		r.queuedSince = time.Time{}
		return false
	}
	if r.queuedSince.IsZero() {
		r.queuedSince = time.Now()
	}
	return time.Since(r.queuedSince) >= flushWait
}

const (
	flushMin  = 64
	flushWait = 10 * time.Second
)

// admitKey: room in the queue, and this key within limit a minute.
func (r *Relay) admitKey(key string, limit int) string {
	now := time.Now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.envs)+len(r.inflight) >= maxQueue {
		return "the relay's queue is full; try again in a minute"
	}
	if r.senders == nil {
		r.senders = map[string][]int64{}
	}
	ts := recent(r.senders[key], now, 60)
	if len(ts) >= limit {
		return fmt.Sprintf("more than %d a minute on this channel; slow down", limit)
	}
	r.senders[key] = append(ts, now)
	return ""
}

// admit: room in the queue, and this sender within its rate.
func (r *Relay) admit(req *http.Request, marker bool) string {
	ip, now := netlimit.Bucket(netlimit.ClientIP(req)), time.Now().Unix() // per IPv4 address, per IPv6 /64
	r.mu.Lock()
	defer r.mu.Unlock()
	if !marker && len(r.envs)+len(r.inflight) >= maxQueue || marker && len(r.markers) >= maxMarkers {
		return "the relay's queue is full; try again in a minute"
	}
	if r.senders == nil {
		r.senders = map[string][]int64{}
	}
	ts := recent(r.senders[ip], now, 60)
	if len(ts) >= perSenderMin {
		return fmt.Sprintf("more than %d a minute from you; slow down", perSenderMin)
	}
	r.senders[ip] = append(ts, now)
	if len(r.senders) > 10000 { // forget idle senders
		for k, v := range r.senders {
			if len(recent(v, now, 60)) == 0 {
				delete(r.senders, k)
			}
		}
	}
	return ""
}

// chunk: the next envelopes that fit in one transaction.
func (r *Relay) chunk() []Envelope {
	maxN, maxB := maxPostEnvs, maxPostBytes
	if r.da {
		maxN, maxB = maxRootEnvs, maxRootBytes
	}
	if r.chunkMax > 0 && r.chunkMax < maxN {
		maxN = r.chunkMax
	}
	n, size := 0, 0
	for n < len(r.envs) && n < maxN {
		l := len(r.envs[n].encode())
		if n > 0 && size+l > maxB {
			break
		}
		size += l
		n++
	}
	c := r.envs[:n:n]
	r.envs = r.envs[n:]
	return c
}

// flush posts what is queued, a chunk per transaction within the Inbox's
// limits. A chunk that keeps failing is halved, and a single envelope that
// keeps failing is set aside (quarantine.bin): one bad envelope never stops
// the relay. The chunk in flight stays in the queue file until it is on chain.
func (r *Relay) flush() error {
	for i := 0; i < chunksPerTick; i++ {
		r.mu.Lock()
		if len(r.inflight) == 0 {
			r.inflight = r.chunk()
		}
		envs := r.inflight
		r.mu.Unlock()
		if len(envs) == 0 {
			break
		}
		r.saveQueue()
		if err := r.post(envs); err != nil {
			r.mu.Lock()
			r.postErr = time.Now() // IP-5: no new receipts until a post goes through
			r.mu.Unlock()
			r.failed(envs, err)
			return err
		}
		r.mu.Lock()
		r.inflight, r.fails, r.chunkMax, r.postErr = nil, 0, 0, time.Time{}
		r.mu.Unlock()
		r.saveQueue()
	}
	r.mu.Lock()
	markers := r.markers
	r.markers = nil
	r.mu.Unlock()
	for len(markers) > 0 {
		if _, err := r.ch.send(r.key, nil, "Commitments", "commit", common.Hash{}, markers[0], common.Hash{}, uint64(0)); err != nil {
			r.mu.Lock()
			r.markers = append(markers, r.markers...)
			r.mu.Unlock()
			return err
		}
		markers = markers[1:]
	}
	return nil
}

func (r *Relay) post(envs []Envelope) error {
	total, err := r.ch.feeFor(len(envs)) // the Inbox refunds the headroom
	if err != nil {
		return err
	}
	batch := encodeBatch(envs)
	if r.da { // keep the bytes, put only the hash on chain
		root := crypto.Keccak256Hash(batch)
		if err := r.put(root, batch); err != nil {
			return fmt.Errorf("storing the batch: %w", err)
		}
		n, size := big.NewInt(int64(len(envs))), big.NewInt(int64(len(batch)))
		if f, err := r.ch.call("Inbox", "rootFee", n, size); err == nil { // by size too, where the Inbox charges so
			total = new(big.Int).Div(new(big.Int).Mul(f[0].(*big.Int), big.NewInt(3)), big.NewInt(2))
		}
		hs := envHashes(envs)
		// with the leaves' Merkle root: what this node's receipts are proven against
		rc, err := r.ch.send(r.key, total, "Inbox", "postRootL", root, n, size, leafRoot(hs))
		if err != nil {
			return err
		}
		r.committed(root, hs, rc.BlockNumber.Uint64())
		return nil
	}
	rc, err := r.ch.send(r.key, total, "Inbox", "post", batch, big.NewInt(int64(len(envs))))
	if err != nil {
		return err
	}
	r.committed(common.Hash{}, envHashes(envs), rc.BlockNumber.Uint64())
	return nil
}

// failed: the chunk stays in flight; after three failures in a row it is
// halved (the rest goes back to the queue), and a lone envelope is set aside.
// Running out of gas money is the relay's problem, not the envelopes'.
func (r *Relay) failed(envs []Envelope, err error) {
	if strings.Contains(err.Error(), "insufficient funds") {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fails++; r.fails < 3 {
		return
	}
	r.fails = 0
	if len(envs) > 1 {
		half := len(envs) / 2
		r.inflight, r.chunkMax = envs[:half:half], half
		r.envs = append(append([]Envelope{}, envs[half:]...), r.envs...)
		return
	}
	log.Printf("relay: setting aside an envelope that can't be posted (%v)", err)
	if r.dir != "" {
		p := filepath.Join(r.dir, "quarantine.bin")
		old, _ := os.ReadFile(p)
		os.WriteFile(p, append(old, encodeBatch(envs)...), 0o600)
	}
	r.inflight, r.chunkMax = nil, 0
}

// saveQueue keeps the envelopes not yet on chain on disk (data nodes with a store).
func (r *Relay) saveQueue() {
	if r.dir == "" {
		return
	}
	r.qmu.Lock()
	defer r.qmu.Unlock()
	r.mu.Lock()
	b := encodeBatch(append(append([]Envelope{}, r.inflight...), r.envs...))
	r.mu.Unlock()
	p := filepath.Join(r.dir, "queue.bin")
	if os.WriteFile(p+".tmp", b, 0o600) == nil {
		os.Rename(p+".tmp", p)
	}
}

// appendQueue adds one accepted envelope to the queue file (callers hold qmu).
func (r *Relay) appendQueue(e Envelope) {
	if r.dir == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(r.dir, "queue.bin"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	f.Write(e.encode())
	f.Close()
}

// loadQueue takes back what a previous run accepted but did not post: its
// queue, and stored batches whose root never made it on chain (a failed post).
func (r *Relay) loadQueue() {
	if r.dir == "" {
		return
	}
	if b, err := os.ReadFile(filepath.Join(r.dir, "queue.bin")); err == nil {
		es, _ := decodeBatch(b)
		r.envs = append(r.envs, es...)
	}
	files, _ := filepath.Glob(filepath.Join(r.dir, "*.batch"))
	for _, f := range files {
		root := common.HexToHash(strings.TrimSuffix(filepath.Base(f), ".batch"))
		st, err := r.ch.call("Inbox", "commitment", root)
		if err != nil || st[2].(uint8) != 0 {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil || crypto.Keccak256Hash(b) != root {
			continue
		}
		if es, err := decodeBatch(b); err == nil {
			log.Printf("relay: re-queuing %d envelopes of batch %s, which never reached the chain", len(es), root.Hex()[:10])
			have := map[common.Hash]bool{}
			for _, e := range r.envs {
				have[e.hash()] = true
			}
			for _, e := range es {
				if !have[e.hash()] { // the queue file may hold them already (in flight when it stopped)
					r.envs = append(r.envs, e)
				}
			}
			os.Remove(f)
			delete(r.store, root)
		}
	}
}

func (r *Relay) Serve(listen string) error {
	if r.da && r.ch.bucketBits() > 0 { // tag buckets: the index, and other nodes' pending envelopes (bucket.go)
		go func() {
			for range time.Tick(500 * time.Millisecond) {
				if err := r.indexBuckets(); err != nil {
					log.Printf("relay: bucket index: %v", err)
				}
			}
		}()
		go func() {
			for range time.Tick(time.Second) {
				r.pollPeers()
			}
		}()
	}
	go func() {
		tick := 0
		for range time.Tick(time.Second) {
			tick++
			if perfFixes {
				r.mu.Lock()
				if r.vdirty {
					r.saveVouchers()
					r.vdirty = false
				}
				r.mu.Unlock()
			}
			if r.price != nil && r.price.Sign() > 0 && tick%60 == 0 { // collect earnings once a minute
				claim := func() {
					if earned, err := r.claimEarningsIf(perfFixes); err != nil {
						log.Printf("relay: claiming earnings: %v", err)
					} else if earned.Sign() > 0 {
						log.Printf("relay: earned %s", fmtCoins(earned))
					}
				}
				if !perfFixes {
					claim()
				} else if r.claiming.CompareAndSwap(false, true) {
					// beside the posting, not in its way: claims used to hold up batches (and receipts' deadlines) for minutes
					go func() { defer r.claiming.Store(false); claim() }()
				}
			}
			if r.flushDue() {
				if err := r.flush(); err != nil {
					log.Printf("relay: %v", err)
				}
			}
			if r.da {
				if err := r.resolveChallenges(); err != nil {
					log.Printf("relay: resolving a challenge: %v", err)
				}
				if err := r.rebutMisses(); err != nil {
					log.Printf("relay: rebutting a missed-receipt claim: %v", err)
				}
				if r.mirrors && r.ch.bucketBits() == 0 { // with buckets the index mirrors (bucket.go)
					if _, err := r.mirror(); err != nil {
						log.Printf("relay: mirroring: %v", err)
					}
				}
			}
		}
	}()
	log.Printf("ternchain relay on %s", listen)
	srv := &http.Server{Addr: listen, Handler: r.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, // streams lift WriteTimeout for themselves
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 16 << 10}
	return srv.ListenAndServe()
}

// relayPost hands an envelope or a marker to a relayer instead of posting it ourselves.
func relayPost(url, path string, body []byte) error { return relayPostWith(url, path, body, nil) }

func relayPostWith(url, path string, body []byte, h http.Header) error {
	_, err := relayPostReply(url, path, body, h)
	return err
}

// relayPostReply also returns the relay's answer (a data node's signed receipt).
func relayPostReply(url, path string, body []byte, h http.Header) ([]byte, error) {
	req, err := http.NewRequest("POST", strings.TrimRight(url, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 202 {
		return nil, fmt.Errorf("relay refused: %s", b)
	}
	return b, nil
}
