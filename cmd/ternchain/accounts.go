// Smart accounts (ERC-4337 v0.8) with passkeys.
//
// An owner's name is held by an OwnerAccount guarded by a passkey (P-256,
// WebAuthn). Here the "authenticator" is a key file in the owner's folder;
// on a phone it is the secure enclave, and the key never leaves it. The
// owner's old secp256k1 key becomes the mail key: it signs messages only.
//
// An agent's name is held by an AgentAccount whose session key is the agent's
// key. The agent has no ETH: TernPaymaster pays its gas from the owner's
// deposit, within the budget per epoch the owner set.
//
// Every send of a routed key becomes a user operation: execute(batch of one
// call) on the account, signed, and submitted to the EntryPoint by a bundler
// (any node runner; on the devnet, the faucet key).
package main

//lint:file-ignore SA1019 raw curve coordinates: secp256k1 (go-ethereum) and the P-256 passkey verifier need them; crypto/ecdh has no secp256k1

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/nenkoru/tern/internal/zk"
)

type AccountRoute struct {
	Account  common.Address
	Name     string            // the Tern name it holds (paymaster data)
	Passkey  *ecdsa.PrivateKey // P-256: owners
	Session  *ecdsa.PrivateKey // secp256k1: agents
	InitCode []byte            // factory call, used until the account exists
	IsAgent  bool
}

type packedUserOp struct {
	Sender             common.Address
	Nonce              *big.Int
	InitCode           []byte
	CallData           []byte
	AccountGasLimits   [32]byte
	PreVerificationGas *big.Int
	GasFees            [32]byte
	PaymasterAndData   []byte
	Signature          []byte
}

func pack128(hi, lo uint64) (out [32]byte) {
	new(big.Int).SetUint64(hi).FillBytes(out[0:16])
	new(big.Int).SetUint64(lo).FillBytes(out[16:32])
	return
}

const (
	verificationGas = 1_500_000 // P-256 in Solidity costs ~300k if the chain has no precompile
	sessionVerify   = 300_000   // a session key: one ECDSA recovery
	callGas         = 2_000_000 // the most a call may take; ops ask for their estimate
	typicalCallGas  = 250_000   // what a payment or a post asks for (estimate + headroom)
	typicalUsedGas  = 200_000   // what such an operation uses in all (measured: ~185k)
	preVerification = 100_000
	pmVerification  = 200_000
	pmPostOp        = 80_000
)

// batchMode: ERC-7579 mode for a batch with the default exec type (ERC-7821).
var batchMode = [32]byte{0x01}

func (ch *Chain) executeCallData(target common.Address, value *big.Int, data []byte) ([]byte, error) {
	if value == nil {
		value = new(big.Int)
	}
	callT, _ := abi.NewType("tuple[]", "", []abi.ArgumentMarshaling{{Name: "target", Type: "address"}, {Name: "value", Type: "uint256"}, {Name: "data", Type: "bytes"}})
	type call struct {
		Target common.Address
		Value  *big.Int
		Data   []byte
	}
	exec, err := abi.Arguments{{Type: callT}}.Pack([]call{{target, value, data}})
	if err != nil {
		return nil, err
	}
	return ch.abis["OwnerAccount"].Pack("execute", batchMode, exec) // same ERC-7821 entry on both accounts
}

func (ch *Chain) addr(contract string) common.Address {
	return common.HexToAddress(ch.dep.Contracts[contract])
}

// sendViaAccount: the call as a user operation from the route's account.
func (ch *Chain) sendViaAccount(r *AccountRoute, value *big.Int, contract, method string, args ...any) (*types.Receipt, error) {
	if _, ok := ch.c[contract]; !ok {
		return nil, fmt.Errorf("contract %s not deployed", contract)
	}
	data, err := ch.abis[contract].Pack(method, args...)
	if err != nil {
		return nil, err
	}
	cd, err := ch.executeCallData(ch.addr(contract), value, data)
	if err != nil {
		return nil, err
	}
	rcpt, err := ch.userOp(r, cd)
	if err != nil {
		return rcpt, fmt.Errorf("%s.%s: %w", contract, method, err)
	}
	return rcpt, nil
}

