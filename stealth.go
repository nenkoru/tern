// Stealth payments (ERC-5564 style, secp256k1): pay someone at a one-time
// address only they can find and spend. Coins received this way form a
// private wallet; paying from it hides the payer too.
//
//	payer:     r random, R = rG, s = keccak(r·V), tag = s[0], P = S + s·G
//	recipient: s = keccak(v·R), check tag, P = S + s·G, key = spend + s (mod n)
package main

//lint:file-ignore SA1019 raw curve coordinates: secp256k1 (go-ethereum) and the P-256 passkey verifier need them; crypto/ecdh has no secp256k1

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

type stealthKeys struct {
	spend, view *ecdsa.PrivateKey
}

func (i *Ident) stealthPath() string { return filepath.Join(i.dir, "stealth.key") }

func (i *Ident) loadStealth() (*stealthKeys, error) {
	b, err := os.ReadFile(i.stealthPath())
	if err != nil {
		return nil, err
	}
	parts := strings.Fields(string(b))
	if len(parts) != 2 {
		return nil, errors.New("bad stealth.key")
	}
	s, err := crypto.HexToECDSA(parts[0])
	if err != nil {
		return nil, err
	}
	v, err := crypto.HexToECDSA(parts[1])
	if err != nil {
		return nil, err
	}
	return &stealthKeys{s, v}, nil
}

// enableStealth creates the keys and publishes the public halves.
func (c *Client) enableStealth() error {
	if _, err := c.loadStealth(); err == nil {
		return nil
	}
	s, _ := crypto.GenerateKey()
	v, _ := crypto.GenerateKey()
	if err := os.WriteFile(c.stealthPath(), []byte(keyHex(s)+"\n"+keyHex(v)+"\n"), 0o600); err != nil {
		return err
	}
	meta := append(crypto.CompressPubkey(&s.PublicKey), crypto.CompressPubkey(&v.PublicKey)...)
	_, err := c.ch.send(c.key, nil, "ERC6538Registry", "registerKeys", big.NewInt(1), meta) // scheme 1: secp256k1 + view tags
	return err
}

// errNoStealthMeta: the name registered no meta-address (the only case a
// pool send may fall back to its bank balance, audit 5 PV-8).
var errNoStealthMeta = errors.New("takes no private payments")

// stealthMeta reads a name's ERC-6538 meta-address (registered by the key the name has now).
func (c *Client) stealthMeta(name string) (*ecdsa.PublicKey, *ecdsa.PublicKey, error) {
	p, err := c.res.party(name)
	if err != nil {
		return nil, nil, err
	}
	addr, _ := p.controllerAt(^uint64(0))
	out, err := c.ch.call("ERC6538Registry", "stealthMetaAddressOf", addr, big.NewInt(1))
	if err != nil {
		return nil, nil, err
	}
	meta := out[0].([]byte)
	if len(meta) == 0 {
		return nil, nil, fmt.Errorf("%s %w", name, errNoStealthMeta)
	}
	if len(meta) != 66 {
		return nil, nil, fmt.Errorf("%s's stealth meta-address is malformed (%d bytes)", name, len(meta))
	}
	sp, err := crypto.DecompressPubkey(meta[:33])
	if err != nil {
		return nil, nil, err
	}
	vp, err := crypto.DecompressPubkey(meta[33:])
	if err != nil {
		return nil, nil, err
	}
	return sp, vp, nil
}

func secpECDH(priv *ecdsa.PrivateKey, pub *ecdsa.PublicKey) []byte {
	x, y := crypto.S256().ScalarMult(pub.X, pub.Y, priv.D.Bytes())
	return crypto.CompressPubkey(&ecdsa.PublicKey{Curve: crypto.S256(), X: x, Y: y})
}

func stealthPoint(spend *ecdsa.PublicKey, s []byte) *ecdsa.PublicKey {
	cv := crypto.S256()
	sx, sy := cv.ScalarBaseMult(s)
	x, y := cv.Add(spend.X, spend.Y, sx, sy)
	return &ecdsa.PublicKey{Curve: cv, X: x, Y: y}
}

