// Reputation without the social graph.
//
//	rep tag LOCK                                     the payee: a one-deal tag for the payer (sealed mail)
//	rep review LOCK --score S                        the payer: the review as a hidden leaf (PrivateReviews)
//	rep prove --scope TEXT [--deals N] [--min-score X] [--out FILE]
//	                                                 the payee: "N distinct paid deals, scores averaging >= X"
//	rep verify FILE --scope TEXT                     anyone: checks a proof against the chain
//
// A public review (attest NAME review … --lock ID) names its reviewer, its
// subject, its deal and its score, so a rating is the social graph. A
// private review is a leaf H(H(s, lock), score + 100) in one tree for the
// whole network (contracts/ext/PrivateReviews.sol, which checks that the
// poster is the lock's payer and that the lock paid). s is the payee's
// reputation secret; H(s, lock) is all the payer learns, for that deal only.
// The payee later proves with the rep circuit (internal/zk/rep.go) that
// Count distinct leaves are its own and their scores add up to at least
// MinTotal, under a nullifier for the verifier's scope: the verifier learns
// the count and the floor, not which deals, which counterparts or when.
//
// What stays public: the Bank's locks (who paid whom: payments privacy is not
// in the public profile) and that a lock got a private review. A proof counts
// up to 8 deals and shows the prover's best ones; it counts deals, not their
// size (a sybil can buy cheap ones, as with public reviews). Public reviews
// stay for those who want them.
package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern/internal/zk"
)

const tRepTag = "tern.rep-tag"

// RepTag: the payee's tag for one deal (tRepTag).
type RepTag struct {
	Lock uint64 `json:"lock"`
	Tag  string `json:"tag"` // decimal: H(s, lock)
}

// RepProof: a presentation (rep prove's output, rep verify's input).
type RepProof struct {
	Kind      string `json:"kind"` // tern.rep-proof/1
	Size      uint32 `json:"tree_size"`
	Root      string `json:"root"`
	Count     int    `json:"deals"`
	MinTotal  int    `json:"min_total"` // sum of (score + 100) at least
	AvgAtLeast int    `json:"avg_score_at_least"`
	Scope     string `json:"scope"` // the verifier's text
	Nullifier string `json:"nullifier"`
	Proof     string `json:"proof"`
}

func (c *Client) repSecret() *big.Int {
	return zk.ModP(crypto.Keccak256([]byte("tern-rep-v1"), crypto.FromECDSA(c.key)))
}

func repTagOf(s *big.Int, lock uint64) *big.Int {
	return zk.HashN(s, new(big.Int).SetUint64(lock))
}

func repLeaf(tag *big.Int, score int) *big.Int { return zk.HashN(tag, big.NewInt(int64(score+100))) }

func repScope(text string) *big.Int {
	return zk.ModP(crypto.Keccak256([]byte("tern-rep-scope"), []byte(text)))
}

func repMessage(count, minTotal int, scope string) *big.Int {
	return zk.ModP(crypto.Keccak256([]byte(fmt.Sprintf("tern-rep|%d|%d|%s", count, minTotal, scope))))
}

func (c *Client) repReady() error {
	if !c.ch.has("PrivateReviews") {
		return errors.New("this network has no PrivateReviews contract (net.json contracts.PrivateReviews): private reviews are off here")
	}
	if _, devnet := zk.Ceremony(); devnet && !zk.IsDevnetChain(c.ch.chainID.Uint64()) {
		return errDevnetKeys(c.ch.chainID)
	}
	return nil
}

// repTagSend: the payee gives the payer its tag for one deal.
func (c *Client) repTagSend(lock uint64) (string, error) {
	if err := c.repReady(); err != nil {
		return "", err
	}
	l, err := c.dealLock(lock)
	if err != nil {
		return "", err
	}
	if !l.payeeSide[ownerName(c.f.Name)] {
		return "", fmt.Errorf("lock %d pays %s, not you: only the payee gives a tag", lock, l.Payee)
	}
	if l.Paid.Sign() == 0 {
		return "", fmt.Errorf("lock %d hasn't paid yet: its review counts once it has", lock)
	}
	tag := repTagOf(c.repSecret(), lock)
	id, err := c.sendDatagram(tRepTag, l.From, RepTag{Lock: lock, Tag: tag.String()})
	if err != nil {
		return "", err
	}
	if !inU64(c.st.RepLocks, lock) {
		c.st.RepLocks = append(c.st.RepLocks, lock)
	}
	return id, c.save()
}

