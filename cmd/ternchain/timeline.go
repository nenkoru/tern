// timeline: what happened on chain between two times, in plain words, with
// the gas each transaction used. For replays and for people who want to see
// what their agents did.
package main

import (
	"fmt"
	"math/big"
	"strings"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type TxEvent struct {
	T     int64  `json:"t"` // unix ms (block time)
	Block uint64 `json:"block"`
	Label string `json:"label"`
	Gas   uint64 `json:"gas"`
	Cost  string `json:"cost"`
}

func (c *Client) timeline(fromMs, toMs int64) ([]TxEvent, error) {
	head, err := c.ch.b.BlockNumber(ctxb())
	if err != nil {
		return nil, err
	}
	addrs := map[common.Address]string{}
	for n, a := range c.ch.dep.Contracts {
		addrs[common.HexToAddress(a)] = n
	}
	// The block range covering [fromMs, toMs], walking headers back from the head.
	var b0, b1 uint64
	for b := head; b > c.ch.dep.Block; b-- {
		h, err := c.ch.b.HeaderByNumber(ctxb(), new(big.Int).SetUint64(b))
		if err != nil {
			break
		}
		t := int64(h.Time) * 1000
		if t > toMs {
			continue
		}
		if b1 == 0 {
			b1 = b
		}
		if t < fromMs {
			break
		}
		b0 = b
	}
	var out []TxEvent
	if b1 == 0 || b0 == 0 {
		return out, nil
	}
	var list []common.Address
	for a := range addrs {
		list = append(list, a)
	}
	logs, err := c.ch.b.FilterLogs(ctxb(), ethereum.FilterQuery{FromBlock: new(big.Int).SetUint64(b0), ToBlock: new(big.Int).SetUint64(b1), Addresses: list})
	if err != nil {
		return nil, err
	}
	done := map[common.Hash]bool{}
	for _, l := range logs {
		if done[l.TxHash] {
			continue
		}
		done[l.TxHash] = true
		r, err := c.ch.b.TransactionReceipt(ctxb(), l.TxHash)
		if err != nil {
			continue
		}
		label := c.describe(r, addrs)
		if label == "" {
			continue
		}
		h, _ := c.ch.b.HeaderByNumber(ctxb(), new(big.Int).SetUint64(l.BlockNumber))
		out = append(out, TxEvent{T: int64(h.Time) * 1000, Block: l.BlockNumber, Label: label, Gas: r.GasUsed,
			Cost: fmt.Sprintf("газ %s", groupDigits(r.GasUsed))})
	}
	return out, nil
}

func groupDigits(n uint64) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteRune(' ')
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// describe names what a transaction did, from its events.
func (c *Client) describe(r *types.Receipt, addrs map[common.Address]string) string {
	var parts []string
	seen := map[string]bool{}
	name := func(h any) string {
		if b, ok := h.([32]byte); ok {
			return c.nameOr(common.Hash(b))
		}
		return ""
	}
	for _, l := range r.Logs {
		cn, ok := addrs[l.Address]
		if !ok || len(l.Topics) == 0 {
			continue
		}
		ab := c.ch.abis[cn]
		for en, ev := range ab.Events {
			if ev.ID != l.Topics[0] {
				continue
			}
			m := map[string]any{}
			c.ch.c[cn].UnpackLogIntoMap(m, en, *l)
			var s string
			switch cn + "." + en {
			case "Inbox.Posted":
				s = fmt.Sprintf("Inbox: %v sealed envelope(s), in full", m["n"])
			case "Inbox.Committed":
				s = fmt.Sprintf("Inbox: a batch of %v envelope(s), only its hash on chain", m["n"])
			case "Bank.Paid":
				s = fmt.Sprintf("Bank: %s paid %s %s", name(m["from"]), name(m["to"]), fmtCoins(m["amount"].(*big.Int)))
			case "Bank.Locked":
				to := "an open payee"
				if p := name(m["payee"]); p != "" && p != "0x00000000" {
					to = p
				}
				s = fmt.Sprintf("Bank: %s locked %s for %s", name(m["from"]), fmtCoins(m["total"].(*big.Int)), to)
			case "Bank.Accepted":
				s = fmt.Sprintf("Bank: %s accepted lock %v", name(m["payee"]), m["id"])
			case "Bank.Released", "Bank.Claimed":
				s = fmt.Sprintf("Bank: lock %v paid out %s", m["id"], fmtCoins(m["amount"].(*big.Int)))
			case "Bank.Refunded", "Bank.GivenBack":
				s = fmt.Sprintf("Bank: lock %v returned %s", m["id"], fmtCoins(m["amount"].(*big.Int)))
			case "Bank.Deposited":
				s = fmt.Sprintf("Bank: %s deposited %s", name(m["owner"]), fmtCoins(m["amount"].(*big.Int)))
			case "EAS.Attested":
				s = "EAS: a signed claim (review, ruling, evidence…)"
			case "Sets.Created":
				s = fmt.Sprintf("Sets: group %v created", m["name"])
			case "Sets.Added":
				s = "Sets: a member joined a group"
			case "Registry.OwnerRegistered":
				s = fmt.Sprintf("Registry: name %v registered", m["name"])
			case "Registry.AgentGranted":
				s = "Registry: an agent granted (limits on chain)"
			case "Artifacts.Registered":
				s = "Artifacts: a sealed file registered (hash only)"
			case "Commitments.Committed":
				s = "Commitments: a sealed commitment"
			case "Commitments.Revealed":
				s = "Commitments: a commitment revealed"
			case "TernPaymaster.Charged":
				s = "Paymaster: gas paid from the owner's deposit"
			}
			if s != "" && !seen[s] {
				seen[s] = true
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "; ")
}
