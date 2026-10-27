package main

import (
	"errors"
	"fmt"
	"maps"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

type worldFlags struct {
	max       uint64
	soulbound bool
	buyer     string
	payTo     string
	perMove   uint64
	referee   string
	minutes   int
	days      int
	hearing   int
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func runWorld(c *Client, cmd string, pos []string, f worldFlags) error {
	coinsAt := func(i int) (*big.Int, error) { return parseCoins(firstOr(pos, i, "")) }
	deadline := uint64(c.chainNow()) + uint64(max(f.days, 1))*86400
	if f.minutes > 0 {
		deadline = uint64(c.chainNow()) + uint64(f.minutes)*60
	}
	switch cmd {
	// ---- tokens
	case "token mint":
		if err := need(pos, 3, "token mint usd NAME|ADDRESS AMOUNT   (devnet test money)"); err != nil {
			return err
		}
		to := common.HexToAddress(pos[1])
		if !common.IsHexAddress(pos[1]) {
			a, err := c.addressOf(pos[1])
			if err != nil {
				return err
			}
			to = a
		}
		amt, err := coinsAt(2)
		if err != nil {
			return err
		}
		if err := c.mintUSD(to, amt); err != nil {
			return err
		}
		out(map[string]any{"minted": pos[2], "token": "tUSD", "to": to.Hex()})
	case "token deposit":
		if err := need(pos, 2, "token deposit usd AMOUNT   (into your owner's bank)"); err != nil {
			return err
		}
		if err := c.useToken(pos[0]); err != nil {
			return err
		}
		amt, err := coinsAt(1)
		if err != nil {
			return err
		}
		if err := c.depositToken(c.f.Owner, amt); err != nil {
			return err
		}
		out(map[string]any{"deposited": pos[1], "token": pos[0], "owner": c.f.Owner})
	case "token balance":
		if err := need(pos, 1, "token balance usd [OWNER]"); err != nil {
			return err
		}
		if err := c.useToken(pos[0]); err != nil {
			return err
		}
		b, err := c.tokenBalance(firstOr(pos, 1, c.f.Owner))
		if err != nil {
			return err
		}
		out(map[string]any{"owner": firstOr(pos, 1, c.f.Owner), "token": pos[0], "balance": fmtCoins(b)})
	case "token limit":
		if err := need(pos, 3, "token limit usd AGENT PER-EPOCH   (owner only)"); err != nil {
			return err
		}
		if err := c.useToken(pos[0]); err != nil {
			return err
		}
		amt, err := coinsAt(2)
		if err != nil {
			return err
		}
		if err := c.setTokenLimit(pos[1], amt); err != nil {
			return err
		}
		out(map[string]any{"agent": pos[1], "token": pos[0], "per_epoch": pos[2]})
	// ---- splits
	case "split create":
		if err := need(pos, 2, "split create NAME:BPS NAME:BPS…   (shares add up to 10000)"); err != nil {
			return err
		}
		var names []string
		var bps []uint16
		for _, p := range pos {
			n, b, ok := strings.Cut(p, ":")
			v, err := strconv.Atoi(b)
			if !ok || err != nil {
				return fmt.Errorf("bad share %q (want name:bps)", p)
			}
			names, bps = append(names, n), append(bps, uint16(v))
		}
		id, err := c.createSplit(names, bps)
		if err != nil {
			return err
		}
		out(map[string]any{"split": id.Hex(), "pay_it": "bank pay " + id.Hex() + " AMOUNT, or use it as a payee"})
	// ---- items
	case "item create":
		if err := need(pos, 1, "item create URI [--max N] [--soulbound]"); err != nil {
			return err
		}
		id, err := c.createItem(pos[0], f.max, f.soulbound)
		if err != nil {
			return err
		}
		out(map[string]any{"item": id, "uri": pos[0], "max": f.max, "soulbound": f.soulbound})
	case "item mint", "item give":
		if err := need(pos, 3, cmd+" ID NAME AMOUNT"); err != nil {
			return err
		}
		var err error
		if cmd == "item mint" {
			err = c.mintItem(atoi(pos[0]), pos[1], atoi(pos[2]))
		} else {
			err = c.giveItem(atoi(pos[0]), pos[1], atoi(pos[2]))
		}
		if err != nil {
			return err
		}
		out(map[string]any{"item": pos[0], "to": pos[1], "amount": pos[2], "done": strings.TrimPrefix(cmd, "item ")})
	case "item balance":
		if err := need(pos, 1, "item balance ID [NAME]"); err != nil {
			return err
		}
		n, err := c.itemBalance(firstOr(pos, 1, c.f.Name), atoi(pos[0]))
		if err != nil {
			return err
		}
		out(map[string]any{"item": pos[0], "holder": firstOr(pos, 1, c.f.Name), "amount": n})
	case "item show":
		if err := need(pos, 1, "item show ID"); err != nil {
			return err
		}
		o, err := c.ch.call("Items", "classes", bigID(pos[0]))
		if err != nil {
			return err
		}
		out(map[string]any{"item": pos[0], "creator": c.nameOr(common.Hash(o[0].([32]byte))), "max": o[1].(*big.Int).String(),
			"minted": o[2].(*big.Int).String(), "soulbound": o[3].(bool), "uri": o[4].(string)})
	case "item sell":
		if err := need(pos, 3, "item sell ID AMOUNT PRICE [--token usd] [--buyer NAME] [--pay-to SPLIT]"); err != nil {
			return err
		}
		price, err := coinsAt(2)
		if err != nil {
			return err
		}
		id, err := c.offerItem(atoi(pos[0]), atoi(pos[1]), price, f.buyer, f.payTo)
		if err != nil {
			return err
		}
		out(map[string]any{"offer": id, "item": pos[0], "amount": pos[1], "price": pos[2], "buy_with": fmt.Sprintf("item buy %d", id)})
	case "item buy":
		if err := need(pos, 1, "item buy OFFER"); err != nil {
			return err
		}
		lock, err := c.buyItem(atoi(pos[0]))
		if err != nil {
			return err
		}
		out(map[string]any{"offer": pos[0], "lock": lock, "done": "items received, seller paid (one transaction)"})
	case "item cancel":
		if err := need(pos, 1, "item cancel OFFER"); err != nil {
			return err
		}
		if err := c.cancelItemOffer(atoi(pos[0])); err != nil {
			return err
		}
		out(map[string]any{"offer": pos[0], "done": "cancelled; items back"})
	case "item offers":
		list, err := c.itemOffers()
		if err != nil {
			return err
		}
		out(list)
	// ---- randomness
	case "random open":
		if err := need(pos, 1, "random open TOPIC --minutes N   (entries until N min, reveals until 2N min)"); err != nil {
			return err
		}
		m := uint64(max(f.minutes, 1))
		now := uint64(c.chainNow())
		if err := c.openRound(pos[0], now+m*60, now+2*m*60); err != nil {
			return err
		}
		out(map[string]any{"round": pos[0], "commit_by": now + m*60, "reveal_by": now + 2*m*60,
			"players": fmt.Sprintf("commit %s VALUE --minutes %d, then reveal ID", pos[0], 2*m)})
	case "random draw":
		if err := need(pos, 1, "random draw TOPIC"); err != nil {
			return err
		}
		seed, n, err := c.drawRound(pos[0])
		if err != nil {
			return err
		}
		out(map[string]any{"round": pos[0], "seed": seed.Hex(), "entries": n})
	case "random roll":
		if err := need(pos, 2, "random roll TOPIC SIDES [INDEX]"); err != nil {
			return err
		}
		r, err := c.roll(pos[0], atoi(pos[1]), atoi(firstOr(pos, 2, "0")))
		if err != nil {
			return err
		}
		out(map[string]any{"round": pos[0], "sides": pos[1], "index": firstOr(pos, 2, "0"), "result": r})
	// ---- the clock
	case "clock start":
		if err := need(pos, 1, "clock start P1,P2[,…] --per-move SECONDS [--referee NAME]"); err != nil {
			return err
		}
		id, err := c.startGame(splitList(pos[0]), f.referee, max(f.perMove, 10))
		if err != nil {
			return err
		}
		info, _ := c.gameInfo(id)
		out(info)
	case "clock move":
		if err := need(pos, 2, "clock move ID DATA…   (public; commit first for a secret move)"); err != nil {
			return err
		}
		if err := c.clockCall("move", atoi(pos[0]), []byte(strings.Join(pos[1:], " "))); err != nil {
			return err
		}
		info, _ := c.gameInfo(atoi(pos[0]))
		out(info)
	case "clock resign", "clock finish":
		if err := need(pos, 1, cmd+" ID"); err != nil {
			return err
		}
		if err := c.clockCall(strings.TrimPrefix(cmd, "clock "), atoi(pos[0]), c.id()); err != nil {
			return err
		}
		info, _ := c.gameInfo(atoi(pos[0]))
		out(info)
	case "clock timeout":
		if err := need(pos, 1, "clock timeout ID   (anyone, once the player to move is out of time)"); err != nil {
			return err
		}
		if err := c.clockCall("timeout", atoi(pos[0])); err != nil {
			return err
		}
		info, _ := c.gameInfo(atoi(pos[0]))
		out(info)
	case "clock show":
		if err := need(pos, 1, "clock show ID"); err != nil {
			return err
		}
		info, err := c.gameInfo(atoi(pos[0]))
		if err != nil {
			return err
		}
		out(info)
	case "clock bond":
		if err := need(pos, 3, "clock bond GAME PAYEE AMOUNT   (yours to PAYEE if you time out or resign; back otherwise)"); err != nil {
			return err
		}
		amt, err := coinsAt(2)
		if err != nil {
			return err
		}
		id, err := c.clockBond(atoi(pos[0]), pos[1], amt, deadline)
		if err != nil {
			return err
		}
		out(map[string]any{"lock": id, "game": pos[0], "payee_if_you_time_out": pos[1], "coins": pos[2]})
	case "clock settle":
		if err := need(pos, 1, "clock settle LOCK   (after the game is over; anyone)"); err != nil {
			return err
		}
		done, err := c.settleClockBond(atoi(pos[0]))
		if err != nil {
			return err
		}
		out(map[string]any{"lock": pos[0], "done": done})
	// ---- a panel, after a hearing
	case "panel open":
		if err := need(pos, 5, "panel open PAYEE AMOUNT K P1,P2,P3 TERMS… [--hearing-minutes N] [--days N]"); err != nil {
			return err
		}
		amt, err := coinsAt(1)
		if err != nil {
			return err
		}
		hearing := uint64(c.chainNow()) + uint64(max(f.hearing, 1))*60
		terms := strings.Join(pos[4:], " ")
		id, subj, err := c.openPanel(pos[0], amt, deadline, uint8(atoi(pos[2])), splitList(pos[3]), terms, hearing)
		if err != nil {
			return err
		}
		r := map[string]any{}
		if parts := []string{c.f.Name, pos[0], "panel", terms, fmt.Sprint(deadline)}; dealSubject(parts...) == subj {
			r = c.sendTerms(id, parts, terms, append([]string{pos[0]}, splitList(pos[3])...))
		}
		maps.Copy(r, map[string]any{"case": id, "subject": subj.Hex(), "k": pos[2], "panel": pos[3], "hearing_until": hearing,
			"parties":             fmt.Sprintf("panel evidence %d FILE NOTE | panel argue %d TEXT (until the hearing ends)", id, id),
			"panel_after_hearing": fmt.Sprintf("panel case %d, then lock decide %d SHARE-BPS REASON", id, id), "then": fmt.Sprintf("lock settle %d", id)})
		out(r)
	case "panel evidence":
		if err := need(pos, 2, "panel evidence CASE FILE [NOTE…]"); err != nil {
			return err
		}
		art, err := c.panelEvidence(atoi(pos[0]), pos[1], strings.Join(pos[2:], " "))
		if err != nil {
			return err
		}
		out(map[string]any{"case": pos[0], "artifact": art, "done": "sealed; hash and time on chain; key sent to the panel and the other party"})
	case "panel argue":
		if err := need(pos, 2, "panel argue CASE TEXT…"); err != nil {
			return err
		}
		art, err := c.panelArgue(atoi(pos[0]), strings.Join(pos[1:], " "))
		if err != nil {
			return err
		}
		out(map[string]any{"case": pos[0], "artifact": art, "done": "argument sealed to the panel and the other party, timestamped on chain"})
	case "panel case":
		if err := need(pos, 1, "panel case CASE"); err != nil {
			return err
		}
		v, err := c.panelView(atoi(pos[0]))
		if err != nil {
			return err
		}
		out(v)
	case "panel rule":
		if err := need(pos, 3, "panel rule CASE SHARE-BPS REASON…   (after the hearing; share of the stake to the payee)"); err != nil {
			return err
		}
		var bps int64
		fmt.Sscan(pos[1], &bps)
		uid, err := c.panelRule(atoi(pos[0]), bps, strings.Join(pos[2:], " "))
		if err != nil {
			return err
		}
		out(map[string]any{"case": pos[0], "ruling": uid.Hex(), "share_bps": bps, "then": "panel settle " + pos[0] + " (anyone, once k rulings exist)"})
	case "panel settle":
		if err := need(pos, 1, "panel settle CASE [UID,UID…]"); err != nil {
			return err
		}
		var err error
		if len(pos) > 1 {
			var uids []common.Hash
			for _, x := range splitList(pos[1]) {
				uids = append(uids, common.HexToHash(x))
			}
			err = c.settlePanel(atoi(pos[0]), uids)
		} else {
			err = c.panelSettleAuto(atoi(pos[0]))
		}
		if err != nil {
			return err
		}
		out(map[string]any{"case": pos[0], "done": "settled by the panel's lower median share"})
	default:
		return unknownCommand(cmd)
	}
	return nil
}

func (c *Client) useToken(s string) error {
	a, err := c.tokenAddr(s)
	if err != nil {
		return err
	}
	if a == (common.Address{}) {
		return errors.New("--token: name a token, like usd (token balance usd shows yours)")
	}
	c.asset = a
	return nil
}
