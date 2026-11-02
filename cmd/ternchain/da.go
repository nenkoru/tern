// The data availability layer: batches of envelopes live at data nodes; the
// chain keeps only their hashes (Inbox.postRoot), with a challenge game that
// forces a missing batch on chain or costs its poster its stake.
//
// A data node is a relayer with a stake: it batches envelopes from many
// senders, keeps the bytes, serves GET /batch/<root>, posts the root, and
// answers challenges by putting the batch on chain. Readers fetch a batch
// from the URL its poster staked with and check its hash.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

type rawBatch struct {
	Block uint64
	Data  []byte
	Root  string // the batch root, for batches committed by hash
	URL   string // the data node it was fetched from
}

// PendingBatch: a committed batch we couldn't fetch yet.
type PendingBatch struct {
	Block  uint64 `json:"block"`
	Poster string `json:"poster"`
	N      uint64 `json:"n,omitempty"`     // envelopes the poster paid for
	Size   uint64 `json:"size,omitempty"`  // bytes it declared
	Tries  int    `json:"tries,omitempty"` // time windows in which every node we asked answered without it
	Down   int    `json:"down,omitempty"`  // time windows in which no node we asked could be reached
	Last   uint64 `json:"last,omitempty"`  // chain time of the last window counted
}

// daMinTries: a batch past its horizon is given up only after every node we
// asked answered without it in this many distinct windows of daTryGap seconds
// of chain time (audit 4 IF-1; audit 5: by time, not by scans, so a node
// restarting empty for a minute doesn't cost a reader its mail).
const daMinTries = 3

// daTryGap: one try per this many seconds of chain time, however often we scan.
const daTryGap = 1800

// daDownAge: a node that can't be reached isn't a try (we may be the ones
// offline, or it is restarting). Its batches are given up only if it stays
// unreachable in daMinTries windows and a day past the horizon, so a dead
// URL's junk roots don't stay pending forever.
const daDownAge = 86400

// statusErr: the node answered, without the batch (a 404): the node works.
type statusErr struct{ status string }

func (e statusErr) Error() string { return "data node: " + e.status }

// errWrongBytes: the node answered, with bytes that don't hash to the root.
var errWrongBytes = errors.New("data node served the wrong bytes")

var httpc = &http.Client{Timeout: 5 * time.Second}

// allowPrivate: may batches be fetched from loopback and private addresses?
// Only on a local network (its RPC is local); otherwise a staked URL could
// point readers at their own internal services.
var allowPrivate = true

// daHTTP fetches batches from staked URLs: short timeouts, no private
// addresses on a public network.
var daHTTP = &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
	DialContext: (&net.Dialer{Timeout: 2 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, _ := net.SplitHostPort(address)
		if ip := net.ParseIP(host); !allowPrivate && ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
			return fmt.Errorf("refusing a private address %s for a public network's data node", host)
		}
		return nil
	}}).DialContext,
	ResponseHeaderTimeout: 3 * time.Second,
}}

// Reading data nodes: a scan spends at most daBudget fetching, tries the
// poster's node and at most daMirrors others per batch, and skips a node for
// the rest of the scan after one failure.
const (
	daBudget  = 10 * time.Second
	daMirrors = 2
)

// daHeads: readers fetch a batch's envelope heads first (GET /heads/ROOT) and
// the batch only if one of them is theirs. Off by default: the heads are not
// committed on chain, so a node could hide one from a reader; the full batch
// is checked against its root. ('ternchain simulate --tag-index' turns it on.)
var daHeads = false

// encodeHeads / decodeHeads: epoch | eph | tag | tag2 per envelope (56 bytes).
func encodeHeads(es []Envelope) []byte {
	out := make([]byte, 0, 56*len(es))
	for _, e := range es {
		var ep [8]byte
		binary.BigEndian.PutUint64(ep[:], e.Epoch)
		out = append(out, ep[:]...)
		out = append(out, e.Eph[:]...)
		out = append(out, e.Tag[:]...)
		out = append(out, e.Tag2[:]...)
	}
	return out
}

