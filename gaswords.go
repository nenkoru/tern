// Out-of-gas errors in words a person or an agent can act on (PROOFREAD-6 #12):
// "insufficient funds for gas * price + value" alone says nothing about whose
// coins ran out or what to do.
package main

import (
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// gasHint: the remedy for an out-of-gas error ("": not one).
func gasHint(msg string) string {
	if !strings.Contains(msg, "insufficient funds for gas") || strings.Contains(msg, "gas coins ran out") {
		return ""
	}
	return " — gas coins ran out: the key that signs this has too few coins for the transaction's gas. " +
		"An agent: tell your owner (tern_ask_owner); the owner tops it up by running owner grant again with --gas (0.05 is about a hundred actions). " +
		"An owner: get coins from the network's faucet or its operator (whoami shows your address and gas_coins)."
}

// friendlyGas: err with the address that ran out and the remedy.
func friendlyGas(err error, a common.Address) error {
	if err == nil {
		return nil
	}
	if h := gasHint(err.Error()); h != "" {
		return fmt.Errorf("%w (address %s)%s", err, a.Hex(), h)
	}
	return err
}
