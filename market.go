// Recipes on the money primitives. The Bank knows only balances, payments,
// allowances and locks with pluggable conditions; everything here is client
// code that anyone could write differently:
//
//	conditional payment  lock + AttestationCondition (threshold)
//	escrow with arbiter  lock + AttestationCondition (split) + release
//	bond                 lock to a beneficiary + AttestationCondition (split) by a slasher
//	subscription         lock + RateCondition
//	payment channel      lock + VoucherCondition (token streaming, metered work)
//	sale of an artifact  escrow with arbiter + the key sent sealed
//
// Governance uses OpenZeppelin Governor + Timelock with wTERN votes.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern/internal/artifact"
)

// firstIndexed returns the first indexed argument (an id) of event in a receipt.
func (c *Client) firstIndexed(r *types.Receipt, contract, event string) (uint64, error) {
	id := c.ch.abis[contract].Events[event].ID
	for _, l := range r.Logs {
		if len(l.Topics) > 1 && l.Topics[0] == id {
			return new(big.Int).SetBytes(l.Topics[1].Bytes()).Uint64(), nil
		}
	}
	return 0, fmt.Errorf("no %s in receipt", event)
}

func u(n uint64) *big.Int { return new(big.Int).SetUint64(n) }

func abiType(s string) abi.Type { t, _ := abi.NewType(s, "", nil); return t }

// argsOf: plain ABI arguments of the given types.
func argsOf(types ...string) abi.Arguments {
	var a abi.Arguments
	for _, t := range types {
		a = append(a, abi.Argument{Type: abiType(t)})
	}
	return a
}

// ---- locks

func (c *Client) lock(payee string, total *big.Int, refundAfter uint64, condition string, demand []byte) (uint64, error) {
	return c.lockGuarded(payee, payee, total, refundAfter, condition, demand)
}

// lockGuarded checks the grant against guardTo: the payee, or for an open pot
// the arbiter who will name one. An owner approval is spent only if the lock
// is made.
func (c *Client) lockGuarded(guardTo, payee string, total *big.Int, refundAfter uint64, condition string, demand []byte) (uint64, error) {
	done, err := c.guardPay(guardTo, total)
	if err != nil {
		return 0, err
	}
	id, err := c.lockNow(payee, total, refundAfter, condition, demand)
	done(err == nil)
	return id, err
}

func (c *Client) lockNow(payee string, total *big.Int, refundAfter uint64, condition string, demand []byte) (uint64, error) {
	cond := common.HexToAddress(c.ch.dep.Contracts[condition])
	p := payeeID(payee)
	if payee == "" {
		p = common.Hash{} // open: the condition names the payee
	}
	var r *types.Receipt
	var err error
	if c.asset != (common.Address{}) { // a lock in a token (set by --token)
		r, err = c.ch.send(c.key, nil, "Bank", "lockToken", c.id(), p, c.asset, total, refundAfter, cond, demand)
	} else {
		r, err = c.ch.send(c.key, nil, "Bank", "lock", c.id(), p, total, refundAfter, cond, demand)
	}
	if err != nil {
		return 0, c.limitErr(err)
	}
	return c.firstIndexed(r, "Bank", "Locked")
}

func (c *Client) claimLock(id uint64, fulfillment []byte) error {
	_, err := c.ch.send(c.key, nil, "Bank", "claim", u(id), fulfillment)
	return err
}

func (c *Client) refundEarly(id uint64, fulfillment []byte) error {
	_, err := c.ch.send(c.key, nil, "Bank", "refundEarly", u(id), fulfillment)
	return err
}

func (c *Client) refundLock(id uint64) error {
	_, err := c.ch.send(c.key, nil, "Bank", "refund", u(id))
	return err
}

func (c *Client) releaseLock(id uint64, amount *big.Int) error {
	out, err := c.ch.call("Bank", "remaining", u(id))
	if err != nil {
		return err
	}
	left := out[0].(*big.Int)
	if left.Sign() == 0 { // released already (escrow release, artifact confirm), or refunded
		if l, err := c.ch.call("Bank", "locks", u(id)); err == nil && l[5].(*big.Int).Sign() > 0 {
			if l[4].(*big.Int).Sign() == 0 {
				return fmt.Errorf("lock %d was refunded to the payer: nothing left to release", id)
			}
			return fmt.Errorf("lock %d is closed: %s paid out, %s refunded; nothing left to release", id, fmtCoins(l[4].(*big.Int)), fmtCoins(l[5].(*big.Int)))
		}
		return fmt.Errorf("lock %d is already fully paid out", id)
	}
	if amount == nil {
		amount = left
	} else if amount.Cmp(left) > 0 {
		return fmt.Errorf("lock %d has only %s left to release", id, fmtCoins(left))
	}
	_, err = c.ch.send(c.key, nil, "Bank", "release", u(id), amount)
	return err
}

// attDemand is AttestationCondition's demand: who attests, about what, and
// how the attestation's score moves the money (mode 0 threshold, 1 split, 2 award).
type attDemand struct {
	Attester [32]byte
	Schema   [32]byte
	Subject  [32]byte
	KindHash [32]byte
	MinScore int64
	Mode     uint8
}

func attDemandType() abi.Arguments {
	t, _ := abi.NewType("tuple", "", []abi.ArgumentMarshaling{
		{Name: "attester", Type: "bytes32"}, {Name: "schema", Type: "bytes32"}, {Name: "subject", Type: "bytes32"},
		{Name: "kindHash", Type: "bytes32"}, {Name: "minScore", Type: "int64"}, {Name: "mode", Type: "uint8"}})
	return abi.Arguments{{Type: t}}
}

// attestationDemand encodes AttestationCondition's demand.
func (c *Client) attestationDemand(attester string, subject common.Hash, kind string, minScore int64, mode uint8) ([]byte, error) {
	return attDemandType().Pack(attDemand{idOf(attester), c.schema(), subject, crypto.Keccak256Hash([]byte(kind)), minScore, mode})
}

// attestationDemandOf decodes an AttestationCondition demand.
func attestationDemandOf(b []byte) (*attDemand, bool) {
	v, err := attDemandType().Unpack(b)
	if err != nil || len(v) != 1 {
		return nil, false
	}
	return abiConvert[attDemand](v[0]), true
}

