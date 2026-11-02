// Recipes for worlds: tokens next to the coin, splits, items, fair
// randomness, a clock for turns, and decisions by a panel.
package main

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// payeeID: a name, or a split id (0x…64 hex).
func payeeID(s string) common.Hash {
	if strings.HasPrefix(s, "0x") && len(s) == 66 {
		return common.HexToHash(s)
	}
	return idOf(s)
}

// addressOf: the address that holds a name's items (an owner's key or an agent's).
func (c *Client) addressOf(name string) (common.Address, error) {
	id := idOf(name)
	if o, err := c.ch.call("Registry", "owners", id); err == nil && o[0].(common.Address) != (common.Address{}) {
		return o[0].(common.Address), nil
	}
	if g, err := c.ch.call("Registry", "grants", id); err == nil && g[1].(common.Address) != (common.Address{}) {
		return g[1].(common.Address), nil
	}
	return common.Address{}, fmt.Errorf("no such name %q", name)
}

// tokenAddr: "usd" is a devnet's test stablecoin (devnets only: no other
// network has TestUSD, so the alias is refused there); otherwise an address.
func (c *Client) tokenAddr(s string) (common.Address, error) {
	switch strings.ToLower(s) {
	case "coin", "coins", "eth":
		return common.Address{}, nil
	case "":
		return c.ch.settleToken(), nil
	case "usdc":
		if t := c.ch.settleToken(); t != (common.Address{}) {
			return t, nil
		}
		return common.Address{}, errors.New("no USDC on this network: give the token's address")
	case "usd", "tusd":
		if t := c.ch.settleToken(); t != (common.Address{}) { // the network's stablecoin
			return t, nil
		}
		a, ok := c.ch.dep.Contracts["TestUSD"]
		if !ok {
			return common.Address{}, errors.New("no test stablecoin on this network (TestUSD is devnet-only): give the token's address")
		}
		return common.HexToAddress(a), nil
	}
	if !common.IsHexAddress(s) {
		return common.Address{}, fmt.Errorf("unknown token %q", s)
	}
	return common.HexToAddress(s), nil
}

// ---- tokens (only the devnet's TestUSD is wired in the CLI)

func (c *Client) mintUSD(to common.Address, amount *big.Int) error {
	if !c.ch.has("TestUSD") {
		return errors.New("no test stablecoin on this network (TestUSD is devnet-only)")
	}
	_, err := c.ch.send(c.key, nil, "TestUSD", "mint", to, amount)
	return err
}

func (c *Client) depositToken(owner string, amount *big.Int) error {
	if err := c.ch.capsCheck("Bank", "depositToken", nil, []any{idOf(owner), c.asset, amount}); err != nil { // before the approval
		return err
	}
	bank := common.HexToAddress(c.ch.dep.Contracts["Bank"])
	// approve on the token itself: any ERC-20, not only TestUSD (docs/BUILD.md: a world's own currency)
	erc20, err := erc20ABI()
	if err != nil {
		return err
	}
	data, err := erc20.Pack("approve", bank, amount)
	if err != nil {
		return err
	}
	if _, err := c.ch.sendRaw(c.key, nil, &c.asset, data); err != nil {
		return fmt.Errorf("approving the Bank on token %s: %w", c.asset.Hex(), err)
	}
	_, err = c.ch.send(c.key, nil, "Bank", "depositToken", idOf(owner), c.asset, amount)
	return err
}

func (c *Client) tokenBalance(owner string) (*big.Int, error) {
	out, err := c.ch.call("Bank", "tokenBalance", idOf(owner), c.asset)
	if err != nil {
		return nil, err
	}
	return out[0].(*big.Int), nil
}

func (c *Client) setTokenLimit(agent string, perEpoch *big.Int) error {
	_, err := c.ch.send(c.key, nil, "Bank", "setTokenLimit", idOf(agent), c.asset, perEpoch)
	return err
}

