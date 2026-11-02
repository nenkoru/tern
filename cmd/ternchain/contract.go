// Bring your own contract (docs/BUILD.md): the network is a plain EVM, and this is
// a thin client for contracts that aren't the network's own.
//
//	contract deploy --artifact FILE [ARGS…] [--args '[…]'] [--value V]   (or --abi A.json --bin B.bin)
//	contract call ADDRESS "fn(type,…)(ret,…)" ARGS… [--abi A.json]       read-only, like cast call
//	contract send ADDRESS "fn(type,…)" ARGS… [--value V] [--abi A.json]  a transaction
//	contract trust ADDRESS --note TEXT | contract untrust ADDRESS | contract list
//	contract info ADDRESS | contract publish ADDRESS --source URL|FILE
//
// Owners deploy and send freely. An agent's guard (guardContract): a send to
// a contract its owner doesn't trust needs a one-time owner approval for that
// contract (owner approve AGENT contract:ADDRESS); value counts against
// free_pay and approvals like pay; a token approval (approve,
// increaseAllowance, setApprovalForAll) to an untrusted spender needs the
// owner each time (contract:approve:SPENDER); a deploy needs contract:deploy;
// what outside contracts return comes fenced. The owner's trust list is on
// chain: claims of kind contract-trust by the owner's name. The Bank's spend
// limit doesn't cover raw contract calls: they spend the key's own gas coins.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	kindContractTrust  = "contract-trust"
	kindContractSource = "contract-source"
	deployKey          = "contract:deploy"
)

// contractKey: what an owner approves for an agent's sends to one contract.
func contractKey(a common.Address) string { return "contract:" + strings.ToLower(a.Hex()) }

// approveKey: what an owner approves for one token approval to an untrusted spender.
func approveKey(spender common.Address) string {
	return "contract:approve:" + strings.ToLower(spender.Hex())
}

// lastingKey: an approval without an amount for this key stays (a correspondent,
// a contract); a deploy or a token approval is approved once each time.
func lastingKey(to string) bool {
	return to != deployKey && !strings.HasPrefix(to, "contract:approve:") && !strings.HasPrefix(to, "token:") && !strings.HasPrefix(to, "thread:") && !strings.HasPrefix(to, "keys:")
}

// tokenKey: what an owner approves for one token transfer of its agent:
// this token, this recipient, this amount (audit 6 CL-1). Approved once.
// approveAmountKey: what an owner approves for one token approval to a
// spender it trusts: that token, that spender, that amount.
func approveAmountKey(token, spender common.Address, amount string) string {
	return "contract:approve:" + strings.ToLower(token.Hex()) + ":" + strings.ToLower(spender.Hex()) + ":" + amount
}

func tokenKey(token, to common.Address, amount string) string {
	return "token:" + strings.ToLower(token.Hex()) + ":" + strings.ToLower(to.Hex()) + ":" + amount
}

// normContractKey: contract:0xAbC… as the guard spells it (lower case).
func normContractKey(to string) string {
	if strings.HasPrefix(to, "contract:") {
		return strings.ToLower(to)
	}
	return to
}

type contractFlags struct {
	artifact, abi, bin, args, value, source, note string
}

const contractUsage = "contract deploy --artifact FILE [ARGS…] [--value V] | contract call|send ADDRESS \"fn(type,…)\" ARGS… [--value V] [--abi A.json] | " +
	"contract trust ADDRESS --note TEXT | contract untrust ADDRESS | contract list | contract info ADDRESS | contract publish ADDRESS --source URL|FILE   (docs/BUILD.md)"

func runContract(c *Client, cmd string, pos []string, f contractFlags) error {
	extra, err := splitArgs(f.args)
	if err != nil {
		return err
	}
	var value *big.Int
	if f.value != "" {
		if value, err = parseCoins(f.value); err != nil {
			return err
		}
	}
	var r any
	switch cmd {
	case "contract deploy":
		r, err = c.contractDeploy(f.artifact, f.abi, f.bin, append(pos, extra...), value)
	case "contract call", "contract send":
		if err := need(pos, 2, contractUsage); err != nil {
			return err
		}
		if cmd == "contract call" {
			r, err = c.contractCall(pos[0], pos[1], append(pos[2:], extra...), f.abi)
		} else {
			r, err = c.contractSend(pos[0], pos[1], append(pos[2:], extra...), f.abi, value)
		}
	case "contract trust", "contract untrust":
		if err := need(pos, 1, cmd+" ADDRESS"+map[bool]string{true: " --note TEXT"}[cmd == "contract trust"]); err != nil {
			return err
		}
		r, err = c.contractTrust(pos[0], f.note, cmd == "contract trust")
	case "contract list":
		r, err = c.contractList()
	case "contract info":
		if err := need(pos, 1, "contract info ADDRESS"); err != nil {
			return err
		}
		r, err = c.contractInfo(pos[0])
	case "contract publish":
		if err := need(pos, 1, "contract publish ADDRESS --source URL|FILE"); err != nil {
			return err
		}
		r, err = c.contractPublish(pos[0], f.source)
	default:
		return unknownCommand(cmd)
	}
	if err != nil {
		return err
	}
	out(r)
	return nil
}

