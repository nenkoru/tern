// One grammar for the money primitive (AUDIT-WHEEL-2, R1): every recipe is a
// Bank lock with a condition, so it is opened, decided and settled by the same
// verbs whatever its kind.
//
//	lock open KIND …                  escrow, optimistic, panel, bounty (pot), bond, sale, pledge,
//	                                  htlc, subscription, channel, cond, reveal
//	lock show|accept LOCK
//	lock decide LOCK SHARE-BPS REASON (bounty: lock decide LOCK PAYEE SHARE-BPS REASON)
//	lock dispute LOCK REASON          a pay-unless-disputed lock, by its payer
//	lock settle LOCK [UID,…]          claims or refunds, whichever applies; finds the decision itself
//	lock claim|refund|release LOCK …  the Bank's own verbs
//
// The recipes' own commands (escrow open, pot award, bond settle…) stay as
// aliases: they print a one-line note on stderr (deprecated, below) and work
// as before. This file only routes: the recipes live where they did.
package main

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// lockKindSpec: one KIND of lock open: the recipe command it runs, its arguments, what it is.
type lockKindSpec struct {
	recipe string // the command that opens it
	args   string // its arguments after "lock open KIND"
	what   string
}

var lockKinds = map[string]lockKindSpec{
	"escrow":       {"escrow open", "WORKER ARBITER AMOUNT TERMS… [--days N]", "an arbiter (neither side's) decides a dispute; the payer releases"},
	"optimistic":   {"escrow open", "WORKER AMOUNT TERMS… --hours H (--arbiter NAME | --panel a,b,c --k N) [--days D]", "paid after H hours unless the payer disputes; then the arbiter or k of the panel decide within D days"},
	"ruling":       {"lock open ruling", "PAYEE AMOUNT TERMS… --panel a,b,c [--k N] [--rule lower-median|median|mean|unanimous|threshold [--quota N]] [--hours H --bond B [--anyone]] [--hearing-minutes N] [--days D]", "k of n deciders' rulings aggregated by a rule (RulingCondition); with --hours: paid after H hours unless disputed (with the bond)"},
	"panel":        {"panel open", "PAYEE AMOUNT TERMS… --panel a,b,c --k N [--hearing-minutes N] [--days N]", "k of n decide after a hearing (also: PAYEE AMOUNT K a,b,c TERMS…)"},
	"bounty":       {"bounty open", "DECIDER AMOUNT TERMS… [--days N]", "the payee is open: the decider's award names it (a game's pot, a bounty for whoever solves it)"},
	"bond":         {"bond add", "SLASHER BENEFICIARY AMOUNT PURPOSE… [--days N]", "a stake a slasher may give (part of) to the beneficiary; back after the term"},
	"sale":         {"artifact buy", "ARTIFACT ARBITER [--days N]", "buy an artifact: the seller delivers the key into the lock"},
	"pledge":       {"fund pledge", "CAMPAIGN AMOUNT", "back a campaign: all or nothing"},
	"htlc":         {"htlc open", "PAYEE AMOUNT HASH|--secret-file F --hours N [--algo sha256|keccak]", "a hash lock: the payee claims with the preimage before the timeout"},
	"subscription": {"sub open", "PROVIDER PER-PERIOD PERIODS [--period SECONDS]", "the provider collects each period; the payer cancels"},
	"channel":      {"channel open", "PEER AMOUNT [--hours N]", "a voucher channel: one lock, then signed vouchers without gas"},
	"cond":         {"cond open", "WORKER VERIFIER SUBJECT KIND MIN-SCORE AMOUNT [--days N]", "paid when the verifier attests a score of at least MIN-SCORE"},
	"reveal":       {"revealbond add", "COMMITMENT PAYEE AMOUNT", "back once the commitment is revealed; the payee's if not in time"},
}