// ---- splits: a payee that shares what it receives

func (c *Client) createSplit(payees []string, bps []uint16) (common.Hash, error) {
	ids := make([][32]byte, len(payees))
	for i, p := range payees {
		ids[i] = idOf(p)
	}
	r, err := c.ch.send(c.key, nil, "Bank", "createSplit", ids, bps)
	if err != nil {
		return common.Hash{}, err
	}
	ev := c.ch.abis["Bank"].Events["SplitCreated"]
	for _, l := range r.Logs {
		if len(l.Topics) > 1 && l.Topics[0] == ev.ID {
			return l.Topics[1], nil
		}
	}
	// It existed already: the id is deterministic.
	b, _ := abi.Arguments{{Type: abiType("string")}, {Type: abiType("bytes32[]")}, {Type: abiType("uint16[]")}}.Pack("tern-split", ids, bps)
	return crypto.Keccak256Hash(b), nil
}

// ---- items (ERC-1155)

func (c *Client) createItem(uri string, maxSupply uint64, soulbound bool) (uint64, error) {
	r, err := c.ch.send(c.key, nil, "Items", "create", c.id(), uri, u(maxSupply), soulbound)
	if err != nil {
		return 0, err
	}
	return c.firstIndexed(r, "Items", "ClassCreated")
}

func (c *Client) mintItem(id uint64, to string, amount uint64) error {
	a, err := c.addressOf(to)
	if err != nil {
		return err
	}
	_, err = c.ch.send(c.key, nil, "Items", "mint", u(id), a, u(amount))
	return err
}

func (c *Client) itemBalance(name string, id uint64) (uint64, error) {
	a, err := c.addressOf(name)
	if err != nil {
		return 0, err
	}
	out, err := c.ch.call("Items", "balanceOf", a, u(id))
	if err != nil {
		return 0, err
	}
	return out[0].(*big.Int).Uint64(), nil
}

func (c *Client) giveItem(id uint64, to string, amount uint64) error {
	if err := c.guardItems(to, "", fmt.Sprintf("item:give:%d:%d:%s", id, amount, to), fmt.Sprintf("giving %d of item %d to %s", amount, id, to)); err != nil {
		return err
	}
	a, err := c.addressOf(to)
	if err != nil {
		return err
	}
	_, err = c.ch.send(c.key, nil, "Items", "safeTransferFrom", c.me(), a, u(id), u(amount), []byte{})
	return err
}

// swapWant is SwapCondition.Want: what an offer lock wants back.
type swapWant struct {
	Token  common.Address
	Item   *big.Int // item id + 1 when it wants an item; 0: coins or tokens
	Amount *big.Int
	PayTo  [32]byte
}

func swapWantType() abi.Arguments {
	t, _ := abi.NewType("tuple", "", []abi.ArgumentMarshaling{{Name: "token", Type: "address"}, {Name: "item", Type: "uint256"},
		{Name: "amount", Type: "uint256"}, {Name: "payTo", Type: "bytes32"}})
	return abi.Arguments{{Type: t}}
}

// itemsInBank: puts amount of item id from our address into our Bank item balance.
func (c *Client) itemsInBank(token common.Address, id, amount uint64) error {
	bank := common.HexToAddress(c.ch.dep.Contracts["Bank"])
	if ok, err := c.ch.call("Items", "isApprovedForAll", c.me(), bank); err != nil || !ok[0].(bool) {
		data, _ := c.ch.abis["Items"].Pack("setApprovalForAll", bank, true)
		if _, err := c.ch.sendRaw(c.key, nil, &token, data); err != nil {
			return fmt.Errorf("approving the Bank on the items: %w", err)
		}
	}
	_, err := c.ch.send(c.key, nil, "Bank", "depositItem", c.id(), token, u(id), u(amount))
	return err
}

