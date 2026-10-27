// CLI for privacy: stealth payments and credentials.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nenkoru/tern/internal/zk"
)

func runPrivate(c *Client, cmd string, pos []string, issuers string, days int, po poolSendOpts) error {
	switch cmd {
	case "private deposit":
		if err := need(pos, 1, "private deposit AMOUNT"); err != nil {
			return err
		}
		amt, err := parseCoins(pos[0])
		if err != nil {
			return err
		}
		n, err := c.poolDeposit(amt)
		if err != nil {
			return err
		}
		o := map[string]any{"deposited": fmtCoins(amt), "label": rootHex(n.label()),
			"note": "the chain shows this address deposited this amount; what leaves the pool later isn't linked to it. Round amounts " +
				"(0.1, 1, 10) hide better: an odd amount sent on whole is recognisable. It can be sent once an association set holds it (asp list)"}
		out(o)
	case "private send":
		if err := need(pos, 2, "private send TO AMOUNT [--asp NAME] [--direct]"); err != nil {
			return err
		}
		amt, err := parseCoins(pos[1])
		if err != nil {
			return err
		}
		m, err := c.poolSend(pos[0], amt, po)
		if err != nil {
			return err
		}
		out(m)
	case "private balance":
		m, err := c.poolBalance()
		if err != nil {
			return err
		}
		if ns, err := c.notes(); err == nil {
			total := new(big.Int)
			for _, n := range ns {
				total.Add(total, n.Amount)
			}
			m["stealth_coins"] = fmtCoins(total) + fmt.Sprintf(" at %d one-time addresses (private notes)", len(ns))
		}
		out(m)
	case "private enable":
		if err := c.enableStealth(); err != nil {
			return err
		}
		if err := c.enableRing(); err != nil {
			return err
		}
		out(map[string]any{"private_payments": "on", "ring_key": "published", "note": "grant agents after this, so they can detect your private payments"})
	case "private pay":
		if err := need(pos, 2, "private pay TO AMOUNT [MEMO-HASH]"); err != nil {
			return err
		}
		amt, err := parseCoins(pos[1])
		if err != nil {
			return err
		}
		// One private payment path: from the shielded pool when a note covers it
		// (and a relayer or --direct can submit), else a stealth payment; both guarded like pay.
		if len(pos) == 2 && c.ch.has("PrivacyPool") && (c.relay != "" || po.direct) {
			if w, err := c.syncNotes(); err == nil {
				for _, n := range c.unspent(w) {
					if n.value().Cmp(amt) > 0 {
						m, err := c.poolSend(pos[0], amt, po)
						if err != nil {
							return err
						}
						out(m)
						return nil
					}
				}
			}
		}
		from, _ := c.noteFor(amt)
		memo := common.Hash{}
		if len(pos) > 2 {
			memo = common.HexToHash(pos[2])
		}
		if err := c.payStealth(pos[0], amt, memo, from); err != nil {
			return err
		}
		out(map[string]any{"paid_privately": pos[1], "from_private_coin": from != nil})
	case "private shield":
		if err := need(pos, 1, "private shield AMOUNT"); err != nil {
			return err
		}
		amt, err := parseCoins(pos[0])
		if err != nil {
			return err
		}
		if err := c.shield(amt); err != nil {
			return err
		}
		out(map[string]any{"shielded": pos[0], "note": "the chain saw this name pay some new address; later spending from it is unlinked"})
	case "private notes":
		ns, err := c.notes()
		if err != nil {
			return err
		}
		var rows []map[string]any
		for _, n := range ns {
			rows = append(rows, map[string]any{"address": n.Address.Hex(), "coins": fmtCoins(n.Amount)})
		}
		out(map[string]any{"private_coins": rows})
	case "cred issue":
		if err := need(pos, 2, "cred issue SUBJECT name=value…"); err != nil {
			return err
		}
		claims := map[string]string{}
		for _, kv := range pos[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return errors.New("cred issue: claims look like name=value (like age_over_18=true)")
			}
			claims[k] = v
		}
		h, err := c.issue(pos[0], claims, max(days, 1))
		if err != nil {
			return err
		}
		out(map[string]any{"credential": h.Hex(), "subject": pos[0], "claims": len(claims)})
	case "cred list":
		if _, err := c.scan(); err != nil {
			return err
		}
		var rows []map[string]any
		for i, h := range c.heldCredentials() {
			issuer, names := h.claimsOf()
			for j := range names {
				names[j] = c.claimName(issuer, names[j])
			}
			rows = append(rows, map[string]any{"n": i, "issuer": issuer, "claims": names})
		}
		out(rows)
	case "cred present":
		if err := need(pos, 3, "cred present N CLAIM[,CLAIM] VERIFIER"); err != nil {
			return err
		}
		held := c.heldCredentials()
		n := int(atoi(pos[0]))
		if n >= len(held) {
			return errors.New("no such credential (cred list shows yours, by number)")
		}
		if err := c.presentTo(held[n], strings.Split(pos[1], ","), pos[2]); err != nil {
			return err
		}
		out(map[string]any{"presented": pos[1], "to": pos[2], "note": "sealed to the verifier only; nothing goes on chain"})
	case "cred check":
		if _, err := c.scan(); err != nil {
			return err
		}
		trusted := map[string]bool{}
		for _, x := range strings.Split(issuers, ",") {
			if x = strings.TrimSpace(x); x != "" {
				trusted[x] = true
			}
		}
		var rows []map[string]any
		for _, it := range c.st.Items {
			if it.Type != tPresentation {
				continue
			}
			var p string
			json.Unmarshal(it.Body, &p)
			claims, err := c.verifyPresentation(p, trusted)
			row := map[string]any{"from": it.From}
			if err != nil {
				row["error"] = err.Error()
			} else {
				row["claims"] = claims
			}
			rows = append(rows, row)
		}
		out(rows)
	default:
		return unknownCommand(cmd)
	}
	return nil
}