// splitArgs: --args as a JSON array, or words.
func splitArgs(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if !strings.HasPrefix(s, "[") {
		return strings.Fields(s), nil
	}
	var raw []any
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("--args: a JSON array, like '[\"Gold\",\"GLD\",1000000]': %v", err)
	}
	var out []string
	for _, v := range raw {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		default:
			b, _ := json.Marshal(x)
			out = append(out, string(b))
		}
	}
	return out, nil
}

// ---- artifacts and signatures

type contractArt struct {
	abi  abi.ABI
	bin  []byte
	name string
}

// loadContractArtifact: a Foundry (out/X.sol/X.json) or Hardhat artifact, a
// solc output entry, or --abi A.json --bin B.bin.
func loadContractArtifact(artifact, abiF, binF string) (*contractArt, error) {
	var abiRaw json.RawMessage
	var code string
	name := ""
	switch {
	case artifact != "":
		b, err := os.ReadFile(artifact)
		if err != nil {
			return nil, err
		}
		var m struct {
			ABI      json.RawMessage `json:"abi"`
			Bytecode json.RawMessage `json:"bytecode"`
			EVM      struct {
				Bytecode struct {
					Object string `json:"object"`
				} `json:"bytecode"`
			} `json:"evm"`
			ContractName string `json:"contractName"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("%s: not an artifact (JSON with abi and bytecode): %v", artifact, err)
		}
		abiRaw, name = m.ABI, m.ContractName
		var obj struct {
			Object string `json:"object"`
		}
		switch {
		case json.Unmarshal(m.Bytecode, &code) == nil: // Hardhat: "bytecode": "0x…"
		case json.Unmarshal(m.Bytecode, &obj) == nil && obj.Object != "": // Foundry: "bytecode": {"object": "0x…"}
			code = obj.Object
		default:
			code = m.EVM.Bytecode.Object // solc's standard JSON
		}
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(artifact), ".json")
		}
	case abiF != "" && binF != "":
		a, err := os.ReadFile(abiF)
		if err != nil {
			return nil, err
		}
		abiRaw = a
		var m struct {
			ABI json.RawMessage `json:"abi"`
		}
		if json.Unmarshal(a, &m) == nil && len(m.ABI) > 0 { // an artifact given as --abi
			abiRaw = m.ABI
		}
		b, err := os.ReadFile(binF)
		if err != nil {
			return nil, err
		}
		code, name = string(b), strings.TrimSuffix(filepath.Base(binF), filepath.Ext(binF))
	default:
		return nil, errors.New("usage: contract deploy --artifact FILE (Foundry or Hardhat JSON: abi + bytecode) [ARGS…], or --abi A.json --bin B.bin")
	}
	parsed, err := abi.JSON(strings.NewReader(string(abiRaw)))
	if err != nil || len(abiRaw) == 0 {
		return nil, fmt.Errorf("the artifact's abi: %v", err)
	}
	code = strings.TrimPrefix(strings.TrimSpace(code), "0x")
	if strings.Contains(code, "__") {
		return nil, errors.New("the bytecode has unlinked library placeholders (__$…$__): link the libraries first (forge build --libraries …)")
	}
	bin, err := hex.DecodeString(code)
	if err != nil {
		return nil, fmt.Errorf("the bytecode is not hex: %v", err)
	}
	if len(bin) == 0 {
		return nil, errors.New("the artifact has no bytecode: an interface or an abstract contract can't be deployed")
	}
	return &contractArt{abi: parsed, bin: bin, name: name}, nil
}

// parenGroup: s starts with "(": what is inside the matching ")", and the rest.
func parenGroup(s string) (string, string, error) {
	if !strings.HasPrefix(s, "(") {
		return "", "", fmt.Errorf("expected ( in %q", s)
	}
	d := 0
	for i, r := range s {
		switch r {
		case '(':
			d++
		case ')':
			d--
			if d == 0 {
				return s[1:i], s[i+1:], nil
			}
		}
	}
	return "", "", fmt.Errorf("unbalanced parentheses in %q", s)
}

// typeList: "address to,uint256" as ABI arguments.
func typeList(s string) (abi.Arguments, error) {
	var out abi.Arguments
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		fs := strings.Fields(part)
		if len(fs) == 0 {
			return nil, fmt.Errorf("an empty type in %q", s)
		}
		if strings.Contains(fs[0], "(") {
			return nil, errors.New("tuple types: give the contract's ABI with --abi")
		}
		t, err := abi.NewType(fs[0], "", nil)
		if err != nil {
			return nil, fmt.Errorf("type %q: %v", fs[0], err)
		}
		out = append(out, abi.Argument{Type: t})
	}
	return out, nil
}

// parseSig: "transfer(address,uint256)", "balanceOf(address)(uint256)" (cast's
// form) or "balanceOf(address) returns (uint256)".
func parseSig(sig string) (abi.Method, error) {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sig), "function "))
	i := strings.Index(s, "(")
	if i <= 0 {
		return abi.Method{}, fmt.Errorf("%q: a function signature, like \"transfer(address,uint256)\" or \"balanceOf(address)(uint256)\"", sig)
	}
	name := strings.TrimSpace(s[:i])
	ins, rest, err := parenGroup(s[i:])
	if err != nil {
		return abi.Method{}, err
	}
	in, err := typeList(ins)
	if err != nil {
		return abi.Method{}, err
	}
	var outs abi.Arguments
	if j := strings.Index(rest, "("); j >= 0 {
		o, _, err := parenGroup(rest[j:])
		if err != nil {
			return abi.Method{}, err
		}
		if outs, err = typeList(o); err != nil {
			return abi.Method{}, err
		}
	}
	return abi.NewMethod(name, name, abi.Function, "", false, false, in, outs), nil
}

// methodFor: the function to call: by signature, or by name from --abi.
func methodFor(sig, abiF string) (abi.Method, error) {
	if abiF == "" {
		return parseSig(sig)
	}
	b, err := os.ReadFile(abiF)
	if err != nil {
		return abi.Method{}, err
	}
	var m struct {
		ABI json.RawMessage `json:"abi"`
	}
	if json.Unmarshal(b, &m) == nil && len(m.ABI) > 0 {
		b = m.ABI
	}
	a, err := abi.JSON(strings.NewReader(string(b)))
	if err != nil {
		return abi.Method{}, fmt.Errorf("--abi %s: %v", abiF, err)
	}
	want := sig
	if strings.Contains(sig, "(") {
		p, err := parseSig(sig)
		if err != nil {
			return abi.Method{}, err
		}
		want = p.Sig
	}
	var names []string
	for _, x := range a.Methods {
		if x.Name == want || x.RawName == want && !strings.Contains(sig, "(") || x.Sig == want {
			return x, nil
		}
		names = append(names, x.Sig)
	}
	sort.Strings(names)
	return abi.Method{}, fmt.Errorf("no function %q in %s: it has %s", sig, abiF, strings.Join(names, ", "))
}

// ---- arguments in, results out

// resolveAddr: 0x…, a network contract's name (Items, Bank), or a Tern name.
func (c *Client) resolveAddr(s string) (common.Address, error) {
	if common.IsHexAddress(s) {
		return common.HexToAddress(s), nil
	}
	if a, ok := c.ch.dep.Contracts[s]; ok {
		return common.HexToAddress(a), nil
	}
	if a, err := c.addressOf(s); err == nil {
		return a, nil
	}
	return common.Address{}, fmt.Errorf("%q is not an address, a network contract (Bank, Items…) or a registered name", s)
}

func (c *Client) abiValue(t abi.Type, s string) (any, error) {
	switch t.T {
	case abi.AddressTy:
		return c.resolveAddr(s)
	case abi.BoolTy:
		return strconv.ParseBool(s)
	case abi.StringTy:
		return s, nil
	case abi.BytesTy:
		if b, err := hex.DecodeString(strings.TrimPrefix(s, "0x")); strings.HasPrefix(s, "0x") && err == nil {
			return b, nil
		}
		return []byte(s), nil // text as its bytes
	case abi.FixedBytesTy:
		b := []byte(s)
		if strings.HasPrefix(s, "0x") {
			var err error
			if b, err = hex.DecodeString(s[2:]); err != nil {
				return nil, fmt.Errorf("%q: not hex", s)
			}
		}
		if len(b) > t.Size {
			return nil, fmt.Errorf("%q: longer than bytes%d", s, t.Size)
		}
		v := reflect.New(t.GetType()).Elem()
		reflect.Copy(v, reflect.ValueOf(b))
		return v.Interface(), nil
	case abi.UintTy, abi.IntTy:
		n, ok := new(big.Int).SetString(s, 0)
		if !ok { // 1e18, 2.5e18
			r, ok2 := new(big.Rat).SetString(s)
			if !ok2 || !r.IsInt() {
				return nil, fmt.Errorf("%q: not an integer (base units; 1e18 is one 18-decimal token)", s)
			}
			n = r.Num()
		}
		if t.T == abi.UintTy && (n.Sign() < 0 || n.BitLen() > t.Size) || t.T == abi.IntTy && n.BitLen() >= t.Size {
			return nil, fmt.Errorf("%s doesn't fit %s", s, t.String())
		}
		if t.Size > 64 {
			return n, nil
		}
		if t.T == abi.UintTy {
			return reflect.ValueOf(n.Uint64()).Convert(t.GetType()).Interface(), nil
		}
		return reflect.ValueOf(n.Int64()).Convert(t.GetType()).Interface(), nil
	case abi.SliceTy, abi.ArrayTy:
		inner := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "["), "]")
		var parts []string
		if strings.TrimSpace(inner) != "" {
			parts = strings.Split(inner, ",")
		}
		if t.T == abi.ArrayTy && len(parts) != t.Size {
			return nil, fmt.Errorf("%s takes %d values, got %d", t.String(), t.Size, len(parts))
		}
		var v reflect.Value
		if t.T == abi.SliceTy {
			v = reflect.MakeSlice(t.GetType(), len(parts), len(parts))
		} else {
			v = reflect.New(t.GetType()).Elem()
		}
		for i, p := range parts {
			x, err := c.abiValue(*t.Elem, strings.Trim(strings.TrimSpace(p), `"`))
			if err != nil {
				return nil, err
			}
			v.Index(i).Set(reflect.ValueOf(x))
		}
		return v.Interface(), nil
	}
	return nil, fmt.Errorf("type %s: not supported here; use forge or cast", t.String())
}