// offerItem puts items up for sale: they go into our Bank item balance and
// into an item lock under SwapCondition that wants the price (in c.asset)
// paid to payTo. The offer's id is that lock's. Atomic: the buyer's price
// lock and this one swap in one transaction (SwapCondition.settle).
func (c *Client) offerItem(id, amount uint64, price *big.Int, buyer, payTo string) (uint64, error) {
	items := common.HexToAddress(c.ch.dep.Contracts["Items"])
	var b common.Hash
	if buyer != "" {
		b = idOf(buyer)
	}
	p := c.id()
	if payTo != "" {
		p = payeeID(payTo)
	}
	if out, err := c.ch.call("Bank", "isPayee", p); err != nil || !out[0].(bool) {
		return 0, errors.New("pay-to must be a name or a split")
	}
	if err := c.guardItems(buyer, payTo, fmt.Sprintf("item:sell:%d:%d:%s:%s", id, amount, fmtCoins(price), buyer),
		fmt.Sprintf("offering %d of item %d for %s coins (buyer %s)", amount, id, fmtCoins(price), firstNonEmpty(buyer, "anyone"))); err != nil {
		return 0, err
	}
	if err := c.itemsInBank(items, id, amount); err != nil {
		return 0, err
	}
	d, err := swapWantType().Pack(swapWant{c.asset, new(big.Int), price, p})
	if err != nil {
		return 0, err
	}
	cond := common.HexToAddress(c.ch.dep.Contracts["SwapCondition"])
	r, err := c.ch.send(c.key, nil, "Bank", "lockItem", c.id(), b, items, u(id), u(amount), c.chainNowU()+30*86400, cond, d)
	if err != nil {
		return 0, c.limitErr(err)
	}
	return c.firstIndexed(r, "Bank", "Locked")
}

// itemOffer: an open item offer (an unswapped item lock under SwapCondition).
func (c *Client) itemOffer(offer uint64) (l []any, w swapWant, item uint64, err error) {
	if l, err = c.ch.call("Bank", "locks", u(offer)); err != nil {
		return
	}
	io, err := c.ch.call("Bank", "itemOf", u(offer))
	if err != nil {
		return
	}
	m, _ := c.ch.call("SwapCondition", "matched", u(offer))
	rem, _ := c.ch.call("Bank", "remaining", u(offer))
	if c.conditionName(l[7].(common.Address)) != "SwapCondition" || io[0].(*big.Int).Sign() == 0 || len(m) == 0 || m[0].(*big.Int).Sign() != 0 ||
		len(rem) == 0 || rem[0].(*big.Int).Sign() == 0 {
		err = errors.New("that offer is not open (item offers lists the open ones)")
		return
	}
	v, e := swapWantType().Unpack(l[8].([]byte))
	if e != nil || len(v) != 1 {
		err = errors.New("not an item offer")
		return
	}
	w = *abiConvert[swapWant](v[0])
	item = io[0].(*big.Int).Uint64() - 1
	return
}

// buyItem locks the price to the offer's pay-to and swaps it for the items
// (SwapCondition.settle: both locks pay out in one transaction), then takes
// the items out of the Bank to our own address.
func (c *Client) buyItem(offer uint64) (uint64, error) {
	l, w, item, err := c.itemOffer(offer)
	if err != nil {
		return 0, err
	}
	if w.Item.Sign() != 0 {
		return 0, errors.New("that offer wants an item, not coins or tokens: answer it with a lock of that item")
	}
	// The item must come from the network's Items: anyone can deploy a
	// collection with the same item ids (audit 6 IT-1).
	if col := l[9].(common.Address); col != common.HexToAddress(c.ch.dep.Contracts["Items"]) && !c.force {
		return 0, fmt.Errorf("offer %d sells item %d of collection %s, not the network's Items (%s): its item %d is another thing, whatever it is called. "+
			"Buy only if you know that collection (--force)", offer, item, col.Hex(), c.ch.dep.Contracts["Items"], item)
	}
	payTo := common.Hash(w.PayTo).Hex() // a name's id or a split: lock() takes either as hex
	if n, err := c.res.nameOf(common.Hash(w.PayTo)); err == nil {
		payTo = n
	}
	c.asset = w.Token
	d, _ := abi.Arguments{{Type: abiType("uint256")}}.Pack(u(offer))
	lock, err := c.lock(payTo, w.Amount, c.chainNowU()+86400, "SwapCondition", d)
	if err != nil {
		return 0, err
	}
	if _, err := c.ch.send(c.key, nil, "SwapCondition", "settle", u(offer), u(lock)); err != nil {
		return lock, fmt.Errorf("lock %d made but the swap failed (take it back: lock refund-early, or it refunds after a day): %w", lock, err)
	}
	if c.isOwner() {
		_, err = c.ch.send(c.key, nil, "Bank", "withdrawItem", c.id(), l[9].(common.Address), u(item), c.me(), l[3].(*big.Int))
	}
	return lock, err
}

