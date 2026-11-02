// Host chains by name (deploy --host): what each one already has (USDC,
// Circle Paymaster, the ERC-8004 registries, an x402 facilitator), checked
// on chain, and whether its money is real.
//
// Base mainnet (--host base, chain 8453) is real money on contracts nobody
// has audited and nobody can change or rescue. Its launch safety, since
// there is no owner to pause anything: net.json "caps" (the most one
// payment, lock, deposit, job or x402 payment may be, and the most a name's
// bank may hold after a deposit), which every client refuses to exceed; a
// notice and a first-run warning that say "a few dollars at most". The
// contracts themselves have no caps (contracts/ is frozen): a client that
// ignores net.json can put in what it likes, at its own risk.
package main

import (
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const baseMainnetChainID = 8453

// hostProfile: a host chain's standard contracts (each checked by eth_getCode
// and a call on the chain: networks/tern-base-1/README.md cites the sources).
type hostProfile struct {
	Name              string
	ChainID           int64
	USDC              Settlement
	CirclePaymaster   string // Circle Paymaster v0.8 (EntryPoint 0x4337…Ff108, token USDC)
	ERC8004Identity   string // github.com/erc-8004/erc-8004-contracts, getVersion 2.0.0
	ERC8004Reputation string
	X402Name          string // x402 version 1's network name
	X402Facilitator   string // the default facilitator x402 serve settles through ("": itself)
	RealMoney         bool   // a mainnet: caps required, the notice and first run say so
}

var hostProfiles = map[string]hostProfile{
	"base-sepolia": {Name: "base-sepolia", ChainID: baseSepoliaChainID, USDC: baseSepoliaUSDC,
		CirclePaymaster: circlePaymasterBaseSepolia, ERC8004Identity: erc8004IdentityBaseSepolia, ERC8004Reputation: erc8004ReputationBaseSepolia,
		X402Name: "base-sepolia", X402Facilitator: x402Facilitator},
	// Base mainnet (checked 2026-11-02 on https://mainnet.base.org):
	// USDC developers.circle.com/stablecoins/usdc-contract-addresses ("USD Coin", "USDC", 6 decimals, EIP-712 version "2");
	// Circle Paymaster v0.8 developers.circle.com/paymaster/addresses-and-events (entryPoint() = the canonical v0.8, token() = USDC,
	// staked on the EntryPoint); ERC-8004 github.com/erc-8004/erc-8004-contracts (getVersion 2.0.0).
	// x402: x402.org's facilitator is for testnets and Coinbase's needs an API key, so x402 serve settles itself by default.
	"base": {Name: "base", ChainID: baseMainnetChainID,
		USDC:            Settlement{Token: "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", Symbol: "USDC", Decimals: 6},
		CirclePaymaster: "0x0578cFB241215b77442a541325d6A4E6dFE700Ec",
		ERC8004Identity: "0x8004A169FB4a3325136EB29fA0ceB6D2e539a432", ERC8004Reputation: "0x8004BAa17C55a88189AE136b182e5fdA19dE9b63",
		X402Name: "base", RealMoney: true},
}

// hostByChain: the profile of the host chain with this id.
func hostByChain(id int64) (hostProfile, bool) {
	for _, h := range hostProfiles {
		if h.ChainID == id {
			return h, true
		}
	}
	return hostProfile{}, false
}

// realMoneyChain: a mainnet someone else runs (Base): its tokens are worth money.
func realMoneyChain(id int64) bool {
	h, ok := hostByChain(id)
	return ok && h.RealMoney
}

// ValueCaps: net.json "caps": what this network's clients refuse to put in at
// once (units of the settlement token; coin_*: of the chain's coin). Not on
// chain: an unaudited network's guard rail for honest clients.
type ValueCaps struct {
	PerPayment     string `json:"per_payment"`                // one payment, lock, escrow, deposit, job budget or x402 payment
	PerName        string `json:"per_name"`                   // a name's bank after a deposit
	CoinPerPayment string `json:"coin_per_payment,omitempty"` // the same in the chain's coin (ETH)
	CoinPerName    string `json:"coin_per_name,omitempty"`
	Note           string `json:"note,omitempty"`
}

const capsNote = "unaudited contracts with real money: clients refuse more than these at once; nobody can rescue funds a bug locks"

// launchCaps: Base mainnet's defaults (deploy --host base).
var launchCaps = ValueCaps{PerPayment: "5", PerName: "20", CoinPerPayment: "0.002", CoinPerName: "0.005", Note: capsNote}

// realMoneyWarning: the first-run message on a mainnet.
const realMoneyWarning = "REAL MONEY, UNAUDITED CONTRACTS: this network is on a mainnet (Base). Nobody has audited its contracts, nobody can " +
	"change them, pause them or give back what a bug locks. Don't put more than a few dollars in. Your client refuses more than " +
	"net.json's caps at once."

// capsCheck: the caps a send must stay within (contract.method and its
// arguments as the client passes them). nil: within, or not capped.
func (ch *Chain) capsCheck(contract, method string, value *big.Int, args []any) error {
	if ch.caps == nil {
		return nil
	}
	arg := func(i int) any {
		if i < len(args) {
			return args[i]
		}
		return nil
	}
	amt := func(i int) *big.Int { v, _ := arg(i).(*big.Int); return v }
	tok := func(i int) common.Address { v, _ := arg(i).(common.Address); return v }
	name := func(i int) *common.Hash { // a name's id, as the client passes it
		switch v := arg(i).(type) {
		case common.Hash:
			return &v
		case [32]byte:
			h := common.Hash(v)
			return &h
		}
		return nil
	}
	var (
		token  common.Address // zero: the coin
		amount *big.Int
		into   *common.Hash // a deposit's name: its bank after it is capped too
	)
	switch contract + "." + method {
	case "Bank.deposit":
		amount, into = value, name(0)
	case "Bank.depositToken":
		token, amount, into = tok(1), amt(2), name(0)
	case "Bank.pay", "Bank.lock":
		amount = amt(2)
	case "Bank.payToken", "Bank.lockToken":
		token, amount = tok(2), amt(3)
	case "TernJobs.setBudget": // the job's token is the network's
		token, amount = ch.settleToken(), amt(1)
	default:
		return nil
	}
	if amount == nil || amount.Sign() == 0 {
		return nil
	}
	if err := ch.capOne(token, amount); err != nil {
		return err
	}
	if into == nil {
		return nil
	}
	per, sym, ok := ch.capFor(token, true)
	if !ok {
		return nil
	}
	var held *big.Int
	if token == (common.Address{}) {
		if out, err := ch.call("Bank", "balance", *into); err == nil {
			held, _ = out[0].(*big.Int)
		}
	} else if out, err := ch.call("Bank", "tokenBalance", *into, token); err == nil {
		held, _ = out[0].(*big.Int)
	}
	if held == nil {
		held = new(big.Int)
	}
	if after := new(big.Int).Add(held, amount); after.Cmp(per) > 0 {
		return fmt.Errorf("this network caps a name's bank at %s %s (net.json caps: %s); it holds %s, so a deposit of %s would make %s",
			ch.fmtUnitsOf(token, per), sym, capsNote, ch.fmtUnitsOf(token, held), ch.fmtUnitsOf(token, amount), ch.fmtUnitsOf(token, after))
	}
	return nil
}

// capOne: one payment of amount in token (zero: the coin) within the per-payment cap.
func (ch *Chain) capOne(token common.Address, amount *big.Int) error {
	if ch.caps == nil || amount == nil {
		return nil
	}
	per, sym, ok := ch.capFor(token, false)
	if ok && amount.Cmp(per) > 0 {
		return fmt.Errorf("this network caps one payment, lock, deposit or job at %s %s (net.json caps: %s); %s asked",
			ch.fmtUnitsOf(token, per), sym, capsNote, ch.fmtUnitsOf(token, amount))
	}
	return nil
}

// capFor: the cap on token (the settlement token, or the coin); ok false:
// none set for it. A token that is neither isn't priced, so not capped.
func (ch *Chain) capFor(token common.Address, perName bool) (*big.Int, string, bool) {
	s, sym, dec := ch.caps.PerPayment, "", uint8(18)
	if perName {
		s = ch.caps.PerName
	}
	switch {
	case token == (common.Address{}):
		s, sym = ch.caps.CoinPerPayment, "ETH"
		if perName {
			s = ch.caps.CoinPerName
		}
	case ch.settle != nil && strings.EqualFold(token.Hex(), ch.settle.Token):
		sym, dec = ch.settle.Symbol, ch.settle.Decimals
	default:
		return nil, "", false
	}
	if s == "" {
		return nil, "", false
	}
	v, err := parseUnits(s, dec)
	if err != nil {
		return nil, "", false
	}
	return v, sym, true
}

func (ch *Chain) fmtUnitsOf(token common.Address, v *big.Int) string {
	if token == (common.Address{}) {
		return fmtUnits(v, 18)
	}
	return fmtUnits(v, ch.tokenDecimals(token))
}

// validCaps: deploy's caps parse (in the settlement token's decimals, and the coin's).
func validCaps(c *ValueCaps, decimals uint8) error {
	for _, f := range []struct {
		name, v string
		dec     uint8
		need    bool
	}{
		{"--cap-per-payment", c.PerPayment, decimals, true}, {"--cap-per-name", c.PerName, decimals, true},
		{"--cap-coin-per-payment", c.CoinPerPayment, 18, false}, {"--cap-coin-per-name", c.CoinPerName, 18, false}} {
		if f.v == "" {
			if f.need {
				return fmt.Errorf("%s: needed on a network with real money", f.name)
			}
			continue
		}
		v, err := parseUnits(f.v, f.dec)
		if err != nil || v.Sign() <= 0 {
			return fmt.Errorf("%s %q: a positive amount", f.name, f.v)
		}
	}
	return nil
}

// mainnetNetworkNotice: net.json's notice for the public contract set on a mainnet.
func mainnetNetworkNotice(name, host string, caps *ValueCaps, symbol string, zkSolo bool) string {
	if name == "" {
		name = "(unnamed)"
	}
	zk := "The secret-ballot zk keys come from a multi-party ceremony (net.json ceremony)."
	if zkSolo {
		zk = "The secret-ballot zk keys were made by the deployer alone: it could forge secret-ballot proofs, so don't let a group's secret ballot decide anything of value."
	}
	return fmt.Sprintf("UNAUDITED, REAL MONEY: don't put more than a few dollars in. ternchain network %s: ternchain's contracts on %s mainnet. "+
		"Nobody has audited them. Nobody runs this network and nobody can change, pause or upgrade these contracts: the deployer gave up "+
		"every role and there are no citizens to vote (check: ternchain roles), so nobody can rescue funds that a bug locks. Clients refuse "+
		"more than %s %s in one payment, lock, deposit or job, and more than %s %s in a name's bank (net.json caps); the contracts themselves have "+
		"no caps. You pay your own gas, in USDC through Circle Paymaster (it adds its own fee) or in ETH. %s",
		name, host, caps.PerPayment, symbol, caps.PerName, symbol, zk)
}

// warnRealMoney: the first-run warning on a mainnet, on stderr (the JSON answer carries it too).
func warnRealMoney(id int64) string {
	if !realMoneyChain(id) {
		return ""
	}
	fmt.Fprint(os.Stderr, "\n!!! "+realMoneyWarning+" !!!\n\n")
	return realMoneyWarning
}