func (c *Client) abiArgs(in abi.Arguments, args []string, what string) ([]any, error) {
	if len(args) != len(in) {
		var ts []string
		for _, a := range in {
			ts = append(ts, a.Type.String())
		}
		return nil, fmt.Errorf("%s takes %d arguments (%s), got %d", what, len(in), strings.Join(ts, ","), len(args))
	}
	var vs []any
	for i, a := range in {
		v, err := c.abiValue(a.Type, args[i])
		if err != nil {
			return nil, fmt.Errorf("%s argument %d: %w", what, i+1, err)
		}
		vs = append(vs, v)
	}
	return vs, nil
}

// abiOut: a decoded value as JSON shows it (addresses and bytes in hex, big numbers as strings).
func abiOut(v any) any {
	switch x := v.(type) {
	case common.Address:
		return x.Hex()
	case *big.Int:
		return x.String()
	case []byte:
		return "0x" + hex.EncodeToString(x)
	case string, bool:
		return x
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			b := make([]byte, rv.Len())
			reflect.Copy(reflect.ValueOf(b), rv)
			return "0x" + hex.EncodeToString(b)
		}
		fallthrough
	case reflect.Slice:
		var out []any
		for i := 0; i < rv.Len(); i++ {
			out = append(out, abiOut(rv.Index(i).Interface()))
		}
		return out
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return fmt.Sprint(v)
	}
	return v
}