func (ch *Chain) userOp(r *AccountRoute, callData []byte) (*types.Receipt, error) {
	if ch.bundler == nil && ch.bundlerURL == "" {
		return nil, errors.New("no bundler: set one (a node that accepts user operations; net.json \"bundler\") to use a smart account")
	}
	ctx := ctxb()
	op := packedUserOp{Sender: r.Account, CallData: callData, PreVerificationGas: big.NewInt(preVerification)}
	// Tight limits: the paymaster reserves (and an agent's budget must hold)
	// the maximum cost of every operation, and the EntryPoint charges for
	// unused call gas, so ask for what the call needs, not the ceiling.
	vgas, cgas := uint64(verificationGas), uint64(callGas)
	if r.Session != nil {
		vgas = sessionVerify
	}
	if code, err := ch.b.CodeAt(ctx, r.Account, nil); err == nil && len(code) == 0 {
		op.InitCode = r.InitCode
		vgas = verificationGas + 3_000_000 // deploying the account (~7 kB of code) happens during validation
	} else {
		cgas = ch.callGasFor(r.Account, callData)
	}
	op.AccountGasLimits = pack128(vgas, cgas)
	n, err := ch.call("EntryPoint", "getNonce", r.Account, new(big.Int))
	if err != nil {
		return nil, err
	}
	op.Nonce = n[0].(*big.Int)
	h, err := ch.b.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	tip := ch.tipCap()
	maxFee := tip
	if h.BaseFee != nil {
		maxFee += 2 * h.BaseFee.Uint64()
	}
	if t, m, ok := ch.bundlerGasPrice(); ok { // a public bundler's own price (Pimlico): it refuses less
		tip, maxFee = t, max(m, maxFee)
	}
	op.GasFees = pack128(tip, maxFee)
	// Sponsored by the network (SponsorPaymaster: a host chain's network pays
	// its accounts' gas within daily caps), else by the owner's paymaster
	// deposit when there is one; agents always are.
	if ch.tokenGas() { // the user pays: in USDC, through the network's ERC-20 paymaster (gas.go)
		pd, err := ch.tokenPaymasterData(r, vgas, cgas, maxFee)
		if err != nil {
			return nil, err
		}
		op.PaymasterAndData = pd
	} else if ch.has("SponsorPaymaster") {
		gl := pack128(pmVerification, pmPostOp)
		pd := append(ch.addr("SponsorPaymaster").Bytes(), gl[:]...)
		op.PaymasterAndData = append(pd, idOf(r.Name).Bytes()...)
	} else if ch.sponsored(r, maxFee, vgas, cgas) {
		gl := pack128(pmVerification, pmPostOp)
		pd := append(ch.addr("TernPaymaster").Bytes(), gl[:]...)
		op.PaymasterAndData = append(pd, idOf(r.Name).Bytes()...)
	}
	// A bundler that estimates (a public one) says what it will accept: its
	// preVerificationGas counts the L1 data fee on an L2. Take the larger of
	// its numbers and ours; a bundler that doesn't estimate (ours) takes ours.
	if est, ok := ch.bundlerEstimate(op); ok {
		up := func(ours uint64, theirs *big.Int) uint64 {
			if theirs != nil && theirs.IsUint64() {
				return max(ours, theirs.Uint64()*11/10)
			}
			return ours
		}
		vgas, cgas = up(vgas, bigOrNil(est.VerificationGasLimit)), up(cgas, bigOrNil(est.CallGasLimit))
		op.AccountGasLimits = pack128(vgas, cgas)
		op.PreVerificationGas = new(big.Int).SetUint64(up(preVerification, bigOrNil(est.PreVerificationGas)))
		if ch.tokenGas() {
			if op.PaymasterAndData, err = ch.tokenPaymasterData(r, vgas, cgas, maxFee); err != nil {
				return nil, err
			}
		}
	}
	hash, err := ch.call("EntryPoint", "getUserOpHash", op)
	if err != nil {
		return nil, err
	}
	digest := common.Hash(hash[0].([32]byte))
	if r.Passkey != nil {
		op.Signature, err = webauthnSign(r.Passkey, digest)
	} else {
		op.Signature, err = crypto.Sign(digest[:], r.Session)
		if err == nil {
			op.Signature[64] += 27
		}
	}
	if err != nil {
		return nil, err
	}
	rc, err := ch.submitOps([]packedUserOp{op})
	if err != nil && ch.tokenGas() && strings.Contains(err.Error(), "AA33") { // the token paymaster couldn't take its prefund
		t := common.HexToAddress(ch.gas.Token)
		err = fmt.Errorf("%w: the account %s pays its own gas in %s and has too little: send it some (its own, not the bank's)", err, r.Account.Hex(), ch.assetSymbol(t))
	}
	if err != nil && strings.Contains(err.Error(), "gas deposit is too low") {
		o := ownerName(r.Name)
		err = fmt.Errorf("%w (%s's paymaster deposit pays this account's gas): send the account %s some ETH of its own, "+
			"or on a local devnet: ternchain gas fund %s AMOUNT --unsafe-paymaster", err, o, r.Account.Hex(), o)
	}
	return rc, err
}