// runASP: association sets (Privacy Pools).
func runASP(c *Client, cmd string, pos []string, name, rule, labels, uri string) error {
	switch cmd {
	case "asp list":
		sets, err := c.aspSets()
		if err != nil {
			return err
		}
		mine := map[string]bool{}
		if w, err := c.syncNotes(); err == nil {
			for _, n := range c.unspent(w) {
				mine[n.Label] = true
			}
		}
		var rows []map[string]any
		for _, s := range sets {
			holds := 0
			for _, l := range s.Labels {
				if mine[l.String()] {
					holds++
				}
			}
			bt, _ := c.ch.blockTime(s.Updated)
			row := map[string]any{"name": s.Name, "publisher": s.Publisher.Hex(), "root": rootHex(s.Root), "deposits": s.Size,
				"updated": bt, "consistent": s.OK, "your_notes_in_it": holds}
			if s.URI != "" {
				row["uri"] = s.URI
			}
			if len(s.Labels) != s.Size {
				row["labels"] = len(s.Labels)
			}
			if !s.OK {
				row["warning"] = s.Problem + ": clients won't use it"
			}
			rows = append(rows, row)
		}
		out(map[string]any{"association_sets": rows, "note": "pick one you trust with private send --asp NAME@PUBLISHER (NAME alone: yours or your relayer's); deposits counts distinct real deposits: a bigger set hides you among more"})
	case "asp publish":
		if name == "" {
			return errors.New("asp publish [ROOT] --name NAME --rule all|clean24h | --labels L1,L2|FILE")
		}
		var ls []*big.Int
		var err error
		switch {
		case rule != "":
			ls, err = c.aspRule(rule)
		case labels != "":
			ls, err = parseLabels(labels)
		default:
			return errors.New("--rule all|clean24h, or --labels L1,L2|FILE")
		}
		if err != nil {
			return err
		}
		if len(pos) > 0 && len(ls) > 0 {
			want, ok := new(big.Int).SetString(strings.TrimPrefix(pos[0], "0x"), 16)
			if got := zk.NewTree(zk.ASPDepth, ls).Root(); !ok || got.Cmp(want) != 0 {
				return fmt.Errorf("those labels give root %s, not %s", rootHex(got), pos[0])
			}
		}
		m, err := c.aspPublish(name, ls, uri)
		if err != nil {
			return err
		}
		out(m)
	case "asp flag":
		if err := need(pos, 2, "asp flag LABEL REASON…"); err != nil {
			return err
		}
		l, ok := new(big.Int).SetString(strings.TrimPrefix(pos[0], "0x"), 16)
		if !ok {
			return errors.New("LABEL: the deposit's label, 0x… (asp list, private balance, or the Deposited event)")
		}
		if !c.isCitizen(ownerName(c.f.Name)) {
			return errors.New("only citizens' flags count for clean24h sets (anyone's claims count for sets that choose to read them)")
		}
		uid, err := c.easAttest(common.BigToHash(l), "pool.flag", []byte(strings.Join(pos[1:], " ")), -100, 0, common.Hash{})
		if err != nil {
			return err
		}
		out(map[string]any{"flagged": rootHex(l), "claim": uid.Hex(), "note": "sets by the clean24h rule drop it at their next publish"})
	default:
		return unknownCommand(cmd)
	}
	return nil
}

func parseLabels(s string) ([]*big.Int, error) {
	if b, err := os.ReadFile(s); err == nil {
		s = string(b)
	}
	var out []*big.Int
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' || r == '\t' }) {
		v, ok := new(big.Int).SetString(strings.TrimPrefix(f, "0x"), 16)
		if !ok || v.Cmp(zk.Modulus) >= 0 {
			return nil, fmt.Errorf("label %q: 0x… below the field size", f)
		}
		out = append(out, v)
	}
	return out, nil
}