// cancelItemOffer: the seller takes an unswapped offer back, items to its address.
func (c *Client) cancelItemOffer(offer uint64) error {
	l, _, item, err := c.itemOffer(offer)
	if err != nil {
		return err
	}
	if err := c.refundEarly(offer, nil); err != nil {
		return err
	}
	if c.isOwner() {
		_, err = c.ch.send(c.key, nil, "Bank", "withdrawItem", c.id(), l[9].(common.Address), u(item), c.me(), l[3].(*big.Int))
	}
	return err
}

// itemOffers: the open item offers (unswapped item locks under SwapCondition).
func (c *Client) itemOffers() ([]map[string]any, error) {
	evs, err := c.ch.events("Bank", "Locked")
	if err != nil {
		return nil, err
	}
	cond := common.HexToAddress(c.ch.dep.Contracts["SwapCondition"])
	var list []map[string]any
	for _, e := range evs {
		if e.Args["condition"].(common.Address) != cond {
			continue
		}
		id := e.Args["id"].(*big.Int).Uint64()
		l, w, item, err := c.itemOffer(id)
		if err != nil || w.Item.Sign() != 0 {
			continue
		}
		tok := "coin"
		if w.Token != (common.Address{}) {
			tok = w.Token.Hex()
			if a, ok := c.ch.dep.Contracts["TestUSD"]; ok && common.HexToAddress(a) == w.Token {
				tok = "tUSD"
			}
		}
		o := map[string]any{"offer": id, "seller": c.nameOr(common.Hash(l[0].([32]byte))), "item": fmt.Sprint(item),
			"amount": l[3].(*big.Int).String(), "price": fmtCoins(w.Amount), "token": tok, "collection": "Items"}
		if col := l[9].(common.Address); col != common.HexToAddress(c.ch.dep.Contracts["Items"]) {
			o["collection"], o["warning"] = col.Hex(), "not the network's Items: another collection's item with this id (audit 6 IT-1)"
		}
		list = append(list, o)
	}
	return list, nil
}

// ---- randomness

func (c *Client) openRound(topic string, commitBy, revealBy uint64) error {
	_, err := c.ch.send(c.key, nil, "Random", "open", crypto.Keccak256Hash([]byte(topic)), commitBy, revealBy)
	return err
}

func (c *Client) drawRound(topic string) (common.Hash, uint32, error) {
	// Drawn already (by anyone)? Then that is the seed.
	if out, err := c.ch.call("Random", "rounds", crypto.Keccak256Hash([]byte(topic))); err == nil && out[4].(bool) {
		return common.Hash(out[2].([32]byte)), out[3].(uint32), nil
	}
	if _, err := c.ch.send(c.key, nil, "Random", "draw", crypto.Keccak256Hash([]byte(topic))); err != nil {
		return common.Hash{}, 0, err
	}
	out, err := c.ch.call("Random", "rounds", crypto.Keccak256Hash([]byte(topic)))
	if err != nil {
		return common.Hash{}, 0, err
	}
	return common.Hash(out[2].([32]byte)), out[3].(uint32), nil
}

