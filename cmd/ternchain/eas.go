// Attestations through EAS (Ethereum Attestation Service), the standard.
// One schema for claims about anything: "bytes32 subject,string kind,bytes
// data,int64 score". The attester is an address; Tern maps it to the name
// whose key it was at that block, and readers choose whose claims count.
package main

import (
	"context"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sync"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

type easRequestData struct {
	Recipient      common.Address
	ExpirationTime uint64
	Revocable      bool
	RefUID         [32]byte
	Data           []byte
	Value          *big.Int
}

type easRequest struct {
	Schema [32]byte
	Data   easRequestData
}

type easRevocationData struct {
	Uid   [32]byte
	Value *big.Int
}

type easRevocation struct {
	Schema [32]byte
	Data   easRevocationData
}

type easAttestation struct {
	Uid            [32]byte
	Schema         [32]byte
	Time           uint64
	ExpirationTime uint64
	RevocationTime uint64
	RefUID         [32]byte
	Recipient      common.Address
	Attester       common.Address
	Revocable      bool
	Data           []byte
}

// Claim2 is a decoded attestation in the claim schema.
type Claim2 struct {
	UID      common.Hash    `json:"uid"`
	Issuer   string         `json:"issuer"` // the Tern name of the attester at that block ("" if none)
	Attester common.Address `json:"attester"`
	Subject  common.Hash    `json:"subject"`
	Kind     string         `json:"kind"`
	Data     string         `json:"data"`
	Score    int64          `json:"score"`
	RefUID   common.Hash    `json:"ref,omitempty"`
	Revoked  bool           `json:"revoked,omitempty"`
	Expires  uint64         `json:"expires,omitempty"`
	Block    uint64         `json:"block"`
	Time     uint64         `json:"time"`
}

var claimArgs = func() abi.Arguments {
	t := func(s string) abi.Type { x, _ := abi.NewType(s, "", nil); return x }
	return abi.Arguments{{Type: t("bytes32")}, {Type: t("string")}, {Type: t("bytes")}, {Type: t("int64")}}
}()

func (c *Client) schema() common.Hash { return common.HexToHash(c.ch.dep.Schema) }

// easAttest makes a claim about subject; it returns the attestation uid.
func (c *Client) easAttest(subject common.Hash, kind string, data []byte, score int64, expires uint64, ref common.Hash) (common.Hash, error) {
	enc, err := claimArgs.Pack(subject, kind, data, score)
	if err != nil {
		return common.Hash{}, err
	}
	req := easRequest{Schema: c.schema(), Data: easRequestData{ExpirationTime: expires, Revocable: true, RefUID: ref, Data: enc, Value: big.NewInt(0)}}
	r, err := c.ch.send(c.key, nil, "EAS", "attest", req)
	if err != nil {
		return common.Hash{}, err
	}
	ev := c.ch.abis["EAS"].Events["Attested"]
	for _, l := range r.Logs {
		if len(l.Topics) > 0 && l.Topics[0] == ev.ID {
			out, err := ev.Inputs.NonIndexed().Unpack(l.Data)
			if err == nil && len(out) == 1 {
				return common.Hash(out[0].([32]byte)), nil
			}
		}
	}
	return common.Hash{}, errors.New("no attestation uid in receipt")
}

func (c *Client) easRevoke(uid common.Hash) error {
	_, err := c.ch.send(c.key, nil, "EAS", "revoke", easRevocation{Schema: c.schema(), Data: easRevocationData{Uid: uid, Value: big.NewInt(0)}})
	return err
}

// easGet reads one attestation and decodes it.
func (c *Client) easGet(uid common.Hash, block uint64) (*Claim2, error) {
	cl, err := c.easGetRaw(uid, block)
	if err != nil {
		return nil, err
	}
	cl.Issuer = c.res.nameAt(cl.Attester, block)
	return cl, nil
}

// easGetRaw: one attestation, decoded, without its issuer's name.
func (c *Client) easGetRaw(uid common.Hash, block uint64) (*Claim2, error) {
	out, err := c.ch.call("EAS", "getAttestation", uid)
	if err != nil {
		return nil, err
	}
	a := abi.ConvertType(out[0], new(easAttestation)).(*easAttestation)
	if common.Hash(a.Schema) != c.schema() {
		return nil, errors.New("not a Tern claim")
	}
	v, err := claimArgs.Unpack(a.Data)
	if err != nil {
		return nil, err
	}
	cl := &Claim2{UID: uid, Attester: a.Attester, Subject: common.Hash(v[0].([32]byte)), Kind: v[1].(string), Data: string(v[2].([]byte)),
		Score: v[3].(int64), RefUID: common.Hash(a.RefUID), Revoked: a.RevocationTime != 0, Expires: a.ExpirationTime, Block: block, Time: a.Time}
	return cl, nil
}

// easClaims lists claims, optionally only those about subject.
func (c *Client) easClaims(subject *common.Hash) ([]*Claim2, error) {
	if perfFixes {
		return c.easClaimsCached(subject)
	}
	ev := c.ch.abis["EAS"].Events["Attested"]
	q := ethereum.FilterQuery{FromBlock: new(big.Int).SetUint64(c.ch.dep.Block), Addresses: []common.Address{c.ch.c["EAS"].Address()},
		Topics: [][]common.Hash{{ev.ID}, nil, nil, {c.schema()}}}
	logs, err := c.ch.b.FilterLogs(context.Background(), q)
	if err != nil {
		return nil, err
	}
	var out []*Claim2
	for _, l := range logs {
		vals, err := ev.Inputs.NonIndexed().Unpack(l.Data)
		if err != nil || len(vals) != 1 {
			continue
		}
		cl, err := c.easGet(common.Hash(vals[0].([32]byte)), l.BlockNumber)
		if err != nil {
			continue
		}
		if subject != nil && cl.Subject != *subject {
			continue
		}
		out = append(out, cl)
	}
	return out, nil
}

// claimCache: the claims a process has read, by uid, and the next block to
// read from. A claim's contents never change; a revocation comes as an event.
// (Before the scale simulation every scan, rating and community read asked
// the node for every claim since deployment, one call each: a scan cost
// O(all claims ever) calls.)
type claimCache struct {
	mu      sync.Mutex
	next    uint64
	order   []common.Hash
	block   map[common.Hash]uint64
	claims  map[common.Hash]*Claim2 // without Issuer (each reader resolves it)
	revoked map[common.Hash]bool
}

func (ch *Chain) claimsCache() *claimCache {
	ch.ccOnce.Do(func() {
		ch.cc = &claimCache{next: ch.dep.Block, block: map[common.Hash]uint64{}, claims: map[common.Hash]*Claim2{}, revoked: map[common.Hash]bool{}}
		ch.cc.load(ch)
	})
	return ch.cc
}

// claimFile: the claims a one-shot process read, kept for the next run
// (ch.cacheDir: the identity's cache/ folder). Claims never change; a
// revocation is an event read again from where the file ends.
type claimFile struct {
	Chain   int64                   `json:"chain_id"`
	EAS     string                  `json:"eas"`
	Schema  string                  `json:"schema"`
	Next    uint64                  `json:"next"`
	Order   []common.Hash           `json:"order"`
	Block   map[common.Hash]uint64  `json:"block"`
	Claims  map[common.Hash]*Claim2 `json:"claims"`
	Revoked map[common.Hash]bool    `json:"revoked,omitempty"`
}

func (cc *claimCache) path(ch *Chain) string {
	if ch.cacheDir == "" || ch.c["EAS"] == nil {
		return ""
	}
	return filepath.Join(ch.cacheDir, "claims-"+ch.c["EAS"].Address().Hex()[2:10]+".json")
}

func (cc *claimCache) load(ch *Chain) {
	p := cc.path(ch)
	if p == "" {
		return
	}
	var f claimFile
	if readJSON(p, &f) != nil || f.Chain != ch.chainID.Int64() || f.EAS != ch.c["EAS"].Address().Hex() || f.Schema != ch.dep.Schema || f.Next < ch.dep.Block {
		return
	}
	if h, err := ch.b.BlockNumber(ctxb()); err != nil || f.Next > h+1 {
		return // a chain started again under the same addresses
	}
	cc.next, cc.order, cc.block, cc.claims = f.Next, f.Order, f.Block, f.Claims
	if f.Revoked != nil {
		cc.revoked = f.Revoked
	}
	if cc.block == nil || cc.claims == nil {
		cc.next, cc.order, cc.block, cc.claims = ch.dep.Block, nil, map[common.Hash]uint64{}, map[common.Hash]*Claim2{}
	}
}

func (cc *claimCache) save(ch *Chain) {
	p := cc.path(ch)
	if p == "" {
		return
	}
	os.MkdirAll(filepath.Dir(p), 0o700)
	writeJSON(p, claimFile{Chain: ch.chainID.Int64(), EAS: ch.c["EAS"].Address().Hex(), Schema: ch.dep.Schema, Next: cc.next, Order: cc.order,
		Block: cc.block, Claims: cc.claims, Revoked: cc.revoked})
}

func (c *Client) easClaimsCached(subject *common.Hash) ([]*Claim2, error) {
	return c.easClaimsOf(subject, nil)
}

// easClaimsOf: the cached claims about subject (nil: any) of these kinds (nil: any).
func (c *Client) easClaimsOf(subject *common.Hash, kinds map[string]bool) ([]*Claim2, error) {
	cc := c.ch.claimsCache()
	cc.mu.Lock()
	head, err := c.ch.b.BlockNumber(context.Background())
	if err != nil {
		cc.mu.Unlock()
		return nil, err
	}
	if head >= cc.next {
		att, rev := c.ch.abis["EAS"].Events["Attested"], c.ch.abis["EAS"].Events["Revoked"]
		q := ethereum.FilterQuery{FromBlock: new(big.Int).SetUint64(cc.next), ToBlock: new(big.Int).SetUint64(head), Addresses: []common.Address{c.ch.c["EAS"].Address()},
			Topics: [][]common.Hash{{att.ID, rev.ID}, nil, nil, {c.schema()}}}
		logs, err := c.ch.b.FilterLogs(context.Background(), q)
		if err != nil {
			cc.mu.Unlock()
			return nil, err
		}
		type want struct {
			uid   common.Hash
			block uint64
			cl    *Claim2
		}
		var todo []*want
		seen := map[common.Hash]bool{}
		for _, l := range logs {
			vals, err := att.Inputs.NonIndexed().Unpack(l.Data)
			if err != nil || len(vals) != 1 {
				continue
			}
			uid := common.Hash(vals[0].([32]byte))
			if l.Topics[0] == rev.ID {
				cc.revoked[uid] = true
				continue
			}
			if _, ok := cc.claims[uid]; ok || seen[uid] {
				continue
			}
			seen[uid] = true
			todo = append(todo, &want{uid: uid, block: l.BlockNumber})
		}
		// one call per new claim, several at a time (a reader that starts
		// cold, as the one-shot CLI does, had thousands to read in a row)
		for len(todo) > 0 { // in chunks, kept as they come (a one-shot process may be stopped midway)
			chunk := todo[:min(len(todo), 512)]
			todo = todo[len(chunk):]
			var wg sync.WaitGroup
			sem := make(chan struct{}, 16)
			for _, x := range chunk {
				wg.Add(1)
				sem <- struct{}{}
				go func(x *want) {
					defer func() { <-sem; wg.Done() }()
					x.cl, _ = c.easGetRaw(x.uid, x.block)
				}(x)
			}
			wg.Wait()
			for _, x := range chunk {
				if x.cl == nil {
					continue
				}
				cc.claims[x.uid], cc.block[x.uid] = x.cl, x.block
				cc.order = append(cc.order, x.uid)
			}
			if len(todo) > 0 {
				cc.save(c.ch)
			}
		}
		cc.next = head + 1
		if len(logs) > 0 {
			cc.save(c.ch)
		}
	}
	var picked []*Claim2
	for _, uid := range cc.order {
		cl := cc.claims[uid]
		if subject != nil && cl.Subject != *subject || kinds != nil && !kinds[cl.Kind] {
			continue
		}
		x := *cl
		x.Revoked = cl.Revoked || cc.revoked[uid]
		picked = append(picked, &x)
	}
	cc.mu.Unlock()
	for _, x := range picked {
		x.Issuer = c.res.nameAt(x.Attester, x.Block)
	}
	return picked, nil
}

// trustedClaims: claims about subject from issuers the reader trusts (nil = all),
// not revoked, not expired, made with the issuer's key of that time.
func (c *Client) trustedClaims(subject common.Hash, trusted map[string]bool) ([]*Claim2, error) {
	all, err := c.easClaims(&subject)
	if err != nil {
		return nil, err
	}
	var out []*Claim2
	for _, cl := range all {
		if cl.Revoked || cl.Issuer == "" {
			continue
		}
		if trusted != nil && !trusted[cl.Issuer] && !trusted[ownerName(cl.Issuer)] {
			continue
		}
		bt, _ := c.ch.blockTime(cl.Block)
		if err := c.res.verifySigner(cl.Issuer, cl.Attester, cl.Block, bt); err != nil {
			continue
		}
		out = append(out, cl)
	}
	return out, nil
}

func (c *Client) claimByUID(uid string) (*Claim2, error) {
	u := common.HexToHash(uid)
	return c.easGet(u, 0)
}