// derive returns the one-time address for a recipient's meta keys.
func deriveStealth(spend, view *ecdsa.PublicKey) (common.Address, []byte, byte, error) {
	r, err := crypto.GenerateKey()
	if err != nil {
		return common.Address{}, nil, 0, err
	}
	s := crypto.Keccak256(secpECDH(r, view))
	p := stealthPoint(spend, s)
	return crypto.PubkeyToAddress(*p), crypto.CompressPubkey(&r.PublicKey), s[0], nil
}

// payStealth pays `to` (its owner, really) privately, through the agent's
// guard like any payment (audit 4 AS-1: scopes.to, taint, approvals).
func (c *Client) payStealth(to string, amount *big.Int, memo common.Hash, from *ecdsa.PrivateKey) error {
	done, err := c.guardPay(to, amount)
	if err != nil {
		return fmt.Errorf("private pay: %w", err)
	}
	err = c.payStealthRaw(to, amount, memo, from)
	done(err == nil)
	return err
}

// payStealthRaw: the payment itself, for callers that guarded it already
// (postage). from: a private wallet key to pay from (hides the payer), or
// nil to pay from our main key.
func (c *Client) payStealthRaw(to string, amount *big.Int, memo common.Hash, from *ecdsa.PrivateKey) error {
	sp, vp, err := c.stealthMeta(ownerName(to))
	if err != nil {
		return err
	}
	addr, eph, tag, err := deriveStealth(sp, vp)
	if err != nil {
		return err
	}
	key := c.key
	if from != nil {
		key = from
	}
	_, err = c.ch.send(key, amount, "StealthPay", "send", addr, eph, [1]byte{tag}, memo)
	return err
}

// Note is a private coin we hold: an amount at a one-time address.
type Note struct {
	Address common.Address `json:"address"`
	Amount  *big.Int       `json:"amount"`
	Memo    common.Hash    `json:"memo"`
	key     *ecdsa.PrivateKey
}

// notes scans announcements for payments to us and returns their keys and balances.
func (c *Client) notes() ([]Note, error) {
	sk, err := c.loadStealth()
	if err != nil {
		return nil, errors.New("private payments are not enabled (ternchain private enable)")
	}
	anns, err := c.announcements(nil)
	if err != nil {
		return nil, err
	}
	n := crypto.S256().Params().N
	seen := map[common.Address]bool{}
	var out []Note
	for _, a := range anns {
		s := crypto.Keccak256(secpECDH(sk.view, a.eph))
		if s[0] != a.viewTag || a.amount.Sign() == 0 { // a zero payment carries nothing: no balance read for it (audit 5 PV-14)
			continue
		}
		p := stealthPoint(&sk.spend.PublicKey, s)
		if crypto.PubkeyToAddress(*p) != a.stealth || seen[a.stealth] {
			continue
		}
		seen[a.stealth] = true
		d := new(big.Int).Add(sk.spend.D, new(big.Int).SetBytes(s))
		d.Mod(d, n)
		key, err := crypto.ToECDSA(common.LeftPadBytes(d.Bytes(), 32))
		if err != nil {
			continue
		}
		bal, err := c.ch.balance(a.stealth)
		if err != nil {
			return nil, err
		}
		out = append(out, Note{Address: a.stealth, Amount: bal, Memo: a.memo, key: key})
	}
	return out, nil
}

type announcement struct {
	stealth common.Address
	eph     *ecdsa.PublicKey
	viewTag byte
	amount  *big.Int
	memo    common.Hash
}