// ---- the chain, without a bound contract

// rawCall: a read-only call.
func (ch *Chain) rawCall(from, to common.Address, data []byte) ([]byte, error) {
	b, err := ch.b.CallContract(context.Background(), ethereum.CallMsg{From: from, To: &to, Data: data}, nil)
	if err != nil {
		return nil, cleanErr(err)
	}
	return b, nil
}

// sendRaw: a transaction to any address (to nil: a contract creation),
// through the key's smart account when it has one.
func (ch *Chain) sendRaw(key *ecdsa.PrivateKey, value *big.Int, to *common.Address, data []byte) (*types.Receipt, error) {
	from := crypto.PubkeyToAddress(key.PublicKey)
	if value == nil {
		value = new(big.Int)
	}
	if r := ch.routes[from]; r != nil {
		if to == nil {
			return nil, errors.New("a smart account doesn't deploy here: deploy from a plain key with gas coins (forge create, or an owner key)")
		}
		cd, err := ch.executeCallData(*to, value, data)
		if err != nil {
			return nil, err
		}
		return ch.userOp(r, cd)
	}
	defer ch.lockSender(from)()
	ctx := context.Background()
	est, err := ch.b.EstimateGas(ctx, ethereum.CallMsg{From: from, To: to, Value: value, Data: data})
	if err != nil {
		return nil, cleanErr(err)
	}
	nonce, err := ch.b.PendingNonceAt(ctx, from)
	if err != nil {
		return nil, err
	}
	tip, err := ch.b.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, err
	}
	head, err := ch.b.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	fee := new(big.Int).Add(tip, new(big.Int).Mul(head.BaseFee, big.NewInt(2)))
	tx := types.NewTx(&types.DynamicFeeTx{ChainID: ch.chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: fee, Gas: est*5/4 + 20_000, To: to, Value: value, Data: data})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(ch.chainID), key)
	if err != nil {
		return nil, err
	}
	err = ch.b.SendTransaction(ctx, signed)
	for i := 0; err != nil && strings.Contains(err.Error(), "insufficient funds") && i < 8; i++ { // the pool's view of the head lags (see send)
		time.Sleep(50 * time.Millisecond)
		err = ch.b.SendTransaction(ctx, signed)
	}
	if err != nil {
		return nil, cleanErr(err)
	}
	return ch.wait(signed)
}

// networkContract: the network's own contract at a, by name ("" if none).
func (c *Client) networkContract(a common.Address) string {
	for n, s := range c.ch.dep.Contracts {
		if common.HexToAddress(s) == a {
			return n
		}
	}
	return ""
}

func (c *Client) contractAt(s string) (common.Address, []byte, error) {
	a, err := c.resolveAddr(s)
	if err != nil {
		return a, nil, err
	}
	code, err := c.ch.b.CodeAt(context.Background(), a, nil)
	if err != nil {
		return a, nil, err
	}
	if len(code) == 0 {
		return a, nil, fmt.Errorf("no contract at %s (no code there)", a.Hex())
	}
	return a, code, nil
}

// ---- the owner's trust list: contract-trust claims on chain

func contractSubject(a common.Address) common.Hash { return common.BytesToHash(a.Bytes()) }

// liveClaims: live claims of a kind (about a, or any), each by the key its issuer held then.
func (c *Client) liveClaims(kind string, a *common.Address) ([]*Claim2, error) {
	var subj *common.Hash
	if a != nil {
		s := contractSubject(*a)
		subj = &s
	}
	all, err := c.easClaimsOf(subj, map[string]bool{kind: true})
	if err != nil {
		return nil, err
	}
	var out []*Claim2
	for _, cl := range all {
		if cl.Kind != kind || cl.Revoked || cl.Issuer == "" || subj != nil && cl.Subject != *subj {
			continue
		}
		bt, _ := c.ch.blockTime(cl.Block)
		if c.res.verifySigner(cl.Issuer, cl.Attester, cl.Block, bt) != nil {
			continue
		}
		out = append(out, cl)
	}
	return out, nil
}

