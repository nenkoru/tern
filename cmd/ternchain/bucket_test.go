package main

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// newMirrorNode: a staked, free data node that mirrors and indexes buckets.
func newMirrorNode(w *world) (*Relay, *httptest.Server) {
	w.t.Helper()
	key, _ := crypto.GenerateKey()
	w.fund2(crypto.PubkeyToAddress(key.PublicKey), coins(10))
	node, err := newDataNode(w.ch, key, w.t.TempDir(), "", nil, true)
	w.must(err)
	srv := httptest.NewServer(node.Handler())
	w.t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	w.must(node.ensureStake(srv.URL))
	node.publicURL = srv.URL
	return node, srv
}

func TestBucketHints(t *testing.T) {
	v := make([]byte, 32)
	for bits := 1; bits <= 16; bits++ {
		b := bucketOf(9, v, bits)
		for i := 0; i < 20; i++ {
			h := bucketHint(9, v, bits)
			var tag [8]byte
			copy(tag[:], h[:])
			if hintBucket(tag, bits) != b {
				t.Fatalf("bits %d: hint says bucket %d, not %d", bits, hintBucket(tag, bits), b)
			}
		}
	}
	if bucketOf(9, v, 8) == bucketOf(10, v, 8) && bucketOf(11, v, 8) == bucketOf(12, v, 8) {
		t.Fatal("buckets should change with the epoch")
	}
	// proofs: every leaf of trees of every size proves against the root
	for n := 1; n <= 13; n++ {
		var hs []common.Hash
		for i := 0; i < n; i++ {
			hs = append(hs, crypto.Keccak256Hash([]byte{byte(i), byte(n)}))
		}
		root := leafRoot(hs)
		for i := 0; i < n; i++ {
			got, ok := proofRoot(hs[i], i, n, leafProof(hs, i))
			if !ok || got != root {
				t.Fatalf("n=%d i=%d: proof doesn't reach the root", n, i)
			}
			if got, _ := proofRoot(hs[(i+1)%n], i, n, leafProof(hs, i)); n > 1 && got == root {
				t.Fatalf("n=%d i=%d: another leaf proved", n, i)
			}
		}
	}
}

// Readers fetch their buckets from a home node, checked against the roots on
// chain, instead of every batch; pending mail comes from the home's copy of
// the other node's memory.
func TestBucketsHomeNodes(t *testing.T) {
	w := newWorld(t)
	w.ch.dep.BucketBits = 4
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	carol, _ := w.owner("carol")
	bob.setContact("alice", true)
	carol.setContact("alice", true)
	a, sa := newMirrorNode(w) // alice's relay
	b, sb := newMirrorNode(w) // bob's home
	alice.relay, bob.relay, carol.relay = sa.URL, sb.URL, sb.URL

	fetched := 0
	old := fetchBatchFn
	fetchBatchFn = func(url string, root common.Hash) ([]byte, error) { fetched++; return fetchBatch(url, root) }
	defer func() { fetchBatchFn = old }()
	got := 0
	bucketHook = func(_ string, n int) { got += n }
	defer func() { bucketHook = nil }()

	// before the batch is on chain: bob's home copied it from alice's node
	id, err := alice.send(SendOpts{To: "bob", Text: "hello by bucket"})
	w.must(err)
	b.pollPeers()
	items := scan(t, bob)
	if len(items) != 1 || items[0].ID != id || items[0].Onchain != onchainPending {
		t.Fatalf("bob's early items: %+v", items)
	}
	for i := 0; i < 30; i++ { // mail to carol fills the batch; carol is in another bucket (or not)
		_, err := alice.send(SendOpts{To: "carol", Text: "noise"})
		w.must(err)
	}
	w.must(a.flush())
	w.must(b.indexBuckets())
	w.must(a.indexBuckets())
	scan(t, bob)
	if fetched != 0 {
		t.Fatalf("bob downloaded %d whole batches", fetched)
	}
	it := bob.item(id)
	if it == nil || it.Onchain != onchainCommitted || it.Batch == "" {
		t.Fatalf("bob's item after the batch landed: %+v", it)
	}
	if got == 0 {
		t.Fatal("no bucket bytes counted")
	}
	e := mustEpoch(t, w)
	bb := func(c *Client) uint16 { return bucketOf(e, c.view.PublicKey().Bytes(), 4) }
	t.Logf("bob read %d bytes of buckets for 1 of 31 envelopes (buckets: alice %d bob %d carol %d)", got, bb(alice), bb(bob), bb(carol))
	if q := bob.queryBuckets(e); !q[bb(alice)] && !q[bb(carol)] && got > 2000 { // what bob asks for: his buckets and decoys (audit 5 BK-1)
		t.Fatalf("bob downloaded %d bytes: other buckets too?", got)
	}
	// carol reads all 30 (one batch, her bucket), none twice
	if n := len(mailTo(scan(t, carol), tMessage)); n != 30 {
		t.Fatalf("carol read %d of 30", n)
	}
	if n := len(scan(t, carol)); n != 0 {
		t.Fatalf("carol read %d again", n)
	}

	// a node that serves an envelope that doesn't check against the chain: dropped, with an alert
	bk := b.bindex()
	bk.mu.Lock()
	for i := range bk.entries {
		bk.entries[i].idx = (bk.entries[i].idx + 1) % bk.entries[i].n // every proof now names the wrong place
	}
	bk.mu.Unlock()
	bob.st.Cursor = w.ch.dep.Block
	bob.st.Items = nil
	bob.itemIdx = nil
	if items := scan(t, bob); len(items) != 0 || !hasAlert(bob, "doesn't check against the chain") {
		t.Fatalf("forged records: %d items, alerts %v", len(items), bob.st.Alerts)
	}

	// A fresh independent archive falls back to whole batches when no home
	// answers. Clearing Items in the old archive cannot erase replay receipts.
	stateDir := t.TempDir()
	w.must(writeJSON(filepath.Join(stateDir, "state.json"), &State{Cursor: w.ch.dep.Block}))
	fallback, err := openClientState(bob.dir, stateDir, w.ch)
	w.must(err)
	fallback.relay = "http://127.0.0.1:1"
	if items := mailTo(scan(t, fallback), tMessage); len(items) != 1 || fetched == 0 {
		t.Fatalf("fallback read: %d items, %d batches fetched", len(items), fetched)
	}
}

func mustEpoch(t *testing.T, w *world) uint64 {
	e, err := w.ch.epoch()
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// Group posts are in the group's bucket; members read them from their home.
func TestBucketsGroups(t *testing.T) {
	w := newWorld(t)
	w.ch.dep.BucketBits = 6
	alice, _ := w.owner("alice")
	bob, _ := w.owner("bob")
	n, s := newMirrorNode(w)
	alice.relay, bob.relay = s.URL, s.URL
	w.must(alice.createGroup("club", true))
	if _, err := bob.joinGroup("club"); err != nil {
		t.Fatal(err)
	}
	scan(t, bob)
	if _, err := alice.postGroup("club", "first post", nil); err != nil {
		t.Fatal(err)
	}
	w.must(n.flush())
	w.must(n.indexBuckets())
	var posts []*Item
	for _, it := range scan(t, bob) {
		if it.To == "group:club" {
			posts = append(posts, it)
		}
	}
	if len(posts) != 1 || posts[0].Onchain == onchainPending {
		t.Fatalf("bob's club posts: %+v", posts)
	}
}