// knownKind names a demand's kind hash when it is one of the recipes' kinds.
func knownKind(h [32]byte) string {
	for _, k := range []string{"ruling", "slash", "award", "eval:tests", "eval", "delivered"} {
		if crypto.Keccak256Hash([]byte(k)) == common.Hash(h) {
			return k
		}
	}
	return common.Hash(h).Hex()
}

// decodeAttestationDemand shows an AttestationCondition demand in readable form.
func (c *Client) decodeAttestationDemand(b []byte) (map[string]any, bool) {
	d, ok := attestationDemandOf(b)
	if !ok {
		return nil, false
	}
	who, _ := c.res.nameOf(common.Hash(d.Attester))
	m := map[string]any{"attester": who, "subject": common.Hash(d.Subject).Hex(), "about": c.subjectName(common.Hash(d.Subject)), "kind": knownKind(d.KindHash)}
	if d.Mode == 2 {
		m["mode"] = "award: the attester names the payee; its score in basis points goes there, the rest back"
	} else if d.Mode == 1 {
		m["mode"] = "split: the attester's score in basis points goes to the payee, the rest back"
	} else {
		m["mode"] = "threshold"
		m["min_score"] = d.MinScore
	}
	return m, true
}

func uidFulfillment(uid common.Hash) []byte {
	b, _ := abi.Arguments{{Type: abiType("bytes32")}}.Pack(uid)
	return b
}

// ---- conditional payment: paid when a chosen verifier attests a score ≥ min

func (c *Client) openConditional(worker, verifier, subject, kind string, min int64, amount *big.Int, deadline uint64) (uint64, error) {
	if err := c.checkDeciders(worker, []string{verifier}); err != nil {
		return 0, err
	}
	d, err := c.attestationDemand(verifier, subjectID(subject), kind, min, 0)
	if err != nil {
		return 0, err
	}
	return c.lock(worker, amount, deadline, "AttestationCondition", d)
}

func (c *Client) attestScored(subject, kind, data string, score int64) (common.Hash, error) {
	return c.easAttest(subjectID(subject), kind, []byte(data), score, 0, common.Hash{})
}

// claim presents an attestation to a lock (anyone may; the money goes to the payee).
func (c *Client) claim(lockID uint64, uid common.Hash) error {
	return c.claimLock(lockID, uidFulfillment(uid))
}

// ---- escrow with an arbiter: the arbiter's ruling is a score = the payee's share in basis points

// noteDeal remembers what a deal's subject stands for: only its hash is on chain.
func (c *Client) noteDeal(subj common.Hash, what string) {
	if c.st.Deals == nil {
		c.st.Deals = map[string]string{}
	}
	c.st.Deals[subj.Hex()] = what
	c.save()
}

// subjectName: a claim subject in words where we can tell (a name, a group,
// a deal we made), else its hash.
func (c *Client) subjectName(h common.Hash) string {
	if n, err := c.res.nameOf(h); err == nil {
		return n
	}
	if d, ok := c.st.Deals[h.Hex()]; ok {
		return d
	}
	for g := range c.st.Groups {
		if setID(g) == h {
			return "group:" + g
		}
	}
	return h.Hex() + " (a deal's hash: its terms are known to its parties)"
}

func dealSubject(parts ...string) common.Hash {
	b := []byte("tern-deal")
	for _, p := range parts {
		b = append(b, 0)
		b = append(b, p...)
	}
	return crypto.Keccak256Hash(b)
}

func (c *Client) openEscrow(worker, arbiter string, amount *big.Int, deadline uint64, terms string) (uint64, common.Hash, error) {
	if err := c.checkDeciders(worker, []string{arbiter}); err != nil {
		return 0, common.Hash{}, err
	}
	subj := dealSubject(c.f.Name, worker, terms, fmt.Sprint(deadline))
	c.noteDeal(subj, fmt.Sprintf("escrow: %s pays %s for %q, %s decides", c.f.Name, worker, terms, arbiter))
	d, err := c.attestationDemand(arbiter, subj, "ruling", 0, 1)
	if err != nil {
		return 0, subj, err
	}
	id, err := c.lock(worker, amount, deadline, "AttestationCondition", d)
	return id, subj, err
}