// ownerTrusts: the owner (for an owner: itself) has a on its trust list. Only
// the owner's own name counts, never one of its agents.
func (c *Client) ownerTrusts(a common.Address) (bool, string) {
	cls, err := c.liveClaims(kindContractTrust, &a)
	if err != nil {
		return false, ""
	}
	for _, cl := range cls {
		if cl.Issuer == c.f.Owner {
			return true, cl.Data
		}
	}
	return false, ""
}

func (c *Client) contractTrust(s, note string, on bool) (map[string]any, error) {
	if !c.isOwner() {
		return nil, fmt.Errorf("only an owner keeps the trust list of outside contracts: ask your owner (ask-owner) to run 'ternchain contract trust %s --note WHY' if it should", s)
	}
	a, _, err := c.contractAt(s)
	if err != nil {
		return nil, err
	}
	if n := c.networkContract(a); n != "" {
		return nil, fmt.Errorf("%s is the network's own %s: no need to trust it", a.Hex(), n)
	}
	cls, err := c.liveClaims(kindContractTrust, &a)
	if err != nil {
		return nil, err
	}
	var mine []*Claim2
	for _, cl := range cls {
		if cl.Issuer == c.f.Name {
			mine = append(mine, cl)
		}
	}
	if !on {
		if len(mine) == 0 {
			return nil, fmt.Errorf("%s is not on your trust list (contract list)", a.Hex())
		}
		for _, cl := range mine {
			if err := c.easRevoke(cl.UID); err != nil {
				return nil, err
			}
		}
		return map[string]any{"untrusted": a.Hex(), "note": "your agents need your approval again to send to it"}, nil
	}
	if strings.TrimSpace(note) == "" {
		return nil, errors.New("contract trust ADDRESS --note TEXT: say what it is and why you trust it (who wrote it, who reviewed it); others in your circle see the note")
	}
	if len(mine) > 0 {
		return map[string]any{"trusted": a.Hex(), "note": mine[0].Data, "already": true}, nil
	}
	uid, err := c.easAttest(contractSubject(a), kindContractTrust, []byte(note), 100, 0, common.Hash{})
	if err != nil {
		return nil, err
	}
	return map[string]any{"trusted": a.Hex(), "note": note, "claim": uid.Hex(),
		"means": "your agents may send to it without asking (value still counts against free_pay; token approvals to untrusted spenders still need you)"}, nil
}

func (c *Client) contractList() (map[string]any, error) {
	cls, err := c.liveClaims(kindContractTrust, nil)
	if err != nil {
		return nil, err
	}
	var list []map[string]any
	for _, cl := range cls {
		if cl.Issuer != c.f.Owner {
			continue
		}
		list = append(list, map[string]any{"address": common.BytesToAddress(cl.Subject.Bytes()).Hex(), "note": cl.Data, "since": time.Unix(int64(cl.Time), 0).UTC().Format(time.RFC3339)})
	}
	return map[string]any{"owner": c.f.Owner, "trusted": list}, nil
}

// inCircle: someone whose word this identity weighs: its family, its contacts.
func (c *Client) inCircle(name string) bool {
	return c.family(name) || c.st.Contacts[name] || c.st.Contacts[ownerName(name)] || c.approvedSender(name)
}

const unauditedWarning = "unaudited outside contract: not one of the network's contracts, nobody reviewed it for you; it can take whatever you send or approve to it"

func (c *Client) contractInfo(s string) (map[string]any, error) {
	a, code, err := c.contractAt(s)
	if err != nil {
		return nil, err
	}
	r := map[string]any{"address": a.Hex(), "code_size": len(code), "code_hash": crypto.Keccak256Hash(code).Hex()}
	if n := c.networkContract(a); n != "" {
		r["network_contract"] = n
		return r, nil
	}
	srcs, err := c.liveClaims(kindContractSource, &a)
	if err != nil {
		return nil, err
	}
	var pub []map[string]any
	for _, cl := range srcs {
		var d struct {
			Source string `json:"source"`
			SHA256 string `json:"sha256"`
			Nonce  int64  `json:"deploy_nonce"`
		}
		json.Unmarshal([]byte(cl.Data), &d)
		p := map[string]any{"by": cl.Issuer, "source": c.outside(cl.Issuer, d.Source), "by_deployer": d.Nonce >= 0 && crypto.CreateAddress(cl.Attester, uint64(d.Nonce)) == a}
		if d.SHA256 != "" {
			p["sha256"] = d.SHA256
		}
		pub = append(pub, p)
	}
	r["source_published"] = pub
	if len(pub) == 0 {
		r["source_published"] = "none: nobody published its source (the deployer can: contract publish ADDRESS --source URL|FILE)"
	} else {
		r["source_note"] = "a published source is its publisher's word: compile it and compare the bytecode (code_hash) before you rely on it"
	}
	trust, err := c.liveClaims(kindContractTrust, &a)
	if err != nil {
		return nil, err
	}
	var circle []map[string]any
	others := 0
	for _, cl := range trust {
		if c.inCircle(cl.Issuer) {
			circle = append(circle, map[string]any{"by": cl.Issuer, "note": c.outside(cl.Issuer, cl.Data)})
		} else {
			others++
		}
	}
	ok, _ := c.ownerTrusts(a)
	r["trusted_by_owner"], r["trusted_in_your_circle"], r["trusted_by_others"] = ok, circle, others
	r["warning"] = unauditedWarning
	return r, nil
}

