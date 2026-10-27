// Anonymous membership with linkable ring signatures (LSAG, Liu–Wei–Wong
// 2004, as in Monero), over edwards25519. A post signed this way proves
// "one of these members wrote it" without saying who. The key image is
// the same for the same member under the same topic, so a thread shows
// stable per-thread pseudonyms (like an imageboard's IDs), and a poll
// counts one vote per member — without knowing any member's choice.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"filippo.io/edwards25519"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var ringKeyTopic = crypto.Keccak256Hash([]byte("tern:ring-key"))

type RingSig struct {
	Ring  []string `json:"ring"`  // member names, in order
	Image string   `json:"image"` // key image, hex
	C0    string   `json:"c0"`
	S     []string `json:"s"`
}

func randomScalar() *edwards25519.Scalar {
	b := make([]byte, 64)
	rand.Read(b)
	s, _ := new(edwards25519.Scalar).SetUniformBytes(b)
	return s
}

func hashToScalar(parts ...[]byte) *edwards25519.Scalar {
	h := sha512.New()
	h.Write([]byte("tern-lsag-c"))
	for _, p := range parts {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(p)))
		h.Write(n[:])
		h.Write(p)
	}
	s, _ := new(edwards25519.Scalar).SetUniformBytes(h.Sum(nil))
	return s
}

// hashToPoint: a point with unknown discrete log (try-and-increment, cofactor cleared).
func hashToPoint(data []byte) *edwards25519.Point {
	for ctr := uint32(0); ; ctr++ {
		h := sha256.New()
		h.Write([]byte("tern-lsag-h"))
		binary.Write(h, binary.BigEndian, ctr)
		h.Write(data)
		p, err := new(edwards25519.Point).SetBytes(h.Sum(nil))
		if err != nil {
			continue
		}
		p.MultByCofactor(p)
		if p.Equal(edwards25519.NewIdentityPoint()) != 1 {
			return p
		}
	}
}

func ringChallenge(msg []byte, topic string, pubs []*edwards25519.Point, img, l, r *edwards25519.Point) *edwards25519.Scalar {
	parts := [][]byte{msg, []byte(topic)}
	for _, p := range pubs {
		parts = append(parts, p.Bytes())
	}
	parts = append(parts, img.Bytes(), l.Bytes(), r.Bytes())
	return hashToScalar(parts...)
}

func lsagSign(msg []byte, topic string, pubs []*edwards25519.Point, idx int, x *edwards25519.Scalar) (*edwards25519.Point, *edwards25519.Scalar, []*edwards25519.Scalar) {
	n := len(pubs)
	h := hashToPoint([]byte(topic))
	img := new(edwards25519.Point).ScalarMult(x, h)
	c := make([]*edwards25519.Scalar, n)
	s := make([]*edwards25519.Scalar, n)
	u := randomScalar()
	l := new(edwards25519.Point).ScalarBaseMult(u)
	r := new(edwards25519.Point).ScalarMult(u, h)
	c[(idx+1)%n] = ringChallenge(msg, topic, pubs, img, l, r)
	for i := (idx + 1) % n; i != idx; i = (i + 1) % n {
		s[i] = randomScalar()
		l = new(edwards25519.Point).VarTimeDoubleScalarBaseMult(c[i], pubs[i], s[i])
		r = new(edwards25519.Point).Add(new(edwards25519.Point).ScalarMult(s[i], h), new(edwards25519.Point).ScalarMult(c[i], img))
		c[(i+1)%n] = ringChallenge(msg, topic, pubs, img, l, r)
	}
	s[idx] = new(edwards25519.Scalar).Subtract(u, new(edwards25519.Scalar).Multiply(c[idx], x))
	return img, c[0], s
}

func lsagVerify(msg []byte, topic string, pubs []*edwards25519.Point, img *edwards25519.Point, c0 *edwards25519.Scalar, s []*edwards25519.Scalar) bool {
	if len(pubs) == 0 || len(s) != len(pubs) {
		return false
	}
	h := hashToPoint([]byte(topic))
	c := c0
	for i := range pubs {
		l := new(edwards25519.Point).VarTimeDoubleScalarBaseMult(c, pubs[i], s[i])
		r := new(edwards25519.Point).Add(new(edwards25519.Point).ScalarMult(s[i], h), new(edwards25519.Point).ScalarMult(c, img))
		c = ringChallenge(msg, topic, pubs, img, l, r)
	}
	return c.Equal(c0) == 1
}

