// Receipts (preconfirmations): a data node that accepts an envelope signs
// {envelope hash, node, deadline, claim}. The sender keeps the receipt. If the
// envelope is in no batch the node committed by the deadline, the receipt
// goes on chain (Inbox.proveMissed) and, unless the node rebuts it with the
// batch root, a Merkle proof and the envelope itself (Inbox.rebutMissed),
// slashes its stake.
//
// claim = keccak(payee, salt): the sender picks a fresh salt per envelope and
// hands only the hash to the node (X-Receipt-Claim). Only the sender can open
// the claim, and the bounty goes to the payee whoever files it (audit 5 IP-1).
//
// Batches are committed with the Merkle root of their envelopes' hashes
// (Inbox.postRootL): leaves in order, pairs hashed 0x01|left|right, an odd
// last node carried up unchanged.
package main

import (
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Receipt: a node's signed promise to commit an envelope by Deadline (chain time).
type Receipt struct {
	Env      string `json:"envelope"`
	Node     string `json:"node"` // the node's address
	Deadline int64  `json:"deadline"`
	Claim    string `json:"claim,omitempty"` // keccak(payee, salt), signed with the rest
	Sig      string `json:"sig"`
	URL      string `json:"url,omitempty"`   // where it was handed over
	Payee    string `json:"payee,omitempty"` // the sender's own: what opens the claim (never sent to the node)
	Salt     string `json:"salt,omitempty"`
}

// receiptWindow: seconds a node promises to commit within (its flush runs every second).
const receiptWindow = 120

// receiptMaxAhead: a receipt promising a commit further out than this is not
// a promise (IP-4: a far deadline would silence the "undelivered" alert).
const receiptMaxAhead = 600

func receiptDigest(ch *Chain, env common.Hash, node common.Address, deadline uint64, claim common.Hash) (common.Hash, error) {
	b, err := argsOf("string", "address", "uint256", "bytes32", "address", "uint64", "bytes32").Pack("tern-receipt",
		ch.addr("Inbox"), ch.chainID, env, node, deadline, claim)
	if err != nil {
		return common.Hash{}, err
	}
	return crypto.Keccak256Hash([]byte("\x19Ethereum Signed Message:\n32"), crypto.Keccak256(b)), nil
}

func signReceipt(ch *Chain, key *ecdsa.PrivateKey, env common.Hash, deadline int64, claim common.Hash) (Receipt, error) {
	node := crypto.PubkeyToAddress(key.PublicKey)
	d, err := receiptDigest(ch, env, node, uint64(deadline), claim)
	if err != nil {
		return Receipt{}, err
	}
	sig, err := crypto.Sign(d[:], key)
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{Env: env.Hex(), Node: node.Hex(), Deadline: deadline, Claim: claim.Hex(), Sig: hex.EncodeToString(sig)}, nil
}

// receiptClaim: a fresh claim for one envelope, and the salt that opens it.
func receiptClaim(payee common.Address) (common.Hash, common.Hash) {
	var salt common.Hash
	randRead(salt[:])
	b, _ := argsOf("address", "bytes32").Pack(payee, salt)
	return crypto.Keccak256Hash(b), salt
}

// verify: signed by the node it names, for this envelope (and its claim).
func (r Receipt) verify(ch *Chain, env common.Hash) error {
	if !strings.EqualFold(r.Env, env.Hex()) {
		return errors.New("receipt for another envelope")
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(r.Sig, "0x"))
	if err != nil || len(sig) != 65 {
		return errors.New("bad receipt signature")
	}
	d, err := receiptDigest(ch, env, common.HexToAddress(r.Node), uint64(r.Deadline), common.HexToHash(r.Claim))
	if err != nil {
		return err
	}
	pub, err := crypto.SigToPub(d[:], sig)
	if err != nil || crypto.PubkeyToAddress(*pub) != common.HexToAddress(r.Node) {
		return errors.New("receipt not signed by the node it names")
	}
	return nil
}

// keepReceipt: the receipt the relay at url gave for our envelope is worth
// keeping (IP-4): signed for this envelope and our claim, by the node staked
// at that very URL, holding the minimum stake, not leaving (exitAt 0), with a
// deadline within receiptMaxAhead. Anything else is no receipt at all, and the
// envelope is watched as one handed to a plain relay (sentGrace).
func (c *Client) keepReceipt(rc *Receipt, env, claim, salt common.Hash, url string) error {
	if err := rc.verify(c.ch, env); err != nil {
		return err
	}
	if common.HexToHash(rc.Claim) != claim {
		return errors.New("receipt for another claim")
	}
	if now := c.chainNow(); rc.Deadline > now+receiptMaxAhead {
		return fmt.Errorf("receipt deadline %d s out: not a promise", rc.Deadline-now)
	}
	node := common.HexToAddress(rc.Node)
	if u := stakedURLOf(c.ch, node); u == "" || !strings.EqualFold(strings.TrimRight(u, "/"), strings.TrimRight(url, "/")) {
		return errors.New("the receipt's node is not the data node staked at this relay's URL")
	}
	if e, err := c.ch.call("Inbox", "exitAt", node); err != nil || e[0].(uint64) != 0 {
		return errors.New("the receipt's node is leaving (exitDA)")
	}
	rc.URL, rc.Payee, rc.Salt = url, c.addr().Hex(), salt.Hex()
	return nil
}

// stakedURLOf: where node is staked as a data node (with the minimum stake), or "".
func stakedURLOf(ch *Chain, node common.Address) string {
	if u, ok := stakedURLs(ch)[node]; ok {
		return u
	}
	out := stakedURLsFetch(ch) // staked within the cache's minute
	if perfFixes {
		ch.staked.Store(&stakedAt{urls: out, at: time.Now()})
	}
	return out[node]
}

// leafRoot: the Merkle root of envelope hashes, as Inbox.leafRootOf computes it.
func leafRoot(h []common.Hash) common.Hash {
	if len(h) == 0 {
		return common.Hash{}
	}
	l := append([]common.Hash{}, h...)
	for n := len(l); n > 1; n = (n + 1) / 2 {
		for k := 0; k < n/2; k++ {
			l[k] = merkleNode(l[2*k], l[2*k+1])
		}
		if n%2 == 1 {
			l[n/2] = l[n-1]
		}
	}
	return l[0]
}

// merkleNode: an inner node, domain-tagged 0x01 (IP-8): leaves are envelope
// hashes (an envelope starts with "TC2"), so a node is never taken for a leaf.
func merkleNode(a, b common.Hash) common.Hash {
	return crypto.Keccak256Hash([]byte{1}, a[:], b[:])
}

// leafProof: the siblings from leaf i up (Inbox.rebutMissed checks them).
func leafProof(h []common.Hash, i int) []common.Hash {
	var proof []common.Hash
	l := append([]common.Hash{}, h...)
	for n := len(l); n > 1; n = (n + 1) / 2 {
		if i%2 == 1 {
			proof = append(proof, l[i-1])
		} else if i+1 < n {
			proof = append(proof, l[i+1])
		}
		for k := 0; k < n/2; k++ {
			l[k] = merkleNode(l[2*k], l[2*k+1])
		}
		if n%2 == 1 {
			l[n/2] = l[n-1]
		}
		i /= 2
	}
	return proof
}

func envHashes(es []Envelope) []common.Hash {
	out := make([]common.Hash, len(es))
	for i := range es {
		out[i] = es[i].hash()
	}
	return out
}

// ---- the sender's side: claiming a missed receipt

// receiptsOf: the receipts our envelope was handed over with, the newest
// first. Once it is on chain, the latest hand-over is the one that landed: only
// earlier ones (before a re-send) can have been missed.
func (r *SentRec) receiptsOf() []*Receipt {
	var out []*Receipt
	if r.Receipt != nil && r.Block == 0 {
		out = append(out, r.Receipt)
	}
	for i := len(r.Earlier) - 1; i >= 0; i-- {
		if r.Earlier[i].Receipt != nil {
			out = append(out, r.Earlier[i].Receipt)
		}
	}
	return out
}

// proveMissed puts the receipt for our envelope id on chain (with the bond).
// Never against a node that committed a batch by the deadline that we can't
// fetch (C2-WITHHOLD): it would rebut with that commit and take the bond.
func (c *Client) proveMissed(id string) (map[string]any, error) {
	k, r := c.sentRec(id)
	if r == nil || r.Receipt == nil && len(r.Earlier) == 0 {
		return nil, fmt.Errorf("no receipt kept for %s (only envelopes handed to a data node that signs receipts have one)", id)
	}
	rs := r.receiptsOf()
	if len(rs) == 0 {
		return nil, fmt.Errorf("%s is on chain (block %d): nothing missed", k[:18], r.Block)
	}
	var rc *Receipt
	now := c.chainNow()
	for _, x := range rs {
		if now > x.Deadline {
			rc = x
			break
		}
	}
	if rc == nil {
		return nil, fmt.Errorf("the node has until %d (%d s more) to commit it", rs[0].Deadline, rs[0].Deadline-now)
	}
	if rc.Salt == "" || rc.Payee == "" {
		return nil, fmt.Errorf("the receipt for %s has no claim we can open (an older receipt): it can't be claimed", k[:18])
	}
	if root, _ := c.withheldRoot(rc); root != "" {
		return nil, fmt.Errorf("data node %s committed batch %s by its deadline and doesn't serve it: your message may be in it, and a missed-receipt claim "+
			"would be rebutted with that commit and cost you the bond. Challenge the batch instead: 'ternchain da challenge %s' (done automatically unless da_autochallenge is off)",
			rc.Node, root[:10], root)
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(rc.Sig, "0x"))
	if err != nil {
		return nil, err
	}
	bond, err := c.ch.call("Inbox", "challengeBond")
	if err != nil {
		return nil, err
	}
	if _, err := c.ch.send(c.key, bond[0].(*big.Int), "Inbox", "proveMissed", common.HexToHash(k), common.HexToAddress(rc.Node), uint64(rc.Deadline),
		common.HexToAddress(rc.Payee), common.HexToHash(rc.Salt), sig); err != nil {
		return nil, err
	}
	rw, _ := c.ch.call("Inbox", "resolveWindow")
	return map[string]any{"claimed": k, "node": rc.Node, "bond": fmtCoins(bond[0].(*big.Int)),
		"next": fmt.Sprintf("unless the node proves it committed the envelope within %v s, 'da slash-missed %s' pays you the bond back and a bounty (the rest of its stake is burned)", rw[0], k[:18])}, nil
}

func (c *Client) slashMissed(id string) (map[string]any, error) {
	k, r := c.sentRec(id)
	if r == nil || r.Receipt == nil && len(r.Earlier) == 0 {
		return nil, fmt.Errorf("no receipt kept for %s", id)
	}
	rs := append([]*Receipt{}, r.receiptsOf()...)
	if r.Receipt != nil && r.Block != 0 {
		rs = append(rs, r.Receipt)
	}
	rc := rs[0]
	for _, x := range rs { // the one claimed (an open claim on chain)
		if st, err := c.ch.call("Inbox", "missOf", common.HexToHash(k), common.HexToAddress(x.Node), common.HexToHash(x.Claim)); err == nil && st[3].(uint8) == 1 {
			rc = x
			break
		}
	}
	if _, err := c.ch.send(c.key, nil, "Inbox", "slashMissed", common.HexToHash(k), common.HexToAddress(rc.Node), common.HexToHash(rc.Claim)); err != nil {
		return nil, err
	}
	return map[string]any{"slashed": rc.Node, "for": k}, nil
}