// contractPublish: a claim of kind contract-source: where the source is, its
// hash, and the nonce the publisher deployed it with (so readers can check the
// publisher is its deployer).
func (c *Client) contractPublish(s, source string) (map[string]any, error) {
	if source == "" {
		return nil, errors.New("contract publish ADDRESS --source URL|FILE: where its source is (a URL, ipfs://…, or a file whose hash goes on chain)")
	}
	a, _, err := c.contractAt(s)
	if err != nil {
		return nil, err
	}
	d := map[string]any{"source": source, "deploy_nonce": -1}
	if b, err := os.ReadFile(source); err == nil {
		sum := sha256.Sum256(b)
		d["source"], d["sha256"] = filepath.Base(source), hex.EncodeToString(sum[:])
	} else if !strings.Contains(source, "://") {
		return nil, fmt.Errorf("--source %s: neither a file nor a URL", source)
	}
	me := c.me()
	if n, err := c.ch.b.NonceAt(context.Background(), me, nil); err == nil {
		for i := uint64(0); i < n && i < 1_000_000; i++ {
			if crypto.CreateAddress(me, i) == a {
				d["deploy_nonce"] = int64(i)
				break
			}
		}
	}
	data, _ := json.Marshal(d)
	uid, err := c.easAttest(contractSubject(a), kindContractSource, data, 0, 0, common.Hash{})
	if err != nil {
		return nil, err
	}
	r := map[string]any{"published": a.Hex(), "source": d["source"], "claim": uid.Hex(), "by_deployer": d["deploy_nonce"] != -1}
	if d["sha256"] != nil {
		r["sha256"] = d["sha256"]
	}
	return r, nil
}

// ---- the agent's guard for outside contracts

// guardValue: coins sent along (to a contract, or into a constructor) count
// like a payment: under free_pay within the day's free budget, else one owner
// approval for at most that (owner approve AGENT KEY AMOUNT). Unlike pay, it
// applies untainted too: the Bank's spend limit doesn't cover these coins.
func (c *Client) guardValue(s agentScopes, key string, value *big.Int, what string) (func(), error) {
	if value == nil || value.Sign() == 0 {
		return func() {}, nil
	}
	if len(s.To) > 0 && !s.allows(key) {
		return nil, s.toRefusal("sending coins to", key)
	}
	var rec *FreeRec
	c.locked(func() {
		if c.freePay(s, value) {
			rec = &FreeRec{Time: c.chainNow(), Amount: value.String(), ID: randHex(8)}
			c.st.Free = append(c.st.Free, *rec)
		}
	})
	if rec != nil {
		return func() { c.locked(func() { c.st.Free = dropFree(c.st.Free, rec.ID) }) }, nil
	}
	if err := c.useApproval(key, value, what); err != nil {
		return nil, c.freeCapNote(err, value)
	}
	nonce := c.lastApproval
	return func() { c.locked(func() { delete(c.st.UsedApprovals, nonce) }) }, nil
}

// tokenApproval: the spender a call lets move the caller's tokens, and how
// much (ERC-20 approve and increaseAllowance, EIP-2612 permit sent by the
// holder, ERC-721/1155 setApprovalForAll: "all"); a zero allowance or false
// withdraws one and needs nothing.
func tokenApproval(data []byte) (common.Address, string, bool) {
	word := func(i int) []byte {
		if len(data) < 4+32*(i+1) {
			return nil
		}
		return data[4+32*i : 4+32*(i+1)]
	}
	if len(data) < 68 {
		return common.Address{}, "", false
	}
	sp, amt := word(0), word(1)
	sel := hex.EncodeToString(data[:4])
	switch sel {
	case "095ea7b3", "39509351", "a22cb465": // approve, increaseAllowance, setApprovalForAll
	case "d505accf": // permit(owner, spender, value, deadline, v, r, s)
		if sp, amt = word(1), word(2); sp == nil || amt == nil {
			return common.Address{}, "", false
		}
	default:
		return common.Address{}, "", false
	}
	n := new(big.Int).SetBytes(amt)
	if n.Sign() == 0 {
		return common.Address{}, "", false
	}
	amount := n.String()
	if sel == "a22cb465" {
		amount = "all"
	}
	return common.BytesToAddress(sp), amount, true
}

// tokenTransfer: where a call moves tokens, and how many: ERC-20 transfer
// and transferFrom, ERC-721 transferFrom/safeTransferFrom (the amount is the
// token id: "id N"), ERC-1155 safeTransferFrom and safeBatchTransferFrom
// ("batch" plus a hash of the call). ok false: not a transfer this knows.
func tokenTransfer(data []byte) (to common.Address, amount string, ok bool) {
	if len(data) < 4 {
		return to, "", false
	}
	word := func(i int) []byte {
		if len(data) < 4+32*(i+1) {
			return nil
		}
		return data[4+32*i : 4+32*(i+1)]
	}
	switch hex.EncodeToString(data[:4]) {
	case "a9059cbb": // transfer(address,uint256)
		if word(1) == nil {
			return to, "", false
		}
		return common.BytesToAddress(word(0)), new(big.Int).SetBytes(word(1)).String(), true
	case "23b872dd", "42842e0e", "b88d4fde": // transferFrom(from,to,v), safeTransferFrom(from,to,id[,bytes])
		if word(2) == nil {
			return to, "", false
		}
		return common.BytesToAddress(word(1)), new(big.Int).SetBytes(word(2)).String(), true
	case "f242432a": // safeTransferFrom(from,to,id,amount,bytes) (ERC-1155)
		if word(3) == nil {
			return to, "", false
		}
		return common.BytesToAddress(word(1)), new(big.Int).SetBytes(word(2)).String() + "x" + new(big.Int).SetBytes(word(3)).String(), true
	case "2eb2c2d6": // safeBatchTransferFrom(from,to,ids,amounts,bytes)
		if word(1) == nil {
			return to, "", false
		}
		return common.BytesToAddress(word(1)), "batch-" + crypto.Keccak256Hash(data).Hex()[2:14], true
	}
	return to, "", false
}

