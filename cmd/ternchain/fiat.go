// Payments made outside the network (a bank transfer, cash, a card) and
// confirmed on it. Not a new primitive: attestations that point at each
// other. Roles:
//
//	payee  fiat request PAYER AMOUNT CURRENCY [METHOD] [REF] [MEMO]   (optional: "you owe me X, ref Z")
//	payer  fiat paid PAYEE AMOUNT CURRENCY METHOD [REF] [MEMO]        "I paid you X by Y, ref Z"
//	payee  fiat confirm|deny CLAIM-UID                               the money arrived, or not
//
// The payer claims "I paid you X by Y, ref Z"; the payee confirms or
// denies it. Anyone can see claimed / confirmed / denied, which builds
// reputation; an arbiter rules disputes. A lock can be released by the
// payee's confirmation (cond open … KIND fiat:received MIN 1): coins for
// a bank transfer, a deposit paid in cash.
package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

type FiatPayment struct {
	To       string `json:"to"`
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
	Method   string `json:"method"`
	Ref      string `json:"ref,omitempty"`
	Memo     string `json:"memo,omitempty"`
	Deal     string `json:"deal,omitempty"` // a deal subject (0x…): the confirmation is about this deal only
}

// fiatPaid: the payer's claim, about the payee.
func (c *Client) fiatPaid(p FiatPayment) (common.Hash, error) {
	if _, err := c.res.nameOf(idOf(p.To)); err != nil {
		return common.Hash{}, fmt.Errorf("no such payee %q", p.To)
	}
	b, _ := json.Marshal(p)
	return c.easAttest(subjectID(p.To), "fiat:paid", b, 0, 0, common.Hash{})
}

// fiatRequest: the payee's side: "p.To owes me this" (an invoice on chain,
// about the payer). The payer answers with fiat paid, citing the same REF.
func (c *Client) fiatRequest(p FiatPayment) (common.Hash, error) {
	if _, err := c.res.nameOf(idOf(p.To)); err != nil {
		return common.Hash{}, fmt.Errorf("no such payer %q", p.To)
	}
	b, _ := json.Marshal(p)
	return c.easAttest(subjectID(p.To), "fiat:request", b, 0, 0, common.Hash{})
}

// fiatAnswer: the payee confirms (true) or denies (false) a claim made to it.
func (c *Client) fiatAnswer(uid common.Hash, received bool, note string) (common.Hash, error) {
	cl, err := c.easGet(uid, 0)
	if err != nil {
		return common.Hash{}, err
	}
	if cl.Kind != "fiat:paid" {
		return common.Hash{}, errors.New("not a fiat payment claim")
	}
	var p FiatPayment
	json.Unmarshal([]byte(cl.Data), &p)
	if p.To != c.f.Name && p.To != c.f.Owner {
		return common.Hash{}, fmt.Errorf("that payment was claimed to %s, not to you", p.To)
	}
	payer := c.res.nameAt(cl.Attester, 0)
	if payer == "" {
		return common.Hash{}, errors.New("unknown payer")
	}
	score := int64(1)
	if !received {
		score = -1
	}
	b, _ := json.Marshal(map[string]string{"amount": p.Amount, "currency": p.Currency, "ref": p.Ref, "note": note})
	// Bound to the deal when the payment names one, so a confirmation releases
	// only the lock waiting for that deal, not every lock about this payer.
	subj := subjectID(payer)
	if p.Deal != "" {
		subj = subjectID(p.Deal) // a 0x… subject as is; any other text is hashed, never mangled
	}
	return c.easAttest(subj, "fiat:received", b, score, 0, uid)
}

// fiatList: claims made by or to name, each with its status.
func (c *Client) fiatList(name string) ([]map[string]any, error) {
	all, err := c.easClaims(nil)
	if err != nil {
		return nil, err
	}
	answers := map[common.Hash]*Claim2{}
	for _, cl := range all {
		if cl.Kind == "fiat:received" && !cl.Revoked {
			answers[cl.RefUID] = cl
		}
	}
	out := []map[string]any{}
	var paidClaims []*Claim2
	for _, cl := range all {
		if cl.Kind == "fiat:paid" && !cl.Revoked {
			paidClaims = append(paidClaims, cl)
		}
	}
	// Requests (the payee's side), with the payer's claim that answers each (same ref, or amount and currency).
	for _, cl := range all {
		if cl.Kind != "fiat:request" || cl.Revoked {
			continue
		}
		var r FiatPayment
		json.Unmarshal([]byte(cl.Data), &r)
		if name != "" && cl.Issuer != name && ownerName(cl.Issuer) != name && r.To != name && ownerName(r.To) != name {
			continue
		}
		status := "requested: " + r.To + " pays outside the network, then runs 'fiat paid " + cl.Issuer + " " + r.Amount + " " + r.Currency + " METHOD " + r.Ref + "'"
		for _, pc := range paidClaims {
			var p FiatPayment
			json.Unmarshal([]byte(pc.Data), &p)
			if ownerName(pc.Issuer) == ownerName(r.To) && ownerName(p.To) == ownerName(cl.Issuer) && (r.Ref != "" && p.Ref == r.Ref || r.Ref == "" && p.Amount == r.Amount && p.Currency == r.Currency) {
				status = "answered: " + pc.Issuer + " claims it paid (claim " + pc.UID.Hex() + "); " + cl.Issuer + " confirms with 'fiat confirm " + pc.UID.Hex() + "'"
			}
		}
		out = append(out, map[string]any{"uid": cl.UID.Hex(), "kind": "request", "from": r.To, "to": cl.Issuer, "amount": r.Amount, "currency": r.Currency,
			"method": r.Method, "ref": r.Ref, "memo": c.outside(cl.Issuer, r.Memo), "status": status, "block": cl.Block})
	}
	for _, cl := range paidClaims {
		var p FiatPayment
		json.Unmarshal([]byte(cl.Data), &p)
		if name != "" && cl.Issuer != name && ownerName(cl.Issuer) != name && p.To != name && ownerName(p.To) != name {
			continue
		}
		status := "claimed, not confirmed yet"
		if a, ok := answers[cl.UID]; ok {
			// Only the payee (or its family) can confirm.
			if ok2 := c.res.nameAt(a.Attester, a.Block); ok2 == p.To || ownerName(ok2) == ownerName(p.To) {
				if a.Score > 0 {
					status = "confirmed by " + ok2
				} else {
					status = "denied by " + ok2
				}
			}
		}
		o := map[string]any{"uid": cl.UID.Hex(), "kind": "payment", "from": cl.Issuer, "to": p.To, "amount": p.Amount,
			"currency": p.Currency, "method": p.Method, "ref": p.Ref, "status": status, "block": cl.Block}
		if p.Deal != "" {
			o["deal"] = subjectID(p.Deal).Hex() // the lock this confirmation can release (its subject)
		} else {
			o["deal"] = "none: a confirmation would release ANY lock waiting on this payer's fiat"
		}
		out = append(out, o)
	}
	return out, nil
}
