// Pay unless disputed: a lock under OptimisticCondition. The payee is paid
// once the challenge window ends, unless the payer disputed within it; a
// dispute hands the decision to the arbiter (k = 1) or the panel named at
// opening, with PanelCondition's rules (k rulings, the lower median share in
// basis points, the parties' own deciders don't count), counting only rulings
// made after the dispute.
//
//	escrow open WORKER AMOUNT TERMS… --optimistic HOURS --arbiter X   (or --panel a,b,c --k N)
//	escrow dispute LOCK REASON…      the payer, within the window
//	lock claim LOCK                  after the window, no dispute: anyone, for the payee
//	lock decide LOCK BPS REASON      a decider, after a dispute (a ruling names its lock)
//	escrow settle LOCK UID[,UID…]    applies the rulings
package main

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func optimisticDemandType() abi.Arguments {
	t, _ := abi.NewType("tuple", "", []abi.ArgumentMarshaling{
		{Name: "windowEnds", Type: "uint64"}, {Name: "deciders", Type: "bytes32[]"}, {Name: "k", Type: "uint8"},
		{Name: "schema", Type: "bytes32"}, {Name: "subject", Type: "bytes32"}, {Name: "kindHash", Type: "bytes32"}})
	return abi.Arguments{{Type: t}}
}

type optimisticDemand struct {
	WindowEnds uint64
	Deciders   [][32]byte
	K          uint8
	Schema     [32]byte
	Subject    [32]byte
	KindHash   [32]byte
}

func decodeOptimistic(b []byte) (*optimisticDemand, error) {
	v, err := optimisticDemandType().Unpack(b)
	if err != nil || len(v) != 1 {
		return nil, errors.New("bad pay-unless-disputed demand")
	}
	return abiConvert[optimisticDemand](v[0]), nil
}

// openOptimistic locks amount for payee: paid after window seconds unless we
// dispute; then k of deciders decide, until decide seconds after the window
// (the lock's refund time: what is left then comes back to us).
func (c *Client) openOptimistic(payee string, amount *big.Int, window, decide uint64, deciders []string, k uint8, terms string) (uint64, common.Hash, error) {
	if window == 0 {
		return 0, common.Hash{}, errors.New("--optimistic HOURS: the challenge window must be at least an hour")
	}
	if len(deciders) == 0 {
		return 0, common.Hash{}, errors.New("a pay-unless-disputed lock needs who decides a dispute: --arbiter NAME, or --panel a,b,c --k N")
	}
	if k == 0 {
		k = 1
	}
	judges := 0
	for _, d := range deciders {
		if _, err := c.res.nameOf(idOf(d)); err != nil {
			return 0, common.Hash{}, fmt.Errorf("no such name %q to decide disputes", d)
		}
		if !isParty(d, c.f.Name, payee) {
			judges++
		}
	}
	if judges < int(k) {
		return 0, common.Hash{}, fmt.Errorf("k=%d but only %d of the deciders are not a party (the payer's or the payee's side never count): a dispute could never be decided", k, judges)
	}
	if err := c.checkDeciders(payee, deciders); err != nil {
		return 0, common.Hash{}, err
	}
	now := c.chainNowU()
	ends := now + window
	refund := ends + max(decide, 12*3600)
	subj := dealSubject(c.f.Name, payee, "optimistic", terms, fmt.Sprint(ends))
	c.noteDeal(subj, fmt.Sprintf("pay unless disputed: %s pays %s for %q after the window; in a dispute %d of %s decide",
		c.f.Name, payee, terms, k, strings.Join(deciders, ",")))
	ids := make([][32]byte, len(deciders))
	for i, d := range deciders {
		ids[i] = idOf(d)
	}
	d, err := optimisticDemandType().Pack(optimisticDemand{ends, ids, k, c.schema(), subj, crypto.Keccak256Hash([]byte("ruling"))})
	if err != nil {
		return 0, subj, err
	}
	id, err := c.lock(payee, amount, refund, "OptimisticCondition", d)
	return id, subj, err
}

// disputeOptimistic: the payer stops the automatic payment; the deciders decide.
func (c *Client) disputeOptimistic(id uint64, reason string) error {
	l, err := c.ch.call("Bank", "locks", u(id))
	if err != nil {
		return err
	}
	if c.conditionName(l[7].(common.Address)) != "OptimisticCondition" {
		return fmt.Errorf("lock %d is not a pay-unless-disputed lock (escrow dispute is for those; lock show %d)", id, id)
	}
	_, err = c.ch.send(c.key, nil, "OptimisticCondition", "dispute", u(id), reason)
	return err
}