// ---- ring keys of identities

func (i *Ident) ringKey() (*edwards25519.Scalar, error) {
	b, err := os.ReadFile(filepath.Join(i.dir, "ring.key"))
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, err
	}
	return new(edwards25519.Scalar).SetCanonicalBytes(raw)
}

// enableRing creates a ring key and publishes its public half (a commitment
// under the ring-key topic, signed by this identity).
func (c *Client) enableRing() error {
	if _, err := c.ringKey(); err == nil {
		return nil
	}
	x := randomScalar()
	if err := os.WriteFile(filepath.Join(c.dir, "ring.key"), []byte(hex.EncodeToString(x.Bytes())), 0o600); err != nil {
		return err
	}
	p := new(edwards25519.Point).ScalarBaseMult(x)
	_, err := c.ch.send(c.key, nil, "Commitments", "commit", c.id(), ringKeyTopic, common.BytesToHash(p.Bytes()), uint64(0))
	return err
}

func (c *Client) ringPub(name string) (*edwards25519.Point, error) {
	evs, err := c.ch.events("Commitments", "Committed", nil, []common.Hash{ringKeyTopic}, []common.Hash{idOf(name)})
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 {
		return nil, fmt.Errorf("%s has no ring key", name)
	}
	b := evs[len(evs)-1].Args["hash"].([32]byte)
	p, err := new(edwards25519.Point).SetBytes(b[:])
	if err != nil {
		return nil, err
	}
	if !torsionFree(p) {
		return nil, fmt.Errorf("%s's ring key is not in the prime-order group", name)
	}
	return p, nil
}

// torsionFree: P has no small-order component, i.e. 8⁻¹·(8·P) == P.
// Without this check one key could make several valid key images (several votes).
func torsionFree(p *edwards25519.Point) bool {
	eight := new(edwards25519.Scalar)
	var b [32]byte
	b[0] = 8
	if _, err := eight.SetCanonicalBytes(b[:]); err != nil {
		return false
	}
	inv := new(edwards25519.Scalar).Invert(eight)
	q := new(edwards25519.Point).MultByCofactor(p)
	q.ScalarMult(inv, q)
	return q.Equal(p) == 1 && p.Equal(edwards25519.NewIdentityPoint()) != 1
}

// ringBucket is the anonymity set: members with ring keys, sorted by name, cut
// into fixed buckets of ringSize (a short last bucket joins the one before).
// The ring depends on which bucket you are in, never on your position, so its
// length and order say nothing about who signed.
const ringSize = 64

func ringBucket(sorted []string, me string) []string {
	idx := sort.SearchStrings(sorted, me)
	if idx >= len(sorted) || sorted[idx] != me {
		return nil
	}
	n := len(sorted)
	buckets := n / ringSize
	if buckets == 0 {
		return sorted
	}
	b := idx / ringSize
	if b >= buckets {
		b = buckets - 1 // the short tail belongs to the last full bucket
	}
	end := (b + 1) * ringSize
	if b == buckets-1 {
		end = n
	}
	return sorted[b*ringSize : end]
}

