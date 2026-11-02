// The contract bricks of review/BRICKS.md, client side: RulingCondition (one
// arbiter or a panel, an aggregation rule, a hearing, an optional dispute
// window and bond), moderation claims from any ICommunity, names for types
// (SchemaNames), secret ballots for any group (GroupTrees), and the citizens'
// personhood points. Item swaps are in world.go.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern/internal/zk"
)

// ---- RulingCondition

// rulingRules: RulingBase's aggregation rules by name.
var rulingRules = map[string]uint8{"lower-median": 0, "median": 1, "mean": 2, "unanimous": 3, "threshold": 4}

type rulingDemand struct {
	Deciders       [][32]byte
	K              uint8
	Rule           uint8
	Quota          uint8
	Schema         [32]byte
	Subject        [32]byte
	KindHash       [32]byte
	HearingUntil   uint64
	WindowEnds     uint64
	DisputeBond    *big.Int
	AnyoneDisputes bool
}

func rulingDemandType() abi.Arguments {
	t, _ := abi.NewType("tuple", "", []abi.ArgumentMarshaling{
		{Name: "deciders", Type: "bytes32[]"}, {Name: "k", Type: "uint8"}, {Name: "rule", Type: "uint8"}, {Name: "quota", Type: "uint8"},
		{Name: "schema", Type: "bytes32"}, {Name: "subject", Type: "bytes32"}, {Name: "kindHash", Type: "bytes32"},
		{Name: "hearingUntil", Type: "uint64"}, {Name: "windowEnds", Type: "uint64"}, {Name: "disputeBond", Type: "uint256"},
		{Name: "anyoneDisputes", Type: "bool"}})
	return abi.Arguments{{Type: t}}
}

func decodeRuling(b []byte) (*rulingDemand, error) {
	v, err := rulingDemandType().Unpack(b)
	if err != nil || len(v) != 1 {
		return nil, errors.New("bad ruling demand")
	}
	return abiConvert[rulingDemand](v[0]), nil
}

// RulingOpts: what a ruling lock asks of its deciders.
type RulingOpts struct {
	Deciders       []string
	K              uint8  // 0: 1
	Rule           string // lower-median (default), median, mean, unanimous, threshold
	Quota          uint8  // threshold: how many of the k must give the payee >= 50%
	HearingUntil   uint64 // rulings before it don't count
	Window         uint64 // seconds: pay unless disputed within it (0: the deciders decide at once)
	DisputeBond    *big.Int
	AnyoneDisputes bool
}

// openRuling locks amount for payee under RulingCondition until refundAfter.
func (c *Client) openRuling(payee string, amount *big.Int, refundAfter uint64, o RulingOpts, terms string) (uint64, common.Hash, error) {
	if o.K == 0 {
		o.K = 1
	}
	if o.Rule == "" {
		o.Rule = "lower-median"
	}
	rule, ok := rulingRules[o.Rule]
	if !ok {
		return 0, common.Hash{}, fmt.Errorf("rule %q: one of lower-median, median, mean, unanimous, threshold", o.Rule)
	}
	if rule == 4 && (o.Quota == 0 || o.Quota > o.K) {
		return 0, common.Hash{}, errors.New("threshold: a quota of 1..k rulings")
	}
	judges := map[string]bool{} // one seat per owner (audit 6 RU-4)
	for _, d := range o.Deciders {
		if !isParty(d, c.f.Name, payee) {
			judges[ownerName(d)] = true
		}
	}
	if len(judges) < int(o.K) {
		return 0, common.Hash{}, fmt.Errorf("k=%d but only %d distinct owners among the deciders are not a party (the payer's or the payee's side never count, one seat per owner): it could never be decided", o.K, len(judges))
	}
	if o.AnyoneDisputes && (o.DisputeBond == nil || o.DisputeBond.Sign() == 0) {
		return 0, common.Hash{}, errors.New("anyone may dispute only with a bond (--bond): otherwise a stranger blocks the payment for free")
	}
	now := c.chainNowU()
	var ends uint64
	if o.Window > 0 {
		ends = now + o.Window
		if refundAfter < ends+minDispute {
			return 0, common.Hash{}, errors.New("the refund time must leave the deciders 12 hours after the window (a dispute needs them)")
		}
	}
	if o.HearingUntil >= refundAfter {
		return 0, common.Hash{}, errors.New("the hearing must end before the refund time")
	}
	bond := o.DisputeBond
	if bond == nil {
		bond = new(big.Int)
	}
	subj := dealSubject(c.f.Name, payee, "ruling", terms, fmt.Sprint(refundAfter))
	c.noteDeal(subj, fmt.Sprintf("ruling case: %s pays %s for %q; %d of %s decide by %s", c.f.Name, payee, terms, o.K, strings.Join(o.Deciders, ","), o.Rule))
	ids := make([][32]byte, len(o.Deciders))
	for i, d := range o.Deciders {
		ids[i] = idOf(d)
	}
	d, err := rulingDemandType().Pack(rulingDemand{ids, o.K, rule, o.Quota, c.schema(), subj, crypto.Keccak256Hash([]byte("ruling")),
		o.HearingUntil, ends, bond, o.AnyoneDisputes})
	if err != nil {
		return 0, subj, err
	}
	id, err := c.lock(payee, amount, refundAfter, "RulingCondition", d)
	return id, subj, err
}