// lockKindAlias: other words for the kinds.
var lockKindAlias = map[string]string{
	"pot": "bounty", "award": "bounty", "sub": "subscription", "voucher": "channel", "voucher-channel": "channel",
	"hashlock": "htlc", "artifact": "sale", "buy": "sale", "fund": "pledge", "revealbond": "reveal", "reveal-bond": "reveal",
	"conditional": "cond", "pay-unless-disputed": "optimistic",
}

func lockKindNames() []string {
	var ks []string
	for k := range lockKinds {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// lockOpenUsage: every kind and its arguments (lock open with none, or a wrong one).
func lockOpenUsage() string {
	var b strings.Builder
	b.WriteString("usage: lock open KIND …, KIND one of:\n")
	for _, k := range lockKindNames() {
		fmt.Fprintf(&b, "  lock open %-12s %s   (%s)\n", k, lockKinds[k].args, lockKinds[k].what)
	}
	return strings.TrimRight(b.String(), "\n")
}

// lockFlags: the flags the recipes take, gathered for lock open.
type lockFlags struct {
	prim    primFlags
	hearing int
	htlc    htlcFlags
	// lock open ruling (RulingCondition)
	rule, bond string
	quota      uint
	anyone     bool
}

// runLockOpen opens a lock of a kind by its recipe, with the recipe's output.
func runLockOpen(c *Client, pos []string, f lockFlags) error {
	if len(pos) == 0 {
		return errors.New(lockOpenUsage())
	}
	kind := strings.ToLower(pos[0])
	if a, ok := lockKindAlias[kind]; ok {
		kind = a
	}
	spec, ok := lockKinds[kind]
	if !ok {
		return fmt.Errorf("no lock kind %q: %s", pos[0], lockOpenUsage())
	}
	pos = pos[1:]
	p := f.prim
	switch kind {
	case "escrow":
		p.optimistic = 0
	case "optimistic":
		if p.optimistic == 0 { // lock open optimistic … --hours H (escrow open's --optimistic H)
			p.optimistic, p.hours = p.hours, 0
		}
		if p.optimistic <= 0 {
			return errors.New("usage: lock open optimistic " + spec.args + "   (--hours: the challenge window)")
		}
	case "panel":
		if p.panel != "" { // the flag form: PAYEE AMOUNT TERMS… --panel a,b,c --k N
			if len(pos) < 3 {
				return errors.New("usage: lock open panel " + spec.args)
			}
			k := p.k
			if k == 0 {
				k = uint64(len(splitList(p.panel))/2 + 1) // a majority, unless --k says
			}
			pos = append([]string{pos[0], pos[1], fmt.Sprint(k), p.panel}, pos[2:]...)
		}
		return runWorld(c, "panel open", pos, worldFlags{days: p.days, minutes: p.minutes, hearing: f.hearing})
	case "ruling":
		return runLockOpenRuling(c, pos, f)
	case "htlc":
		return runHTLC(c, "htlc open", pos, f.htlc)
	case "pledge":
		return runFund(c, "fund pledge", pos, p.days, p.minutes, "", "")
	}
	return runPrimitive(c, spec.recipe, pos, p)
}

// runLockOpenRuling: lock open ruling PAYEE AMOUNT TERMS… --panel a,b,c …:
// a RulingCondition lock (bricks.go openRuling), the terms sent sealed to the
// payee and the deciders.
func runLockOpenRuling(c *Client, pos []string, f lockFlags) error {
	usage := "lock open ruling " + lockKinds["ruling"].args
	if err := need(pos, 3, usage); err != nil {
		return err
	}
	deciders := splitList(f.prim.panel)
	if len(deciders) == 0 && f.prim.arbiter != "" {
		deciders = []string{f.prim.arbiter}
	}
	if len(deciders) == 0 {
		return errors.New("usage: " + usage + "   (--panel: who decides; --arbiter NAME for one)")
	}
	amt, err := c.parseMoney(pos[1])
	if err != nil {
		return err
	}
	k := f.prim.k
	if k == 0 {
		k = uint64(len(deciders)/2 + 1) // a majority, unless --k says
	}
	if k > 255 || f.quota > 255 {
		return errors.New("--k and --quota: at most 255")
	}
	days := f.prim.days
	if days <= 0 {
		days = 30
	}
	now := c.chainNowU()
	refundAfter := now + uint64(days)*86400
	o := RulingOpts{Deciders: deciders, K: uint8(k), Rule: f.rule, Quota: uint8(f.quota), Window: uint64(max(f.prim.hours, 0)) * 3600,
		AnyoneDisputes: f.anyone}
	if f.hearing > 0 {
		o.HearingUntil = now + uint64(f.hearing)*60
	}
	if f.bond != "" {
		if o.DisputeBond, err = parseCoins(f.bond); err != nil {
			return err
		}
	}
	if f.anyone && (o.DisputeBond == nil || o.DisputeBond.Sign() == 0) {
		return errors.New("--anyone needs a --bond above 0 (a free dispute by a third party would only grief the payee)")
	}
	if err := c.spendCheck(amt); err != nil {
		return err
	}
	terms := strings.Join(pos[2:], " ")
	id, subj, err := c.openRuling(pos[0], amt, refundAfter, o, terms)
	if err != nil {
		return err
	}
	r := map[string]any{"lock": id, "kind": "ruling", "payee": pos[0], "amount": c.fmtMoney(amt), "deciders": deciders, "k": k,
		"rule": firstNonEmpty(f.rule, "lower-median"), "refund_after": time.Unix(int64(refundAfter), 0).UTC().Format(time.RFC3339)}
	if o.Window > 0 {
		r["pay_unless_disputed_hours"] = f.prim.hours
	}
	if parts := []string{c.f.Name, pos[0], "ruling", terms, fmt.Sprint(refundAfter)}; dealSubject(parts...) == subj {
		for k, v := range c.sendTerms(id, parts, terms, append([]string{pos[0]}, deciders...)) {
			r[k] = v
		}
	}
	out(r)
	return nil
}

// lockRow: a lock's fields (Bank.locks) and its condition's name.
func (c *Client) lockRow(id uint64) ([]any, string, error) {
	if n, err := c.ch.call("Bank", "lockCount"); err == nil && u(id).Cmp(n[0].(*big.Int)) >= 0 {
		return nil, "", fmt.Errorf("no lock %d (there are %s): check the number with lock show", id, n[0].(*big.Int))
	}
	l, err := c.ch.call("Bank", "locks", u(id))
	if err != nil {
		return nil, "", err
	}
	return l, c.conditionName(l[7].(common.Address)), nil
}

// lockLeft: what is still in a lock.
func (c *Client) lockLeft(id uint64) *big.Int {
	if r, err := c.ch.call("Bank", "remaining", u(id)); err == nil {
		return r[0].(*big.Int)
	}
	return new(big.Int)
}

// runLockDecide: the decider of a lock rules on it, whatever its kind.
func runLockDecide(c *Client, pos []string) error {
	const usage = "lock decide LOCK SHARE-BPS REASON…   (escrow, bond, panel, pay-unless-disputed: the payee's share in basis points)\n" +
		"       lock decide LOCK PAYEE SHARE-BPS REASON…   (bounty: the award names the payee)\n" +
		"       lock decide LOCK SCORE REASON…   (cond: the verifier's score)"
	if err := need(pos, 2, usage); err != nil {
		return err
	}
	id := atoi(pos[0])
	l, cond, err := c.lockRow(id)
	if err != nil {
		return err
	}
	num := func(i int) (int64, error) {
		var n int64
		if _, err := fmt.Sscan(firstOr(pos, i, ""), &n); err != nil {
			return 0, fmt.Errorf("lock decide %d: %q is not a number (%s)", id, firstOr(pos, i, ""), usage)
		}
		return n, nil
	}
	shareBps := func(i int) (int64, error) {
		n, err := num(i)
		if err == nil && (n < 0 || n > 10000) {
			err = fmt.Errorf("lock decide %d: the share is 0..10000 basis points of the lock, to the payee (got %d)", id, n)
		}
		return n, err
	}
	reason := func(i int) string { return strings.Join(pos[min(i, len(pos)):], " ") }
	payee := c.nameOr(common.Hash(l[2].([32]byte))) // the lock's payee (a bounty's is named by its award)
	var uid common.Hash
	r := map[string]any{"lock": id}
	switch cond {
	case "PanelCondition":
		share, err := shareBps(1)
		if err != nil {
			return err
		}
		if uid, err = c.panelRule(id, share, reason(2)); err != nil {
			return err
		}
		r["share_bps"] = share
	case "RulingCondition":
		d, err := decodeRuling(l[8].([]byte))
		if err != nil {
			return err
		}
		if !c.decidesAmong(d.Deciders) {
			return fmt.Errorf("you don't decide lock %d (lock show %d)", id, id)
		}
		share, err := shareBps(1)
		if err != nil {
			return err
		}
		if uid, err = c.rulingDecide(id, share, reason(2)); err != nil {
			return err
		}
		r["share_bps"] = share
	case "OptimisticCondition":
		d, err := decodeOptimistic(l[8].([]byte))
		if err != nil {
			return err
		}
		if !c.decidesAmong(d.Deciders) {
			return fmt.Errorf("you don't decide lock %d: %s do (lock show %d)", id, strings.Join(c.optimisticDeciders(l[8].([]byte)), ", "), id)
		}
		share, err := shareBps(1)
		if err != nil {
			return err
		}
		if uid, err = c.decideOn(id, common.Hash(d.Subject), "ruling", []byte(reason(2)), share, payee, false); err != nil {
			return err
		}
		r["share_bps"] = share
	case "AttestationCondition":
		d, ok := attestationDemandOf(l[8].([]byte))
		if !ok {
			return fmt.Errorf("lock %d: unreadable terms", id)
		}
		if !c.decidesAmong([][32]byte{d.Attester}) {
			return fmt.Errorf("you don't decide lock %d: %s does (lock show %d)", id, c.nameOr(common.Hash(d.Attester)), id)
		}
		subj, kind := common.Hash(d.Subject), knownKind(d.KindHash)
		switch {
		case d.Mode == 2: // bounty: the award names the payee
			if err := need(pos, 3, usage); err != nil {
				return err
			}
			share, err := shareBps(2)
			if err != nil {
				return err
			}
			if uid, err = c.awardOn(id, subj, pos[1], share, reason(3)); err != nil {
				return err
			}
			r["payee"], r["share_bps"] = pos[1], share
		case d.Mode == 1 && (kind == "ruling" || kind == "slash"): // escrow, bond
			share, err := shareBps(1)
			if err != nil {
				return err
			}
			if uid, err = c.decideOn(id, subj, kind, []byte(reason(2)), share, payee, false); err != nil {
				return err
			}
			r["share_bps"] = share
		case d.Mode == 0 && !strings.HasPrefix(kind, "0x"): // cond: a verifier's score
			score, err := num(1)
			if err != nil {
				return err
			}
			if uid, err = c.decideOn(id, subj, kind, []byte(reason(2)), score, payee, false); err != nil {
				return err
			}
			r["score"], r["min_score"] = score, d.MinScore
		default:
			return fmt.Errorf("lock %d waits for a %q claim this client can't name: the verifier attests it with 'attest SUBJECT KIND DATA --score N'", id, kind)
		}
	default:
		return fmt.Errorf("lock %d (%s) has no decider: lock settle %d claims or refunds it by its own rule", id, c.lockKind(cond, l[8].([]byte)), id)
	}
	r["decision"], r["next"] = uid.Hex(), fmt.Sprintf("lock settle %d (anyone)", id)
	out(r)
	return nil
}

// decidesAmong: this identity is one of the deciders (by name).
func (c *Client) decidesAmong(ids [][32]byte) bool {
	for _, x := range ids {
		if common.Hash(x) == idOf(c.f.Name) {
			return true
		}
	}
	return false
}

// decisionsOn: the unrevoked claims of a kind about subject by these
// deciders, made at or after since, newest first. bound: only those made for
// this lock (rulings name their lock, audit 4 MO-6; a verifier's score needn't).
func (c *Client) decisionsOn(lock uint64, bound bool, subject common.Hash, kindHash [32]byte, deciders [][32]byte, since uint64) []common.Hash {
	cls, err := c.easClaims(&subject)
	if err != nil {
		return nil
	}
	by := map[common.Hash]bool{}
	for _, d := range deciders {
		by[common.Hash(d)] = true
	}
	var uids []common.Hash
	for i := len(cls) - 1; i >= 0; i-- {
		cl := cls[i]
		if cl.Revoked || cl.Time < since || crypto.Keccak256Hash([]byte(cl.Kind)) != common.Hash(kindHash) || cl.Issuer == "" || !by[idOf(cl.Issuer)] {
			continue
		}
		if bound && c.ruledFor(cl.UID) != c.rulingTag(lock) {
			continue
		}
		uids = append(uids, cl.UID)
	}
	return uids
}

// rulingNeedsAccept: a lock of this condition is refunded early by a ruling
// only once its payee accepted it (Bank.accepted).
func rulingNeedsAccept(cond string) bool {
	return cond == "OptimisticCondition" || cond == "PanelCondition" || cond == "AttestationCondition" || cond == "RulingCondition"
}

// lockAccepted: the payee accepted lock id (or claimed from it itself).
func (c *Client) lockAccepted(id uint64) bool {
	v, err := c.ch.call("Bank", "accepted", u(id))
	return err == nil && v[0].(bool)
}

// runLockSettle claims or refunds a lock, whichever its condition allows now.
// Without UIDs it finds the decision on chain.
func runLockSettle(c *Client, pos []string) error {
	if err := need(pos, 1, "lock settle LOCK [UID[,UID…]]   (claims or refunds, whichever applies; without UID it finds the decision)"); err != nil {
		return err
	}
	id := atoi(pos[0])
	l, cond, err := c.lockRow(id)
	if err != nil {
		return err
	}
	var uids []common.Hash
	if len(pos) > 1 {
		for _, x := range splitList(pos[1]) {
			uids = append(uids, common.HexToHash(x))
		}
	}
	if c.lockLeft(id).Sign() == 0 {
		return fmt.Errorf("lock %d is settled already: %s paid out, %s refunded (lock show %d)", id, c.fmtMoney(l[4].(*big.Int)), c.fmtMoney(l[5].(*big.Int)), id)
	}
	refundDue := c.chainNowU() > l[6].(uint64) // Bank.refund: after the refund time
	r := map[string]any{"lock": id, "kind": c.lockKind(cond, l[8].([]byte))}
	finish := func(done string) error {
		left := c.lockLeft(id)
		r["done"], r["left"] = done, c.fmtMoney(left)
		if after, err := c.ch.call("Bank", "locks", u(id)); err == nil {
			r["paid_out"], r["refunded"] = c.fmtMoney(after[4].(*big.Int)), c.fmtMoney(after[5].(*big.Int))
			// A ruling paid the payee its share, and the payer's part stays: the
			// payee hasn't accepted the lock, so no ruling refunds early (PROOFREAD-5 F7).
			if left.Sign() > 0 && after[4].(*big.Int).Cmp(l[4].(*big.Int)) > 0 && !refundDue && rulingNeedsAccept(cond) && !c.lockAccepted(id) {
				r["left"] = fmt.Sprintf("%s comes back to the payer at %s, or at once if %s runs lock accept %d", c.fmtMoney(left),
					time.Unix(int64(l[6].(uint64)), 0).UTC().Format(time.RFC3339), c.nameOr(common.Hash(after[2].([32]byte))), id)
			}
		}
		out(r)
		return nil
	}
	refund := func(why string) error {
		if !refundDue {
			return errors.New(why)
		}
		if err := c.refundLock(id); err != nil {
			return err
		}
		return finish("refunded: the refund time has passed")
	}
	switch cond {
	case "RulingCondition": // the rulings named (lock settle LOCK UID,UID…), or none: the undisputed payout after the window
		if err := c.settleRuling(id, uids); err != nil {
			return refund(err.Error())
		}
		return finish("settled by the ruling condition")
	case "OptimisticCondition":
		d, err := decodeOptimistic(l[8].([]byte))
		if err != nil {
			return err
		}
		var at uint64
		if v, err := c.ch.call("OptimisticCondition", "disputedAt", u(id)); err == nil {
			at = v[0].(uint64)
		}
		if at != 0 && len(uids) == 0 { // the rulings that count: by a decider who is neither side, after the dispute
			payer, payee := c.nameOr(common.Hash(l[1].([32]byte))), c.nameOr(common.Hash(l[2].([32]byte)))
			var judges [][32]byte
			for _, x := range d.Deciders {
				if !isParty(c.nameOr(common.Hash(x)), payer, payee) {
					judges = append(judges, x)
				}
			}
			uids = c.decisionsOn(id, true, common.Hash(d.Subject), d.KindHash, judges, at)
			if len(uids) < int(d.K) {
				return refund(fmt.Sprintf("lock %d was disputed: %d of the %d rulings needed so far (the deciders run lock decide %d SHARE-BPS REASON)", id, len(uids), max(d.K, 1), id))
			}
		}
		if at == 0 && c.chainNowU() <= d.WindowEnds {
			return fmt.Errorf("lock %d: its challenge window is still open (lock show %d); after it, lock settle pays the payee", id, id)
		}
		if err := c.settleOptimistic(id, uids); err != nil {
			if at != 0 {
				return refund(err.Error()) // the rulings applied already: what is left goes back after the refund time
			}
			return err
		}
		if at == 0 {
			return finish("paid to the payee: not disputed in the window")
		}
		return finish("settled by the deciders' rulings")
	case "PanelCondition":
		if len(uids) > 0 {
			err = c.settlePanel(id, uids)
		} else {
			err = c.panelSettleAuto(id)
		}
		if err != nil {
			return refund(err.Error())
		}
		return finish("settled by the panel's lower median share")
	case "RevealCondition":
		done, err := c.settleRevealBond(id)
		if err != nil {
			return err
		}
		return finish(done)
	case "Clock":
		done, err := c.settleClockBond(id)
		if err != nil {
			return err
		}
		return finish(done)
	case "RateCondition":
		if err := c.collect(id); err != nil {
			return fmt.Errorf("nothing due yet on subscription %d: %w", id, err)
		}
		return finish("collected what is due to the provider (the payer cancels with sub cancel)")
	case "HashlockCondition":
		return refund(fmt.Sprintf("hash lock %d is open until its timeout: the payee claims with 'htlc claim %d PREIMAGE --expect AMOUNT'; after it, lock settle refunds the payer", id, id))
	case "VoucherCondition":
		return refund(fmt.Sprintf("channel %d is open: the payee claims its latest voucher with 'channel claim'; after the channel's time, lock settle refunds the payer", id))
	case "AttestationCondition":
		d, ok := attestationDemandOf(l[8].([]byte))
		if !ok {
			return fmt.Errorf("lock %d: unreadable terms", id)
		}
		if len(uids) == 0 {
			uids = c.decisionsOn(id, d.Mode != 0, common.Hash(d.Subject), d.KindHash, [][32]byte{d.Attester}, 0)
		}
		if len(uids) == 0 {
			return refund(fmt.Sprintf("lock %d waits for %s's decision (lock decide %d …); after the refund time (lock show %d), lock settle refunds the payer",
				id, c.nameOr(common.Hash(d.Attester)), id, id))
		}
		// The decisions first, newest first, each until one applies; only then,
		// after the refund time, a refund (audit 5 RF-4). Anyone may apply an
		// award or a ruling: the money goes where it says.
		apply := c.settle // escrow, bond, bounty: the share both ways
		if d.Mode == 0 {
			apply = c.claim // cond: paid when the score is high enough
		}
		var used common.Hash
		var last error
		for _, uid := range uids {
			if last = apply(id, uid); last == nil {
				used = uid
				break
			}
		}
		if last != nil {
			return refund(last.Error()) // applied already: what is left goes back after the refund time
		}
		switch d.Mode {
		case 2: // bounty: the award named the payee
			after, err := c.ch.call("Bank", "locks", u(id))
			if err != nil {
				return finish("claimed")
			}
			winner := c.nameOr(common.Hash(after[2].([32]byte)))
			r["paid_to"], r["paid_now"] = winner, c.fmtMoney(new(big.Int).Sub(after[4].(*big.Int), l[4].(*big.Int)))
			if back := new(big.Int).Sub(after[5].(*big.Int), l[5].(*big.Int)); back.Sign() > 0 {
				r["back_to_payer"] = c.fmtMoney(back)
			}
			if ownerName(winner) == ownerName(c.f.Name) {
				r["done"] = "paid to you"
			} else {
				r["done"] = "paid to " + winner + ", whom the award names"
			}
			out(r)
			return nil
		case 1:
			return finish("settled by " + c.nameOr(common.Hash(d.Attester)) + "'s decision " + used.Hex())
		default:
			return finish("paid to the payee: the verifier's score met the minimum")
		}
	}
	// Pledges, item sales and any other condition: claim if it allows, else refund early, else after the refund time.
	if err := c.claimLock(id, nil); err == nil {
		return finish("claimed for the payee")
	}
	if err := c.refundEarly(id, nil); err == nil {
		return finish("refunded to the payer by the condition")
	}
	return refund(fmt.Sprintf("nothing to settle on lock %d yet (lock show %d)", id, id))
}

// deprecated: old recipe commands and what replaces them. They keep working;
// run prints this as a one-line note on stderr (TERNCHAIN_NO_DEPRECATION=1
// silences it).
var deprecated = map[string]string{
	"escrow open": "lock open escrow (with --optimistic: lock open optimistic … --hours H)", "escrow rule": "lock decide LOCK SHARE-BPS REASON",
	"escrow settle": "lock settle LOCK", "escrow dispute": "lock dispute LOCK REASON", "escrow release": "lock release LOCK", "escrow refund": "lock refund LOCK",
	"bond add": "lock open bond", "bond slash": "lock decide LOCK SHARE-BPS REASON", "bond settle": "lock settle LOCK", "bond unbond": "lock refund LOCK",
	"pot open": "lock open bounty (pot is merged into bounty)", "pot award": "lock decide LOCK PAYEE SHARE-BPS REASON", "pot claim": "lock settle LOCK",
	"bounty open": "lock open bounty", "bounty award": "lock decide LOCK PAYEE SHARE-BPS REASON", "bounty claim": "lock settle LOCK",
	"cond open": "lock open cond", "cond claim": "lock settle LOCK (or lock claim LOCK UID)", "cond refund": "lock refund LOCK",
	"sub open": "lock open subscription", "sub collect": "lock settle LOCK",
	"channel open": "lock open channel",
	"htlc open":    "lock open htlc",
	"panel open":   "lock open panel", "panel rule": "lock decide LOCK SHARE-BPS REASON", "panel settle": "lock settle LOCK",
	"revealbond add": "lock open reveal", "revealbond settle": "lock settle LOCK",
	"fund pledge":  "lock open pledge CAMPAIGN AMOUNT",
	"artifact buy": "lock open sale", "artifact confirm": "lock release LOCK", "artifact refund": "lock refund LOCK",
	"commons spend": "commons commission NAME TO AMOUNT MEMO… --plain",
}

// deprecationNote prints the one-line note for an old command name.
func deprecationNote(cmd string) {
	if n, ok := deprecated[cmd]; ok && os.Getenv("TERNCHAIN_NO_DEPRECATION") != "1" {
		fmt.Fprintf(os.Stderr, "note: '%s' is an old name, now '%s'; it keeps working\n", cmd, n)
	}
}