// setMembers lists current members of a set (from its events).
func (c *Client) setMembers(set string) ([]string, error) {
	sid := setID(set)
	evs, err := c.ch.events("Sets", "Added", []common.Hash{sid})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range evs {
		mid := common.Hash(e.Args["member"].([32]byte))
		name, err := c.res.nameOf(mid)
		if err != nil || seen[name] {
			continue
		}
		if o, err := c.ch.call("Sets", "members", sid, mid); err == nil && o[0].(bool) {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

// ringSign signs msg as "a member of set" under topic.
func (c *Client) ringSign(set, topic string, msg []byte) (*RingSig, error) {
	x, err := c.ringKey()
	if err != nil { // first anonymous post or vote: publish a ring key now
		if err := c.enableRing(); err != nil {
			return nil, fmt.Errorf("no ring key, and publishing one failed: %w (run private enable)", err)
		}
		if x, err = c.ringKey(); err != nil {
			return nil, errors.New("no ring key (run private enable)")
		}
	}
	members, err := c.setMembers(set)
	if err != nil {
		return nil, err
	}
	keyed := map[string]*edwards25519.Point{}
	var names []string
	for _, m := range members {
		if c.bannedInCommunity(set, m) || c.notCitizenHere(set, m) {
			continue // a banned name (or a banned owner's agent), or a non-citizen in a citizens-only community, is not one of the members a post speaks for
		}
		if p, err := c.ringPub(m); err == nil {
			keyed[m] = p
			names = append(names, m)
		}
	}
	sort.Strings(names)
	var ring []string
	var pubs []*edwards25519.Point
	idx := -1
	for i, m := range ringBucket(names, c.f.Name) {
		if m == c.f.Name {
			idx = i
		}
		ring = append(ring, m)
		pubs = append(pubs, keyed[m])
	}
	if idx < 0 {
		return nil, fmt.Errorf("you are not a member of %s with a ring key", set)
	}
	if len(ring) < 2 {
		return nil, errors.New("the ring has only you: nobody else in the set has a ring key yet")
	}
	img, c0, s := lsagSign(msg, topic, pubs, idx, x)
	rs := &RingSig{Ring: ring, Image: hex.EncodeToString(img.Bytes()), C0: hex.EncodeToString(c0.Bytes())}
	for _, si := range s {
		rs.S = append(rs.S, hex.EncodeToString(si.Bytes()))
	}
	return rs, nil
}

// ringVerify checks that one current member of set signed msg under topic.
// bannedInCommunity: set is a community and m (or its owner) is banned there.
func (c *Client) bannedInCommunity(set, m string) bool {
	return !abuseFixesOff && c.ch.has("Commons") && c.isCommunity(set) && c.commonsBool("isBanned", idOf(set), idOf(m))
}

// notCitizenHere: set is a citizens-only community and m is an agent or a
// name that is not a citizen now (its set membership stays: audit 5 HM-4).
func (c *Client) notCitizenHere(set, m string) bool {
	if !c.ch.has("Commons") || !c.isCommunity(set) {
		return false
	}
	if h, _ := c.citizensHouse(set); h == (common.Address{}) {
		return false
	}
	return fromAgent(m) || !c.citizenNow(m)
}

func (c *Client) ringVerify(set, topic string, msg []byte, rs *RingSig) error {
	if len(rs.Ring) > 2*ringSize {
		return errors.New("ring too large")
	}
	for i := 1; i < len(rs.Ring); i++ {
		if rs.Ring[i] <= rs.Ring[i-1] {
			return errors.New("ring not in canonical order, or with duplicates")
		}
	}
	var pubs []*edwards25519.Point
	for _, m := range rs.Ring {
		if o, err := c.ch.call("Sets", "members", setID(set), idOf(m)); err != nil || !o[0].(bool) {
			return fmt.Errorf("%s is not a member of %s", m, set)
		}
		if c.bannedInCommunity(set, m) {
			return fmt.Errorf("%s is banned from %s: a ring with it doesn't speak for the members", m, set)
		}
		if c.notCitizenHere(set, m) {
			return fmt.Errorf("%s is not a citizen now: %s is citizens only, and a ring with it doesn't speak for its citizens", m, set)
		}
		p, err := c.ringPub(m)
		if err != nil {
			return err
		}
		pubs = append(pubs, p)
	}
	ib, err1 := hex.DecodeString(rs.Image)
	cb, err2 := hex.DecodeString(rs.C0)
	if err1 != nil || err2 != nil {
		return errors.New("bad ring signature encoding")
	}
	img, err := new(edwards25519.Point).SetBytes(ib)
	if err != nil {
		return err
	}
	if !torsionFree(img) {
		return errors.New("key image has a small-order component (one member, many images)")
	}
	c0, err := new(edwards25519.Scalar).SetCanonicalBytes(cb)
	if err != nil {
		return err
	}
	var s []*edwards25519.Scalar
	for _, x := range rs.S {
		b, err := hex.DecodeString(x)
		if err != nil {
			return err
		}
		si, err := new(edwards25519.Scalar).SetCanonicalBytes(b)
		if err != nil {
			return err
		}
		s = append(s, si)
	}
	if !lsagVerify(msg, topic, pubs, img, c0, s) {
		return errors.New("ring signature does not verify")
	}
	return nil
}