// disputeRuling disputes a ruling lock within its window, with its bond.
func (c *Client) disputeRuling(id uint64, reason string) error {
	l, err := c.ch.call("Bank", "locks", u(id))
	if err != nil {
		return err
	}
	if c.conditionName(l[7].(common.Address)) != "RulingCondition" {
		return fmt.Errorf("lock %d is not a ruling lock", id)
	}
	d, err := decodeRuling(l[8].([]byte))
	if err != nil {
		return err
	}
	_, err = c.ch.send(c.key, d.DisputeBond, "RulingCondition", "dispute", u(id), reason)
	return err
}

// rulingDecide: a decider's ruling on a ruling lock (the payee's share in bps).
func (c *Client) rulingDecide(id uint64, shareBps int64, reason string) (common.Hash, error) {
	l, err := c.ch.call("Bank", "locks", u(id))
	if err != nil {
		return common.Hash{}, err
	}
	d, err := decodeRuling(l[8].([]byte))
	if err != nil {
		return common.Hash{}, err
	}
	return c.decideOn(id, d.Subject, "ruling", []byte(reason), shareBps, c.nameOr(common.Hash(l[2].([32]byte))), false)
}

// settleRuling: no rulings, a claim after the window; with rulings, both ways.
func (c *Client) settleRuling(id uint64, uids []common.Hash) error {
	if len(uids) == 0 {
		return c.claimLock(id, nil)
	}
	ids := make([][32]byte, len(uids))
	for i, x := range uids {
		ids[i] = x
	}
	f, _ := abi.Arguments{{Type: abiType("bytes32[]")}}.Pack(ids)
	err1 := c.claimLock(id, f)
	err2 := c.refundEarly(id, f)
	if err1 != nil && err2 != nil {
		return fmt.Errorf("nothing moved: %v / %v", err1, err2)
	}
	return nil
}

// ---- moderation claims (contracts/Community.sol MODERATION_SCHEMA)

// Moderation actions.
const (
	modHide   = 1
	modBan    = 2
	modUnban  = 3
	modUnhide = 4
)

var moderationArgs = abi.Arguments{{Type: abiType("address")}, {Type: abiType("bytes32")}, {Type: abiType("uint8")},
	{Type: abiType("bytes32")}, {Type: abiType("bytes32")}, {Type: abiType("string")}}

// ModClaim: one moderation claim, and whether it counts now (the standard's rule).
type ModClaim struct {
	UID       common.Hash    `json:"uid"`
	Community common.Address `json:"community"`
	ID        common.Hash    `json:"id"`
	Action    uint8          `json:"action"`
	Target    common.Hash    `json:"target"`
	By        common.Hash    `json:"by"`
	Reason    string         `json:"reason"`
	Attester  common.Address `json:"attester"`
	Time      uint64         `json:"time"`
	Counts    bool           `json:"counts"`
}

// community: any ICommunity contract (Sets, Commons, or anyone's).
func (c *Client) communityAt(a common.Address) *bind.BoundContract {
	return bind.NewBoundContract(a, c.ch.abis["Sets"], c.ch.b, c.ch.b, c.ch.b) // Sets' ABI carries ICommunity
}