// callGasFor estimates the account's execution of callData (as the EntryPoint
// calls it), with headroom; the ceiling if it can't (a failing call keeps its
// real revert reason that way).
func (ch *Chain) callGasFor(account common.Address, callData []byte) uint64 {
	ep := ch.addr("EntryPoint")
	est, err := ch.b.EstimateGas(ctxb(), ethereum.CallMsg{From: ep, To: &account, Data: callData})
	if err != nil {
		return callGas
	}
	return min(est*3/2+60_000, callGas)
}

// opReserve: what the paymaster sets aside for one operation (its maximum cost).
func opReserve(vgas, cgas, maxFee uint64) *big.Int {
	return new(big.Int).Mul(new(big.Int).SetUint64(vgas+cgas+preVerification+pmVerification+pmPostOp), new(big.Int).SetUint64(maxFee))
}

// maxFeeNow: the fee per gas a user operation offers now.
func (ch *Chain) maxFeeNow() uint64 {
	fee := ch.tipCap()
	if h, err := ch.b.HeaderByNumber(ctxb(), nil); err == nil && h.BaseFee != nil {
		fee += 2 * h.BaseFee.Uint64()
	}
	return fee
}

// sponsored: does TernPaymaster pay this operation? Never off a devnet (its
// deposits can be drained, audit H-1: the account pays its own ETH there).
// An agent uses its owner's deposit, unless there is none and its account
// holds ETH of its own (owner grant --gas without --unsafe-paymaster).
func (ch *Chain) sponsored(r *AccountRoute, maxFee, vgas, cgas uint64) bool {
	if !ch.devnet() {
		return false
	}
	d, err := ch.call("TernPaymaster", "deposit", idOf(ownerName(r.Name)))
	if err != nil {
		return false
	}
	if r.IsAgent {
		if d[0].(*big.Int).Sign() > 0 {
			return true
		}
		b, err := ch.balance(r.Account)
		if err != nil {
			return true
		}
		if ep, err := ch.call("EntryPoint", "balanceOf", r.Account); err == nil { // earlier prefunds' change lands there
			b.Add(b, ep[0].(*big.Int))
		}
		return b.Sign() == 0 // no ETH either: the paymaster's error says what to do
	}
	return d[0].(*big.Int).Cmp(opReserve(vgas, cgas, maxFee)) >= 0
}

// devnet: a local devnet's chain id (1337, 31337), where coins are worthless.
func (ch *Chain) devnet() bool { return zk.IsDevnetChain(ch.chainID.Uint64()) }

