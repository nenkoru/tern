// A deal's terms reach its worker. The chain holds only their hash (the
// claim subject, dealSubject); the payer's client sends the text, sealed, to
// the payee and the deciders when it opens the lock, and lock show prints it
// once it matches the subject on chain (PROOFREAD-5 §3: "the worker accepts
// a hash").
package main

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/ethereum/go-ethereum/common"
)

// dealTerms: what the payer sends. Parts are dealSubject's inputs, so the
// receiver recomputes the subject and checks it against the lock.
type dealTerms struct {
	Lock  uint64   `json:"lock"`
	Parts []string `json:"parts"`
	Terms string   `json:"terms"`
}

type dealTermsData struct {
	DealTerms *dealTerms `json:"deal_terms"`
}

// sendTerms: the terms to each of to (the payee and the deciders that are not
// us), best effort: the lock stands either way. What it returns goes into the
// command's output.
func (c *Client) sendTerms(id uint64, parts []string, terms string, to []string) map[string]any {
	data, _ := json.Marshal(dealTermsData{&dealTerms{Lock: id, Parts: parts, Terms: terms}})
	// A refusal here must not become what ask-owner carries (the lock was the
	// action), nor wait for an approval nobody asked for.
	prevBlocked, prevWait := c.st.Blocked, approvalWait
	approvalWait = 0
	defer func() {
		approvalWait = prevWait
		if c.st.Blocked != prevBlocked {
			c.st.Blocked = prevBlocked
			c.save()
		}
	}()
	sent, failed := []string{}, map[string]string{}
	seen := map[string]bool{}
	for _, n := range to {
		if n == "" || seen[n] || n == c.f.Name {
			continue
		}
		seen[n] = true
		_, err := c.send(SendOpts{To: n, Intent: "inform", Subject: fmt.Sprintf("terms of lock %d", id),
			Text: fmt.Sprintf("the terms of lock %d: %s", id, terms), Data: data})
		if err != nil {
			failed[n] = err.Error()
			continue
		}
		sent = append(sent, n)
	}
	r := map[string]any{"terms_sent_to": sent}
	if len(failed) > 0 {
		r["terms_not_sent"] = failed
		r["terms_hint"] = fmt.Sprintf("the lock stands; send the terms yourself (send --to NAME 'the terms of lock %d: …'), word for word: lock show checks them against the hash", id)
	}
	return r
}

// lockSubject: the claim subject a lock's demand names, where its kind has one.
func lockSubject(cond string, demand []byte) (common.Hash, bool) {
	switch cond {
	case "AttestationCondition":
		if d, ok := attestationDemandOf(demand); ok {
			return common.Hash(d.Subject), true
		}
	case "OptimisticCondition":
		if d, err := decodeOptimistic(demand); err == nil {
			return common.Hash(d.Subject), true
		}
	case "PanelCondition":
		if v, err := panelDemandType().Unpack(demand); err == nil && len(v) == 1 {
			return common.Hash(abiConvert[panelDemand](v[0]).Subject), true
		}
	case "RulingCondition":
		if d, err := decodeRuling(demand); err == nil {
			return common.Hash(d.Subject), true
		}
	}
	return common.Hash{}, false
}

// lockTerms: the terms of lock id (its Bank.locks row l) as its payee reads
// them. has: the lock's kind names a subject and the other side wrote it, so
// there are terms to check (lock show's terms_checked); ok: they arrived and
// hash to the subject. Unchecked text is never returned.
func (c *Client) lockTerms(id uint64, l []any) (terms string, subj common.Hash, has, ok bool) {
	from, _ := c.res.nameOf(common.Hash(l[1].([32]byte)))
	payee, _ := c.res.nameOf(common.Hash(l[2].([32]byte)))
	subj, has = lockSubject(c.conditionName(l[7].(common.Address)), l[8].([]byte))
	if !has || from == "" || c.family(from) {
		return "", subj, false, false
	}
	if terms, ok = c.termsFor(id, from, payee, subj); !ok {
		c.scan() // the terms may be in mail not fetched yet
		terms, ok = c.termsFor(id, from, payee, subj)
	}
	return terms, subj, true, ok
}

// termsFor: the terms of lock id from mail its payer sent us, if they hash
// to the subject on chain. Unchecked text is never returned.
func (c *Client) termsFor(id uint64, from, payee string, subj common.Hash) (string, bool) {
	for i := len(c.st.Items) - 1; i >= 0; i-- {
		it := c.st.Items[i]
		if it.Mail == nil || len(it.Mail.Data) == 0 || it.From != from {
			continue
		}
		if t, ok := checkTerms(it.Mail.Data, id, from, payee, subj); ok {
			return t, true
		}
	}
	return "", false
}

// checkTerms: data is a deal_terms message for lock id between from and
// payee, and its parts hash to subj.
func checkTerms(data json.RawMessage, id uint64, from, payee string, subj common.Hash) (string, bool) {
	var d dealTermsData
	if json.Unmarshal(data, &d) != nil || d.DealTerms == nil {
		return "", false
	}
	t := d.DealTerms
	if t.Lock != id || len(t.Parts) < 3 || t.Parts[0] != from || payee != "" && t.Parts[1] != payee ||
		!slices.Contains(t.Parts[2:], t.Terms) || dealSubject(t.Parts...) != subj {
		return "", false
	}
	return t.Terms, true
}

// optimisticEnds: when a pay-unless-disputed lock's challenge window ends (a
// part of its subject).
func (c *Client) optimisticEnds(id uint64) (uint64, bool) {
	l, err := c.ch.call("Bank", "locks", u(id))
	if err != nil {
		return 0, false
	}
	d, err := decodeOptimistic(l[8].([]byte))
	if err != nil {
		return 0, false
	}
	return d.WindowEnds, true
}