// checkDeciders: the payer's warning at opening. A lock whose decider is on
// the payee's side (the payee itself, its owner, another agent of that owner)
// lets the payee's side alone say where the money goes, so it is refused
// unless --force. (The payee is warned again at lock accept.)
func (c *Client) checkDeciders(payee string, deciders []string) error {
	if c.force || payee == "" || strings.HasPrefix(payee, "0x") || ownerName(payee) == ownerName(c.f.Name) {
		return nil
	}
	var bad []string
	for _, d := range deciders {
		if d != "" && ownerName(d) == ownerName(payee) {
			bad = append(bad, d)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("refusing: the decider %s is the payee's side (%s, owner %s): it alone would decide where your money goes. "+
		"Pick someone neither side owns (a friend, a community's arbiter), or pass --force if you really mean it",
		strings.Join(bad, ", "), payee, ownerName(payee))
}

// ---- a bounty: an open payee; the decider's award names who gets it.
// A game's stakes for a winner not known yet (once called a pot: the same
// lock); a bounty for whoever solves it first. The payee is fixed at the
// first claim.

func (c *Client) openBounty(arbiter string, amount *big.Int, deadline uint64, terms string) (uint64, common.Hash, error) {
	// The guard checks the decider (the payee is open), and a family decider
	// passes it unchecked: an agent's own award would move its owner's money
	// to anyone (audit 5 RF-1). Its owner may decide; its agents may not.
	if !c.isOwner() && c.family(arbiter) && arbiter != c.f.Owner {
		return 0, common.Hash{}, fmt.Errorf("refusing: an agent can't open a bounty that it or another agent of %s decides "+
			"(the payee is open, so nothing would check where the award sends the money); name a decider outside your family, or %s itself", c.f.Owner, c.f.Owner)
	}
	subj := dealSubject(c.f.Name, "pot", terms, fmt.Sprint(deadline)) // "pot": the subject's form since the first pots
	c.noteDeal(subj, fmt.Sprintf("bounty of %s: %q, %s awards it", c.f.Name, terms, arbiter))
	d, err := c.attestationDemand(arbiter, subj, "award", 0, 2)
	if err != nil {
		return 0, subj, err
	}
	id, err := c.lockGuarded(arbiter, "", amount, deadline, "AttestationCondition", d)
	return id, subj, err
}

// award names the payee of a bounty and its share in basis points, on the
// one open bounty on subject that we decide (awardOn names the lock).
func (c *Client) award(subject common.Hash, payee string, shareBps int64, reason string) (common.Hash, error) {
	lock, err := c.decidedLock(subject, "award")
	if err != nil {
		return common.Hash{}, err
	}
	return c.awardOn(lock, subject, payee, shareBps, reason)
}

// awardOn: the award for one lock (its data starts with the payee's id).
func (c *Client) awardOn(lock uint64, subject common.Hash, payee string, shareBps int64, reason string) (common.Hash, error) {
	if _, err := c.res.nameOf(idOf(payee)); err != nil {
		return common.Hash{}, fmt.Errorf("no such payee %q", payee)
	}
	data := append(idOf(payee).Bytes(), []byte(reason)...)
	return c.decideOn(lock, subject, "award", data, shareBps, payee, true)
}

// rulingFor: the recipient a decision on a lock carries (Conditions.sol
// rulingFor). Rulings, awards and slashes count only on the lock they name,
// so one made for a sham deal can't decide a real one with the same subject
// and decider (audit 4 MO-6).
func rulingFor(bank common.Address, lock uint64) common.Address {
	var tag [32]byte
	copy(tag[:], "tern-ruling")
	h := crypto.Keccak256(tag[:], common.LeftPadBytes(bank.Bytes(), 32), common.LeftPadBytes(u(lock).Bytes(), 32))
	return common.BytesToAddress(h[12:])
}

func (c *Client) rulingTag(lock uint64) common.Address {
	return rulingFor(c.ch.c["Bank"].Address(), lock)
}

// ruledFor: the recipient of an attestation (rulingTag of the lock a decision was made for).
func (c *Client) ruledFor(uid common.Hash) common.Address {
	out, err := c.ch.call("EAS", "getAttestation", uid)
	if err != nil || len(out) == 0 {
		return common.Address{}
	}
	return abi.ConvertType(out[0], new(easAttestation)).(*easAttestation).Recipient
}

// decideOn: a decider's claim on one lock (a ruling, an award, a slash, a
// verifier's score), made for that lock. The guard comes first: a decision
// moves the lock's money (guardDecide).
func (c *Client) decideOn(lock uint64, subject common.Hash, kind string, data []byte, score int64, payee string, award bool) (common.Hash, error) {
	if err := c.guardDecide(lock, payee, score, award); err != nil {
		return common.Hash{}, err
	}
	enc, err := claimArgs.Pack(subject, kind, data, score)
	if err != nil {
		return common.Hash{}, err
	}
	req := easRequest{Schema: c.schema(), Data: easRequestData{Recipient: c.rulingTag(lock), Revocable: true, Data: enc, Value: big.NewInt(0)}}
	r, err := c.ch.send(c.key, nil, "EAS", "attest", req)
	if err != nil {
		return common.Hash{}, err
	}
	ev := c.ch.abis["EAS"].Events["Attested"]
	for _, l := range r.Logs {
		if len(l.Topics) > 0 && l.Topics[0] == ev.ID {
			if out, err := ev.Inputs.NonIndexed().Unpack(l.Data); err == nil && len(out) == 1 {
				uid := common.Hash(out[0].([32]byte))
				return uid, c.recordRuling(lock, uid)
			}
		}
	}
	return common.Hash{}, errors.New("no attestation uid in receipt")
}

// recordRuling: on a ruling lock (RulingCondition, PanelCondition,
// OptimisticCondition) a ruling counts once its decider records it on the
// condition: the first k deciders to record decide, and a decider's newer
// ruling replaces its own until then (audit 6 RU-1). Other locks: nothing to do.
func (c *Client) recordRuling(lock uint64, uid common.Hash) error {
	l, err := c.ch.call("Bank", "locks", u(lock))
	if err != nil {
		return nil // no such lock (yet): the ruling can't count anyway
	}
	switch cond := c.conditionName(l[7].(common.Address)); cond {
	case "RulingCondition", "PanelCondition", "OptimisticCondition":
		if _, err := c.ch.send(c.key, nil, cond, "record", u(lock), uid); err != nil {
			return fmt.Errorf("ruling %s attested, but it doesn't count on lock %d: %w", uid.Hex(), lock, err)
		}
	}
	return nil
}

// decideSubject: a decision by subject (the older verbs: escrow rule, bond
// slash, pot award), on the one open lock about it that we decide.
func (c *Client) decideSubject(subject common.Hash, kind string, data []byte, score int64) (common.Hash, error) {
	lock, err := c.decidedLock(subject, kind)
	if err != nil {
		return common.Hash{}, err
	}
	payee := ""
	if l, err := c.ch.call("Bank", "locks", u(lock)); err == nil {
		payee = c.nameOr(common.Hash(l[2].([32]byte)))
	}
	return c.decideOn(lock, subject, kind, data, score, payee, false)
}

// decidedLock: the one open lock about subject that this identity decides
// with claims of this kind (an arbiter's escrow, a slasher's bond, a bounty,
// a panel, a pay-unless-disputed lock). A decision names its lock, and two
// locks with one subject can't be told apart by it: then lock decide LOCK.
func (c *Client) decidedLock(subject common.Hash, kind string) (uint64, error) {
	evs, err := c.ch.events("Bank", "Locked")
	if err != nil {
		return 0, err
	}
	kh := crypto.Keccak256Hash([]byte(kind))
	var found []string
	var lock uint64
	for _, ev := range evs {
		demand, _ := ev.Args["demand"].([]byte)
		cond, _ := ev.Args["condition"].(common.Address)
		var subj, kindHash [32]byte
		var deciders [][32]byte
		switch c.conditionName(cond) {
		case "AttestationCondition":
			d, ok := attestationDemandOf(demand)
			if !ok || d.Mode == 0 {
				continue
			}
			subj, kindHash, deciders = d.Subject, d.KindHash, [][32]byte{d.Attester}
		case "PanelCondition":
			v, err := panelDemandType().Unpack(demand)
			if err != nil || len(v) != 1 {
				continue
			}
			d := abiConvert[panelDemand](v[0])
			subj, kindHash, deciders = d.Subject, d.KindHash, d.Panel
		case "OptimisticCondition":
			d, err := decodeOptimistic(demand)
			if err != nil {
				continue
			}
			subj, kindHash, deciders = d.Subject, d.KindHash, d.Deciders
		case "RulingCondition":
			d, err := decodeRuling(demand)
			if err != nil {
				continue
			}
			subj, kindHash, deciders = d.Subject, d.KindHash, d.Deciders
		default:
			continue
		}
		id := ev.Args["id"].(*big.Int).Uint64()
		if common.Hash(subj) != subject || common.Hash(kindHash) != kh || !c.decidesAmong(deciders) || c.lockLeft(id).Sign() == 0 {
			continue
		}
		lock = id
		found = append(found, fmt.Sprint(id))
	}
	switch len(found) {
	case 0:
		return 0, fmt.Errorf("no open lock about %s that you decide with a %q: a decision names its lock (lock decide LOCK …)", subject.Hex(), kind)
	case 1:
		return lock, nil
	}
	return 0, fmt.Errorf("locks %s are all about %s and decided by you: a decision names its lock, so decide the one you mean with lock decide LOCK …",
		strings.Join(found, ", "), subject.Hex())
}

// claimAward applies an award to a pot or bounty, and says plainly whose it
// is: a non-winner learns who won instead of seeing "claimed", and a winner
// whose payout already happened is told so.
func (c *Client) claimAward(lockID uint64, uid common.Hash) (map[string]any, error) {
	l, err := c.ch.call("Bank", "locks", u(lockID))
	if err != nil {
		return nil, err
	}
	payee, paid := common.Hash(l[2].([32]byte)), l[4].(*big.Int)
	if cl, err := c.easGet(uid, 0); err == nil && cl.Kind == "award" && len(cl.Data) >= 32 && payee == (common.Hash{}) {
		payee = common.BytesToHash([]byte(cl.Data)[:32]) // the award names the payee at the first claim
	}
	winner := c.nameOr(payee)
	mine := payee != (common.Hash{}) && ownerName(winner) == ownerName(c.f.Name)
	if payee != (common.Hash{}) && !mine {
		if paid.Sign() > 0 {
			return nil, fmt.Errorf("not yours: lock %d was awarded to %s and %s coins are already paid to them", lockID, winner, fmtCoins(paid))
		}
		return nil, fmt.Errorf("not yours: lock %d is awarded to %s; the money can only go to them (to apply the award for them: lock claim %d %s)", lockID, winner, lockID, uid.Hex())
	}
	rem, err := c.ch.call("Bank", "remaining", u(lockID))
	if err == nil && rem[0].(*big.Int).Sign() == 0 {
		if mine && paid.Sign() > 0 {
			return map[string]any{"lock": lockID, "done": "nothing to do", "already_paid_to_you": fmtCoins(paid)}, nil
		}
		return nil, fmt.Errorf("nothing to claim: lock %d is empty (paid %s, refunded %s)", lockID, fmtCoins(paid), fmtCoins(l[5].(*big.Int)))
	}
	if err := c.settle(lockID, uid); err != nil {
		if mine && paid.Sign() > 0 {
			return map[string]any{"lock": lockID, "done": "nothing more to claim", "already_paid_to_you": fmtCoins(paid)}, nil
		}
		return nil, err
	}
	after, err := c.ch.call("Bank", "locks", u(lockID))
	if err != nil {
		return map[string]any{"lock": lockID, "done": "claimed"}, nil
	}
	got := new(big.Int).Sub(after[4].(*big.Int), paid)
	m := map[string]any{"lock": lockID, "paid_to": c.nameOr(common.Hash(after[2].([32]byte))), "paid_now": fmtCoins(got)}
	if back := new(big.Int).Sub(after[5].(*big.Int), l[5].(*big.Int)); back.Sign() > 0 {
		m["back_to_payer"] = fmtCoins(back)
	}
	if got.Sign() > 0 {
		m["done"] = "paid to you"
	} else {
		m["done"] = "nothing moved to you"
	}
	return m, nil
}

// giveBack: the payee side returns money to the payer now (nil = all that is left).
func (c *Client) giveBack(id uint64, amount *big.Int) error {
	if amount == nil {
		out, err := c.ch.call("Bank", "remaining", u(id))
		if err != nil {
			return err
		}
		amount = out[0].(*big.Int)
	}
	_, err := c.ch.send(c.key, nil, "Bank", "giveBack", u(id), amount)
	return err
}

// ---- reveal bond: back once the commitment is revealed; the payee's if not in time

func (c *Client) revealBond(commitment uint64, payee string, amount *big.Int) (uint64, uint64, error) {
	cm, err := c.ch.call("Commitments", "all", u(commitment))
	if err != nil {
		return 0, 0, err
	}
	revealBy := cm[3].(uint64)
	if revealBy == 0 {
		return 0, 0, errors.New("that commitment is a marker; it is never revealed")
	}
	now := c.chainNowU()
	if revealBy <= now {
		return 0, 0, errors.New("the reveal deadline has passed: post the bond before it")
	}
	// The payee needs time to claim after the reveal deadline passes.
	grace := (revealBy - now) + 600
	after := revealBy + grace
	d, _ := abi.Arguments{{Type: abiType("uint256")}}.Pack(u(commitment))
	id, err := c.lock(payee, amount, after, "RevealCondition", d)
	return id, after, err
}

func (c *Client) chainNowU() uint64 { return uint64(c.chainNow()) }

func (c *Client) settleRevealBond(id uint64) (string, error) {
	if out, err := c.ch.call("Bank", "remaining", u(id)); err == nil && out[0].(*big.Int).Sign() == 0 {
		return "already settled: nothing left in this bond (anyone may settle it)", nil
	}
	if err := c.refundEarly(id, nil); err == nil {
		return "revealed: the bond went back to its payer", nil
	}
	if err := c.claimLock(id, nil); err == nil {
		return "not revealed in time: the bond went to the payee", nil
	}
	return "", errors.New("nothing to settle yet: not revealed, and the reveal deadline hasn't passed")
}

func (c *Client) revealBondView(demand []byte) map[string]any {
	v, err := abi.Arguments{{Type: abiType("uint256")}}.Unpack(demand)
	if err != nil || len(v) != 1 {
		return nil
	}
	cid := v[0].(*big.Int)
	m := map[string]any{"commitment": cid.String(), "rule": "back to the payer once revealed; to the payee if not revealed by reveal_by"}
	if cm, err := c.ch.call("Commitments", "all", cid); err == nil {
		by, _ := c.res.nameOf(common.Hash(cm[0].([32]byte)))
		m["committed_by"] = by
		m["reveal_by"] = cm[3].(uint64)
		m["revealed"] = cm[4].(bool)
	}
	return m
}

// rule: an arbiter's decision, share to the payee in basis points (0..10000),
// on the one open lock about subject that we decide (decideOn names the lock).
func (c *Client) rule(subject common.Hash, shareBps int64, reason string) (common.Hash, error) {
	return c.decideSubject(subject, "ruling", []byte(reason), shareBps)
}

// settle applies a ruling to a lock in both directions.
func (c *Client) settle(lockID uint64, ruling common.Hash) error {
	err1 := c.claimLock(lockID, uidFulfillment(ruling))
	err2 := c.refundEarly(lockID, uidFulfillment(ruling))
	if err1 != nil && err2 != nil {
		return fmt.Errorf("nothing moved: %v / %v", err1, err2)
	}
	return nil
}

// ---- bonds: stake that a slasher may give (part of) to a beneficiary

func (c *Client) bond(slasher, beneficiary string, amount *big.Int, unlockAt uint64, purpose string) (uint64, common.Hash, error) {
	if err := c.checkDeciders(beneficiary, []string{slasher}); err != nil {
		return 0, common.Hash{}, err
	}
	subj := dealSubject("bond", c.f.Name, slasher, purpose, fmt.Sprint(unlockAt))
	c.noteDeal(subj, fmt.Sprintf("bond of %s: %q, %s may slash it", c.f.Name, purpose, slasher))
	d, err := c.attestationDemand(slasher, subj, "slash", 0, 1)
	if err != nil {
		return 0, subj, err
	}
	id, err := c.lock(beneficiary, amount, unlockAt, "AttestationCondition", d)
	return id, subj, err
}

// ---- subscriptions: a fixed amount per period, cancellable by the payer

func (c *Client) subscribe(provider string, perPeriod *big.Int, periods, period uint64) (uint64, error) {
	h, err := c.ch.b.HeaderByNumber(ctxb(), nil)
	if err != nil {
		return 0, err
	}
	t, _ := abi.NewType("tuple", "", []abi.ArgumentMarshaling{
		{Name: "perPeriod", Type: "uint256"}, {Name: "start", Type: "uint64"}, {Name: "period", Type: "uint64"}, {Name: "payer", Type: "bytes32"}})
	d, err := abi.Arguments{{Type: t}}.Pack(struct {
		PerPeriod *big.Int
		Start     uint64
		Period    uint64
		Payer     [32]byte
	}{perPeriod, h.Time, period, c.id()})
	if err != nil {
		return 0, err
	}
	total := new(big.Int).Mul(perPeriod, u(periods))
	return c.lock(provider, total, h.Time+period*periods+period, "RateCondition", d)
}

func (c *Client) collect(id uint64) error   { return c.claimLock(id, nil) }
func (c *Client) cancelSub(id uint64) error { return c.refundEarly(id, nil) }

// ---- payment channels: vouchers signed off chain, claimed whenever

func (c *Client) openChannel(payee string, total *big.Int, refundAfter uint64) (uint64, error) {
	d, _ := abi.Arguments{{Type: abiType("address")}}.Pack(c.addr())
	return c.lock(payee, total, refundAfter, "VoucherCondition", d)
}

// relayChannelCap: the most a channel to our own relay may hold without the
// grant's payment rules. The relay is infrastructure whoever runs the agent
// configured (--relay, TERNCHAIN_RELAY), not a payee the agent chose; mail
// can't change it, and the chain's spend limit still applies.
var relayChannelCap = mustCoins("0.1")

// openRelayChannel pays for posting through the configured relay.
func (c *Client) openRelayChannel(payee string, total *big.Int, refundAfter uint64) (uint64, error) {
	if total.Cmp(relayChannelCap) > 0 {
		return c.openChannel(payee, total, refundAfter) // a pricey node: the usual rules
	}
	d, _ := abi.Arguments{{Type: abiType("address")}}.Pack(c.addr())
	return c.lockNow(payee, total, refundAfter, "VoucherCondition", d)
}

// voucher signs "I owe you `cumulative` in total under this lock".
func (c *Client) voucher(lockID uint64, cumulative *big.Int) ([]byte, error) {
	out, err := c.ch.call("VoucherCondition", "voucherDigest", c.ch.c["Bank"].Address(), u(lockID), cumulative)
	if err != nil {
		return nil, err
	}
	d := out[0].([32]byte)
	return crypto.Sign(d[:], c.key)
}

func (c *Client) claimVoucher(lockID uint64, cumulative *big.Int, sig []byte) error {
	f, err := abi.Arguments{{Type: abiType("uint256")}, {Type: abiType("bytes")}}.Pack(cumulative, sig)
	if err != nil {
		return err
	}
	return c.claimLock(lockID, f)
}

// ---- artifacts: files off chain, encrypted; their key sold for payment.
// Sealing, verifying and opening (streamed, for files of any size): bigfile.go.

func (c *Client) registerArtifact(hash [32]byte, size uint64, uri, license string, price *big.Int, key []byte) (uint64, error) {
	commit := common.Hash{}
	if key != nil {
		commit = crypto.Keccak256Hash(key)
	}
	r, err := c.ch.send(c.key, nil, "Artifacts", "register", c.id(), common.Hash(hash), size, uri, license, price, commit)
	if err != nil {
		return 0, err
	}
	return c.firstIndexed(r, "Artifacts", "Registered")
}

func (c *Client) artifact(id uint64) (owner common.Hash, price *big.Int, active bool, hash, commit common.Hash, err error) {
	out, err := c.ch.call("Artifacts", "get", u(id))
	if err != nil {
		return
	}
	return common.Hash(out[0].([32]byte)), out[1].(*big.Int), out[2].(bool), common.Hash(out[3].([32]byte)), common.Hash(out[4].([32]byte)), nil
}

const tKey = "tern.key"

// KeyDelivery is the sealed datagram that carries an artifact's key to a buyer.
type KeyDelivery struct {
	Lock     uint64 `json:"lock"`
	Artifact uint64 `json:"artifact"`
	Key      []byte `json:"key"`
}

// buy: an escrow for the artifact's price to its owner, with an arbiter.
func (c *Client) buy(artifact uint64, arbiter string, deadline uint64) (uint64, common.Hash, error) {
	owner, price, active, _, _, err := c.artifact(artifact)
	if err != nil {
		return 0, common.Hash{}, err
	}
	if !active || price.Sign() == 0 {
		return 0, common.Hash{}, fmt.Errorf("artifact %d is not for sale (artifact show %d: its price and state)", artifact, artifact)
	}
	seller, err := c.res.nameOf(owner)
	if err != nil {
		return 0, common.Hash{}, err
	}
	return c.openEscrow(seller, arbiter, price, deadline, fmt.Sprintf("artifact %d", artifact))
}

// minDispute: the chain refuses to accept a lock its payer can take back
// sooner (Bank.MIN_DISPUTE).
const minDispute = 12 * 3600

// acceptLock: the payee accepts a lock's terms (who decides). Check them first.
func (c *Client) acceptLock(id uint64) error {
	if l, err := c.ch.call("Bank", "locks", u(id)); err == nil {
		if ra := int64(l[6].(uint64)); ra < c.chainNow()+minDispute {
			return fmt.Errorf("lock %d can be refunded to its payer by timeout at %s, less than 12 hours from now, too short to dispute: "+
				"too short to deliver into (ask the payer for a lock with a later deadline)", id, time.Unix(ra, 0).UTC().Format(time.RFC3339))
		}
		if c.conditionName(l[7].(common.Address)) == "OptimisticCondition" { // pay unless disputed: time to claim after the window
			if d, err := decodeOptimistic(l[8].([]byte)); err == nil && l[6].(uint64) < d.WindowEnds+minDispute {
				return fmt.Errorf("lock %d's refund time leaves less than 12 hours after its challenge window to claim: the payer could take it back "+
					"by timeout; ask for a lock with a later refund time", id)
			}
		}
		if c.conditionName(l[7].(common.Address)) == "RulingCondition" {
			d, err := decodeRuling(l[8].([]byte))
			if err != nil {
				return fmt.Errorf("lock %d's ruling terms can't be read: refuse it (%v)", id, err)
			}
			if d.WindowEnds != 0 && l[6].(uint64) < d.WindowEnds+minDispute {
				return fmt.Errorf("lock %d's refund time leaves less than 12 hours after its challenge window: ask for a lock with a later refund time", id)
			}
			if err := c.rulingDemandProblem(d, l[6].(uint64), common.Hash(l[0].([32]byte)), common.Hash(l[2].([32]byte))); err != nil {
				return fmt.Errorf("lock %d: %v: don't accept it (ask the payer for a lock that can be decided)", id, err)
			}
		}
	}
	_, err := c.ch.send(c.key, nil, "Bank", "acceptLock", u(id))
	if err != nil && strings.Contains(err.Error(), "12") {
		return fmt.Errorf("the lock's refund deadline is less than 12 hours away, so it can't be accepted: %w", err)
	}
	return err
}

// rulingDemandProblem: why a RulingCondition demand can never decide fairly
// (audit 6 RU-3): no such rule or quota, fewer distinct deciders off both
// parties' sides than k, a hearing that leaves the deciders under 12 hours,
// anyone disputing for free. nil: none found.
func (c *Client) rulingDemandProblem(d *rulingDemand, refundAfter uint64, payer, payee common.Hash) error {
	if d.Rule > 4 {
		return fmt.Errorf("aggregation rule %d doesn't exist (0..4): no claim can ever pass", d.Rule)
	}
	if d.K == 0 || d.Rule == 4 && (d.Quota == 0 || d.Quota > d.K) {
		return errors.New("k or the threshold's quota is out of range: no claim can ever pass")
	}
	payerOwner, payeeOwner := ownerName(c.nameOr(payer)), ownerName(c.nameOr(payee))
	owners := map[string]bool{}
	for _, p := range d.Deciders {
		o := ownerName(c.nameOr(common.Hash(p)))
		if o != "" && o != payerOwner && o != payeeOwner {
			owners[o] = true
		}
	}
	if len(owners) < int(d.K) {
		return fmt.Errorf("only %d distinct deciders are off both parties' sides, and %d must rule", len(owners), d.K)
	}
	if d.HearingUntil+minDispute > refundAfter {
		return errors.New("the hearing ends less than 12 hours before the refund time: the deciders couldn't rule in time")
	}
	if d.AnyoneDisputes && (d.DisputeBond == nil || d.DisputeBond.Sign() == 0) {
		return errors.New("anyone may dispute without a bond: a stranger could block your payment for free")
	}
	return nil
}

// checkSale: before delivering goods into a lock (H1-R, DESIGN §9, audit 4
// MO-3/4/5): one of the network's conditions (not a contract the payer
// deployed), in the network coin, paid to us, at least the price, untouched,
// with time to dispute, opened by the buyer; and its demand is exactly what
// the buyer's client builds, decided by names on our own list of trusted
// arbiters (ternchain arbiter trust NAME): a ghost arbiter, a panel that can
// never rule or the buyer's free second name means we can never win.
func (c *Client) checkSale(lockID uint64, buyer string, price *big.Int, artifact uint64) error {
	l, err := c.ch.call("Bank", "locks", u(lockID))
	if err != nil {
		return err
	}
	cond := c.conditionName(l[7].(common.Address))
	if cond != "AttestationCondition" && cond != "PanelCondition" && cond != "HashlockCondition" {
		return fmt.Errorf("lock %d uses condition %s, not one of the network's (AttestationCondition, PanelCondition, HashlockCondition): refuse it", lockID, l[7].(common.Address).Hex())
	}
	if p := common.Hash(l[2].([32]byte)); p != c.id() && p != idOf(c.f.Owner) {
		return fmt.Errorf("lock %d doesn't pay you", lockID)
	}
	if tok := l[9].(common.Address); tok != (common.Address{}) {
		return fmt.Errorf("lock %d holds token %s, not the network coin the price is in: refuse it", lockID, tok.Hex())
	}
	total := l[3].(*big.Int)
	if total.Cmp(price) < 0 {
		return fmt.Errorf("lock %d holds %s, less than the price %s", lockID, fmtCoins(total), fmtCoins(price))
	}
	if rem, err := c.ch.call("Bank", "remaining", u(lockID)); err != nil || rem[0].(*big.Int).Cmp(total) != 0 {
		return fmt.Errorf("lock %d was already partly paid out or refunded", lockID)
	}
	refundAfter := l[6].(uint64)
	if ra := int64(refundAfter); ra < c.chainNow()+minDispute {
		return fmt.Errorf("lock %d can be refunded to its payer by timeout within 12 hours: too short to dispute, refuse it", lockID)
	}
	if from := common.Hash(l[1].([32]byte)); from != idOf(buyer) && common.Hash(l[0].([32]byte)) != idOf(buyer) {
		return fmt.Errorf("lock %d was not opened by %s: refuse it", lockID, buyer)
	}
	demand := l[8].([]byte)
	switch cond {
	case "AttestationCondition":
		d, ok := c.decodeAttestationDemand(demand)
		if !ok {
			return fmt.Errorf("lock %d's demand doesn't decode: refuse it", lockID)
		}
		att, _ := d["attester"].(string)
		if att == "" || strings.HasPrefix(att, "0x") {
			return fmt.Errorf("lock %d is decided by a name nobody registered: nobody can ever rule for you; refuse it", lockID)
		}
		seller, err := c.artifactSeller(artifact)
		if err != nil {
			return err
		}
		want, err := c.attestationDemand(att, dealSubject(buyer, seller, fmt.Sprintf("artifact %d", artifact), fmt.Sprint(refundAfter)), "ruling", 0, 1)
		if err != nil || !bytes.Equal(want, demand) {
			return fmt.Errorf("lock %d's demand is not an escrow for artifact %d with %s ruling (schema, subject, kind or mode differ): refuse it", lockID, artifact, att)
		}
		if dd, err := c.ch.call("AttestationCondition", "deciderOf", demand); err == nil {
			owner := common.Hash(dd[1].([32]byte))
			if owner == (common.Hash{}) {
				return fmt.Errorf("lock %d is decided by %s, which has no owner on chain: refuse it", lockID, att)
			}
			if owner == idOf(ownerName(buyer)) || owner == common.Hash(l[0].([32]byte)) {
				return fmt.Errorf("lock %d is decided by %s, the payer's own side: refuse it", lockID, att)
			}
		}
		if !c.trustsArbiter(att) {
			return fmt.Errorf("lock %d is decided by %s, who is not on your list of arbiters you trust: refuse it "+
				"(owner ids can't tell a stranger from the buyer's second name; 'ternchain arbiter trust %s' if you know them)", lockID, att, att)
		}
	case "PanelCondition":
		v, err := panelDemandType().Unpack(demand)
		if err != nil || len(v) != 1 {
			return fmt.Errorf("lock %d's panel demand doesn't decode: refuse it", lockID)
		}
		pd := *abi.ConvertType(v[0], new(panelDemand)).(*panelDemand)
		if common.Hash(pd.Schema) != c.schema() || common.Hash(pd.KindHash) != crypto.Keccak256Hash([]byte("ruling")) || pd.K == 0 ||
			int(pd.K) > len(pd.Panel) {
			return fmt.Errorf("lock %d's panel can never rule (schema, kind or k): refuse it", lockID)
		}
		seller, err := c.artifactSeller(artifact)
		if err != nil {
			return err
		}
		if want := dealSubject(buyer, seller, "panel", fmt.Sprintf("artifact %d", artifact), fmt.Sprint(refundAfter)); common.Hash(pd.Subject) != want {
			return fmt.Errorf("lock %d's panel case is about another deal (its subject is not this sale's: the buyer opens it as "+
				"panel open %s AMOUNT K PANEL artifact %d, refunding at the lock's time): refuse it", lockID, seller, artifact)
		}
		opened, _ := c.ch.call("Bank", "openedAt", u(lockID))
		if len(opened) == 1 && pd.HearingUntil < opened[0].(uint64) {
			return fmt.Errorf("lock %d's hearing ended before the lock was opened: old rulings would decide it; refuse it", lockID)
		}
		if refundAfter < pd.HearingUntil+minDispute {
			return fmt.Errorf("lock %d refunds less than 12 hours after its hearing: no time to rule; refuse it", lockID)
		}
		judges := 0
		for _, id := range pd.Panel {
			n, err := c.res.nameOf(common.Hash(id))
			if err != nil || isParty(n, buyer, c.f.Name) || !c.trustsArbiter(n) {
				continue
			}
			judges++
		}
		if judges < int(pd.K) {
			return fmt.Errorf("lock %d needs %d rulings, but only %d of its panel are registered names you trust that are not a party: refuse it "+
				"('ternchain arbiter trust NAME' for panelists you know)", lockID, pd.K, judges)
		}
	}
	return c.checkDecider(lockID, buyer)
}

// artifactSeller: the name an artifact's owner id resolves to.
func (c *Client) artifactSeller(artifact uint64) (string, error) {
	owner, _, _, _, _, err := c.artifact(artifact)
	if err != nil {
		return "", err
	}
	return c.res.nameOf(owner)
}

// trustsArbiter: name (or its owner) is on this identity's list of arbiters.
func (c *Client) trustsArbiter(name string) bool {
	for _, a := range c.st.Arbiters {
		if a == name || a == ownerName(name) {
			return true
		}
	}
	return false
}

// checkDecider refuses a lock whose decider is the other party's own side.
func (c *Client) checkDecider(lockID uint64, other string) error {
	l, err := c.ch.call("Bank", "locks", u(lockID))
	if err != nil {
		return err
	}
	if d, ok := c.decodeAttestationDemand(l[8].([]byte)); ok {
		if att, _ := d["attester"].(string); ownerName(att) == ownerName(other) {
			return fmt.Errorf("lock %d is decided by %s, the other party's own side: refuse it", lockID, att)
		}
	}
	return nil
}

func (c *Client) deliverKey(lockID, artifact uint64, buyer string, key []byte) error {
	// Before handing over the goods: the arbiter must not be the buyer's side,
	// and accepting the lock is what lets the arbiter refund the buyer at all.
	_, price, _, _, _, err := c.artifact(artifact)
	if err != nil {
		return err
	}
	if err := c.checkSale(lockID, buyer, price, artifact); err != nil {
		return err
	}
	if l, err := c.htlc(lockID); err == nil {
		// A hash lock on the key's commitment: claiming publishes the key and
		// pays us in one step (atomic, no arbiter). The key becomes public:
		// whoever has the file can open it, so use it for public releases.
		if l.Algo != algoKeccak || l.Hash != commitOf(key) {
			return fmt.Errorf("hash lock %d is not on this artifact's key commitment (keccak256 of the key): refuse it", lockID)
		}
		if err := c.htlcClaim(lockID, key, htlcTerms{Amount: price}); err != nil {
			return fmt.Errorf("claiming hash lock %d with the key: %w", lockID, err)
		}
		_, err = c.sendDatagram(tKey, buyer, KeyDelivery{Lock: lockID, Artifact: artifact, Key: key})
		return err
	}
	if err := c.acceptLock(lockID); err != nil {
		return fmt.Errorf("accepting lock %d: %w", lockID, err)
	}
	_, err = c.sendDatagram(tKey, buyer, KeyDelivery{Lock: lockID, Artifact: artifact, Key: key})
	return err
}

// receiveKeyFor finds the key delivered for a lock in our mail and checks it
// against the seller's commitment.
func (c *Client) receiveKeyFor(lockID uint64) (*KeyDelivery, error) {
	if _, err := c.scan(); err != nil {
		return nil, err
	}
	for i := len(c.st.Items) - 1; i >= 0; i-- {
		it := c.st.Items[i]
		if it.Type != tKey || it.Status == "refused" {
			continue
		}
		var k KeyDelivery
		if json.Unmarshal(it.Body, &k) != nil || k.Lock != lockID {
			continue
		}
		_, _, _, _, commit, err := c.artifact(k.Artifact)
		if err != nil {
			return nil, err
		}
		if crypto.Keccak256Hash(k.Key) != commit {
			return nil, errors.New("the key does not match the seller's commitment")
		}
		return &k, nil
	}
	return nil, errors.New("no key delivered for that lock yet")
}

// receiveKey finds the key for a lock in our mail and checks it against the
// seller's commitment and, with the file, the registered hash (in memory:
// small files; big ones go through receiveKeyFor + decryptArtifact).
func (c *Client) receiveKey(lockID uint64, file []byte) ([]byte, []byte, error) {
	if _, err := c.scan(); err != nil {
		return nil, nil, err
	}
	for i := len(c.st.Items) - 1; i >= 0; i-- {
		it := c.st.Items[i]
		if it.Type != tKey || it.Status == "refused" {
			continue
		}
		var k KeyDelivery
		if json.Unmarshal(it.Body, &k) != nil || k.Lock != lockID {
			continue
		}
		_, _, _, hash, commit, err := c.artifact(k.Artifact)
		if err != nil {
			return nil, nil, err
		}
		if crypto.Keccak256Hash(k.Key) != commit {
			return k.Key, nil, errors.New("the key does not match the seller's commitment")
		}
		if file == nil {
			return k.Key, nil, nil
		}
		if sha256.Sum256(file) != [32]byte(hash) {
			return k.Key, nil, errors.New("the file does not match the registered hash")
		}
		plain, err := artifact.OpenFile(file, k.Key)
		if err != nil {
			return k.Key, nil, fmt.Errorf("the key does not open the file: %w", err)
		}
		return k.Key, plain, nil
	}
	return nil, nil, errors.New("no key delivered for that lock yet")
}

// ---- allowances

func (c *Client) approve(spender common.Address, amount *big.Int) error {
	_, err := c.ch.send(c.key, nil, "Bank", "approve", c.id(), spender, amount)
	return err
}

// ---- governance: OpenZeppelin Governor, wTERN votes, Timelock exit window

// lockVotes wraps coins into wTERN and delegates the votes to ourselves.
func (c *Client) lockVotes(amount *big.Int) error {
	if _, err := c.ch.send(c.key, amount, "WrappedTern", "deposit"); err != nil {
		return err
	}
	_, err := c.ch.send(c.key, nil, "WrappedTern", "delegate", c.me())
	return err
}

func (c *Client) unlockVotes(amount *big.Int) error {
	_, err := c.ch.send(c.key, nil, "WrappedTern", "withdraw", amount)
	return err
}

// ---- milestones: one escrow per milestone, each with its own deadline and
// terms; a delivery is bound to its artifact's hash by a "delivered" claim on
// the milestone's subject (PRIMITIVES.md, "Big projects, paid by milestones").

type Milestone struct {
	Amount *big.Int
	Terms  string
}

// parseMilestones: "AMOUNT:TERMS" each.
func parseMilestones(args []string) ([]Milestone, error) {
	var ms []Milestone
	for _, a := range args {
		amt, terms, ok := strings.Cut(a, ":")
		if !ok || strings.TrimSpace(terms) == "" {
			return nil, fmt.Errorf("milestone %q: AMOUNT:TERMS, e.g. 10:\"playable alpha\"", a)
		}
		v, err := parseCoins(amt)
		if err != nil {
			return nil, err
		}
		ms = append(ms, Milestone{v, strings.TrimSpace(terms)})
	}
	if len(ms) == 0 {
		return nil, errors.New("no milestones: give AMOUNT:TERMS pairs, like 1:design 2:build")
	}
	return ms, nil
}

// openMilestones opens one escrow per milestone; milestone i is due i×every seconds from now.
func (c *Client) openMilestones(worker, arbiter string, ms []Milestone, every uint64) ([]map[string]any, error) {
	now := c.chainNowU()
	var out []map[string]any
	for i, m := range ms {
		due := now + uint64(i+1)*every
		terms := fmt.Sprintf("milestone %d of %d: %s", i+1, len(ms), m.Terms)
		id, subj, err := c.openEscrow(worker, arbiter, m.Amount, due, terms)
		if err != nil {
			return out, fmt.Errorf("milestone %d: %w (the ones above are open)", i+1, err)
		}
		out = append(out, map[string]any{"milestone": i + 1, "lock": id, "subject": subj.Hex(), "amount": fmtCoins(m.Amount),
			"refund_after_utc": time.Unix(int64(due), 0).UTC().Format(time.RFC3339), "terms": terms})
	}
	return out, nil
}

// runArbiter: the seller's own list of arbiters it trusts (DESIGN §9, MO-5).
func runArbiter(c *Client, cmd string, pos []string) error {
	switch cmd {
	case "arbiter trust", "arbiter untrust":
		if err := need(pos, 1, cmd+" NAME"); err != nil {
			return err
		}
		var keep []string
		for _, a := range c.st.Arbiters {
			if a != pos[0] {
				keep = append(keep, a)
			}
		}
		if cmd == "arbiter trust" {
			if o, err := c.ch.call("Registry", "ownerOf", idOf(pos[0])); err != nil || common.Hash(o[0].([32]byte)) == (common.Hash{}) {
				return fmt.Errorf("%s is not a registered name", pos[0])
			}
			keep = append(keep, pos[0])
		}
		c.st.Arbiters = keep
		if err := c.save(); err != nil {
			return err
		}
		out(map[string]any{"arbiters": c.st.Arbiters})
	case "arbiter list":
		out(map[string]any{"arbiters": c.st.Arbiters})
	default:
		return unknownCommand(cmd)
	}
	return nil
}
