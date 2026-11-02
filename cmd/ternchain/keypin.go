// Correspondents' keys, pinned (red team C-RPC).
//
// The directory (who holds a name, with which keys) is read from Registry
// logs, and an RPC can add, hide or alter logs: one forged KeysPublished log
// made a sender seal to an attacker's keys without a word. Now:
//
//   - A publish counts only when its transaction proves it: a plain
//     transaction's sender (recovered from its signature) is the name's
//     controller at that block (an agent's: its granted key or its owner's
//     controller) and its calldata is that very publishKeys; or, through the
//     ERC-4337 EntryPoint, a user operation in handleOps from the controller's
//     account carries it, signed (userOpHash, computed here) by the account's
//     key. An RPC can't make either signature. Anything else is ignored, and
//     a log its sender contradicts is reported as a forgery.
//   - Pins (State.KeyPins): per name, the controller, its version and an
//     account's key, and the newest publish taken (its block and first epoch),
//     on first use. Re-publishing (every few epochs, or rekey) changes no pin.
//     A controller change its old key signed (setController) is followed. A
//     guardian recovery, a change no transaction of the old key proves, or a
//     new account key is a KEY CHANGE: an alert, and nothing is sealed to the
//     name until its owner checks out of band and accepts ('pins accept
//     NAME'; for an agent, its owner's 'owner approve AGENT keys:NAME:ID').
//     A newest publish older than the pinned one (an RPC hiding newer keys)
//     is refused.
//   - Witnesses: with a second RPC in net.json (or, strict, the call RPC
//     beside the log RPC), a first pin and every pin change are checked
//     against its state at the same block (Registry.owners, grants,
//     keysCommit, the account's key) and that block's hash. Strict privacy
//     refuses a first pin no second RPC confirms. One RPC, standard: trust on
//     first use, plus the signature check above.
//   - verify_blocks (net.json, or TERNCHAIN_VERIFY_BLOCKS=1): the pinned log's
//     block hash must also be the second RPC's at that height and, on a chain
//     of our own, its transaction and receipt must be in that block (the
//     header's transactions and receipts roots). docs/THREATS-STATE.md says
//     what an OP-stack host chain does and doesn't let a client check.
package main

//lint:file-ignore SA1019 the P-256 passkey's raw coordinates (as accounts.go)

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/trie"
)

// ---- which transaction a log came from

const (
	authOK      = iota
	authForged  // the transaction says otherwise: another sender, another call, no signature of the account's key
	authUnknown // can't tell now: the RPC has no such transaction, an account or a bundler of a kind not read here
)

type authRes struct {
	v   int
	why string
}

func logBefore(a, b event) bool { return a.Block < b.Block || a.Block == b.Block && a.Index < b.Index }

func shortHash(h common.Hash) string { return h.Hex()[:12] + "…" }

// authTx: is txh a transaction from one of who (a key; or an account, through
// the EntryPoint, its operation signed by the account's key) that calls the
// Registry as want says? Definite answers are kept under key.
func (r *Resolver) authTx(key string, txh common.Hash, who []common.Address, want func(method string, args []any) bool) authRes {
	r.mu.Lock()
	a, ok := r.auth[key]
	r.mu.Unlock()
	if ok {
		return a
	}
	a = r.checkTx(txh, who, want)
	if a.v != authUnknown {
		r.mu.Lock()
		if r.auth == nil {
			r.auth = map[string]authRes{}
		}
		r.auth[key] = a
		r.mu.Unlock()
	}
	return a
}

func (r *Resolver) checkTx(txh common.Hash, who []common.Address, want func(string, []any) bool) authRes {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, _, err := r.ch.b.TransactionByHash(ctx, txh)
	if errors.Is(err, ethereum.NotFound) {
		return authRes{authUnknown, "the RPC has no transaction " + shortHash(txh)}
	}
	if err != nil {
		return authRes{authUnknown, "reading transaction " + shortHash(txh) + ": " + err.Error()}
	}
	if tx.Hash() != txh {
		return authRes{authForged, "the RPC's transaction " + shortHash(txh) + " hashes to another"}
	}
	if !tx.Protected() || tx.ChainId().Cmp(r.ch.chainID) != 0 {
		return authRes{authForged, "transaction " + shortHash(txh) + " is not bound to this chain"}
	}
	from, err := types.Sender(types.LatestSignerForChainID(r.ch.chainID), tx)
	if err != nil || tx.To() == nil {
		return authRes{authForged, "transaction " + shortHash(txh) + " has no valid signature or recipient"}
	}
	switch to := *tx.To(); {
	case to == r.ch.addr("Registry"):
		if !slices.Contains(who, from) {
			return authRes{authForged, fmt.Sprintf("transaction %s was sent by %s, not by the name's key %s", shortHash(txh), from.Hex(), addrList(who))}
		}
		if m, args, ok := r.ch.decodeCall("Registry", tx.Data()); !ok || !want(m, args) {
			return authRes{authForged, fmt.Sprintf("transaction %s calls Registry.%s with other arguments", shortHash(txh), m)}
		}
		return authRes{v: authOK}
	case r.ch.has("EntryPoint") && to == r.ch.addr("EntryPoint"):
		return r.checkOps(txh, tx.Data(), who, want)
	}
	return authRes{authUnknown, "transaction " + shortHash(txh) + " goes through a contract this client doesn't read"}
}

func addrList(as []common.Address) string {
	var s []string
	for _, a := range as {
		s = append(s, a.Hex())
	}
	return strings.Join(s, " or ")
}