// randomWarning: the deployed Random can be steered (audit ext M-3, static M-3).
const randomWarning = "not fair against a determined participant (entrants can withhold reveals, the drawer picks the block): don't let it decide anything of value"

func (c *Client) roll(topic string, sides, index uint64) (uint64, error) {
	out, err := c.ch.call("Random", "roll", crypto.Keccak256Hash([]byte(topic)), u(sides), u(index))
	if err != nil {
		return 0, err
	}
	return out[0].(*big.Int).Uint64(), nil
}

// ---- the clock

func (c *Client) startGame(players []string, referee string, perMove uint64) (uint64, error) {
	ids := make([][32]byte, len(players))
	for i, p := range players {
		ids[i] = idOf(p)
	}
	var ref common.Hash
	if referee != "" {
		ref = idOf(referee)
	}
	r, err := c.ch.send(c.key, nil, "Clock", "start", ids, ref, perMove)
	if err != nil {
		return 0, err
	}
	id, err := c.firstIndexed(r, "Clock", "Started")
	if err == nil {
		err = c.joinGame(id)
	}
	return id, err
}

// joinGame: anyone can start a game naming anyone as a player (audit ext
// L-5); its moves reach us only once we joined it: started, bonded or moved.
func (c *Client) joinGame(id uint64) error {
	if c.st.Games == nil {
		c.st.Games = map[string]bool{}
	}
	c.st.Games[fmt.Sprint(id)] = true
	return c.save()
}

// joinedGame: did we join game id (here, or by a move of ours on chain)?
func (c *Client) joinedGame(id *big.Int, me map[common.Hash]bool) bool {
	if c.st.Games[id.String()] {
		return true
	}
	for h := range me {
		if evs, err := c.ch.events("Clock", "Moved", []common.Hash{common.BigToHash(id)}, []common.Hash{h}); err == nil && len(evs) > 0 {
			return true
		}
	}
	return false
}

func (c *Client) clockCall(method string, id uint64, args ...any) error {
	_, err := c.ch.send(c.key, nil, "Clock", method, append([]any{u(id)}, args...)...)
	if err == nil && method == "move" {
		err = c.joinGame(id)
	}
	return err
}

func (c *Client) gameInfo(id uint64) (map[string]any, error) {
	out, err := c.ch.call("Clock", "info", u(id))
	if err != nil {
		return nil, err
	}
	var players []string
	for _, p := range out[0].([][32]byte) {
		players = append(players, c.nameOr(common.Hash(p)))
	}
	state := map[uint8]string{1: "playing", 2: "over"}[out[4].(uint8)]
	m := map[string]any{"game": id, "players": players, "turn": out[3].(uint32), "state": state, "deadline": out[2].(uint64)}
	if n := common.Hash(out[1].([32]byte)); n != (common.Hash{}) {
		m["to_move"] = c.nameOr(n)
		m["seconds_left"] = int64(out[2].(uint64)) - c.chainNow()
	}
	if l := common.Hash(out[5].([32]byte)); l != (common.Hash{}) {
		m["lost_by_time_or_resignation"] = c.nameOr(l)
	}
	// The moves so far, so players see what the other played.
	if evs, err := c.ch.events("Clock", "Moved", []common.Hash{common.BigToHash(u(id))}); err == nil {
		var moves []string
		for _, ev := range evs {
			by := c.nameOr(common.Hash(ev.Args["by"].([32]byte)))
			moves = append(moves, fmt.Sprintf("%d %s: %s", ev.Args["turn"].(uint32), by, c.outside(by, string(ev.Args["data"].([]byte))))) // a player's text: fenced (L-5)
		}
		m["moves"] = moves
	}
	return m, nil
}