// settleOptimistic: no rulings, a claim after the window; with rulings, applies them both ways.
func (c *Client) settleOptimistic(id uint64, uids []common.Hash) error {
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

func (c *Client) isOptimistic(id uint64) bool {
	l, err := c.ch.call("Bank", "locks", u(id))
	return err == nil && c.conditionName(l[7].(common.Address)) == "OptimisticCondition"
}

// optimisticView: who decides, the window, whether it was disputed, what to do next.
func (c *Client) optimisticView(id uint64, demand []byte) map[string]any {
	d, err := decodeOptimistic(demand)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var names []string
	for _, x := range d.Deciders {
		names = append(names, c.nameOr(common.Hash(x)))
	}
	at := uint64(0)
	if v, err := c.ch.call("OptimisticCondition", "disputedAt", u(id)); err == nil {
		at = v[0].(uint64)
	}
	now := c.chainNowU()
	m := map[string]any{"rule": "paid to the payee after the challenge window unless the payer disputes in time; a dispute goes to the deciders",
		"deciders": names, "k": d.K, "window_ends": time.Unix(int64(d.WindowEnds), 0).UTC().Format(time.RFC3339),
		"about": c.subjectName(common.Hash(d.Subject)), "subject": common.Hash(d.Subject).Hex()}
	switch {
	case at != 0:
		m["disputed_at"] = time.Unix(int64(at), 0).UTC().Format(time.RFC3339)
		m["next"] = fmt.Sprintf("the deciders rule (lock decide %d SHARE-BPS REASON), then anyone: lock settle %d", id, id)
	case now <= d.WindowEnds:
		m["window_left"] = (time.Duration(d.WindowEnds-now) * time.Second).String()
		m["next"] = fmt.Sprintf("the payer may dispute until the window ends (escrow dispute %d REASON); otherwise the payee claims after it (lock claim %d)", id, id)
	default:
		m["next"] = fmt.Sprintf("not disputed, window over: lock claim %d pays the payee (anyone may run it)", id)
	}
	return m
}

// optimisticDeciders: the deciders' names (for the payer's warning and the watch).
func (c *Client) optimisticDeciders(demand []byte) []string {
	d, err := decodeOptimistic(demand)
	if err != nil {
		return nil
	}
	var out []string
	for _, x := range d.Deciders {
		out = append(out, c.nameOr(common.Hash(x)))
	}
	return out
}

// optimisticEvents: disputes in locks we are in, and the end of a window for the payee.
func (c *Client) optimisticEvents(from, now, head uint64, w *WatchState, emit func(Event), once func(string, Event)) error {
	if !c.ch.has("OptimisticCondition") {
		return nil
	}
	evs, err := c.ch.eventsFrom(from, "OptimisticCondition", "Disputed")
	if err != nil {
		return err
	}
	for _, ev := range evs {
		id := ev.Args["lockId"].(*big.Int).String()
		side := w.Locks[id]
		if side == "" {
			continue
		}
		text := "lock " + id + " was disputed by its payer: "
		l, _ := c.ch.call("Bank", "locks", ev.Args["lockId"].(*big.Int))
		payer := ""
		if len(l) > 1 {
			payer = c.nameOr(common.Hash(l[1].([32]byte)))
		}
		text += c.outside(payer, ev.Args["reason"].(string))
		if side == "decider" {
			text += fmt.Sprintf("; you decide: lock decide %s SHARE-BPS REASON", id)
		}
		emit(Event{"disputed", ev.Block, text, map[string]any{"lock": id, "side": side}})
	}
	for id, side := range w.Locks {
		if side != "payee" {
			continue
		}
		l, err := c.ch.call("Bank", "locks", bigID(id))
		if err != nil || c.conditionName(l[7].(common.Address)) != "OptimisticCondition" {
			continue
		}
		d, err := decodeOptimistic(l[8].([]byte))
		if err != nil || now <= d.WindowEnds {
			continue
		}
		if v, err := c.ch.call("OptimisticCondition", "disputedAt", bigID(id)); err != nil || v[0].(uint64) != 0 {
			continue
		}
		if rem, err := c.ch.call("Bank", "remaining", bigID(id)); err != nil || rem[0].(*big.Int).Sign() == 0 {
			continue
		}
		once("optimistic:"+id, Event{"claimable", head, "lock " + id + ": the challenge window is over and nobody disputed: 'lock claim " + id + "'", map[string]any{"lock": id}})
	}
	return nil
}