func inU64(xs []uint64, x uint64) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// acceptRepTag: a payee's tag for a lock we paid.
func (c *Client) acceptRepTag(it *Item, d *Datagram) {
	var t RepTag
	if json.Unmarshal(d.Body, &t) != nil {
		return
	}
	l, err := c.dealLock(t.Lock)
	if err != nil || !l.payeeSide[ownerName(d.From)] || !l.payerSide[ownerName(c.f.Name)] {
		it.Verified += " (ignored: not a lock between us)"
		return
	}
	if _, ok := new(big.Int).SetString(t.Tag, 10); !ok {
		return
	}
	if c.st.RepTags == nil {
		c.st.RepTags = map[uint64]string{}
	}
	c.st.RepTags[t.Lock] = t.Tag
	it.Verified += fmt.Sprintf(" (rep review %d --score S reviews it privately)", t.Lock)
}

// repReview: the payer's private review of a paid lock.
func (c *Client) repReview(lock uint64, score int) error {
	if err := c.repReady(); err != nil {
		return err
	}
	if score < -100 || score > 100 {
		return errors.New("--score: -100..100")
	}
	tag, ok := new(big.Int).SetString(c.st.RepTags[lock], 10)
	if !ok {
		return fmt.Errorf("no tag for lock %d: the payee sends it with rep tag %d (then run inbox)", lock, lock)
	}
	_, err := c.ch.send(c.key, nil, "PrivateReviews", "review", u(lock), c.id(), repLeaf(tag, score))
	if err == nil {
		delete(c.st.RepTags, lock)
		err = c.save()
	}
	return err
}

// repTree: every private review's leaf, by index, and the locks they cite.
func (c *Client) repTree() ([]*big.Int, []uint64, error) {
	evs, err := c.ch.events("PrivateReviews", "Review")
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].Args["index"].(uint32) < evs[j].Args["index"].(uint32) })
	leaves, locks := make([]*big.Int, len(evs)), make([]uint64, len(evs))
	for i, e := range evs {
		if int(e.Args["index"].(uint32)) != i {
			return nil, nil, errors.New("private reviews: a gap in the tree's events (try again)")
		}
		leaves[i], locks[i] = bigArg(e.Args["leaf"]), bigArg(e.Args["lock"]).Uint64()
	}
	return leaves, locks, nil
}

type repDeal struct {
	index int
	lock  uint64
	score int
}

// repMine: our leaves, with the scores the payers gave (found by trying the 201).
func (c *Client) repMine(leaves []*big.Int, locks []uint64) []repDeal {
	s := c.repSecret()
	var out []repDeal
	for i, l := range locks {
		if !inU64(c.st.RepLocks, l) {
			continue
		}
		tag := repTagOf(s, l)
		for sc := -100; sc <= 100; sc++ {
			if repLeaf(tag, sc).Cmp(leaves[i]) == 0 {
				out = append(out, repDeal{i, l, sc})
				break
			}
		}
	}
	return out
}

