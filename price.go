// Prices: what things cost, agreed by the network (FeeOracle). The coin
// floats; fees are set in a basket of real things whose price in coins is
// voted each round by bonded commit-reveal, as a weighted median that moves
// at most a step per round, times a congestion multiplier. A vote's salt is
// kept in the identity's state until it is revealed.
package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// PriceVote: a sealed price we committed, to reveal later.
type PriceVote struct {
	Price    string `json:"price"` // wei per basket
	Salt     string `json:"salt"`
	Bond     string `json:"bond"`
	Revealed bool   `json:"revealed,omitempty"`
}

func (c *Client) oracleU64(method string, args ...any) uint64 {
	out, err := c.ch.call("FeeOracle", method, args...)
	if err != nil {
		return 0
	}
	switch v := out[0].(type) {
	case uint64:
		return v
	case uint16:
		return uint64(v)
	case *big.Int:
		return v.Uint64()
	}
	return 0
}

func (c *Client) oracleBig(method string, args ...any) *big.Int {
	out, err := c.ch.call("FeeOracle", method, args...)
	if err != nil {
		return new(big.Int)
	}
	return out[0].(*big.Int)
}

func (c *Client) priceRound() (round uint64, revealing bool, left int64) {
	round = c.oracleU64("round")
	start, length := int64(c.oracleU64("start")), int64(c.oracleU64("roundLength"))
	if length == 0 {
		return round, false, 0
	}
	off := (c.chainNow() - start) % length
	if off >= length/2 {
		return round, true, length - off
	}
	return round, false, length/2 - off
}

// priceShow: the basket price, the multiplier, the round and phase, and what an envelope costs now.
func (c *Client) priceShow() (map[string]any, error) {
	if !c.ch.has("FeeOracle") {
		return nil, errors.New("this network has no FeeOracle (an older devnet): fees are the Inbox's fixed fee")
	}
	round, rev, left := c.priceRound()
	phase := "commit"
	if rev {
		phase = "reveal"
	}
	mult := c.oracleBig("currentMultiplier")
	fee, err := c.ch.fee()
	if err != nil {
		return nil, err
	}
	uri, _ := c.ch.call("FeeOracle", "basketURI")
	spec, _ := c.ch.call("FeeOracle", "basketSpec")
	v := map[string]any{
		"basket_price_coins":    fmtCoins(c.oracleBig("basketPrice")),
		"congestion_multiplier": new(big.Rat).SetFrac(mult, coinUnit).FloatString(4),
		"round":                 round,
		"phase":                 phase,
		"phase_ends_in_s":       left,
		"envelope_fee_coins":    fmtFine(fee),
		"envelope_target_ppm":   c.oracleBig("target", envelopeKind).String(),
		"max_step_bps":          c.oracleU64("maxStepBps"),
		"min_bond_coins":        fmtCoins(c.oracleBig("minBond")),
		"quorum_coins":          fmtCoins(c.oracleBig("quorum")),
		"last_finalized_round":  int64(c.oracleU64("lastFinalized")) - 1,
	}
	if len(uri) > 0 {
		v["basket_uri"] = uri[0].(string)
	}
	if len(spec) > 0 {
		v["basket_spec"] = common.Hash(spec[0].([32]byte)).Hex()
	}
	if votes, err := c.ch.call("FeeOracle", "votesOf", round); err == nil {
		v["votes_this_round"] = reflect.ValueOf(votes[0]).Len()
	}
	if pv := c.st.PriceVotes[strconv.FormatUint(round, 10)]; pv != nil {
		p, _ := new(big.Int).SetString(pv.Price, 10)
		v["my_vote"] = map[string]any{"price_coins": fmtCoins(p), "revealed": pv.Revealed}
	}
	if owed := c.oracleBig("owed", c.me()); owed.Sign() > 0 {
		v["bond_to_withdraw_coins"] = fmtCoins(owed)
	}
	return v, nil
}

// fmtFine: coins to 18 decimals, trailing zeros dropped (fees are tiny).
func fmtFine(w *big.Int) string {
	s := strings.TrimRight(new(big.Rat).SetFrac(w, coinUnit).FloatString(18), "0")
	return strings.TrimSuffix(s, ".")
}

func priceSeal(price *big.Int, salt [32]byte, voter common.Address) common.Hash {
	b, _ := argsOf("uint256", "bytes32", "address").Pack(price, salt, voter)
	return crypto.Keccak256Hash(b)
}