// guardToken: a token transfer by an agent is a payment the Bank's limit
// doesn't see (audit 6 CL-1). Trusting the token contract doesn't cover it:
// the recipient must be the family or allowed by scopes.to, and every
// transfer out of the family needs the owner's approval for exactly it
// (token, recipient, amount), unless the agent is untainted and the
// recipient is one scopes.to names.
func (c *Client) guardToken(s agentScopes, token common.Address, data []byte) (func(), error) {
	to, amount, ok := tokenTransfer(data)
	if !ok {
		return func() {}, nil
	}
	if to == c.me() {
		return func() {}, nil
	}
	if oa, err := c.addressOf(c.f.Owner); err == nil && oa == to {
		return func() {}, nil
	}
	named := "" // the name in scopes.to that holds this address
	for _, n := range s.To {
		if a, err := c.addressOf(n); err == nil && a == to {
			named = n
			break
		}
	}
	if len(s.To) > 0 && named == "" {
		return nil, s.toRefusal("moving tokens to", to.Hex())
	}
	if named != "" && len(c.st.Taint) == 0 {
		return func() {}, nil
	}
	if err := c.useApproval(tokenKey(token, to, amount), nil, fmt.Sprintf("moving tokens of %s (%s) to %s (the Bank's limit doesn't cover tokens)", token.Hex(), amount, to.Hex())); err != nil {
		return nil, err
	}
	nonce := c.lastApproval
	return func() { c.locked(func() { delete(c.st.UsedApprovals, nonce) }) }, nil
}

// guardContract: may this agent send this call (to nil: a deploy) now? done(ok) after the transaction.
func (c *Client) guardContract(to *common.Address, data []byte, value *big.Int) (func(bool), error) {
	none := func(bool) {}
	if c.isOwner() {
		return none, nil
	}
	c.syncGuard()
	s, err := c.scopes()
	if err != nil {
		return none, err
	}
	dir := c.ownerDirWords()
	if to == nil {
		if err := c.useApproval(deployKey, nil, "deploying a contract (outside code, run under your owner's name)"); err != nil {
			return none, err
		}
		nonce := c.lastApproval
		undoV, err := c.guardValue(s, deployKey, value, fmt.Sprintf("sending %s coins into a new contract", fmtCoins(orZero(value))))
		if err != nil {
			c.locked(func() { delete(c.st.UsedApprovals, nonce) })
			return none, err
		}
		return func(ok bool) {
			if !ok {
				undoV()
				c.locked(func() { delete(c.st.UsedApprovals, nonce) })
			}
		}, nil
	}
	a := *to
	if n := c.networkContract(a); n != "" {
		return none, fmt.Errorf("%s is the network's own %s: use its commands (pay, lock …, item …, commons …), which carry your guard and limits; raw sends to the network's contracts are for owners", a.Hex(), n)
	}
	if ok, _ := c.ownerTrusts(a); !ok {
		if err := c.useApproval(contractKey(a), nil, fmt.Sprintf("sending to outside contract %s (not on %s's trust list; unaudited code)", a.Hex(), c.f.Owner)); err != nil {
			return none, fmt.Errorf("%w. Or, to let all its agents use it, the owner trusts it: ternchain contract trust --dir %s %s --note WHY (contract info %s shows what is known about it)", err, dir, a.Hex(), a.Hex())
		}
	}
	undoV, err := c.guardValue(s, contractKey(a), value, fmt.Sprintf("sending %s coins to outside contract %s", fmtCoins(orZero(value)), a.Hex()))
	if err != nil {
		return none, err
	}
	undo := []func(){undoV}
	undoT, err := c.guardToken(s, a, data)
	if err != nil {
		undoV()
		return none, err
	}
	undo = append(undo, undoT)
	if sp, amount, ok := tokenApproval(data); ok && c.networkContract(sp) == "" {
		// A spender, trusted or not, moves the tokens with transferFrom: guardToken
		// never sees that transfer, so the allowance is the payment (red team F2).
		// Untrusted: the owner approves the spender. Trusted (an opted-in module):
		// the owner approves exactly this token, spender and amount.
		key, what := approveKey(sp), fmt.Sprintf("a token approval that lets %s move your tokens (a spender not on %s's trust list)", sp.Hex(), c.f.Owner)
		if trusted, _ := c.ownerTrusts(sp); trusted {
			key, what = approveAmountKey(a, sp, amount), fmt.Sprintf("a token approval that lets %s (trusted by %s) move %s of token %s: the Bank's limit doesn't cover what it moves", sp.Hex(), c.f.Owner, amount, a.Hex())
		}
		if err := c.useApproval(key, nil, what); err != nil {
			for _, u := range undo {
				u()
			}
			return none, err
		}
		nonce := c.lastApproval
		undo = append(undo, func() { c.locked(func() { delete(c.st.UsedApprovals, nonce) }) })
	}
	return func(ok bool) {
		if !ok {
			for _, u := range undo {
				u()
			}
		}
	}, nil
}