// errUnsafePaymaster: TernPaymaster pools every owner's deposit in one
// EntryPoint balance and its postOp charges less than the EntryPoint takes,
// so whoever bundles their own operations drains the others' deposits (audit
// core H-1). The deployed contracts can't change: this client puts no ETH in
// it, except on a local devnet with --unsafe-paymaster.
var errUnsafePaymaster = errors.New("this client won't deposit into TernPaymaster: anyone who bundles their own user operations can drain " +
	"the ETH deposited in it (audit H-1). Pay gas in USDC through Circle Paymaster (a network whose net.json \"gas\" names it, " +
	"such as Base Sepolia) or with your own ETH (send it to the account; owner grant --gas sends it to an agent's account). " +
	"A deposit already in it comes out with: gas withdraw")

// paymasterFundOK: may this process deposit into TernPaymaster?
func (ch *Chain) paymasterFundOK() error {
	if !ch.devnet() {
		return fmt.Errorf("%w (--unsafe-paymaster only works on a local devnet, chain id 1337 or 31337; this is %s)", errUnsafePaymaster, ch.chainID)
	}
	if !ch.unsafePaymaster {
		return fmt.Errorf("%w (on this local devnet: add --unsafe-paymaster)", errUnsafePaymaster)
	}
	return nil
}

// submitOps: the bundler's part. Simulate first (a failed validation would
// cost the bundler), then send handleOps and read the operation's outcome.
// Without a bundler key: the network's bundler (net.json "bundler") does it.
func (ch *Chain) submitOps(ops []packedUserOp) (*types.Receipt, error) {
	if ch.bundler == nil && ch.bundlerURL != "" && len(ops) == 1 {
		return ch.submitRemote(ops[0])
	}
	return ch.submitOpsWith(ch.bundler, ops)
}

func (ch *Chain) submitOpsWith(bundler *ecdsa.PrivateKey, ops []packedUserOp) (*types.Receipt, error) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	beneficiary := crypto.PubkeyToAddress(bundler.PublicKey)
	defer ch.lockSender(beneficiary)() // the bundler may be a key that also sends plain transactions (the devnet faucet)
	data, err := ch.abis["EntryPoint"].Pack("handleOps", ops, beneficiary)
	if err != nil {
		return nil, err
	}
	ep := ch.addr("EntryPoint")
	if _, err := ch.b.CallContract(ctxb(), ethereum.CallMsg{From: beneficiary, To: &ep, Data: data, Gas: 12_000_000}, nil); err != nil {
		return nil, ch.entryPointError(err)
	}
	opts, err := ch.opts(bundler, nil)
	if err != nil {
		return nil, err
	}
	opts.GasLimit = 12_000_000
	tx, err := ch.c["EntryPoint"].RawTransact(opts, data)
	if err != nil {
		return nil, ch.entryPointError(err)
	}
	rcpt, err := ch.wait(tx)
	if err != nil {
		return rcpt, err
	}
	return rcpt, ch.opOutcome(rcpt, common.Hash{})
}

// entryPointError turns FailedOp / FailedOpWithRevert into a readable error.
func (ch *Chain) entryPointError(err error) error {
	var de interface{ ErrorData() any }
	if errors.As(err, &de) {
		if s, ok := de.ErrorData().(string); ok {
			if raw, e := hex.DecodeString(strings.TrimPrefix(s, "0x")); e == nil && len(raw) >= 4 {
				for _, name := range []string{"FailedOp", "FailedOpWithRevert"} {
					e := ch.abis["EntryPoint"].Errors[name]
					if [4]byte(raw[:4]) == [4]byte(e.ID[:4]) {
						v, err := e.Inputs.Unpack(raw[4:])
						if err == nil && len(v) >= 2 {
							msg := v[1].(string)
							if len(v) == 3 {
								msg += ": " + decodeRevert(v[2].([]byte))
							}
							return errors.New(msg)
						}
					}
				}
				return errors.New(decodeRevert(raw))
			}
		}
	}
	return cleanErr(err)
}

// decodeRevert reads Error(string), or shows the raw data.
func decodeRevert(b []byte) string {
	if len(b) >= 4 && hex.EncodeToString(b[:4]) == "08c379a0" {
		if v, err := (abi.Arguments{{Type: abiType("string")}}).Unpack(b[4:]); err == nil {
			return v[0].(string)
		}
	}
	if len(b) == 0 {
		return "reverted"
	}
	return "reverted: 0x" + hex.EncodeToString(b)
}