// checkOps: a handleOps bundle holds an operation from one of who that makes
// the call, signed by that account's key.
func (r *Resolver) checkOps(txh common.Hash, data []byte, who []common.Address, want func(string, []any) bool) authRes {
	m, args, ok := r.ch.decodeCall("EntryPoint", data)
	if !ok || m != "handleOps" {
		return authRes{authUnknown, "transaction " + shortHash(txh) + " is an EntryPoint call other than handleOps"}
	}
	var in struct {
		Ops         []packedUserOp
		Beneficiary common.Address
	}
	if err := r.ch.abis["EntryPoint"].Methods["handleOps"].Inputs.Copy(&in, args); err != nil {
		return authRes{authUnknown, "transaction " + shortHash(txh) + ": " + err.Error()}
	}
	res := authRes{authForged, fmt.Sprintf("no operation in bundle %s from %s makes this call", shortHash(txh), addrList(who))}
	for _, op := range in.Ops {
		if !slices.Contains(who, op.Sender) {
			continue
		}
		calls, ok := r.ch.executeCalls(op.CallData)
		if !ok {
			res = authRes{authUnknown, "an operation of " + op.Sender.Hex() + " in a form this client doesn't read"}
			continue
		}
		hit := false
		for _, c := range calls {
			if c.Target == r.ch.addr("Registry") {
				if m, a, ok := r.ch.decodeCall("Registry", c.Data); ok && want(m, a) {
					hit = true
				}
			}
		}
		if !hit {
			continue
		}
		k, err := r.accountKey(op.Sender)
		if err != nil || !strings.Contains(k, ":") || strings.HasPrefix(k, "code:") {
			return authRes{authUnknown, "the key of account " + op.Sender.Hex() + " can't be read"}
		}
		if is7702(op.InitCode) {
			return authRes{authUnknown, "an EIP-7702 operation (not checked here)"}
		}
		if !opSigned(op.Signature, userOpHash(op, r.ch.addr("EntryPoint"), r.ch.chainID), k) {
			return authRes{authForged, fmt.Sprintf("the operation of %s in bundle %s isn't signed by the account's key", op.Sender.Hex(), shortHash(txh))}
		}
		return authRes{v: authOK}
	}
	return res
}

func (ch *Chain) decodeCall(contract string, data []byte) (string, []any, bool) {
	a, ok := ch.abis[contract]
	if !ok || len(data) < 4 {
		return "", nil, false
	}
	m, err := a.MethodById(data[:4])
	if err != nil {
		return "", nil, false
	}
	args, err := m.Inputs.Unpack(data[4:])
	return m.Name, args, err == nil
}

type execCall struct {
	Target common.Address
	Value  *big.Int
	Data   []byte
}

// executeCalls: the calls an account's execute(mode, data) makes (ERC-7579
// single or batch, the default exec type).
func (ch *Chain) executeCalls(cd []byte) ([]execCall, bool) {
	m, args, ok := ch.decodeCall("OwnerAccount", cd)
	if !ok || m != "execute" {
		return nil, false
	}
	mode, ed := args[0].([32]byte), args[1].([]byte)
	switch mode {
	case batchMode:
		callT, _ := abi.NewType("tuple[]", "", []abi.ArgumentMarshaling{{Name: "target", Type: "address"}, {Name: "value", Type: "uint256"}, {Name: "data", Type: "bytes"}})
		as := abi.Arguments{{Type: callT}}
		v, err := as.Unpack(ed)
		if err != nil {
			return nil, false
		}
		var out []execCall
		if err := as.Copy(&out, v); err != nil {
			return nil, false
		}
		return out, true
	case [32]byte{}:
		if len(ed) < 52 {
			return nil, false
		}
		return []execCall{{common.BytesToAddress(ed[:20]), new(big.Int).SetBytes(ed[20:52]), ed[52:]}}, true
	}
	return nil, false
}

func is7702(initCode []byte) bool {
	return len(initCode) >= 20 && initCode[0] == 0x77 && initCode[1] == 0x02 && bytes.Equal(initCode[2:20], make([]byte, 18))
}

