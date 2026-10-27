// Who holds the coins at block 0 of a ternchain L1 (L1-READINESS C8, E3).
// No single key holds the supply: the Treasury contract gets the rest, and
// only the timelock (both houses) spends it. Keys get gas allowances only:
// the deployer enough to deploy, each founder and validator a little, and a
// testnet's faucet its allowance (none on mainnet). `validator genesis
// --devnet` keeps the old local-demo genesis (everything to the deployer).
package main

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Chain ids of the ternchain L1 (both free on chainid.network / ethereum-lists
// on 2026-10-26; "TERN" on a phone keypad is 8376). A devnet keeps 1337.
const (
	l1MainnetChainID = 8376
	l1TestnetChainID = 83760
)

// treasuryNonce: the deployer's nonce whose CREATE address is the Treasury
// (deploy in chain.go: Registry 0, TernTimelock 1, Directory 2, Treasury 3).
// The genesis puts the treasury's coins there before the contract exists; a
// CREATE onto an address with a balance keeps it. `deploy` checks both ends.
const treasuryNonce = 3

// maxKeyShareBps: no key but the Treasury may hold more than this of the supply.
const maxKeyShareBps = 100 // 1%

// treasuryAddress: where deploy from a fresh deployer key puts the Treasury.
func treasuryAddress(deployer common.Address) common.Address {
	return crypto.CreateAddress(deployer, treasuryNonce)
}

// allocRow: one line of the genesis table.
type allocRow struct {
	Who     string `json:"who"`
	Address string `json:"address"`
	Coins   string `json:"coins"`
	Why     string `json:"why"`
	wei     *big.Int
}

type allocPlan struct {
	supply         *big.Int
	deployer       common.Address
	deployCoins    *big.Int
	founders       []GenesisCitizen
	founderCoins   *big.Int
	validators     []common.Address
	validatorNames []string
	validatorCoins *big.Int
	faucet         common.Address
	faucetCoins    *big.Int
	chainID        int64
}

// genesisTable: the rows, the Treasury's last. Refused: a faucet on mainnet,
// a key over 1% of the supply, an address twice, allowances above the supply.
func genesisTable(p allocPlan) ([]allocRow, error) {
	if p.deployer == (common.Address{}) {
		return nil, errors.New("a deployer address: it deploys the contracts with its gas allowance, and the Treasury's address follows from it")
	}
	if p.supply == nil || p.supply.Sign() <= 0 {
		return nil, errors.New("--supply: a positive number of coins")
	}
	var rows []allocRow
	seen := map[common.Address]string{}
	add := func(who string, a common.Address, wei *big.Int, why string) error {
		if wei == nil || wei.Sign() == 0 {
			return nil
		}
		if prev, ok := seen[a]; ok {
			return fmt.Errorf("%s and %s are one address %s: one row per key", prev, who, a.Hex())
		}
		seen[a] = who
		rows = append(rows, allocRow{Who: who, Address: a.Hex(), Coins: fmtCoins(wei), Why: why, wei: wei})
		return nil
	}
	if err := add("deployer", p.deployer, p.deployCoins, "gas to deploy the contracts; no other power (validator powers checks)"); err != nil {
		return nil, err
	}
	for _, f := range p.founders {
		if err := add("founder "+f.Name, f.Key, p.founderCoins, "gas for a founding citizen's votes and vouches"); err != nil {
			return nil, err
		}
	}
	for i, v := range p.validators {
		who := "validator " + v.Hex()[:10]
		if i < len(p.validatorNames) && p.validatorNames[i] != "" {
			who = "validator " + p.validatorNames[i]
		}
		if err := add(who, v, p.validatorCoins, "gas for the node key (rotation, reports)"); err != nil {
			return nil, err
		}
	}
	if p.faucet != (common.Address{}) {
		if p.chainID == l1MainnetChainID {
			return nil, fmt.Errorf("no faucet on mainnet (chain %d): coins go out by this table and by treasury grants", l1MainnetChainID)
		}
		if err := add("faucet", p.faucet, p.faucetCoins, "test coins for newcomers (testnet only; they never become mainnet coins)"); err != nil {
			return nil, err
		}
	}
	limit := new(big.Int).Div(new(big.Int).Mul(p.supply, big.NewInt(maxKeyShareBps)), big.NewInt(10000))
	given := new(big.Int)
	for _, r := range rows {
		if r.wei.Cmp(limit) > 0 {
			return nil, fmt.Errorf("%s would hold %s coins, more than 1%% of the supply (%s): no single key holds the supply", r.Who, r.Coins, fmtCoins(limit))
		}
		given.Add(given, r.wei)
	}
	rest := new(big.Int).Sub(p.supply, given)
	if rest.Sign() <= 0 {
		return nil, fmt.Errorf("the allowances (%s) leave nothing of the supply (%s) for the Treasury", fmtCoins(given), fmtCoins(p.supply))
	}
	t := treasuryAddress(p.deployer)
	if prev, ok := seen[t]; ok {
		return nil, fmt.Errorf("%s is at the Treasury's address %s", prev, t.Hex())
	}
	rows = append(rows, allocRow{Who: "treasury", Address: t.Hex(), Coins: fmtCoins(rest), wei: rest,
		Why: "the network's treasury (Treasury.sol, deployed there by deploy from this deployer's first transactions): spent only by the timelock, i.e. both houses"})
	return rows, nil
}

// planSummary: what validator genesis prints about the table.
func planSummary(rows []allocRow, supply *big.Int) map[string]any {
	var t allocRow
	keys := new(big.Int)
	for _, r := range rows {
		if r.Who == "treasury" {
			t = r
		} else {
			keys.Add(keys, r.wei)
		}
	}
	share := new(big.Int).Div(new(big.Int).Mul(keys, big.NewInt(10000)), supply)
	return map[string]any{"treasury": t.Address, "treasury_coins": t.Coins, "keys_coins": fmtCoins(keys),
		"keys_share": fmt.Sprintf("%d.%02d%%", share.Int64()/100, share.Int64()%100)}
}

// coinsFlag: a coin amount flag, "" meaning none.
func coinsFlag(name, s string) (*big.Int, error) {
	if strings.TrimSpace(s) == "" {
		return new(big.Int), nil
	}
	v, err := parseCoins(s)
	if err != nil {
		return nil, fmt.Errorf("--%s: %w", name, err)
	}
	return v, nil
}

// genesisRows: the table from validator genesis's flags.
func genesisRows(founders, faucet, deployCoins, founderCoins, validatorCoins, faucetCoins string, supply *big.Int,
	deployer common.Address, vs []common.Address, shown []string, chainID int64) ([]allocRow, error) {
	p := allocPlan{supply: supply, deployer: deployer, validators: vs, chainID: chainID}
	var err error
	if p.deployCoins, err = coinsFlag("deploy-coins", deployCoins); err != nil {
		return nil, err
	}
	if p.founderCoins, err = coinsFlag("founder-coins", founderCoins); err != nil {
		return nil, err
	}
	if p.validatorCoins, err = coinsFlag("validator-coins", validatorCoins); err != nil {
		return nil, err
	}
	if p.faucetCoins, err = coinsFlag("faucet-coins", faucetCoins); err != nil {
		return nil, err
	}
	if strings.TrimSpace(founders) != "" {
		if p.founders, err = parseGenesisStrict(founders); err != nil {
			return nil, fmt.Errorf("--founders: %w", err)
		}
	}
	for _, s := range shown {
		n, _, _ := strings.Cut(s, "=")
		if strings.Contains(n, " ") { // "0x… (unbound)"
			n = ""
		}
		p.validatorNames = append(p.validatorNames, n)
	}
	if faucet != "" {
		if !common.IsHexAddress(faucet) {
			return nil, fmt.Errorf("--faucet %q: an address (the faucet key's)", faucet)
		}
		p.faucet = common.HexToAddress(faucet)
	}
	return genesisTable(p)
}