// ---- WebAuthn: what a browser's navigator.credentials.get() returns

const rpID = "tern.local"

func webauthnSign(k *ecdsa.PrivateKey, challenge common.Hash) ([]byte, error) {
	client := `{"type":"webauthn.get","challenge":"` + base64.RawURLEncoding.EncodeToString(challenge[:]) +
		`","origin":"https://` + rpID + `","crossOrigin":false}`
	rp := sha256.Sum256([]byte(rpID))
	authData := append(rp[:], 0x05, 0, 0, 0, 1) // flags: user present + user verified; sign count 1
	ch := sha256.Sum256([]byte(client))
	msg := sha256.Sum256(append(append([]byte{}, authData...), ch[:]...))
	r, s, err := ecdsa.Sign(rand.Reader, k, msg[:])
	if err != nil {
		return nil, err
	}
	n := elliptic.P256().Params().N
	if s.Cmp(new(big.Int).Rsh(n, 1)) > 0 { // low-s, as P256.verify requires
		s = new(big.Int).Sub(n, s)
	}
	var rb, sb [32]byte
	r.FillBytes(rb[:])
	s.FillBytes(sb[:])
	// The WebAuthnAuth fields encoded in place (OZ WebAuthn.tryDecodeAuth reads them without an outer offset).
	args := abi.Arguments{{Type: abiType("bytes32")}, {Type: abiType("bytes32")}, {Type: abiType("uint256")},
		{Type: abiType("uint256")}, {Type: abiType("bytes")}, {Type: abiType("string")}}
	return args.Pack(rb, sb, big.NewInt(int64(strings.Index(client, `"challenge":`))), big.NewInt(int64(strings.Index(client, `"type":`))), authData, client)
}

func passkeyXY(k *ecdsa.PrivateKey) (qx, qy [32]byte) {
	k.PublicKey.X.FillBytes(qx[:])
	k.PublicKey.Y.FillBytes(qy[:])
	return
}

func loadPasskey(dir string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(filepath.Join(dir, "passkey.key"))
	if err != nil {
		return nil, err
	}
	d, ok := new(big.Int).SetString(strings.TrimSpace(string(b)), 16)
	if !ok {
		return nil, errors.New("bad passkey file")
	}
	k := &ecdsa.PrivateKey{D: d}
	k.PublicKey.Curve = elliptic.P256()
	k.PublicKey.X, k.PublicKey.Y = elliptic.P256().ScalarBaseMult(d.Bytes())
	return k, nil
}

// ---- turning an identity into a smart account

// enablePasskeyAccount: an owner's name will be held by a passkey account.
// Call before registering the name.
func (c *Client) enablePasskeyAccount() error {
	if !c.isOwner() || c.f.Account != "" {
		return errors.New("an owner without an account only")
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.dir, "passkey.key"), []byte(k.D.Text(16)+"\n"), 0o600); err != nil {
		return err
	}
	qx, qy := passkeyXY(k)
	salt := idOf(c.f.Name)
	out, err := c.ch.call("AccountFactory", "ownerAddress", qx, qy, c.addr(), salt)
	if err != nil {
		return err
	}
	data, _ := c.ch.abis["AccountFactory"].Pack("ownerAccount", qx, qy, c.addr(), salt)
	c.f.Account = out[0].(common.Address).Hex()
	c.f.AccountInit = hex.EncodeToString(append(c.ch.addr("AccountFactory").Bytes(), data...))
	if err := writeJSON(filepath.Join(c.dir, "identity.json"), c.f); err != nil {
		return err
	}
	return c.installRoute()
}

// enableAgentAccount: the agent's key becomes the session key of its account.
func (c *Client) enableAgentAccount() error {
	if c.isOwner() || c.f.Account != "" {
		return errors.New("an agent without an account only")
	}
	return c.enableKeyAccount()
}

