// Money in a network's settlement token. A network on a host chain settles
// in a stablecoin there (Base Sepolia: Circle's test USDC, 6 decimals), not
// in a coin of its own: payments, locks, escrows, pledges and agents' limits
// are in that token by default, and amounts are read and shown in its units.
// The chain's own coin (ETH on Base) stays what pays gas, fees and stakes.
package main

import (
	"fmt"
	"math/big"
	"strings"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Settlement: the token a network settles in (net.json "settle").
type Settlement struct {
	Token    string `json:"token"`
	Symbol   string `json:"symbol"`
	Decimals uint8  `json:"decimals"`
	Faucet   string `json:"faucet,omitempty"` // where testers get some
}

// Circle's test USDC on Base Sepolia (developers.circle.com/stablecoins/usdc-contract-addresses):
// 6 decimals, EIP-3009 transferWithAuthorization and EIP-2612 permit (x402 pays with the former).
var baseSepoliaUSDC = Settlement{Token: "0x036CbD53842c5426634e7929541eC2318f3dCF7e", Symbol: "USDC", Decimals: 6,
	Faucet: "https://faucet.circle.com"}

func unitOf(decimals uint8) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
}

// parseUnits: "0.5" in a token with these decimals.
func parseUnits(s string, decimals uint8) (*big.Int, error) {
	if !coinsRE.MatchString(s) {
		return nil, fmt.Errorf("bad amount %q: a number, like 0.5", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return nil, fmt.Errorf("bad amount %q: a number, like 0.5", s)
	}
	r.Mul(r, new(big.Rat).SetInt(unitOf(decimals)))
	if !r.IsInt() {
		return nil, fmt.Errorf("amount %q has more than %d decimals", s, decimals)
	}
	return r.Num(), nil
}

func fmtUnits(w *big.Int, decimals uint8) string {
	if w == nil {
		w = new(big.Int)
	}
	return new(big.Rat).SetFrac(w, unitOf(decimals)).FloatString(int(min(decimals, 6)))
}

// tokenDecimals: the network's coin and unknown tokens 18; the settlement
// token as net.json says; any other token as it says on chain (cached).
func (ch *Chain) tokenDecimals(t common.Address) uint8 {
	if t == (common.Address{}) {
		return 18
	}
	if ch.settle != nil && strings.EqualFold(ch.settle.Token, t.Hex()) {
		return ch.settle.Decimals
	}
	if v, ok := ch.decimals.Load(t); ok {
		return v.(uint8)
	}
	d := uint8(18)
	if a, err := erc20ABI(); err == nil { // any ERC-20's decimals()
		if data, err := a.Pack("decimals"); err == nil {
			if out, err := ch.b.CallContract(ctxb(), ethereum.CallMsg{To: &t, Data: data}, nil); err == nil {
				if v, err := a.Unpack("decimals", out); err == nil && len(v) == 1 {
					d = v[0].(uint8)
				}
			}
		}
	}
	ch.decimals.Store(t, d)
	return d
}

// erc20ABI: any token's ERC-20 calls (TestUSD's ABI is plain OpenZeppelin
// ERC-20; it is there on every network, deployed or not).
func erc20ABI() (abi.ABI, error) {
	a, err := loadArtifact("TestUSD")
	return a.abi, err
}

// settleToken: the network's settlement token (zero: its coin).
func (ch *Chain) settleToken() common.Address {
	if ch.settle == nil || !common.IsHexAddress(ch.settle.Token) {
		return common.Address{}
	}
	return common.HexToAddress(ch.settle.Token)
}

// assetName: a token's name for people (USDC, tUSD, or its address).
func (ch *Chain) assetSymbol(t common.Address) string {
	switch {
	case t == (common.Address{}):
		return "coins"
	case ch.settle != nil && strings.EqualFold(ch.settle.Token, t.Hex()):
		return ch.settle.Symbol
	case strings.EqualFold(ch.dep.Contracts["TestUSD"], t.Hex()):
		return "tUSD"
	case strings.EqualFold(ch.dep.Contracts["TestUSDC"], t.Hex()): // a devnet's look-alike of Circle's
		return "USDC"
	}
	return t.Hex()
}

// parseMoney / fmtMoney: an amount of what this client pays in (--token, or
// the network's settlement token, or its coin).
func (c *Client) parseMoney(s string) (*big.Int, error) {
	return parseUnits(s, c.ch.tokenDecimals(c.asset))
}

func (c *Client) fmtMoney(w *big.Int) string { return fmtUnits(w, c.ch.tokenDecimals(c.asset)) }

// fmtIn: an amount of a given token (a lock's).
func (c *Client) fmtIn(w *big.Int, token common.Address) string {
	return fmtUnits(w, c.ch.tokenDecimals(token))
}

// The network's own money (its settlement token, else its coin): what
// prices people set for each other are in (cards' postage).
func (c *Client) parseNet(s string) (*big.Int, error) {
	return parseUnits(s, c.ch.tokenDecimals(c.ch.settleToken()))
}

func (c *Client) fmtNet(w *big.Int) string {
	t := c.ch.settleToken()
	if t == (common.Address{}) {
		return fmtCoins(w) // the coin, as it always read
	}
	return fmtUnits(w, c.ch.tokenDecimals(t)) + " " + c.ch.assetSymbol(t)
}

// payNet: a payment in the network's money from this identity's bank (Bank.pay or payToken).
func (c *Client) payNet(to common.Hash, amount *big.Int, memo common.Hash) error {
	if t := c.ch.settleToken(); t != (common.Address{}) {
		_, err := c.ch.send(c.key, nil, "Bank", "payToken", c.id(), to, t, amount, memo)
		return c.limitErr(err)
	}
	_, err := c.ch.send(c.key, nil, "Bank", "pay", c.id(), to, amount, memo)
	return err
}

// tokenDomain: a token's EIP-712 domain name and version, as its permit and
// transferWithAuthorization check them: name() and version() from the chain,
// kept only when they give the token's DOMAIN_SEPARATOR(). Circle's USDC is
// "USDC" "2" on Base Sepolia but "USD Coin" "2" on Base mainnet; a token
// that says nothing: fallback.
func (ch *Chain) tokenDomain(t common.Address, fallbackName, fallbackVersion string) (string, string) {
	if v, ok := ch.domains.Load(t); ok {
		d := v.([2]string)
		return d[0], d[1]
	}
	str := func(sig string) (string, bool) {
		data := crypto.Keccak256([]byte(sig))[:4]
		out, err := ch.b.CallContract(ctxb(), ethereum.CallMsg{To: &t, Data: data}, nil)
		if err != nil || len(out) < 64 {
			return "", false
		}
		v, err := abi.Arguments{{Type: abiType("string")}}.Unpack(out)
		if err != nil || len(v) != 1 {
			return "", false
		}
		s, ok := v[0].(string)
		return s, ok && s != ""
	}
	name, okN := str("name()")
	version, okV := str("version()")
	if !okN || !okV {
		return fallbackName, fallbackVersion
	}
	sep, err := ch.b.CallContract(ctxb(), ethereum.CallMsg{To: &t, Data: crypto.Keccak256([]byte("DOMAIN_SEPARATOR()"))[:4]}, nil)
	if err != nil || len(sep) != 32 || common.BytesToHash(sep) != eip712DomainSeparator(name, version, ch.chainID, t) {
		return fallbackName, fallbackVersion
	}
	ch.domains.Store(t, [2]string{name, version})
	return name, version
}