func (c *Client) canModerate(community common.Address, id, who common.Hash) bool {
	var out []any
	err := c.communityAt(community).Call(&bind.CallOpts{Context: context.Background()}, &out, "canModerate", id, who)
	return err == nil && len(out) == 1 && out[0].(bool)
}

// bannedFamily: is who, or its owner, banned in the community? A ban of an
// owner leaves its agents' moderator powers in the deployed Commons (audit
// ext M-2): their claims don't count for this client.
func (c *Client) bannedFamily(community common.Address, id, who common.Hash) bool {
	ids := []common.Hash{who}
	if o, err := c.ch.call("Registry", "ownerOf", who); err == nil {
		ids = append(ids, common.Hash(o[0].([32]byte)))
	}
	for _, x := range ids {
		var out []any
		if err := c.communityAt(community).Call(&bind.CallOpts{Context: context.Background()}, &out, "isBanned", id, x); err == nil && len(out) == 1 && out[0].(bool) {
			return true
		}
	}
	return false
}

// moderate: our own moderation claim for a community we moderate.
func (c *Client) moderationClaim(community common.Address, id common.Hash, action uint8, target common.Hash, reason string) (common.Hash, error) {
	if !c.canModerate(community, id, c.id()) {
		return common.Hash{}, errors.New("you don't moderate that community: a claim from you wouldn't count")
	}
	enc, err := moderationArgs.Pack(community, id, action, target, c.id(), reason)
	if err != nil {
		return common.Hash{}, err
	}
	req := easRequest{Schema: schemaUID(moderationSchema), Data: easRequestData{Revocable: true, Data: enc, Value: big.NewInt(0)}}
	r, err := c.ch.send(c.key, nil, "EAS", "attest", req)
	if err != nil {
		return common.Hash{}, err
	}
	ev := c.ch.abis["EAS"].Events["Attested"]
	for _, l := range r.Logs {
		if len(l.Topics) > 0 && l.Topics[0] == ev.ID {
			if out, err := ev.Inputs.NonIndexed().Unpack(l.Data); err == nil && len(out) == 1 {
				return common.Hash(out[0].([32]byte)), nil
			}
		}
	}
	return common.Hash{}, errors.New("no attestation uid in receipt")
}

