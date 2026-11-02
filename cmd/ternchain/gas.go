// Who pays for gas. On a host chain (Base Sepolia) the network pays nobody's
// gas: each user pays their own, either in the chain's coin (a key with ETH,
// or an account holding ETH) or in USDC through an ERC-20 paymaster (Circle
// Paymaster): the account signs an EIP-2612 permit for the paymaster in each
// operation, and the paymaster takes the gas's price in USDC from the
// account and gives back what wasn't used. A newcomer needs only test USDC.
package main

import (
	"errors"
	"fmt"
	"math/big"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// GasConfig: net.json "gas".
type GasConfig struct {
	Pays      string `json:"pays"`                // "user": each user pays their own gas
	Paymaster string `json:"paymaster,omitempty"` // an ERC-20 paymaster users pay through (Circle Paymaster v0.8)
	Token     string `json:"token,omitempty"`     // what it takes (USDC)
	Note      string `json:"note,omitempty"`
}

// Circle Paymaster v0.8 on Base Sepolia (developers.circle.com, checked on
// chain: EntryPoint v0.8, staked and funded). It takes USDC only.
const circlePaymasterBaseSepolia = "0x3BA9A96eE3eFf3A69E2B18886AcF52027EFF8966"

const (
	tokenPMVerification = 200_000 // Circle's recommendation
	tokenPMPostOp       = 50_000  // at least 35,000 (Circle)
	// tokenPMPrice: USDC (6 decimals) per ETH the permit budgets for: Circle's
	// test oracle says 3000; the permit allows three times the worst case,
	// at least 1 USDC (an allowance to the paymaster, not a payment).
	tokenPMPrice = 3000_000_000
)

// tokenGas: this network's users pay gas in a token through a paymaster.
func (ch *Chain) tokenGas() bool { return ch.gas != nil && common.IsHexAddress(ch.gas.Paymaster) }

// accountsByDefault: owner init and agent init make smart accounts (their
// gas paid in USDC, or by the operator's sponsor where net.json names one).
func (ch *Chain) accountsByDefault() bool { return ch.tokenGas() || ch.has("SponsorPaymaster") }

var maxUint256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// tokenPaymasterData: paymaster | verificationGas | postOpGas | 0x00 | token |
// permitAmount | the account's EIP-2612 permit for the paymaster (deadline
// max), signed by the route's key (or passkey) and checked by the token
// through the account's ERC-1271.
func (ch *Chain) tokenPaymasterData(r *AccountRoute, vgas, cgas, maxFee uint64) ([]byte, error) {
	pm, token := common.HexToAddress(ch.gas.Paymaster), common.HexToAddress(ch.gas.Token)
	maxCost := new(big.Int).Mul(new(big.Int).SetUint64(vgas+cgas+preVerification+tokenPMVerification+tokenPMPostOp), new(big.Int).SetUint64(maxFee))
	need := new(big.Int).Add(maxCost, new(big.Int).Mul(big.NewInt(tokenPMPostOp), new(big.Int).SetUint64(maxFee)))
	need.Mul(need, big.NewInt(tokenPMPrice)).Div(need, coinUnit)
	amount := new(big.Int).Mul(need, big.NewInt(3))
	if one := unitOf(ch.tokenDecimals(token)); amount.Cmp(one) < 0 {
		amount = one
	}
	usdc, err := loadArtifact("TestUSDC") // USDC's calls: nonces, permit
	if err != nil {
		return nil, err
	}
	data, _ := usdc.abi.Pack("nonces", r.Account)
	out, err := ch.b.CallContract(ctxb(), ethereum.CallMsg{To: &token, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("the token's permit nonce: %w", err)
	}
	nonce := new(big.Int).SetBytes(out)
	args := argsOf("bytes32", "address", "address", "uint256", "uint256", "uint256")
	th := crypto.Keccak256Hash([]byte("Permit(address owner,address spender,uint256 value,uint256 nonce,uint256 deadline)"))
	sb, err := args.Pack(th, r.Account, pm, amount, nonce, maxUint256)
	if err != nil {
		return nil, err
	}
	// USDC's EIP-712 domain, read from the token: "USDC" "2" on Base Sepolia (and the devnet's TestUSDC),
	// "USD Coin" "2" on Base mainnet (a wrong name: the permit fails and the paymaster finds no allowance)
	dn, dv := ch.tokenDomain(token, "USDC", "2")
	digest := crypto.Keccak256Hash([]byte{0x19, 0x01}, eip712DomainSeparator(dn, dv, ch.chainID, token).Bytes(), crypto.Keccak256(sb))
	var sig []byte
	switch {
	case r.Passkey != nil:
		sig, err = webauthnSign(r.Passkey, digest)
	case r.Session != nil:
		sig, err = crypto.Sign(digest[:], r.Session)
		if err == nil {
			sig[64] += 27
		}
	default:
		err = errors.New("no key to sign the gas permit")
	}
	if err != nil {
		return nil, err
	}
	gl := pack128(tokenPMVerification, tokenPMPostOp)
	pd := append(append(pm.Bytes(), gl[:]...), 0x00)
	pd = append(pd, token.Bytes()...)
	pd = append(pd, common.BigToHash(amount).Bytes()...)
	return append(pd, sig...), nil
}

// tokenOf: an address's balance of a token (its own, not the bank's).
func (c *Client) tokenOf(token, who common.Address) (*big.Int, error) {
	erc20, err := erc20ABI()
	if err != nil {
		return nil, err
	}
	data, _ := erc20.Pack("balanceOf", who)
	out, err := c.ch.b.CallContract(ctxb(), ethereum.CallMsg{To: &token, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(out), nil
}