// clockBond: my stake that goes to payee if I time out or resign, back to me otherwise.
func (c *Client) clockBond(game uint64, payee string, amount *big.Int, deadline uint64) (uint64, error) {
	d, _ := abi.Arguments{{Type: abiType("uint256")}, {Type: abiType("bytes32")}}.Pack(u(game), c.id())
	id, err := c.lock(payee, amount, deadline, "Clock", d)
	if err == nil {
		err = c.joinGame(game)
	}
	return id, err
}

// settleClockBond: after the game, the stake goes to the payee if its player
// timed out or resigned, back to the player otherwise (anyone may run it).
func (c *Client) settleClockBond(id uint64) (string, error) {
	if err := c.claimLock(id, nil); err == nil {
		return "the player timed out or resigned: paid to the payee", nil
	}
	if err := c.refundEarly(id, nil); err != nil {
		return "", errors.New("nothing to settle: the game isn't over, or the lock is settled")
	}
	return "the game ended normally: back to the player", nil
}

// ---- a panel: k of n, after a hearing (see panel.go for the case itself)

func panelDemandType() abi.Arguments {
	t, _ := abi.NewType("tuple", "", []abi.ArgumentMarshaling{
		{Name: "panel", Type: "bytes32[]"}, {Name: "k", Type: "uint8"}, {Name: "schema", Type: "bytes32"},
		{Name: "subject", Type: "bytes32"}, {Name: "kindHash", Type: "bytes32"}, {Name: "hearingUntil", Type: "uint64"}})
	return abi.Arguments{{Type: t}}
}

type panelDemand struct {
	Panel        [][32]byte
	K            uint8
	Schema       [32]byte
	Subject      [32]byte
	KindHash     [32]byte
	HearingUntil uint64
}

func (c *Client) openPanel(payee string, amount *big.Int, deadline uint64, k uint8, panel []string, terms string, hearingUntil uint64) (uint64, common.Hash, error) {
	if hearingUntil >= deadline {
		return 0, common.Hash{}, errors.New("the hearing must end before the lock's deadline")
	}
	judges := 0 // panelists who are not a party: only they count
	for _, p := range panel {
		if !isParty(p, c.f.Name, payee) {
			judges++
		}
	}
	if k == 0 || judges < int(k) {
		return 0, common.Hash{}, fmt.Errorf("k=%d but only %d of the panel are not a party to the case (the payer's or the payee's side never count): it could never be decided", k, judges)
	}
	subj := dealSubject(c.f.Name, payee, "panel", terms, fmt.Sprint(deadline))
	c.noteDeal(subj, fmt.Sprintf("panel case: %s pays %s for %q, %d of %s decide", c.f.Name, payee, terms, k, strings.Join(panel, ",")))
	ids := make([][32]byte, len(panel))
	for i, p := range panel {
		ids[i] = idOf(p)
	}
	d, err := panelDemandType().Pack(panelDemand{ids, k, c.schema(), subj, crypto.Keccak256Hash([]byte("ruling")), hearingUntil})
	if err != nil {
		return 0, subj, err
	}
	id, err := c.lock(payee, amount, deadline, "PanelCondition", d)
	return id, subj, err
}

func (c *Client) settlePanel(lock uint64, uids []common.Hash) error {
	if out, err := c.ch.call("Bank", "remaining", u(lock)); err == nil && out[0].(*big.Int).Sign() == 0 {
		return errors.New("already settled: nothing left in this case (see 'panel case' or 'lock show')")
	}
	ids := make([][32]byte, len(uids))
	for i, x := range uids {
		ids[i] = x
	}
	f, _ := abi.Arguments{{Type: abiType("bytes32[]")}}.Pack(ids)
	err1 := c.claimLock(lock, f)
	err2 := c.refundEarly(lock, f)
	if err1 != nil && err2 != nil {
		return fmt.Errorf("nothing moved: %v / %v", err1, err2)
	}
	return nil
}