// moderationClaims: every moderation claim about one community, oldest first,
// each marked whether it counts: not revoked, and issued by the community
// contract itself or by a key acting for a name that moderates it now.
func (c *Client) moderationClaims(community common.Address, id common.Hash) ([]ModClaim, error) {
	evs, err := c.ch.events("EAS", "Attested", nil, nil, []common.Hash{schemaUID(moderationSchema)})
	if err != nil {
		return nil, err
	}
	var out []ModClaim
	for _, e := range evs {
		uid := common.Hash(e.Args["uid"].([32]byte))
		r, err := c.ch.call("EAS", "getAttestation", uid)
		if err != nil {
			continue
		}
		a := abi.ConvertType(r[0], new(easAttestation)).(*easAttestation)
		v, err := moderationArgs.Unpack(a.Data)
		if err != nil || v[0].(common.Address) != community || common.Hash(v[1].([32]byte)) != id {
			continue
		}
		m := ModClaim{UID: uid, Community: community, ID: id, Action: v[2].(uint8), Target: common.Hash(v[3].([32]byte)), By: common.Hash(v[4].([32]byte)),
			Reason: v[5].(string), Attester: a.Attester, Time: a.Time}
		if a.RevocationTime == 0 {
			if a.Attester == community {
				m.Counts = true
			} else if ok, err := c.ch.call("Registry", "canAct", m.By, a.Attester); err == nil && ok[0].(bool) {
				m.Counts = c.canModerate(community, id, m.By) && !c.bannedFamily(community, id, m.By)
			}
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out, nil
}

// moderationState: what the counting claims say now: hidden refs and banned names.
func (c *Client) moderationState(community common.Address, id common.Hash) (hidden, banned map[common.Hash]bool, err error) {
	ms, err := c.moderationClaims(community, id)
	if err != nil {
		return nil, nil, err
	}
	hidden, banned = map[common.Hash]bool{}, map[common.Hash]bool{}
	for _, m := range ms {
		if !m.Counts {
			continue
		}
		switch m.Action {
		case modHide:
			hidden[m.Target] = true
		case modUnhide:
			delete(hidden, m.Target)
		case modBan:
			banned[m.Target] = true
		case modUnban:
			delete(banned, m.Target)
		}
	}
	return hidden, banned, nil
}

// ---- names for types (SchemaNames)

// schemaRecord is EAS's SchemaRecord.
type schemaRecord struct {
	Uid       [32]byte
	Resolver  common.Address
	Revocable bool
	Schema    string
}

// publishType registers schema in EAS (if it isn't yet) and publishes it as the next version of name.
func (c *Client) publishType(name, schema, uri string) (uint64, common.Hash, error) {
	uid := schemaUID(schema)
	if out, err := c.ch.call("SchemaRegistry", "getSchema", uid); err != nil || abiConvert[schemaRecord](out[0]).Uid != uid {
		if _, err := c.ch.send(c.key, nil, "SchemaRegistry", "register", schema, common.Address{}, true); err != nil {
			return 0, uid, err
		}
	}
	r, err := c.ch.send(c.key, nil, "SchemaNames", "publish", c.id(), name, uid, uri)
	if err != nil {
		return 0, uid, err
	}
	n, err := c.firstIndexed(r, "SchemaNames", "Published")
	return n, uid, err
}

// typeOf: a publisher's newest schema uid for name and its version (names
// are per publisher: SchemaNames has no global names, audit 6 CM-3).
func (c *Client) typeOf(publisher, name string) (common.Hash, uint64, error) {
	out, err := c.ch.call("SchemaNames", "latest", idOf(publisher), name)
	if err != nil {
		return common.Hash{}, 0, err
	}
	n := out[1].(*big.Int).Uint64()
	if n == 0 {
		return common.Hash{}, 0, fmt.Errorf("%s has published no type named %q", publisher, name)
	}
	return common.Hash(out[0].([32]byte)), n, nil
}

// schemaNameList: the newest version of each type name, per publisher. With
// pinned publishers, only theirs (each publisher's own list: another
// publisher's flood can't push them out of the window); without, every
// publisher's, the first publisher of a name winning (at most 4096 entries).
func (c *Client) schemaNameList(pinned []string) ([]typeName, error) {
	type ref struct {
		pub  common.Hash
		name string
	}
	var refs []ref
	if len(pinned) > 0 {
		for _, p := range pinned {
			out, err := c.ch.call("SchemaNames", "countBy", idOf(p))
			if err != nil {
				return nil, err
			}
			n := out[0].(*big.Int).Int64()
			for i := int64(0); i < n && i < 4096; i++ {
				if r, err := c.ch.call("SchemaNames", "nameAtBy", idOf(p), big.NewInt(i)); err == nil {
					refs = append(refs, ref{idOf(p), r[0].(string)})
				}
			}
		}
	} else {
		out, err := c.ch.call("SchemaNames", "count")
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		n := out[0].(*big.Int).Int64()
		for i := int64(0); i < n && i < 4096; i++ {
			if r, err := c.ch.call("SchemaNames", "entryAt", big.NewInt(i)); err == nil && !seen[r[1].(string)] {
				seen[r[1].(string)] = true
				refs = append(refs, ref{common.Hash(r[0].([32]byte)), r[1].(string)})
			}
		}
	}
	var list []typeName
	for _, x := range refs {
		l, err := c.ch.call("SchemaNames", "latest", x.pub, x.name)
		if err != nil || l[1].(*big.Int).Sign() == 0 {
			continue
		}
		uri := ""
		if v, err := c.ch.call("SchemaNames", "version", x.pub, x.name, l[1].(*big.Int)); err == nil {
			uri = versionURI(v[0])
		}
		list = append(list, typeName{x.name, c.nameOr(x.pub), uri})
	}
	return list, nil
}

// ---- secret ballots for any group (GroupTrees)

func (c *Client) groupID(source common.Address, set common.Hash) common.Hash {
	b, _ := abi.Arguments{{Type: abiType("address")}, {Type: abiType("bytes32")}}.Pack(source, set)
	return crypto.Keccak256Hash(b)
}

// groupJoinTree: registers our voting identity in GroupTrees (if needed) and syncs our leaf in group g.
func (c *Client) groupJoinTree(g common.Hash) error {
	com := zk.IdentityCommitment(c.identitySecret(0))
	if have, err := c.bigCall("GroupTrees", "identityOf", c.id()); err != nil {
		return err
	} else if have.Cmp(com) != 0 {
		if _, err := c.ch.send(c.key, nil, "GroupTrees", "register", c.id(), com); err != nil {
			return err
		}
	}
	_, err := c.ch.send(c.key, nil, "GroupTrees", "sync", g, c.id())
	return err
}

// groupLeavesOf: group g's leaves after its MemberSet events up to `before` (nil: all).
func (c *Client) groupLeavesOf(g common.Hash, before *event) ([]*big.Int, error) {
	evs, err := c.ch.events("GroupTrees", "MemberSet", []common.Hash{g})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(evs, func(i, j int) bool {
		return evs[i].Block < evs[j].Block || evs[i].Block == evs[j].Block && evs[i].Index < evs[j].Index
	})
	var leaves []*big.Int
	for _, e := range evs {
		if before != nil && (e.Block > before.Block || e.Block == before.Block && e.Index > before.Index) {
			break
		}
		i := int(e.Args["index"].(*big.Int).Int64())
		for len(leaves) <= i {
			leaves = append(leaves, big.NewInt(0))
		}
		leaves[i] = e.Args["commitment"].(*big.Int)
	}
	return leaves, nil
}

// ballotVote proves our vote for option on ballot id and submits it with
// `relay` (a relayer's key: the voter's own address would show who voted).
func (c *Client) ballotVote(id uint64, option uint8, relay *ecdsa.PrivateKey) (*big.Int, error) {
	evs, err := c.ch.events("GroupTrees", "BallotOpened", []common.Hash{common.BigToHash(u(id))})
	if err != nil || len(evs) == 0 {
		return nil, fmt.Errorf("no ballot %d", id)
	}
	g := common.Hash(evs[0].Args["group"].([32]byte))
	rootOpen := evs[0].Args["root"].(*big.Int)
	openLeaves, err := c.groupLeavesOf(g, &evs[0])
	if err != nil {
		return nil, err
	}
	nowLeaves, err := c.groupLeavesOf(g, nil)
	if err != nil {
		return nil, err
	}
	openT, nowT := zk.NewTree(zk.GroupDepth, openLeaves), zk.NewTree(zk.GroupDepth, nowLeaves)
	rootNow, err := c.bigCall("GroupTrees", "rootOf", g)
	if err != nil {
		return nil, err
	}
	if openT.Root().Cmp(rootOpen) != 0 || nowT.Root().Cmp(rootNow) != 0 {
		return nil, errors.New("the group's tree doesn't match its events: try again")
	}
	secret := c.identitySecret(0)
	com := zk.IdentityCommitment(secret)
	oi, ni := openT.IndexOf(com), nowT.IndexOf(com)
	if oi < 0 || ni < 0 {
		return nil, errors.New("your identity wasn't in the group's tree when the ballot opened, or isn't now")
	}
	scope, err := c.bigCall("GroupTrees", "scopeOf", u(id))
	if err != nil {
		return nil, err
	}
	nullifier := zk.HashN(scope, secret)
	op, ob, _ := openT.Proof(oi)
	np, nb, _ := nowT.Proof(ni)
	a := &zk.VoteCircuit{RootOpen: rootOpen, RootNow: rootNow, Nullifier: nullifier, Scope: scope, Message: big.NewInt(int64(option)), Secret: secret}
	copy(a.OpenPath[:], zk.Bigs(op))
	copy(a.OpenBits[:], zk.Ints(ob))
	copy(a.NowPath[:], zk.Bigs(np))
	copy(a.NowBits[:], zk.Ints(nb))
	if err := c.ch.zkCheck("vote"); err != nil {
		return nil, err
	}
	proof, err := zk.Prove("vote", a)
	if err != nil {
		return nil, err
	}
	_, err = c.ch.send(relay, nil, "GroupTrees", "vote", u(id), rootNow, nullifier, option, proof)
	return nullifier, err
}

// ---- citizens: personhood points

// personhood: a name's personhood points (10000 admit, with age and the other rules).
func (c *Client) personhood(name string) (uint64, error) {
	v, err := c.bigCall("Citizens", "personhood", idOf(name))
	if err != nil {
		return 0, err
	}
	return v.Uint64(), nil
}