// priceCommit seals a basket price (coin-wei) with a bond, in the commit half of the round.
func (c *Client) priceCommit(price, bond *big.Int) (uint64, error) {
	round, rev, _ := c.priceRound()
	if rev {
		return 0, fmt.Errorf("round %d is in its reveal phase: commit in the next round", round)
	}
	if bond == nil || bond.Sign() == 0 {
		bond = c.oracleBig("minBond")
	}
	var salt [32]byte
	copy(salt[:], randBytes(32))
	if _, err := c.ch.send(c.key, bond, "FeeOracle", "commit", priceSeal(price, salt, c.me())); err != nil {
		return 0, err
	}
	if c.st.PriceVotes == nil {
		c.st.PriceVotes = map[string]*PriceVote{}
	}
	c.st.PriceVotes[strconv.FormatUint(round, 10)] = &PriceVote{Price: price.String(), Salt: hex.EncodeToString(salt[:]), Bond: bond.String()}
	return round, c.save()
}

// priceReveal opens this round's vote; the bond is then ours to withdraw (done here too).
func (c *Client) priceReveal() (uint64, *big.Int, error) {
	round, rev, left := c.priceRound()
	pv := c.st.PriceVotes[strconv.FormatUint(round, 10)]
	switch {
	case pv == nil:
		return round, nil, fmt.Errorf("no vote of yours in round %d", round)
	case pv.Revealed:
		return round, nil, fmt.Errorf("already revealed in round %d", round)
	case !rev:
		return round, nil, fmt.Errorf("round %d reveals in %d s (advance or wait)", round, left)
	}
	price, _ := new(big.Int).SetString(pv.Price, 10)
	var salt [32]byte
	b, _ := hex.DecodeString(pv.Salt)
	copy(salt[:], b)
	if _, err := c.ch.send(c.key, nil, "FeeOracle", "reveal", price, salt); err != nil {
		return round, nil, err
	}
	pv.Revealed = true
	c.save()
	return round, price, c.withdrawBonds()
}

// withdrawBonds takes back revealed or evicted bonds.
func (c *Client) withdrawBonds() error {
	if c.oracleBig("owed", c.me()).Sign() == 0 {
		return nil
	}
	_, err := c.ch.send(c.key, nil, "FeeOracle", "withdraw", c.me())
	return err
}

// priceFinalize settles a finished round (default: the last one) and returns the new price.
func (c *Client) priceFinalize(round *uint64) (map[string]any, error) {
	r := c.oracleU64("round")
	if round == nil {
		if r == 0 {
			return nil, errors.New("round 0 is still running: finalize after it ends (price show)")
		}
		x := r - 1
		round = &x
	}
	before := c.oracleBig("basketPrice")
	rc, err := c.ch.send(c.key, nil, "FeeOracle", "finalize", *round)
	if err != nil {
		return nil, err
	}
	v := map[string]any{"round": *round, "basket_price_before": fmtCoins(before), "basket_price_now": fmtCoins(c.oracleBig("basketPrice"))}
	ev := c.ch.abis["FeeOracle"].Events["Finalized"]
	for _, l := range rc.Logs {
		if len(l.Topics) > 0 && l.Topics[0] == ev.ID {
			if xs, err := ev.Inputs.NonIndexed().Unpack(l.Data); err == nil && len(xs) == 3 {
				v["weighted_median"], v["revealed_bond"] = fmtCoins(xs[0].(*big.Int)), fmtCoins(xs[2].(*big.Int))
				if xs[0].(*big.Int).Sign() == 0 {
					v["note"] = "no move: no reveals, or revealed bonds under the quorum"
				}
			}
		}
	}
	if fee, err := c.ch.fee(); err == nil {
		v["envelope_fee_coins"] = fmtFine(fee)
	}
	return v, nil
}

func runPrice(c *Client, cmd string, pos []string, bondS string) error {
	switch cmd {
	case "price show":
		v, err := c.priceShow()
		if err != nil {
			return err
		}
		out(v)
	case "price commit":
		if err := need(pos, 1, "price commit PRICE --bond B   (PRICE: coins per basket)"); err != nil {
			return err
		}
		price, err := parseCoins(pos[0])
		if err != nil || price.Sign() == 0 {
			return fmt.Errorf("bad price %q", pos[0])
		}
		var bond *big.Int
		if bondS != "" {
			if bond, err = parseCoins(bondS); err != nil {
				return err
			}
		}
		r, err := c.priceCommit(price, bond)
		if err != nil {
			return err
		}
		out(map[string]any{"committed": pos[0], "round": r, "note": "sealed; 'price reveal' in the second half of the round, or lose the bond"})
	case "price reveal":
		r, p, err := c.priceReveal()
		if err != nil {
			return err
		}
		out(map[string]any{"revealed_coins_per_basket": fmtCoins(p), "round": r, "bond": "returned"})
	case "price finalize":
		var round *uint64
		if len(pos) > 0 {
			n, err := strconv.ParseUint(pos[0], 10, 64)
			if err != nil {
				return fmt.Errorf("bad round %q", pos[0])
			}
			round = &n
		}
		v, err := c.priceFinalize(round)
		if err != nil {
			return err
		}
		out(v)
	default:
		return unknownCommand(cmd)
	}
	return nil
}