// repProve: a proof of our best deals (up to zk.RepSlots), for one scope.
func (c *Client) repProve(scope string, deals, minScore int) (*RepProof, error) {
	if err := c.repReady(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(scope) == "" {
		return nil, errors.New("--scope: the verifier's text (its name and a fresh nonce): a proof for one verifier, not a badge anyone can copy")
	}
	leaves, locks, err := c.repTree()
	if err != nil {
		return nil, err
	}
	mine := c.repMine(leaves, locks)
	sort.Slice(mine, func(i, j int) bool { return mine[i].score > mine[j].score })
	if deals <= 0 {
		deals = min(len(mine), zk.RepSlots)
	}
	if deals > zk.RepSlots {
		return nil, fmt.Errorf("a proof counts at most %d deals", zk.RepSlots)
	}
	if deals == 0 || len(mine) < deals {
		return nil, fmt.Errorf("%d private reviews of your deals on chain, %d asked for", len(mine), deals)
	}
	mine = mine[:deals]
	total := 0
	for _, d := range mine {
		total += d.score + 100
	}
	minTotal := total
	if minScore > -101 {
		if (minScore+100)*deals > total {
			return nil, fmt.Errorf("your best %d deals average %d, below %d", deals, total/deals-100, minScore)
		}
		minTotal = (minScore + 100) * deals
	}
	t := zk.NewTree(zk.RepDepth, leaves)
	sc := repScope(scope)
	secret := c.repSecret()
	a := &zk.RepCircuit{Root: t.Root(), Count: deals, MinTotal: minTotal, Scope: sc, Nullifier: zk.HashN(sc, secret),
		Message: repMessage(deals, minTotal, scope), Secret: secret}
	for i := 0; i < zk.RepSlots; i++ {
		a.Active[i], a.Lock[i], a.Score[i] = 0, 0, 0
		path, bits := make([]*big.Int, zk.RepDepth), make([]int, zk.RepDepth)
		if i < len(mine) {
			a.Active[i], a.Lock[i], a.Score[i] = 1, mine[i].lock, mine[i].score+100
			if path, bits, err = t.Proof(mine[i].index); err != nil {
				return nil, err
			}
		} else {
			for j := range path {
				path[j] = new(big.Int)
			}
		}
		copy(a.Path[i][:], zk.Bigs(path))
		copy(a.Bits[i][:], zk.Ints(bits))
	}
	proof, err := zk.ProveRaw("rep", a)
	if err != nil {
		return nil, err
	}
	return &RepProof{Kind: "tern.rep-proof/1", Size: uint32(len(leaves)), Root: t.Root().String(), Count: deals, MinTotal: minTotal,
		AvgAtLeast: minTotal/deals - 100, Scope: scope, Nullifier: a.Nullifier.(*big.Int).String(), Proof: hex.EncodeToString(proof)}, nil
}

// repVerify checks a proof against the chain's tree at the size it names.
func (c *Client) repVerify(p *RepProof, scope string) (map[string]any, error) {
	if err := c.repReady(); err != nil {
		return nil, err
	}
	if p.Kind != "tern.rep-proof/1" || p.Count <= 0 || p.Count > zk.RepSlots {
		return nil, errors.New("not a reputation proof")
	}
	if scope != "" && p.Scope != scope {
		return nil, fmt.Errorf("made for scope %q, not %q: a proof for someone else (or replayed)", p.Scope, scope)
	}
	leaves, _, err := c.repTree()
	if err != nil {
		return nil, err
	}
	if int(p.Size) > len(leaves) || p.Size == 0 {
		return nil, fmt.Errorf("names a tree of %d reviews; the chain has %d", p.Size, len(leaves))
	}
	root := zk.NewTree(zk.RepDepth, leaves[:p.Size]).Root()
	if root.String() != p.Root {
		return nil, errors.New("its root isn't the private reviews' tree at that size")
	}
	nul, ok := new(big.Int).SetString(p.Nullifier, 10)
	proof, err := hex.DecodeString(p.Proof)
	if !ok || err != nil {
		return nil, errors.New("bad proof encoding")
	}
	pub := &zk.RepCircuit{Root: root, Count: p.Count, MinTotal: p.MinTotal, Scope: repScope(p.Scope), Nullifier: nul,
		Message: repMessage(p.Count, p.MinTotal, p.Scope)}
	if err := zk.Verify("rep", proof, pub); err != nil {
		return nil, errors.New("the proof doesn't verify")
	}
	return map[string]any{"ok": true, "deals": p.Count, "avg_score_at_least": p.MinTotal/p.Count - 100, "nullifier": p.Nullifier,
		"note": "distinct paid deals reviewed privately; which ones and with whom is not in the proof. The same nullifier for the same scope: the same prover"}, nil
}

func readRepProof(path string) (*RepProof, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p RepProof
	return &p, json.Unmarshal(b, &p)
}
