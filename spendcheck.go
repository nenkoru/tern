// Before the guard asks an owner to approve a payment, say when it can't
// pass anyway (the agent's limit for the epoch is spent), and name the
// free-pay day cap when that is what stopped a small payment (PROOFREAD-5 §3).
package main

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// spendCheck: an agent's payment (or lock) of amount, against what is left of
// its limit this epoch. An approval doesn't raise the limit, so asking the
// owner for a payment over it only wastes the owner's time.
func (c *Client) spendCheck(amount *big.Int) error {
	if c.isOwner() || c.asset != (common.Address{}) || amount == nil {
		return nil
	}
	lim, err := c.ch.call("Registry", "spendLimit", c.id())
	if err != nil {
		return nil // the chain decides
	}
	e, err := c.ch.epoch()
	if err != nil {
		return nil
	}
	sp, err := c.ch.call("Bank", "spent", c.id(), e)
	if err != nil {
		return nil
	}
	limit, spent := lim[0].(*big.Int), sp[0].(*big.Int)
	left := new(big.Int).Sub(limit, spent)
	if left.Sign() < 0 {
		left.SetInt64(0)
	}
	if amount.Cmp(left) <= 0 {
		return nil
	}
	return fmt.Errorf("over the agent's limit: %s coins, but only %s are left this epoch (limit %s, spent %s; %s). "+
		"An owner's approval doesn't raise the limit, so this can't pass: your owner raises it with a new grant (owner grant … --spend N), or pays or locks it itself",
		fmtCoins(amount), fmtCoins(left), fmtCoins(limit), fmtCoins(spent), c.epochWords())
}

// freeCapNote: when a payment within free_pay was refused for want of an
// approval because the day's free budget (free_pay_day) is used up, say so
// first: the agent and its owner otherwise read a plain taint refusal.
func (c *Client) freeCapNote(err error, amount *big.Int) error {
	if err == nil || amount == nil || c.isOwner() || !strings.HasPrefix(err.Error(), "blocked:") {
		return err
	}
	s, serr := c.scopes()
	if serr != nil || s.FreePay == "" {
		return err
	}
	free, perr := parseCoins(s.FreePay)
	if perr != nil || amount.Cmp(free) > 0 {
		return err // above free_pay: the plain refusal is right
	}
	day := new(big.Int).Mul(free, big.NewInt(5))
	if d, derr := parseCoins(s.FreePayDay); derr == nil && s.FreePayDay != "" {
		day = d
	}
	used := freeUsed(c.st.Free, c.chainNow(), s.knob("free_pay_window"))
	if new(big.Int).Add(used, amount).Cmp(day) <= 0 {
		return err
	}
	return fmt.Errorf("free payments today: %s of %s (free_pay_day; a payment up to free_pay %s is free until the day's total would pass it), "+
		"so this %s needs your owner's approval. %w", fmtCoins(used), fmtCoins(day), fmtCoins(free), fmtCoins(amount), err)
}

// freeUsed: what free payments in the last window (24 hours by default:
// scopes.free_pay_window) add up to.
func freeUsed(fs []FreeRec, now, window int64) *big.Int {
	sum := new(big.Int)
	for _, f := range fs {
		if now-f.Time < window {
			if v, ok := new(big.Int).SetString(f.Amount, 10); ok {
				sum.Add(sum, v)
			}
		}
	}
	return sum
}

// ownerDirWords: the owner's folder as an agent may be told it: by the
// owner's name, never as a path. The agent must not learn where its owner's
// keys live (START: "the agent never reads the owner's folder"); the owner
// knows its own folder.
func (c *Client) ownerDirWords() string {
	return "<" + c.f.Owner + "'s folder>"
}