// ---- deploy, call, send

func (c *Client) contractDeploy(artifact, abiF, binF string, args []string, value *big.Int) (map[string]any, error) {
	art, err := loadContractArtifact(artifact, abiF, binF)
	if err != nil {
		return nil, err
	}
	vs, err := c.abiArgs(art.abi.Constructor.Inputs, args, "the constructor")
	if err != nil {
		return nil, err
	}
	enc, err := art.abi.Pack("", vs...)
	if err != nil {
		return nil, err
	}
	data := append(append([]byte{}, art.bin...), enc...)
	summary := fmt.Sprintf("deploy %s (%d bytes) from %s with %v, value %s", art.name, len(art.bin), c.f.Name, args, fmtCoins(orZero(value)))
	done, err := c.guardContract(nil, data, value)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr, "ternchain: "+summary)
	r, err := c.ch.sendRaw(c.key, value, nil, data)
	done(err == nil)
	if err != nil {
		return nil, fmt.Errorf("deploy %s: %w", art.name, err)
	}
	return map[string]any{"deployed": art.name, "address": r.ContractAddress.Hex(), "tx": r.TxHash.Hex(), "gas_used": r.GasUsed, "block": r.BlockNumber.Uint64(),
		"summary": summary,
		"next": fmt.Sprintf("contract publish %s --source URL|FILE (others can read what it does); owners: contract trust %s --note WHY lets your agents use it",
			r.ContractAddress.Hex(), r.ContractAddress.Hex())}, nil
}

func orZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}

func (c *Client) packCall(addrS, sig string, args []string, abiF string) (common.Address, abi.Method, []byte, error) {
	m, err := methodFor(sig, abiF)
	if err != nil {
		return common.Address{}, m, nil, err
	}
	a, _, err := c.contractAt(addrS)
	if err != nil {
		return a, m, nil, err
	}
	vs, err := c.abiArgs(m.Inputs, args, m.Sig)
	if err != nil {
		return a, m, nil, err
	}
	enc, err := m.Inputs.Pack(vs...)
	if err != nil {
		return a, m, nil, err
	}
	return a, m, append(append([]byte{}, m.ID...), enc...), nil
}

// outsideErr: an outside contract's revert reason is its author's text: an
// agent gets it fenced, and is tainted by it (audit 6 CL-2).
func (c *Client) outsideErr(a common.Address, sig string, err error) error {
	if c.isOwner() || c.networkContract(a) != "" {
		return fmt.Errorf("%s at %s: %w", sig, a.Hex(), err)
	}
	return fmt.Errorf("%s at %s failed; its error text: %s", sig, a.Hex(), c.outside("contract "+a.Hex(), err.Error()))
}

// contractCall: read-only. What an outside contract returns reaches an agent fenced.
func (c *Client) contractCall(addrS, sig string, args []string, abiF string) (map[string]any, error) {
	a, m, data, err := c.packCall(addrS, sig, args, abiF)
	if err != nil {
		return nil, err
	}
	ret, err := c.ch.rawCall(c.me(), a, data)
	if err != nil {
		return nil, c.outsideErr(a, m.Sig, err)
	}
	var result any = "0x" + hex.EncodeToString(ret)
	if len(m.Outputs) > 0 {
		vals, err := m.Outputs.Unpack(ret)
		if err != nil {
			return nil, fmt.Errorf("%s returned %d bytes that aren't %v: %v", m.Sig, len(ret), m.Outputs, err)
		}
		var outs []any
		for _, v := range vals {
			outs = append(outs, abiOut(v))
		}
		result = outs
		if len(outs) == 1 {
			result = outs[0]
		}
	}
	r := map[string]any{"contract": a.Hex(), "function": m.Sig, "result": result}
	if n := c.networkContract(a); n != "" {
		r["network_contract"] = n
	} else if !c.isOwner() {
		b, _ := json.Marshal(result)
		r["result"] = c.outside("contract "+a.Hex(), string(b))
	}
	return r, nil
}

func (c *Client) contractSend(addrS, sig string, args []string, abiF string, value *big.Int) (map[string]any, error) {
	a, m, data, err := c.packCall(addrS, sig, args, abiF)
	if err != nil {
		return nil, err
	}
	done, err := c.guardContract(&a, data, value)
	if err != nil {
		return nil, err
	}
	summary := fmt.Sprintf("send %s%v to %s from %s, value %s", m.Sig, args, a.Hex(), c.f.Name, fmtCoins(orZero(value)))
	fmt.Fprintln(os.Stderr, "ternchain: "+summary)
	r, err := c.ch.sendRaw(c.key, value, &a, data)
	done(err == nil)
	if err != nil {
		return nil, c.outsideErr(a, m.Sig, err)
	}
	res := map[string]any{"sent": m.Sig, "contract": a.Hex(), "tx": r.TxHash.Hex(), "gas_used": r.GasUsed, "block": r.BlockNumber.Uint64(), "logs": len(r.Logs), "summary": summary}
	if c.networkContract(a) == "" {
		res["warning"] = unauditedWarning
	}
	return res, nil
}