var (
	opTypeHash = crypto.Keccak256Hash([]byte("PackedUserOperation(address sender,uint256 nonce,bytes initCode,bytes callData," +
		"bytes32 accountGasLimits,uint256 preVerificationGas,bytes32 gasFees,bytes paymasterAndData)"))
	domainTypeHash = crypto.Keccak256Hash([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
)

// userOpHash: EntryPoint v0.8's getUserOpHash (EIP-712), computed here: the
// RPC isn't asked what was signed.
func userOpHash(op packedUserOp, ep common.Address, chainID *big.Int) common.Hash {
	b32, u256, addr := abi.Argument{Type: abiType("bytes32")}, abi.Argument{Type: abiType("uint256")}, abi.Argument{Type: abiType("address")}
	enc, _ := abi.Arguments{b32, addr, u256, b32, b32, b32, u256, b32, b32}.Pack(opTypeHash, op.Sender, op.Nonce,
		crypto.Keccak256Hash(op.InitCode), crypto.Keccak256Hash(op.CallData), op.AccountGasLimits, op.PreVerificationGas, op.GasFees,
		crypto.Keccak256Hash(op.PaymasterAndData))
	dom, _ := abi.Arguments{b32, b32, b32, u256, addr}.Pack(domainTypeHash, crypto.Keccak256Hash([]byte("ERC4337")),
		crypto.Keccak256Hash([]byte("1")), chainID, ep)
	return crypto.Keccak256Hash([]byte{0x19, 0x01}, crypto.Keccak256(dom), crypto.Keccak256(enc))
}

// opSigned: sig is key's signature of h: "ecdsa:ADDRESS" (an agent's or a
// key account: 65 bytes) or "p256:QXQY" (a passkey: a WebAuthn assertion as
// webauthnSign makes it, or raw r‖s).
func opSigned(sig []byte, h common.Hash, key string) bool {
	switch kind, v, _ := strings.Cut(key, ":"); kind {
	case "ecdsa":
		if len(sig) != 65 {
			return false
		}
		s := append([]byte{}, sig...)
		if s[64] >= 27 {
			s[64] -= 27
		}
		pub, err := crypto.SigToPub(h[:], s)
		return err == nil && crypto.PubkeyToAddress(*pub) == common.HexToAddress(v)
	case "p256":
		xy, err := hex.DecodeString(v)
		if err != nil || len(xy) != 64 {
			return false
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xy[:32]), Y: new(big.Int).SetBytes(xy[32:])}
		if len(sig) == 64 {
			return ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
		}
		return webauthnVerify(pub, h, sig)
	}
	return false
}

// webauthnVerify: an assertion (OZ WebAuthn's encoding) whose client data
// carries challenge, signed by pub.
func webauthnVerify(pub *ecdsa.PublicKey, challenge common.Hash, sig []byte) bool {
	args := abi.Arguments{{Type: abiType("bytes32")}, {Type: abiType("bytes32")}, {Type: abiType("uint256")},
		{Type: abiType("uint256")}, {Type: abiType("bytes")}, {Type: abiType("string")}}
	v, err := args.Unpack(sig)
	if err != nil {
		return false
	}
	r, s := v[0].([32]byte), v[1].([32]byte)
	ci, ti := v[2].(*big.Int), v[3].(*big.Int)
	authData, client := v[4].([]byte), v[5].(string)
	at := func(i *big.Int, want string) bool {
		return i.IsInt64() && i.Int64() >= 0 && i.Int64() <= int64(len(client)) && strings.HasPrefix(client[i.Int64():], want)
	}
	if !at(ci, `"challenge":"`+base64.RawURLEncoding.EncodeToString(challenge[:])+`"`) || !at(ti, `"type":"webauthn.get"`) ||
		len(authData) < 37 || authData[32]&0x01 == 0 {
		return false
	}
	cd := sha256.Sum256([]byte(client))
	msg := sha256.Sum256(append(append([]byte{}, authData...), cd[:]...))
	return ecdsa.Verify(pub, msg[:], new(big.Int).SetBytes(r[:]), new(big.Int).SetBytes(s[:]))
}

// accountKey: the key that signs an address's operations: "" a plain key (no
// code), "p256:QXQY" an owner's passkey account, "ecdsa:0x…" an agent's or a
// key account, "code:HASH" an account of another kind.
func (r *Resolver) accountKey(a common.Address) (string, error) {
	r.mu.Lock()
	k, ok := r.acct[a]
	r.mu.Unlock()
	if ok {
		return k, nil
	}
	k, err := r.ch.accountKeyVia(r.ch.b, a, nil)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	if r.acct == nil {
		r.acct = map[common.Address]string{}
	}
	r.acct[a] = k
	r.mu.Unlock()
	return k, nil
}

func (ch *Chain) accountKeyVia(b Backend, a common.Address, at *big.Int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code, err := b.CodeAt(ctx, a, at)
	if err != nil {
		return "", err
	}
	if len(code) == 0 {
		return "", nil
	}
	var out []any
	if err := bind.NewBoundContract(a, ch.abis["OwnerAccount"], b, b, b).Call(&bind.CallOpts{Context: ctx, BlockNumber: at}, &out, "signer"); err == nil && len(out) == 2 {
		qx, qy := out[0].([32]byte), out[1].([32]byte)
		return "p256:" + hex.EncodeToString(qx[:]) + hex.EncodeToString(qy[:]), nil
	}
	out = nil
	if err := bind.NewBoundContract(a, ch.abis["AgentAccount"], b, b, b).Call(&bind.CallOpts{Context: ctx, BlockNumber: at}, &out, "signer"); err == nil && len(out) == 1 {
		return "ecdsa:" + out[0].(common.Address).Hex(), nil
	}
	return "code:" + crypto.Keccak256Hash(code).Hex(), nil
}

// ---- which logs count

// authKeys: a publish of p's keys, sent by p's controller at that block (an
// agent's: its granted key, or its owner's controller).
func (r *Resolver) authKeys(p *Party, k keysEvent) authRes {
	who, sure, err := r.keyAuthorities(p, k.block)
	if err != nil {
		return authRes{authUnknown, err.Error()}
	}
	id := [32]byte(idOf(p.Name))
	key := fmt.Sprintf("keys|%s|%s|%d|%x|%s", k.src.Tx.Hex(), p.Name, k.first, crypto.Keccak256(k.view, k.raw), addrList(who))
	a := r.authTx(key, k.src.Tx, who, func(m string, a []any) bool {
		return m == "publishKeys" && len(a) == 4 && a[0] == any(id) && a[1] == any(k.first) &&
			bytes.Equal(a[2].([]byte), k.view) && bytes.Equal(a[3].([]byte), k.raw)
	})
	if a.v == authForged && !sure { // the agent's grant couldn't be checked: its key may well be the sender
		return authRes{authUnknown, "the agent's grant can't be checked now"}
	}
	return a
}

// keyAuthorities: who may publish p's keys at block b. sure: false when an
// agent's grant couldn't be checked (only its owner's controller is listed).
func (r *Resolver) keyAuthorities(p *Party, b uint64) ([]common.Address, bool, error) {
	if !p.IsAgent {
		c, _ := p.controllerAt(b)
		return []common.Address{c}, true, nil
	}
	op, err := r.party(p.Owner)
	if err != nil {
		return nil, false, err
	}
	oc, _ := op.controllerAt(b)
	var g *grantEvent
	for i := range p.grants {
		if p.grants[i].block <= b {
			g = &p.grants[i]
		}
	}
	if g == nil {
		return []common.Address{oc}, true, nil
	}
	switch r.authGrant(p, *g).v {
	case authOK:
		return []common.Address{g.agent, oc}, true, nil
	case authForged:
		return []common.Address{oc}, true, nil
	}
	return []common.Address{oc}, false, nil
}

// authGrant: an AgentGranted log, sent by the owner's controller at that block.
func (r *Resolver) authGrant(p *Party, g grantEvent) authRes {
	op, err := r.party(p.Owner)
	if err != nil {
		return authRes{authUnknown, err.Error()}
	}
	oc, _ := op.controllerAt(g.block)
	label, _, _ := strings.Cut(p.Name, ".")
	owner := [32]byte(idOf(p.Owner))
	key := fmt.Sprintf("grant|%s|%s|%d|%s|%s", g.src.Tx.Hex(), p.Name, g.src.Index, g.agent.Hex(), oc.Hex())
	return r.authTx(key, g.src.Tx, []common.Address{oc}, func(m string, a []any) bool {
		return m == "grant" && len(a) == 6 && a[0] == any(owner) && a[1] == any(label) && a[2] == any(g.agent) &&
			a[3] == any(g.expires) && a[4].(*big.Int).Cmp(g.spend) == 0 && a[5] == any(g.scopes)
	})
}

// authCtrl: the i-th controller change of o (i ≥ 1), a setController its
// previous controller sent. A guardian recovery has no such transaction.
func (r *Resolver) authCtrl(name string, ctrl []ctrlChange, i int) authRes {
	c, prev := ctrl[i], ctrl[i-1]
	if c.recovery {
		return authRes{authUnknown, fmt.Sprintf("a guardian recovery at block %d moved it to %s", c.block, c.controller.Hex())}
	}
	id := [32]byte(idOf(name))
	key := fmt.Sprintf("ctrl|%s|%s|%d|%s|%s", c.src.Tx.Hex(), name, c.src.Index, prev.controller.Hex(), c.controller.Hex())
	return r.authTx(key, c.src.Tx, []common.Address{prev.controller}, func(m string, a []any) bool {
		return m == "setController" && len(a) == 3 && a[0] == any(id) && a[1] == any(c.controller) && a[2] == any(c.version != prev.version)
	})
}

// dirFacts: a name's directory entry now, as the logs say, publishes checked.
type dirFacts struct {
	name       string
	ctrl       []ctrlChange // the owner's history (an agent's: its owner's)
	controller common.Address
	version    uint32
	accountKey string
	agentKey   string      // the granted account's operation signer, separate from its owner
	newest     *keysEvent  // the newest publish its own key signed
	grant      *grantEvent // agents: the latest grant
	at         uint64      // a block at or after every log used: other RPCs are asked there
	forged     []string    // newer logs the transactions contradict (an RPC's)
	unsure     string      // a newer log that couldn't be checked now
}

func (r *Resolver) facts(name string) (*dirFacts, error) {
	p, err := r.party(name)
	if err != nil {
		return nil, err
	}
	o := p
	if p.IsAgent {
		if o, err = r.party(p.Owner); err != nil {
			return nil, err
		}
	}
	if len(o.ctrl) == 0 {
		return nil, fmt.Errorf("no owner %s", o.Name)
	}
	last := o.ctrl[len(o.ctrl)-1]
	f := &dirFacts{name: name, ctrl: o.ctrl, controller: last.controller, version: last.version, at: max(p.at, o.at, last.block)}
	if f.accountKey, err = r.accountKey(f.controller); err != nil {
		return nil, err
	}
	for i := len(p.keys) - 1; i >= 0 && f.newest == nil; i-- {
		switch a := r.authKeys(p, p.keys[i]); a.v {
		case authOK:
			f.newest = &p.keys[i]
			f.at = max(f.at, p.keys[i].block)
		case authForged:
			f.forged = append(f.forged, a.why)
		default:
			if f.unsure == "" {
				f.unsure = a.why
			}
		}
	}
	if p.IsAgent && len(p.grants) > 0 {
		f.grant = &p.grants[len(p.grants)-1]
		f.at = max(f.at, f.grant.block)
		if f.agentKey, err = r.accountKey(f.grant.agent); err != nil {
			return nil, err
		}
		if a := r.authGrant(p, *f.grant); a.v == authForged {
			f.forged = append(f.forged, "grant: "+a.why)
		}
	}
	return f, nil
}

func (f *dirFacts) grantAddress() string {
	if f.grant == nil {
		return ""
	}
	return f.grant.agent.Hex()
}

// alarmID: what an acceptance names (a change of controller, version or account key).
func (f *dirFacts) alarmID() string {
	return crypto.Keccak256Hash([]byte(fmt.Sprintf("%s|%s|%d|%s|%s|%s", f.name, f.controller.Hex(), f.version, f.accountKey, f.grantAddress(), f.agentKey))).Hex()[2:14]
}

// keysCommitOf: Registry.keysCommit of a publish.
func keysCommitOf(k *keysEvent) common.Hash {
	enc, _ := abi.Arguments{{Type: abiType("uint64")}, {Type: abiType("bytes")}, {Type: abiType("bytes")}}.Pack(k.first, k.view, k.raw)
	return crypto.Keccak256Hash(enc)
}

// ---- other RPCs

// witnessURLs: the RPCs that may confirm what the log RPC says: every other
// one in net.json, not the log RPC (strict splits log scans off to the last
// one: then the call RPC is a witness already, see witnesses).
func witnessURLs(all []string, primary string, split bool) []string {
	var out []string
	for i, u := range all {
		if u != primary && !(split && i == len(all)-1) {
			out = append(out, u)
		}
	}
	return out
}

// witnesses: a second RPC, dialed when first needed.
func (ch *Chain) witnesses() []Backend {
	ch.witnessOnce.Do(func() {
		if _, split := ch.b.(splitLogs); split { // logs come from another RPC than calls: the call RPC is the witness
			ch.witness = append(ch.witness, ch.b)
			return
		}
		for _, u := range ch.witnessURLs {
			if b := dialWitness(u, ch.chainID); b != nil {
				ch.witness = append(ch.witness, b)
				return
			}
		}
	})
	return ch.witness
}

func dialWitness(u string, chainID *big.Int) Backend {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cl, err := ethclient.DialContext(ctx, u)
	if err != nil {
		return nil
	}
	if id, err := cl.ChainID(ctx); err != nil || id.Cmp(chainID) != 0 {
		cl.Close()
		return nil
	}
	return cl
}

// witness: does another RPC agree with f? agreed false, err nil: none could
// be asked (why). err: one disagrees.
func (r *Resolver) witness(f *dirFacts) (agreed bool, why string, err error) {
	ws := r.ch.witnesses()
	if len(ws) == 0 {
		return false, "net.json lists no second RPC", nil
	}
	for _, w := range ws {
		ok, y, err := r.witnessOne(w, f)
		if err != nil {
			return false, "", err
		}
		if ok {
			return true, "", nil
		}
		why = y
	}
	return false, why, nil
}

// witnessOne: w's state at block f.at (the owner's controller and version,
// the controller account's key, an agent's grant, the keys' commitment) and
// its hash of the newest publish's block, against ours.
func (r *Resolver) witnessOne(w Backend, f *dirFacts) (bool, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 0; ; i++ { // a witness a block or two behind: a moment
		h, err := w.BlockNumber(ctx)
		if err != nil {
			return false, "the second RPC: " + err.Error(), nil
		}
		if h >= f.at {
			break
		}
		if i >= 10 {
			return false, fmt.Sprintf("the second RPC is behind (block %d, these logs %d)", h, f.at), nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	at := new(big.Int).SetUint64(f.at)
	reg := bind.NewBoundContract(r.ch.addr("Registry"), r.ch.abis["Registry"], w, w, w)
	call := func(method string, args ...any) ([]any, error) {
		var out []any
		err := reg.Call(&bind.CallOpts{Context: ctx, BlockNumber: at}, &out, method, args...)
		return out, err
	}
	var diff []string
	{
		out, err := call("owners", idOf(ownerName(f.name)))
		if err != nil {
			return false, "the second RPC: " + err.Error(), nil
		}
		if c, v := out[0].(common.Address), out[2].(uint32); c != f.controller || v != f.version {
			diff = append(diff, fmt.Sprintf("controller %s v%d there, %s v%d here", c.Hex(), v, f.controller.Hex(), f.version))
		}
		k, err := r.ch.accountKeyVia(w, f.controller, at)
		if err != nil {
			return false, "the second RPC cannot check the controller account's key: " + err.Error(), nil
		}
		if k != f.accountKey {
			diff = append(diff, "the controller account's key")
		}
	}
	if f.grant != nil {
		out, err := call("grants", idOf(f.name))
		if err != nil {
			return false, "the second RPC: " + err.Error(), nil
		}
		if a := out[1].(common.Address); a != f.grant.agent {
			diff = append(diff, fmt.Sprintf("granted key %s there, %s here", a.Hex(), f.grant.agent.Hex()))
		}
		k, err := r.ch.accountKeyVia(w, f.grant.agent, at)
		if err != nil {
			return false, "the second RPC cannot check the agent account's key: " + err.Error(), nil
		}
		if k != f.agentKey {
			diff = append(diff, "the agent account's key")
		}
	}
	if f.newest != nil {
		out, err := call("keysCommit", idOf(f.name))
		if err != nil {
			return false, "the second RPC: " + err.Error(), nil
		}
		if common.Hash(out[0].([32]byte)) != keysCommitOf(f.newest) {
			diff = append(diff, "the newest keys")
		}
		n := new(big.Int).SetUint64(f.newest.block)
		wh, werr := w.HeaderByNumber(ctx, n)
		ph, perr := r.ch.b.HeaderByNumber(ctx, n)
		if werr != nil {
			return false, "the second RPC cannot check the publish block: " + werr.Error(), nil
		}
		if perr != nil {
			return false, "the primary RPC cannot check the publish block: " + perr.Error(), nil
		}
		if wh.Hash() != ph.Hash() {
			diff = append(diff, fmt.Sprintf("block %d", f.newest.block))
		}
	}
	if len(diff) > 0 {
		return false, "", fmt.Errorf("another RPC says otherwise: %s", strings.Join(diff, "; "))
	}
	return true, "", nil
}

// blockCheck (verify_blocks): the log's block is the second RPC's at that
// height; on a chain of our own, its transaction and its receipt (succeeded,
// holding this very log) are in that block by the header's roots. A hosted
// OP-stack chain's deposit transactions aren't decodable here: only the
// first part holds there.
func (r *Resolver) blockCheck(ev event) error {
	ws := r.ch.witnesses()
	if len(ws) == 0 {
		return errors.New("verify_blocks needs a second RPC (net.json \"rpcs\")")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	wh, err := ws[0].HeaderByNumber(ctx, new(big.Int).SetUint64(ev.Block))
	if err != nil {
		return fmt.Errorf("block %d from the second RPC: %w", ev.Block, err)
	}
	if wh.Hash() != ev.bhash {
		return fmt.Errorf("the log names block %s at height %d; the second RPC has %s there", shortHash(ev.bhash), ev.Block, shortHash(wh.Hash()))
	}
	if r.ch.hosted {
		return nil
	}
	blk, err := r.ch.b.BlockByHash(ctx, ev.bhash)
	if err != nil {
		return fmt.Errorf("block %d: %w", ev.Block, err)
	}
	txs := blk.Transactions()
	if blk.Hash() != ev.bhash || types.DeriveSha(txs, trie.NewStackTrie(nil)) != blk.TxHash() {
		return fmt.Errorf("block %d: its transactions aren't the header's", ev.Block)
	}
	if len(txs) > 2000 {
		return fmt.Errorf("block %d has %d transactions: too many to check here", ev.Block, len(txs))
	}
	idx := slices.IndexFunc(txs, func(t *types.Transaction) bool { return t.Hash() == ev.Tx })
	if idx < 0 {
		return fmt.Errorf("transaction %s is not in block %d", shortHash(ev.Tx), ev.Block)
	}
	var rs types.Receipts
	for _, t := range txs {
		rc, err := r.ch.b.TransactionReceipt(ctx, t.Hash())
		if err != nil {
			return fmt.Errorf("block %d's receipts: %w", ev.Block, err)
		}
		rs = append(rs, rc)
	}
	if types.DeriveSha(rs, trie.NewStackTrie(nil)) != blk.ReceiptHash() {
		return fmt.Errorf("block %d: its receipts aren't the header's", ev.Block)
	}
	if rs[idx].Status != types.ReceiptStatusSuccessful {
		return fmt.Errorf("transaction %s failed", shortHash(ev.Tx))
	}
	for _, l := range rs[idx].Logs {
		if l.Index == ev.Index && l.Address == r.ch.addr("Registry") && slices.Equal(l.Topics, ev.topics) && bytes.Equal(l.Data, ev.data) {
			return nil
		}
	}
	return fmt.Errorf("the log is not in transaction %s's receipt", shortHash(ev.Tx))
}

// ---- pins

// KeyPin: a correspondent's directory entry as this identity took it.
type KeyPin struct {
	Controller string    `json:"controller,omitempty"` // owners: the controller (a key or an account)
	Version    uint32    `json:"version,omitempty"`
	AccountKey string    `json:"account_key,omitempty"` // the controller is an account: the key that signs its operations
	First      uint64    `json:"first_epoch"`           // the newest publish taken: its first epoch and block (no going back)
	Block      uint64    `json:"block"`
	LogIndex   uint      `json:"log_index"`
	Commit     string    `json:"commit,omitempty"`
	Agent      string    `json:"agent,omitempty"`
	AgentKey   string    `json:"agent_key,omitempty"`
	Witnessed  bool      `json:"witnessed,omitempty"` // a second RPC agreed when it was pinned
	Rev        int       `json:"rev"`                 // bumped on every change: a merge keeps the higher
	Alarm      *PinAlarm `json:"alarm,omitempty"`     // a change not accepted: nothing is sealed to the name
}

type PinAlarm struct {
	ID         string `json:"id"` // accept names it (owner approve AGENT keys:NAME:ID)
	Why        string `json:"why"`
	Controller string `json:"controller"` // what the entry would move to
	Version    uint32 `json:"version"`
	AccountKey string `json:"account_key,omitempty"`
	Since      int64  `json:"since"`
	Ask        string `json:"ask,omitempty"` // an agent: the ask filed with its owner
}

func pinAlertPrefix(name string) string { return "KEY CHANGE for " + name + ": " }

func (c *Client) pin(name string) *KeyPin { return c.st.KeyPins[name] }

// sealCheck: may this identity seal to name (and its owner) now?
func (c *Client) sealCheck(name string) error {
	if name == "" || name == c.f.Name {
		return nil
	}
	if o := ownerName(name); o != c.f.Name {
		if err := c.pinCheck(o); err != nil {
			return err
		}
	}
	if fromAgent(name) {
		return c.pinCheck(name)
	}
	return nil
}

var errPinMoved = errors.New("the pin changed under us")

func (c *Client) pinCheck(name string) error {
	err := errPinMoved
	for try := 0; try < 3 && errors.Is(err, errPinMoved); try++ {
		err = c.pinCheckOnce(name)
	}
	return err
}

func (c *Client) pinCheckOnce(name string) error {
	f, err := c.res.facts(name)
	if err != nil {
		return fmt.Errorf("refusing to seal to %s: %w", name, err)
	}
	if len(f.forged) > 0 {
		return c.pinRefuse(name, fmt.Sprintf("your RPC serves a directory log for %s that %s's own key didn't sign (%s): it may be lying. "+
			"Nothing is sealed to %s through it; use another RPC (net.json \"rpcs\", --home-rpc)", name, name, f.forged[0], name))
	}
	if f.newest == nil {
		why := f.unsure
		if why == "" {
			why = "none published"
		}
		return fmt.Errorf("refusing to seal to %s: no keys of %s that its own key signed (%s)", name, name, why)
	}
	pin := c.pin(name)
	if pin != nil && pin.Alarm != nil {
		c.syncGuard() // the owner may have accepted it from another process
		pin = c.pin(name)
	}
	if pin == nil {
		return c.firstPin(name, f)
	}
	if k := f.newest; k.block < pin.Block || k.first < pin.First || k.block == pin.Block && k.src.Index < pin.LogIndex {
		if f.unsure != "" {
			return fmt.Errorf("refusing to seal to %s: its newest keys can't be checked now (%s)", name, f.unsure)
		}
		return c.pinRefuse(name, fmt.Sprintf("your RPC serves %s's keys of block %d (epoch %d on), older than the ones already seen (block %d, epoch %d on): "+
			"it may be hiding newer keys. Nothing is sealed to %s through it", name, k.block, k.first, pin.Block, pin.First, name))
	}
	same := f.controller.Hex() == pin.Controller && f.version == pin.Version && f.accountKey == pin.AccountKey &&
		f.grantAddress() == pin.Agent && f.agentKey == pin.AgentKey
	if same {
		if pin.Alarm != nil || f.newest.block != pin.Block || f.newest.src.Index != pin.LogIndex || keysCommitOf(f.newest).Hex() != pin.Commit {
			ok, why, err := c.res.witness(f)
			if err != nil {
				return c.pinRefuse(name, fmt.Sprintf("RPCs disagree about %s's new publish: %v", name, err))
			}
			if !ok && (pin.Witnessed || strict() && homeRPC == "") {
				return fmt.Errorf("refusing to seal to %s: a second RPC cannot confirm its new publish (%s)", name, why)
			}
			if c.ch.verifyBlocks {
				if err := c.res.blockCheck(f.newest.src); err != nil {
					return c.pinRefuse(name, fmt.Sprintf("%s's new publish doesn't check against its block: %v", name, err))
				}
			}
			return c.setPin(name, pin.Rev, f, ok)
		}
		return nil
	}
	why, follow := c.followable(name, pin, f)
	if follow {
		ok, wy, err := c.res.witness(f)
		if err != nil {
			return c.pinRefuse(name, fmt.Sprintf("%s's key changed (signed by its old key), but RPCs disagree about it: %v. Nothing is sealed to %s until they agree", name, err, name))
		}
		if !ok && (pin.Witnessed || strict() && homeRPC == "") {
			return fmt.Errorf("refusing to seal to %s: its key changed, and a second RPC must confirm the change (%s)", name, wy)
		}
		if c.ch.verifyBlocks {
			if err := c.res.blockCheck(f.ctrl[len(f.ctrl)-1].src); err != nil {
				return c.pinRefuse(name, fmt.Sprintf("%s's key change doesn't check against its block: %v", name, err))
			}
		}
		return c.setPin(name, pin.Rev, f, ok)
	}
	if pin.Alarm == nil || pin.Alarm.ID != f.alarmID() {
		al := &PinAlarm{ID: f.alarmID(), Why: why, Controller: f.controller.Hex(), Version: f.version, AccountKey: f.accountKey, Since: c.chainNow()}
		moved := false
		err := c.locked(func() {
			p := c.st.KeyPins[name]
			if p == nil || p.Rev != pin.Rev {
				moved = true
				return
			}
			c.retractAlerts(pinAlertPrefix(name))
			p.Alarm, p.Rev = al, p.Rev+1
			c.addAlert(pinAlertPrefix(name) + why + ". Nothing is sealed to " + name + " until its owner checks with " + name +
				" out of band and accepts the new key" + c.acceptWords(name, al.ID))
		})
		if moved {
			return errPinMoved
		}
		if err != nil {
			return err
		}
		pin = c.pin(name)
	}
	return c.alarmed(name, pin, f)
}

// followable: the controller moved from the pinned one only by changes its
// previous key signed (setController), and the account key didn't change
// under the same controller. Else why it is a KEY CHANGE.
func (c *Client) followable(name string, pin *KeyPin, f *dirFacts) (string, bool) {
	if f.grant != nil {
		if f.grantAddress() == pin.Agent && f.agentKey != pin.AgentKey {
			return fmt.Sprintf("the key of %s's agent account %s changed", name, pin.Agent), false
		}
		if f.grantAddress() != pin.Agent {
			if a := c.res.authGrant(&Party{Name: name, Owner: ownerName(name)}, *f.grant); a.v != authOK {
				return "the changed agent account has no grant signed by its owner", false
			}
			if f.controller.Hex() == pin.Controller && f.version == pin.Version && f.accountKey == pin.AccountKey {
				return "", true
			}
		}
	}
	j := -1
	for i, x := range f.ctrl {
		if x.controller.Hex() == pin.Controller && x.version == pin.Version {
			j = i
		}
	}
	if j < 0 {
		return fmt.Sprintf("the RPC's history of %s doesn't hold the key pinned for it (%s, v%d); it says %s, v%d now", name, pin.Controller, pin.Version, f.controller.Hex(), f.version), false
	}
	if j == len(f.ctrl)-1 {
		if pin.AccountKey == "" { // an account registered before it was deployed: its key is learnt now (a second RPC confirms it when there is one)
			return "", true
		}
		return fmt.Sprintf("the key of %s's account %s changed", name, f.controller.Hex()), false
	}
	for i := j + 1; i < len(f.ctrl); i++ {
		if a := c.res.authCtrl(name, f.ctrl, i); a.v != authOK {
			why := a.why
			if !f.ctrl[i].recovery {
				why = fmt.Sprintf("a change of its key at block %d to %s that no transaction of its old key proves (%s)", f.ctrl[i].block, f.ctrl[i].controller.Hex(), a.why)
			}
			return why, false
		}
	}
	return "", true
}

func (c *Client) acceptWords(name, id string) string {
	if c.isOwner() {
		return ": ternchain pins accept --dir " + shellQuote(c.dir) + " " + name
	}
	return ": ternchain owner approve --dir " + c.ownerDirWords() + " " + c.f.Name + " keys:" + name + ":" + id
}

// alarmed: the refusal for a KEY CHANGE; an agent takes its owner's approval
// of exactly this change (keys:NAME:ID) and files an ask once.
func (c *Client) alarmed(name string, pin *KeyPin, f *dirFacts) error {
	al := pin.Alarm
	head := fmt.Sprintf("refusing to seal to %s: KEY CHANGE: %s", name, al.Why)
	if c.isOwner() {
		return fmt.Errorf("%s. Check with %s out of band (in person, or a call you trust: not through tern) that the change is theirs, then run: "+
			"ternchain pins accept --dir %s %s", head, name, shellQuote(c.dir), name)
	}
	key := "keys:" + name + ":" + al.ID
	err := c.useApproval(key, nil, "taking "+name+"'s changed key ("+al.Why+")")
	if err == nil {
		return c.setPin(name, pin.Rev, f, false)
	}
	if !strings.HasPrefix(err.Error(), "blocked:") {
		return err
	}
	if al.Ask == "" && name != c.f.Owner { // tell the owner once (sealing to the owner itself is what is refused)
		if r, aerr := c.askOwner("key change: "+name, fmt.Sprintf("%s's directory entry changed: %s. I won't seal to %s until you check with them out of band "+
			"and approve exactly this change (keys:%s:%s).", name, al.Why, name, name, al.ID)); aerr == nil {
			id, _ := r["sent"].(string)
			c.locked(func() {
				if p := c.st.KeyPins[name]; p != nil && p.Alarm != nil && p.Alarm.ID == al.ID {
					p.Alarm.Ask, p.Rev = id, p.Rev+1
				}
			})
		}
	}
	return fmt.Errorf("%s. Your owner checks with %s out of band and approves exactly this change: ternchain owner approve --dir %s %s %s "+
		"(an ask was filed with them)", head, name, c.ownerDirWords(), c.f.Name, key)
}

// setPin: f becomes name's pin (no alarm), if the pin is still the one the
// decision was made on (rev; 0: none).
func (c *Client) setPin(name string, rev int, f *dirFacts, witnessed bool) error {
	moved := false
	err := c.locked(func() {
		old := c.st.KeyPins[name]
		if old != nil && old.Rev != rev || old == nil && rev != 0 {
			moved = true
			return
		}
		p := &KeyPin{First: f.newest.first, Block: f.newest.block, LogIndex: f.newest.src.Index, Commit: keysCommitOf(f.newest).Hex(), Witnessed: witnessed, Rev: rev + 1}
		p.Controller, p.Version, p.AccountKey = f.controller.Hex(), f.version, f.accountKey
		p.Agent, p.AgentKey = f.grantAddress(), f.agentKey
		if old != nil && old.Alarm != nil {
			c.retractAlerts(pinAlertPrefix(name))
		}
		if c.st.KeyPins == nil {
			c.st.KeyPins = map[string]*KeyPin{}
		}
		c.st.KeyPins[name] = p
	})
	if moved {
		return errPinMoved
	}
	return err
}

func (c *Client) firstPin(name string, f *dirFacts) error {
	ok, why, err := c.res.witness(f)
	if err != nil {
		return c.pinRefuse(name, fmt.Sprintf("RPCs disagree about %s: %v. Nothing is sealed to %s until they agree (one of them may be lying)", name, err, name))
	}
	if !ok && strict() && homeRPC == "" {
		return fmt.Errorf("refusing to seal to %s: --privacy strict takes a correspondent's keys the first time only when a second RPC agrees (%s): "+
			"list another RPC in net.json \"rpcs\", or give --home-rpc (your own node)", name, why)
	}
	if c.ch.verifyBlocks {
		if err := c.res.blockCheck(f.newest.src); err != nil {
			return c.pinRefuse(name, fmt.Sprintf("%s's keys don't check against their block: %v", name, err))
		}
	}
	return c.setPin(name, 0, f, ok)
}

// pinRefuse: an alert (once) and the refusal.
func (c *Client) pinRefuse(name, why string) error {
	c.locked(func() { c.addAlert(why) })
	return fmt.Errorf("refusing to seal to %s: %s", name, why)
}

func (c *Client) addAlert(a string) {
	if !slices.Contains(c.st.Alerts, a) {
		c.st.Alerts = append(c.st.Alerts, a)
	}
}

// acceptPin: 'pins accept NAME': the owner, having checked with NAME out of
// band, takes its changed key. Agents can't: their owner approves.
func (c *Client) acceptPin(name string) (map[string]any, error) {
	if !c.isOwner() {
		return nil, fmt.Errorf("an agent can't accept a changed key (a message could talk it into that): your owner checks with %s out of band and runs "+
			"ternchain owner approve --dir %s %s keys:%s:ID (the ID is in the refusal)", name, c.ownerDirWords(), c.f.Name, name)
	}
	c.syncGuard()
	pin := c.pin(name)
	if pin == nil || pin.Alarm == nil {
		return nil, fmt.Errorf("no key change of %s is waiting ('ternchain pins' lists them)", name)
	}
	c.res.forget()
	f, err := c.res.facts(name)
	if err != nil {
		return nil, err
	}
	if f.newest == nil || f.alarmID() != pin.Alarm.ID {
		return nil, fmt.Errorf("%s's entry changed again since the alarm (now %s v%d): send to it to see the new alarm, and check again", name, f.controller.Hex(), f.version)
	}
	ok, _, err := c.res.witness(f)
	if err != nil {
		return nil, fmt.Errorf("RPCs disagree about %s: %w", name, err)
	}
	if err := c.setPin(name, pin.Rev, f, ok); err != nil {
		return nil, err
	}
	return map[string]any{"accepted": name, "controller": f.controller.Hex(), "version": f.version, "account_key": f.accountKey, "witnessed": ok}, nil
}

// pinsList: 'pins': every pin, alarms first.
func (c *Client) pinsList() map[string]any {
	c.syncGuard()
	names := make([]string, 0, len(c.st.KeyPins))
	for n := range c.st.KeyPins {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := c.st.KeyPins[names[i]].Alarm != nil, c.st.KeyPins[names[j]].Alarm != nil
		return a && !b || a == b && names[i] < names[j]
	})
	var out []map[string]any
	alarms := 0
	for _, n := range names {
		p := c.st.KeyPins[n]
		m := map[string]any{"name": n, "first_epoch": p.First, "block": p.Block, "witnessed": p.Witnessed}
		if p.Controller != "" {
			m["controller"], m["version"] = p.Controller, p.Version
		}
		if p.AccountKey != "" {
			m["account_key"] = p.AccountKey
		}
		if p.Agent != "" {
			m["agent"], m["agent_key"] = p.Agent, p.AgentKey
		}
		if p.Alarm != nil {
			alarms++
			m["key_change"] = map[string]any{"why": p.Alarm.Why, "id": p.Alarm.ID, "to": p.Alarm.Controller, "version": p.Alarm.Version}
		}
		out = append(out, m)
	}
	return map[string]any{"pins": out, "key_changes": alarms}
}

// mergePins: another process's pins, under the state lock: the higher rev wins.
func (s *State) mergePins(d *State) {
	for n, dp := range d.KeyPins {
		if s.KeyPins == nil {
			s.KeyPins = map[string]*KeyPin{}
		}
		if sp := s.KeyPins[n]; sp == nil || dp.Rev > sp.Rev {
			if sp != nil && sp.Alarm != nil && dp.Alarm == nil { // accepted (or cleared) elsewhere: its alert goes too
				keep := s.Alerts[:0]
				for _, a := range s.Alerts {
					if strings.HasPrefix(a, pinAlertPrefix(n)) {
						delete(s.AlertsAt, a)
					} else {
						keep = append(keep, a)
					}
				}
				s.Alerts = keep
			}
			s.KeyPins[n] = dp
		}
	}
}