func decodeHeads(b []byte) ([]Envelope, error) {
	if len(b)%56 != 0 {
		return nil, fmt.Errorf("bad heads: %d bytes", len(b))
	}
	var out []Envelope
	for i := 0; i+56 <= len(b); i += 56 {
		var e Envelope
		e.Epoch = binary.BigEndian.Uint64(b[i:])
		copy(e.Eph[:], b[i+8:i+40])
		copy(e.Tag[:], b[i+40:i+48])
		copy(e.Tag2[:], b[i+48:i+56])
		out = append(out, e)
	}
	return out, nil
}

func fetchHeads(url string, root common.Hash) ([]byte, error) {
	if url == "" || strings.ContainsAny(url, "?#") {
		return nil, fmt.Errorf("no data node")
	}
	resp, err := daHTTP.Get(strings.TrimRight(url, "/") + "/heads/" + root.Hex())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, statusErr{resp.Status}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

var fetchHeadsFn = fetchHeads

// headsMine: is any envelope in these heads ours (a family tag or a group's)?
func (c *Client) headsMine(es []Envelope) bool {
	for i := range es {
		if c.mineView(&es[i]) || c.groupFor(&es[i]) != nil {
			return true
		}
		if r := c.st.Sent[es[i].hash().Hex()]; r != nil && r.Block == 0 { // one we sent, not confirmed yet: read it to see it landed
			return true
		}
	}
	return false
}

// fetchBatchFn: how a reader's scan fetches a batch (a simulation of many
// readers in one process shares the fetches, and counts what each would download).
var fetchBatchFn = fetchBatch

func fetchBatch(url string, root common.Hash) ([]byte, error) {
	if url == "" {
		return nil, fmt.Errorf("no data node")
	}
	if strings.ContainsAny(url, "?#") { // the path we append must stay a path (audit 4 DV-1: "…/advance?seconds=N&x=")
		return nil, fmt.Errorf("data node URL %q has a query or fragment: refused", url)
	}
	resp, err := daHTTP.Get(strings.TrimRight(url, "/") + "/batch/" + root.Hex())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, statusErr{resp.Status}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if crypto.Keccak256Hash(b) != root {
		return nil, fmt.Errorf("%w for %s", errWrongBytes, root.Hex()[:10])
	}
	return b, nil
}

// daURLs: where each poster's batches are served (the URL it staked with),
// for posters that hold the minimum stake now: a URL registered for free
// is not a data node.
func (c *Client) daURLs() map[common.Address]string { return stakedURLs(c.ch) }

func stakedURLs(ch *Chain) map[common.Address]string {
	if perfFixes { // asked on every scan with a batch to fetch: the whole Staked history plus a call per node; a minute-old answer will do
		if v, ok := ch.staked.Load().(*stakedAt); ok && time.Since(v.at) < time.Minute {
			out := make(map[common.Address]string, len(v.urls))
			for k, u := range v.urls {
				out[k] = u
			}
			return out
		}
		out := stakedURLsFetch(ch)
		ch.staked.Store(&stakedAt{urls: out, at: time.Now()})
		cp := make(map[common.Address]string, len(out))
		for k, u := range out {
			cp[k] = u
		}
		return cp
	}
	return stakedURLsFetch(ch)
}

type stakedAt struct {
	urls map[common.Address]string
	at   time.Time
}

func stakedURLsFetch(ch *Chain) map[common.Address]string {
	out := map[common.Address]string{}
	min, err := ch.call("Inbox", "minStake")
	if err != nil {
		return out
	}
	need := min[0].(*big.Int)
	if need.Sign() == 0 {
		need = big.NewInt(1)
	}
	if evs, err := ch.events("Inbox", "Staked"); err == nil {
		for _, ev := range evs {
			out[ev.Args["poster"].(common.Address)] = ev.Args["url"].(string)
		}
	}
	for a := range out {
		if st, err := ch.call("Inbox", "stake", a); err != nil || st[0].(*big.Int).Cmp(need) < 0 {
			delete(out, a)
		}
	}
	return out
}

// batchesSince gathers every batch from block `from`: posted in full,
// resolved on chain after a challenge, or committed by hash and fetched from
// its data node. Batches that can't be fetched yet are kept and retried.
func (c *Client) batchesSince(from uint64) ([]rawBatch, error) {
	var out []rawBatch
	var posted, resolved, committed []event
	var err error
	if perfFixes { // one log query for the three (a scan made three, each over the same blocks)
		evs, err := c.ch.eventsMulti(from, "Inbox", "Posted", "Resolved", "Committed")
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			switch e.Name {
			case "Posted":
				posted = append(posted, e)
			case "Resolved":
				resolved = append(resolved, e)
			case "Committed":
				committed = append(committed, e)
			}
		}
	} else if posted, err = c.ch.eventsFrom(from, "Inbox", "Posted"); err != nil {
		return nil, err
	}
	for _, ev := range posted {
		out = append(out, rawBatch{Block: ev.Block, Data: ev.Args["batch"].([]byte)})
	}
	if !perfFixes {
		if resolved, err = c.ch.eventsFrom(from, "Inbox", "Resolved"); err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	for _, ev := range resolved {
		root := common.Hash(ev.Args["root"].([32]byte)).Hex()
		seen[root] = true
		delete(c.st.PendingDA, root)
		out = append(out, rawBatch{ev.Block, ev.Args["batch"].([]byte), root, "on chain (resolved after a challenge)"})
	}
	if !perfFixes {
		if committed, err = c.ch.eventsFrom(from, "Inbox", "Committed"); err != nil {
			return nil, err
		}
	}
	if c.st.PendingDA == nil {
		c.st.PendingDA = map[string]PendingBatch{}
	}
	for _, ev := range committed {
		root := common.Hash(ev.Args["root"].([32]byte)).Hex()
		if !seen[root] {
			c.st.PendingDA[root] = PendingBatch{Block: ev.Block, Poster: ev.Args["poster"].(common.Address).Hex(),
				N: ev.Args["n"].(*big.Int).Uint64(), Size: ev.Args["size"].(*big.Int).Uint64()}
		}
	}
	if len(c.st.PendingDA) == 0 {
		return out, nil
	}
	urls := c.daURLs()
	if perfFixes { // a poster that staked since the list was read
		for _, p := range c.st.PendingDA {
			if _, ok := urls[common.HexToAddress(p.Poster)]; !ok {
				urls = stakedURLsFetch(c.ch)
				c.ch.staked.Store(&stakedAt{urls: urls, at: time.Now()})
				break
			}
		}
	}
	others := sortedURLs(urls) // other nodes, in a fixed order
	// A batch nobody can force on chain any more (its challenge and resolve
	// windows are over) is never coming: stop asking for it.
	var horizon uint64
	if cw, err := c.ch.callFixed("Inbox", "challengeWindow"); err == nil { // immutables
		if rw, err := c.ch.callFixed("Inbox", "resolveWindow"); err == nil {
			horizon = cw[0].(uint64) + 2*rw[0].(uint64)
		}
	}
	now := uint64(c.chainNow())
	deadline, failed := time.Now().Add(daBudget), map[string]bool{}
	for root, p := range c.st.PendingDA {
		h := common.HexToHash(root)
		// The batch's state on chain matters only once its node failed to
		// serve it (perfFixes: a reader catching up asked once per batch before
		// even trying; a resolved batch also comes as a Resolved event).
		st := []any{nil, uint64(0), uint8(1)}
		if !perfFixes || p.Tries+p.Down > 0 {
			var err error
			if st, err = c.ch.call("Inbox", "commitment", h); err != nil {
				continue
			}
		}
		if at := st[1].(uint64); horizon > 0 && st[2].(uint8) <= 2 && now > at+horizon &&
			(p.Tries >= daMinTries || p.Tries+p.Down >= daMinTries && now > at+horizon+daDownAge) {
			delete(c.st.PendingDA, root)
			c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("batch %s (block %d, poster %s) given up: no data node had it (%d tries %d min apart, %d more with its node unreachable), and it can no longer be forced on chain",
				root[:10], p.Block, p.Poster, p.Tries, daTryGap/60, p.Down))
			continue
		}
		switch st[2].(uint8) {
		case 4: // lost: its poster paid the challenger; nothing to read
			delete(c.st.PendingDA, root)
			continue
		case 3: // resolved: the batch is on chain
			if evs, err := c.ch.events("Inbox", "Resolved", []common.Hash{h}); err == nil && len(evs) > 0 {
				out = append(out, rawBatch{p.Block, evs[0].Args["batch"].([]byte), root, "on chain (resolved after a challenge)"})
				delete(c.st.PendingDA, root)
			}
			continue
		}
		if !time.Now().Before(deadline) {
			continue // out of time this scan: the rest waits for the next one
		}
		order := c.daOrder(urls[common.HexToAddress(p.Poster)], others)
		asked, missed, answered := 0, 0, 0
		got, outOfTime := false, false
		for _, url := range order {
			if !time.Now().Before(deadline) {
				outOfTime = true
				continue
			}
			if url == "" || failed[url] { // no node, or one that didn't answer this scan: a miss
				continue
			}
			asked++
			if daHeads {
				if hb, err := fetchHeadsFn(url, h); err == nil {
					if es, err := decodeHeads(hb); err == nil && (p.N == 0 || uint64(len(es)) <= p.N) && !c.headsMine(es) {
						got = true // nothing of ours in it: read
						delete(c.st.PendingDA, root)
						break
					}
				}
			}
			b, err := fetchBatchFn(url, h)
			if err != nil {
				missed++
				if _, ok := err.(statusErr); !ok { // only a node that doesn't answer is skipped for the scan (IF-3)
					failed[url] = true
				}
				if _, ok := err.(statusErr); ok || errors.Is(err, errWrongBytes) {
					answered++ // it answered without the batch: a try
				}
				continue
			}
			got = true
			delete(c.st.PendingDA, root)
			// The reader rule (DESIGN §5): the poster paid for N envelopes of Size
			// bytes; a batch of another length is not read, and only its first N
			// envelopes are, so one fee can't carry thousands.
			if p.N != 0 {
				es, err := decodeBatch(b)
				if err != nil || uint64(len(b)) != p.Size {
					c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("batch %s from %s is %d bytes, but %d were paid for: not read", root[:10], url, len(b), p.Size))
					break
				}
				if uint64(len(es)) > p.N {
					c.st.Alerts = append(c.st.Alerts, fmt.Sprintf("batch %s from %s holds %d envelopes but paid for %d: only those are read (anyone may proveFraud it)",
						root[:10], url, len(es), p.N))
					b = encodeBatch(es[:p.N])
				}
			}
			out = append(out, rawBatch{p.Block, b, root, url})
			break
		}
		if !got && !outOfTime && missed == asked && now >= p.Last+daTryGap { // one try a window (IF-1)
			if answered > 0 {
				p.Tries++
			} else {
				p.Down++ // nothing answered: not a try
			}
			p.Last = now
			c.st.PendingDA[root] = p
		}
	}
	return out, nil
}