// enableKeyAccount: the identity's key becomes the only signer of a smart
// account (AgentAccount: ECDSA) that holds its name. An owner's too, on a
// network whose paymaster pays accounts' gas: no ETH needed to take a name.
func (c *Client) enableKeyAccount() error {
	if c.f.Account != "" {
		return errors.New("this identity has an account already: " + c.f.Account)
	}
	salt := idOf(c.f.Name)
	out, err := c.ch.call("AccountFactory", "agentAddress", c.addr(), salt)
	if err != nil {
		return err
	}
	data, _ := c.ch.abis["AccountFactory"].Pack("agentAccount", c.addr(), salt)
	c.f.Account, c.f.AccountKey = out[0].(common.Address).Hex(), c.isOwner()
	c.f.AccountInit = hex.EncodeToString(append(c.ch.addr("AccountFactory").Bytes(), data...))
	if err := writeJSON(filepath.Join(c.dir, "identity.json"), c.f); err != nil {
		return err
	}
	return c.installRoute()
}

func (c *Client) installRoute() error {
	if c.f.Account == "" {
		return nil
	}
	init, _ := hex.DecodeString(c.f.AccountInit)
	r := &AccountRoute{Account: common.HexToAddress(c.f.Account), Name: c.f.Name, InitCode: init, IsAgent: !c.isOwner()}
	if c.isOwner() && !c.f.AccountKey {
		k, err := loadPasskey(c.dir)
		if err != nil {
			return fmt.Errorf("passkey: %w", err)
		}
		r.Passkey = k
	} else { // an agent's account, or an owner's guarded by its key (enableKeyAccount)
		r.Session = c.key
	}
	c.ch.routes[c.addr()] = r
	return nil
}

// me: this identity's address on chain (its account, or its key).
func (c *Client) me() common.Address {
	if c.f.Account != "" {
		return common.HexToAddress(c.f.Account)
	}
	return c.addr()
}

// signerOf: for a smart account, the key that signs its off-chain messages
// (an owner account's mail key, an agent account's session key).
func (ch *Chain) signerOf(a common.Address) (common.Address, bool) {
	code, err := ch.b.CodeAt(ctxb(), a, nil)
	if err != nil || len(code) == 0 {
		return common.Address{}, false
	}
	for _, try := range []struct{ abi, method string }{{"OwnerAccount", "mailKey"}, {"AgentAccount", "signer"}} {
		bc := bind.NewBoundContract(a, ch.abis[try.abi], ch.b, ch.b, ch.b)
		var out []any
		if err := bc.Call(&bind.CallOpts{Context: ctxb()}, &out, try.method); err == nil && len(out) == 1 {
			if k, ok := out[0].(common.Address); ok && k != (common.Address{}) {
				return k, true
			}
		}
	}
	return common.Address{}, false
}

// ---- gas through the paymaster

func (c *Client) fundGas(owner string, amount *big.Int) error {
	_, err := c.ch.send(c.key, amount, "TernPaymaster", "fund", idOf(owner)) // send refuses it unless paymasterFundOK
	return err
}

// withdrawGas takes the owner's TernPaymaster deposit (all of it when amount
// is nil) out to the address to. It is the way out of H-1 and always allowed.
func (c *Client) withdrawGas(amount *big.Int, to common.Address) (*big.Int, error) {
	if !c.isOwner() {
		return nil, errors.New("only an owner withdraws its gas deposit: run it with the owner's --dir")
	}
	d, err := c.ch.call("TernPaymaster", "deposit", c.id())
	if err != nil {
		return nil, err
	}
	have := d[0].(*big.Int)
	if amount == nil {
		amount = have
	}
	if amount.Sign() == 0 {
		return amount, errors.New("your TernPaymaster deposit is empty: nothing to withdraw")
	}
	if amount.Cmp(have) > 0 {
		return nil, fmt.Errorf("your TernPaymaster deposit is %s, less than %s", fmtCoins(have), fmtCoins(amount))
	}
	_, err = c.ch.send(c.key, nil, "TernPaymaster", "withdraw", c.id(), to, amount)
	return amount, err
}

func (c *Client) setGasBudget(agent string, perEpoch *big.Int) error {
	_, err := c.ch.send(c.key, nil, "TernPaymaster", "setGasBudget", idOf(agent), perEpoch)
	return err
}