// announcements reads ERC-5564 scheme-1 announcements; metadata for native
// transfers: viewTag | 0xeeeeeeee | 0xEeee…EEeE | amount(32) | memo(32).
// Only those made by StealthPay or the pool count: they write the amount
// they really sent. announce() itself is open to anyone, who could claim
// any amount (audit 5 PV-3, PV-14).
func (c *Client) announcements(memo *common.Hash) ([]announcement, error) {
	evs, err := c.ch.events("ERC5564Announcer", "Announcement", []common.Hash{common.BigToHash(big.NewInt(1))})
	if err != nil {
		return nil, err
	}
	payers := map[common.Address]bool{}
	for _, k := range []string{"StealthPay", "PrivacyPool"} {
		if c.ch.has(k) {
			payers[c.ch.addr(k)] = true
		}
	}
	var out []announcement
	for _, e := range evs {
		if caller, ok := e.Args["caller"].(common.Address); !ok || !payers[caller] {
			continue
		}
		md := e.Args["metadata"].([]byte)
		eph, err := crypto.DecompressPubkey(e.Args["ephemeralPubKey"].([]byte))
		if err != nil || len(md) < 1 {
			continue
		}
		a := announcement{stealth: e.Args["stealthAddress"].(common.Address), eph: eph, viewTag: md[0], amount: big.NewInt(0)}
		if len(md) >= 57 {
			a.amount = new(big.Int).SetBytes(md[25:57])
		}
		if len(md) >= 89 {
			a.memo = common.BytesToHash(md[57:89])
		}
		if memo != nil && a.memo != *memo {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// noteFor picks a private coin that can pay amount plus gas.
func (c *Client) noteFor(amount *big.Int) (*ecdsa.PrivateKey, error) {
	ns, err := c.notes()
	if err != nil {
		return nil, err
	}
	need := new(big.Int).Add(amount, gasReserve)
	for _, n := range ns {
		if n.Amount.Cmp(need) >= 0 {
			return n.key, nil
		}
	}
	return nil, errors.New("no private coin large enough (receive privately first, or pay from your main key)")
}

var gasReserve = new(big.Int).Div(coinUnit, big.NewInt(100)) // 0.01 coin kept for gas

// shield moves coins from the main key into our own private wallet. The
// chain sees "this name paid some new address": the amount links, the
// later spending doesn't. A shielded pool (zero knowledge) would hide even that.
func (c *Client) shield(amount *big.Int) error {
	return c.payStealth(c.f.Owner, amount, common.Hash{}, nil)
}

// stealthView returns the view private key and spend public key: from the
// owner's stealth.key, or from stealth.view, which an owner gives its agents
// (it detects payments; it can't spend them).
func (c *Client) stealthView() (*ecdsa.PrivateKey, *ecdsa.PublicKey, error) {
	if sk, err := c.loadStealth(); err == nil {
		return sk.view, &sk.spend.PublicKey, nil
	}
	b, err := os.ReadFile(filepath.Join(c.dir, "stealth.view"))
	if err != nil {
		return nil, nil, err
	}
	parts := strings.Fields(string(b))
	if len(parts) != 2 {
		return nil, nil, errors.New("bad stealth.view")
	}
	v, err := crypto.HexToECDSA(parts[0])
	if err != nil {
		return nil, nil, err
	}
	raw, _ := hex.DecodeString(parts[1])
	sp, err := crypto.DecompressPubkey(raw)
	return v, sp, err
}

func (c *Client) stealthViewBundle() string {
	sk, err := c.loadStealth()
	if err != nil {
		return ""
	}
	return keyHex(sk.view) + " " + hex.EncodeToString(crypto.CompressPubkey(&sk.spend.PublicKey))
}

// paidStealthPostage: a private payment to us, with this envelope as memo, of at least price.
func (c *Client) paidStealthPostage(envelope string, price *big.Int) bool {
	v, sp, err := c.stealthView()
	if err != nil {
		return false
	}
	m := common.HexToHash(envelope)
	anns, err := c.announcements(&m)
	if err != nil {
		return false
	}
	for _, a := range anns {
		if a.amount.Cmp(price) < 0 {
			continue
		}
		s := crypto.Keccak256(secpECDH(v, a.eph))
		if s[0] == a.viewTag && crypto.PubkeyToAddress(*stealthPoint(sp, s)) == a.stealth {
			return true
		}
	}
	return false
}

// sweep moves a private coin's balance to an address (minus gas).
func (c *Client) sweep(n Note, to common.Address) error {
	h, err := c.ch.b.HeaderByNumber(context.Background(), nil)
	if err != nil {
		return err
	}
	tip, err := c.ch.b.SuggestGasTipCap(context.Background())
	if err != nil {
		return err
	}
	// the same fee cap transfer() uses: tip + 2 × base fee, for 21000 gas
	fee := new(big.Int).Mul(new(big.Int).Add(tip, new(big.Int).Mul(h.BaseFee, big.NewInt(2))), big.NewInt(21000))
	amt := new(big.Int).Sub(n.Amount, fee)
	if amt.Sign() <= 0 {
		return errors.New("too little to move: the balance does not cover the gas")
	}
	return c.ch.transfer(n.key, to, amt)
}