// sortedURLs: the staked nodes' URLs, in a fixed order.
func sortedURLs(urls map[common.Address]string) []string {
	var out []string
	for _, u := range urls {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// daOrder: where a batch is asked for. Our own relay first (net.json "relay":
// a nearer or reachable copy, e.g. through a tunnel where the poster's public
// URL is cut), then the poster's node (own), then a few other nodes (mirrors).
// Any of them is safe: the bytes must hash to the root on chain.
func (c *Client) daOrder(own string, others []string) []string {
	order := []string{own}
	if c.relay != "" && strings.TrimRight(c.relay, "/") != strings.TrimRight(own, "/") {
		order = []string{c.relay, own}
	}
	for _, u := range others {
		if len(order) > daMirrors {
			break
		}
		if u != own {
			order = append(order, u)
		}
	}
	return order
}

// fetchFrom: a batch from the first node in order that serves it, and which
// one did.
func fetchFrom(order []string, root common.Hash) ([]byte, string, error) {
	err := fmt.Errorf("no data node")
	for _, url := range order {
		if url == "" {
			continue
		}
		var b []byte
		if b, err = fetchBatchFn(url, root); err == nil {
			return b, url, nil
		}
	}
	return nil, "", err
}

// committedEnvs: the envelopes a reader reads from a committed batch (its
// first n, of a batch of the size paid for), and the node it came from.
func (c *Client) committedEnvs(ev event, urls map[common.Address]string, others []string) ([]Envelope, string, error) {
	root := common.Hash(ev.Args["root"].([32]byte))
	b, url, err := fetchFrom(c.daOrder(urls[ev.Args["poster"].(common.Address)], others), root)
	if err != nil {
		return nil, "", err
	}
	es, err := decodeBatch(b)
	if err != nil {
		return nil, url, err
	}
	if n, ok := ev.Args["n"].(*big.Int); ok && n.Sign() > 0 {
		if size, ok := ev.Args["size"].(*big.Int); ok && uint64(len(b)) != size.Uint64() {
			return nil, url, fmt.Errorf("batch %s is %d bytes, %d were paid for", root.Hex()[:10], len(b), size.Uint64())
		}
		if uint64(len(es)) > n.Uint64() {
			es = es[:n.Uint64()]
		}
	}
	return es, url, nil
}

// findBatch: which committed batch at `block` holds envelope id, and the
// node that serves it ("", "" if it was posted on chain in full).
func (c *Client) findBatch(id string, block uint64) (string, string) {
	evs, err := c.ch.eventsFrom(block, "Inbox", "Committed")
	if err != nil {
		return "", ""
	}
	urls := c.daURLs()
	others := sortedURLs(urls)
	for _, ev := range evs {
		if ev.Block != block {
			continue
		}
		root := common.Hash(ev.Args["root"].([32]byte))
		es, url, err := c.committedEnvs(ev, urls, others)
		if err != nil {
			return root.Hex(), urls[ev.Args["poster"].(common.Address)] + " (not reachable now)"
		}
		for _, e := range es {
			if e.hash().Hex() == id {
				return root.Hex(), url
			}
		}
	}
	return "", ""
}

// Looking for lost envelopes on chain (findLost): at most lostPerScan records
// a scan, each at most once, at most lostFetches batches fetched in all.
const (
	lostPerScan = 5
	lostFetches = 16
	lostSlack   = 600 // seconds before the hand-over the search starts (clocks, a relay's queue)
)

// findLost: an envelope we called lost (not on chain long after a relay took
// it) may be on chain all the same, in a batch our scan never read: the
// cursor had passed its block (an older client skipped it, a node served it
// late). For a lost record under alertTTL old it is looked for: first as a
// batch of its own (the envelope id is that batch's root: Inbox.commitment),
// then in the batches posted or committed since it was handed over, fetched
// in the reader's order. Found: where it landed is kept and the alert taken back.
func (c *Client) findLost() {
	if len(c.st.Sent) == 0 {
		return
	}
	now := c.chainNow()
	var ids []string
	for id, r := range c.st.Sent {
		if r.Lost && r.Block == 0 && now-r.Time <= alertTTL && len(id) >= 18 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	sort.Slice(ids, func(i, j int) bool { // the newest first, then by id: a fixed order
		a, b := c.st.Sent[ids[i]], c.st.Sent[ids[j]]
		return a.Time > b.Time || a.Time == b.Time && ids[i] < ids[j]
	})
	if len(ids) > lostPerScan {
		ids = ids[:lostPerScan]
	}
	var urls map[common.Address]string
	stakedURL := func(a common.Address) string {
		if urls == nil {
			urls = c.daURLs()
		}
		return urls[a]
	}
	// (a) a batch of its own: one call each.
	var rest []string
	for _, id := range ids {
		r := c.st.Sent[id]
		st, err := c.ch.call("Inbox", "commitment", common.HexToHash(id))
		if err != nil || st[2].(uint8) == 0 || st[2].(uint8) == 4 { // unknown, or lost (its poster paid the challenger: nobody can read it)
			rest = append(rest, id)
			continue
		}
		if r.Withheld == id && st[2].(uint8) != 3 { // committed, but its node doesn't serve it: not landed (C2-WITHHOLD)
			continue
		}
		at, block := int64(st[1].(uint64)), uint64(0)
		from := c.blockAt(at)
		if evs, err := c.ch.eventsFrom(from, "Inbox", "Committed", nil, []common.Hash{common.HexToHash(id)}); err == nil && len(evs) > 0 {
			block = evs[0].Block
		} else {
			block = from
		}
		c.foundSent(id, r, block, common.HexToHash(id).Hex(), stakedURL(st[0].(common.Address)))
	}
	if len(rest) == 0 {
		return
	}
	// (b) in a batch with others: posted in full, or committed and fetched.
	head, err := c.ch.headNumber(0)
	if err != nil {
		return
	}
	from := head + 1
	for _, id := range rest {
		r := c.st.Sent[id]
		if r.Sought == 0 {
			r.Sought = c.blockAt(max(r.Time-lostSlack, now-alertTTL))
		}
		from = min(from, r.Sought)
	}
	if from > head {
		return
	}
	evs, err := c.ch.eventsMulti(from, "Inbox", "Posted", "Committed")
	if err != nil {
		return
	}
	want := map[string]bool{}
	for _, id := range rest {
		want[id] = true
	}
	var others []string
	fetched, deadline := 0, time.Now().Add(daBudget)
	stop := uint64(0) // the block of the first event not read (0: all read)
	for _, ev := range evs {
		if len(want) == 0 {
			break
		}
		need := false // a record still looked for whose search hasn't passed this block
		for id := range want {
			if c.st.Sent[id].Sought <= ev.Block {
				need = true
				break
			}
		}
		if !need {
			continue
		}
		var es []Envelope
		root, url := "", ""
		switch ev.Name {
		case "Posted":
			es, _ = decodeBatch(ev.Args["batch"].([]byte))
		case "Committed":
			if fetched >= lostFetches || !time.Now().Before(deadline) {
				stop = ev.Block
			} else {
				fetched++
				if urls == nil {
					urls = c.daURLs()
				}
				if others == nil {
					others = sortedURLs(urls)
				}
				root = common.Hash(ev.Args["root"].([32]byte)).Hex()
				es, url, _ = c.committedEnvs(ev, urls, others) // not served: not found here (the scan's own reading retries it)
			}
		}
		if stop != 0 {
			break
		}
		for _, e := range es {
			id := e.hash().Hex()
			if want[id] && c.st.Sent[id].Sought <= ev.Block { // posted in full: no root, no node (as landed keeps it)
				c.foundSent(id, c.st.Sent[id], ev.Block, root, url)
				delete(want, id)
			}
		}
	}
	next := head + 1
	if stop != 0 {
		next = stop // the rest of the window waits for the next scan
	} else if len(evs) > 0 && evs[len(evs)-1].Block >= next {
		next = evs[len(evs)-1].Block + 1
	}
	for id := range want {
		if r := c.st.Sent[id]; r.Sought < next {
			r.Sought = next
		}
	}
}

// blockAt: the first block made at or after chain time t (the head if none).
func (c *Client) blockAt(t int64) uint64 {
	lo := c.ch.dep.Block
	hi, err := c.ch.headNumber(0)
	if err != nil || hi < lo {
		return lo
	}
	if bt, err := c.ch.blockTime(hi); err != nil || bt < t {
		return hi
	}
	for lo < hi {
		mid := lo + (hi-lo)/2
		if bt, err := c.ch.blockTime(mid); err == nil && bt >= t {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// ---- the data node side (in the relayer)

func (r *Relay) ensureStake(url string) error {
	need, err := r.ch.call("Inbox", "minStake")
	if err != nil {
		return err
	}
	have, err := r.ch.call("Inbox", "stake", crypto.PubkeyToAddress(r.key.PublicKey))
	if err != nil {
		return err
	}
	add := new(big.Int).Sub(need[0].(*big.Int), have[0].(*big.Int))
	if add.Sign() < 0 {
		add = new(big.Int)
	}
	_, err = r.ch.send(r.key, add, "Inbox", "stakeDA", url)
	return err
}

// resolveChallenges puts on chain every challenged batch this node holds.
func (r *Relay) resolveChallenges() error {
	evs, err := r.ch.eventsFrom(r.watched, "Inbox", "Challenged")
	if err != nil {
		return err
	}
	head, _ := r.ch.b.BlockNumber(context.Background())
	for _, ev := range evs {
		root := common.Hash(ev.Args["root"].([32]byte))
		b, ok := r.get(root)
		if !ok {
			continue
		}
		if st, err := r.ch.call("Inbox", "commitment", root); err != nil || st[2].(uint8) != 2 {
			continue
		}
		if _, err := r.ch.send(r.key, nil, "Inbox", "resolve", root, b); err != nil {
			return err
		}
	}
	r.watched = head + 1
	return nil
}
