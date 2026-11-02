// x402: HTTP payments in USDC (github.com/coinbase/x402; version 2, version 1 read too).
//
// A ternchain agent pays an x402 endpoint: the server answers 402 with what
// it wants (PAYMENT-REQUIRED), the agent signs an EIP-3009
// transferWithAuthorization for exactly that (its key holds the USDC; it
// needs no ETH: whoever settles pays the gas) and asks again with
// PAYMENT-SIGNATURE. Before signing, the owner's rules apply as to any
// payment: scopes.to (the payee is "x402:HOST"), free_pay and the owner's
// approvals for more (owner approve AGENT x402:HOST AMOUNT), and the
// agent's per-epoch limit for the token, which the Bank keeps for its
// payments and the client counts for these (the money moves outside the
// Bank, from the agent's own key, which holds only what the owner gave it:
// x402 fund).
//
// A ternchain service charges through x402 (x402 serve): it answers 402,
// checks the payment and settles it, by a facilitator (the public one on
// Base Sepolia: https://x402.org/facilitator) or itself (its account sends
// transferWithAuthorization; on a sponsored network the paymaster pays).
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const x402Facilitator = "https://x402.org/facilitator" // the public one: testnets, no key

type x402Requirements struct {
	Scheme            string         `json:"scheme"`
	Network           string         `json:"network"`
	Amount            string         `json:"amount,omitempty"`            // v2
	MaxAmountRequired string         `json:"maxAmountRequired,omitempty"` // v1
	Asset             string         `json:"asset"`
	PayTo             string         `json:"payTo"`
	MaxTimeoutSeconds int64          `json:"maxTimeoutSeconds"`
	Resource          string         `json:"resource,omitempty"` // v1
	Description       string         `json:"description,omitempty"`
	MimeType          string         `json:"mimeType,omitempty"`
	Extra             map[string]any `json:"extra,omitempty"`
}

func (r x402Requirements) amount() (*big.Int, bool) {
	s := r.Amount
	if s == "" {
		s = r.MaxAmountRequired
	}
	return new(big.Int).SetString(s, 10)
}

type x402Resource struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type x402Required struct {
	X402Version int                `json:"x402Version"`
	Error       string             `json:"error,omitempty"`
	Resource    *x402Resource      `json:"resource,omitempty"`
	Accepts     []x402Requirements `json:"accepts"`
}

type x402Authorization struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Value       string `json:"value"`
	ValidAfter  string `json:"validAfter"`
	ValidBefore string `json:"validBefore"`
	Nonce       string `json:"nonce"`
}

type x402Payload struct {
	X402Version int               `json:"x402Version"`
	Resource    *x402Resource     `json:"resource,omitempty"`
	Accepted    *x402Requirements `json:"accepted,omitempty"` // v2
	Scheme      string            `json:"scheme,omitempty"`   // v1
	Network     string            `json:"network,omitempty"`  // v1
	Payload     struct {
		Signature     string            `json:"signature"`
		Authorization x402Authorization `json:"authorization"`
	} `json:"payload"`
}

type x402Settlement struct {
	Success     bool   `json:"success"`
	Transaction string `json:"transaction"`
	Network     string `json:"network"`
	Payer       string `json:"payer,omitempty"`
	ErrorReason string `json:"errorReason,omitempty"`
}

func b64JSON(v any) string {
	b, _ := json.Marshal(v)
	return base64.StdEncoding.EncodeToString(b)
}

func unb64JSON(s string, v any) error {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// x402Networks: what x402 calls this chain (v2: CAIP-2; v1: a name).
func (ch *Chain) x402Networks() []string {
	id := ch.chainID.Int64()
	out := []string{fmt.Sprintf("eip155:%d", id)}
	if h, ok := hostByChain(id); ok && h.X402Name != "" {
		out = append(out, h.X402Name)
	}
	return out
}

// eip3009Digest: the EIP-712 hash of a TransferWithAuthorization for a token
// whose domain is (name, version, this chain, token): what USDC checks.
func eip3009Digest(name, version string, chainID *big.Int, token common.Address, a x402Authorization) (common.Hash, error) {
	u := func(s string) (*big.Int, error) {
		v, ok := new(big.Int).SetString(s, 10)
		if !ok {
			return nil, fmt.Errorf("not a number: %q", s)
		}
		return v, nil
	}
	value, err := u(a.Value)
	if err != nil {
		return common.Hash{}, err
	}
	after, err := u(a.ValidAfter)
	if err != nil {
		return common.Hash{}, err
	}
	before, err := u(a.ValidBefore)
	if err != nil {
		return common.Hash{}, err
	}
	nonce := common.HexToHash(a.Nonce)
	args := abi.Arguments{{Type: abiType("bytes32")}, {Type: abiType("address")}, {Type: abiType("address")}, {Type: abiType("uint256")},
		{Type: abiType("uint256")}, {Type: abiType("uint256")}, {Type: abiType("bytes32")}}
	th := crypto.Keccak256Hash([]byte("TransferWithAuthorization(address from,address to,uint256 value,uint256 validAfter,uint256 validBefore,bytes32 nonce)"))
	sb, err := args.Pack(th, common.HexToAddress(a.From), common.HexToAddress(a.To), value, after, before, nonce)
	if err != nil {
		return common.Hash{}, err
	}
	return crypto.Keccak256Hash([]byte{0x19, 0x01}, eip712DomainSeparator(name, version, chainID, token).Bytes(), crypto.Keccak256(sb)), nil
}

// eip712DomainSeparator: EIP712Domain(name, version, chainId, verifyingContract).
func eip712DomainSeparator(name, version string, chainID *big.Int, token common.Address) common.Hash {
	dargs := abi.Arguments{{Type: abiType("bytes32")}, {Type: abiType("bytes32")}, {Type: abiType("bytes32")}, {Type: abiType("uint256")}, {Type: abiType("address")}}
	dth := crypto.Keccak256Hash([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	db, _ := dargs.Pack(dth, crypto.Keccak256Hash([]byte(name)), crypto.Keccak256Hash([]byte(version)), chainID, token)
	return crypto.Keccak256Hash(db)
}

func extraString(m map[string]any, k, def string) string {
	if v, ok := m[k].(string); ok && v != "" {
		return v
	}
	return def
}

// x402Pick: the requirement this client can pay: the exact scheme, on this
// chain, in a token it knows, at most max.
func (c *Client) x402Pick(req x402Required, max *big.Int) (x402Requirements, *big.Int, error) {
	var why []string
	for _, r := range req.Accepts {
		amt, ok := r.amount()
		switch {
		case r.Scheme != "exact":
			why = append(why, "scheme "+r.Scheme)
		case !containsFold(c.ch.x402Networks(), r.Network):
			why = append(why, "network "+r.Network)
		case !common.IsHexAddress(r.Asset) || !common.IsHexAddress(r.PayTo) || !ok || amt.Sign() <= 0:
			why = append(why, "a malformed requirement")
		case max != nil && amt.Cmp(max) > 0:
			why = append(why, fmt.Sprintf("%s %s, over --max", c.fmtIn(amt, common.HexToAddress(r.Asset)), c.ch.assetSymbol(common.HexToAddress(r.Asset))))
		default:
			return r, amt, nil
		}
	}
	return x402Requirements{}, nil, fmt.Errorf("nothing this agent can pay (%s): it pays the exact scheme on %s, up to --max", strings.Join(why, "; "),
		strings.Join(c.ch.x402Networks(), " / "))
}

func containsFold(xs []string, s string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// X402Rec: an x402 payment made (state: the epoch's sum against the token limit).
type X402Rec struct {
	Epoch  uint64 `json:"epoch"`
	Token  string `json:"token"`
	Amount string `json:"amount"`
	To     string `json:"to"`
	PayTo  string `json:"pay_to,omitempty"` // the address the server named
}

// x402MaxValid: the longest an x402 authorization this client signs is valid (seconds).
const x402MaxValid = 600

// taintLabel: an agent has read something from outside the network's names
// (an x402 server: "x402:HOST"), kept as is in the taint list.
func (c *Client) taintLabel(label string) {
	if c.isOwner() {
		return
	}
	n := len(c.st.Taint)
	c.st.Taint = uniq(append(c.st.Taint, label))
	if len(c.st.Taint) != n {
		c.save()
	}
}

// x402Within: an agent's x402 payments this epoch, with this one, stay within
// the per-epoch limit its owner set for the token (owner grant --spend on a
// USDC network; token limit).
func (c *Client) x402Within(token common.Address, amount *big.Int) error {
	if err := c.ch.capOne(token, amount); err != nil { // a mainnet's per-payment cap (net.json caps), owners' too
		return err
	}
	if c.isOwner() {
		return nil
	}
	e, err := c.ch.epoch()
	if err != nil {
		return err
	}
	lim, err := c.ch.call("Bank", "tokenLimit", c.id(), token)
	if err != nil {
		return err
	}
	sum := new(big.Int).Set(amount)
	if sp, err := c.ch.call("Bank", "tokenSpent", c.id(), token, e); err == nil { // what it paid through the Bank counts too
		sum.Add(sum, sp[0].(*big.Int))
	}
	for _, r := range c.st.X402 {
		if r.Epoch == e && strings.EqualFold(r.Token, token.Hex()) {
			v, _ := new(big.Int).SetString(r.Amount, 10)
			if v != nil {
				sum.Add(sum, v)
			}
		}
	}
	if l := lim[0].(*big.Int); sum.Cmp(l) > 0 {
		return fmt.Errorf("over the agent's limit: %s %s per epoch (x402 and Bank payments together; the owner raises it with a new grant)",
			c.fmtIn(l, token), c.ch.assetSymbol(token))
	}
	return nil
}

// x402Pay: GET (or POST body) url; if it asks for an x402 payment, pay it
// (within the owner's rules) and ask again. The answer and what was paid.
func (c *Client) x402Pay(rawURL, method string, body []byte, max *big.Int) (*http.Response, []byte, map[string]any, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, nil, nil, fmt.Errorf("x402 pay: an http(s) URL, not %q", rawURL)
	}
	do := func(h map[string]string) (*http.Response, []byte, error) {
		var rd io.Reader
		if body != nil {
			rd = strings.NewReader(string(body))
		}
		req, err := http.NewRequest(method, rawURL, rd)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		return resp, b, nil
	}
	label := "x402:" + u.Host
	resp, b, err := do(nil)
	if err != nil || resp.StatusCode != http.StatusPaymentRequired {
		if err == nil {
			c.taintLabel(label) // what the server answers is outside text, paid or not (red team E4)
		}
		return resp, b, nil, err
	}
	var req x402Required
	if h := resp.Header.Get("PAYMENT-REQUIRED"); h != "" { // v2
		if err := unb64JSON(h, &req); err != nil {
			return nil, nil, nil, fmt.Errorf("x402: PAYMENT-REQUIRED: %w", err)
		}
	} else if err := json.Unmarshal(b, &req); err != nil || len(req.Accepts) == 0 { // v1: in the body
		return resp, b, nil, errors.New("402 without x402 payment requirements")
	}
	r, amt, err := c.x402Pick(req, max)
	if err != nil {
		return nil, nil, nil, err
	}
	token := common.HexToAddress(r.Asset)
	payee := label
	c.payNote = "pay-to " + common.HexToAddress(r.PayTo).Hex() + ", chosen by the server"
	done, err := c.guardPayIn(payee, amt, token) // scopes.to, free_pay, the owner's approvals (owner approve AGENT x402:HOST AMOUNT)
	c.payNote = ""
	if err != nil {
		return nil, nil, nil, err
	}
	ok := false
	defer func() { done(ok) }()
	if err := c.x402Within(token, amt); err != nil {
		return nil, nil, nil, err
	}
	now := time.Now().Unix()
	if h, err := c.ch.b.HeaderByNumber(ctxb(), nil); err == nil { // the chain's clock decides validity
		now = int64(h.Time)
	}
	// The authorization lives as long as the server says, but never more than
	// x402MaxValid: a signature the server keeps unused can't be banked for
	// years (red team I5).
	timeout := r.MaxTimeoutSeconds
	if timeout <= 0 {
		timeout = 60
	}
	timeout = min(timeout, x402MaxValid)
	var nonce [32]byte
	rand.Read(nonce[:])
	a := x402Authorization{From: c.addr().Hex(), To: common.HexToAddress(r.PayTo).Hex(), Value: amt.String(),
		ValidAfter: strconv.FormatInt(now-600, 10), ValidBefore: strconv.FormatInt(now+timeout, 10), Nonce: "0x" + hex.EncodeToString(nonce[:])}
	dn, dv := c.ch.tokenDomain(token, extraString(r.Extra, "name", "USDC"), extraString(r.Extra, "version", "2")) // the token's own, over what the server says
	d, err := eip3009Digest(dn, dv, c.ch.chainID, token, a)
	if err != nil {
		return nil, nil, nil, err
	}
	sig, err := crypto.Sign(d[:], c.key)
	if err != nil {
		return nil, nil, nil, err
	}
	sig[64] += 27
	p := x402Payload{X402Version: req.X402Version}
	p.Payload.Signature, p.Payload.Authorization = "0x"+hex.EncodeToString(sig), a
	header := "PAYMENT-SIGNATURE"
	if req.X402Version >= 2 {
		p.Resource, p.Accepted = req.Resource, &r
	} else {
		p.X402Version, p.Scheme, p.Network, header = 1, r.Scheme, r.Network, "X-PAYMENT"
	}
	// A signed authorization handed to the server is money it can take, by
	// settling it itself, whatever it answers. So it counts as paid from here
	// on: against the token limit, the day's free budget and the approval
	// (red team I5), and stays counted even if the server refuses or fails.
	ok = true
	if e, err := c.ch.epoch(); err == nil {
		c.locked(func() {
			c.st.X402 = append(c.st.X402, X402Rec{Epoch: e, Token: token.Hex(), Amount: amt.String(), To: payee, PayTo: common.HexToAddress(r.PayTo).Hex()})
			if len(c.st.X402) > 1000 {
				c.st.X402 = c.st.X402[len(c.st.X402)-1000:]
			}
		})
		c.save()
	} else {
		return nil, nil, nil, fmt.Errorf("x402: can't read the epoch to count this payment, not paying: %w", err)
	}
	resp, b, err = do(map[string]string{header: b64JSON(p)})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("x402: the payment was signed and sent, so it counts against your limit until it expires (at most %d s): %w", timeout, err)
	}
	c.taintLabel(label) // the answer (content or refusal) is outside text
	info := map[string]any{"paid": c.fmtIn(amt, token) + " " + c.ch.assetSymbol(token), "to": r.PayTo, "payee": payee, "from": c.addr().Hex(),
		"network": r.Network, "x402_version": req.X402Version, "status": resp.StatusCode}
	var st x402Settlement
	for _, h := range []string{"PAYMENT-RESPONSE", "X-PAYMENT-RESPONSE"} {
		if v := resp.Header.Get(h); v != "" && unb64JSON(v, &st) == nil {
			info["settlement"] = st
		}
	}
	if resp.StatusCode/100 != 2 || (info["settlement"] != nil && !st.Success) {
		why := c.outside(label, firstNonEmpty(st.ErrorReason, strings.TrimSpace(string(b[:min(len(b), 300)]))))
		return resp, b, info, fmt.Errorf("x402: the server refused the payment: %s %s (the signed payment still counts against your limit: the server may settle it anyway)", resp.Status, why)
	}
	return resp, b, info, nil
}

// x402Fund: an owner gives its agent's key USDC to pay x402 with (from the
// owner's bank: the agent never holds more than this).
func (c *Client) x402Fund(agent string, amount *big.Int) error {
	if !c.isOwner() {
		return errors.New("an owner funds its agents")
	}
	if c.asset == (common.Address{}) {
		return errors.New("x402 pays in a token: this network has no settlement token (--token ADDRESS)")
	}
	if ownerName(agent) != c.f.Name {
		return fmt.Errorf("%s is not your agent", agent)
	}
	g, err := c.ch.call("Registry", "grants", idOf(agent))
	if err != nil {
		return err
	}
	key := g[1].(common.Address) // the agent's key, or its account: x402 signs with the key (the account's signer)
	if code, err := c.ch.b.CodeAt(ctxb(), key, nil); err == nil && len(code) > 0 {
		data, _ := c.ch.abis["AgentAccount"].Pack("signer")
		out, err := c.ch.b.CallContract(ctxb(), ethereum.CallMsg{To: &key, Data: data}, nil)
		if err != nil || len(out) < 32 {
			return fmt.Errorf("%s's account has no signer to pay x402 with: %v", agent, err)
		}
		key = common.BytesToAddress(out[12:32])
	}
	_, err = c.ch.send(c.key, nil, "Bank", "withdrawToken", c.id(), c.asset, key, amount)
	return err
}

// ---- a ternchain service that charges through x402

type x402Server struct {
	c           *Client
	price       *big.Int
	token       common.Address
	payTo       common.Address
	facilitator string // "" = settle itself
	desc        string
	content     func(w http.ResponseWriter, r *http.Request)
}

func (s *x402Server) requirements(r *http.Request) x402Required {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	res := &x402Resource{URL: scheme + "://" + r.Host + r.URL.RequestURI(), Description: s.desc, MimeType: "application/octet-stream"}
	name, version := s.c.ch.tokenDomain(s.token, "USDC", "2") // "USD Coin" on Base mainnet
	req := x402Requirements{Scheme: "exact", Network: s.c.ch.x402Networks()[0], Amount: s.price.String(), Asset: s.token.Hex(),
		PayTo: s.payTo.Hex(), MaxTimeoutSeconds: 120, Extra: map[string]any{"name": name, "version": version}}
	return x402Required{X402Version: 2, Resource: res, Accepts: []x402Requirements{req}}
}

func (s *x402Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	want := s.requirements(r)
	h := r.Header.Get("PAYMENT-SIGNATURE")
	if h == "" {
		h = r.Header.Get("X-PAYMENT")
	}
	refuse := func(why string) {
		want.Error = why
		w.Header().Set("PAYMENT-REQUIRED", b64JSON(want))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		json.NewEncoder(w).Encode(want) // v1 clients read the body
	}
	if h == "" {
		refuse("PAYMENT-SIGNATURE header is required")
		return
	}
	var p x402Payload
	if err := unb64JSON(h, &p); err != nil {
		refuse("malformed payment")
		return
	}
	acc := want.Accepts[0]
	a := p.Payload.Authorization
	if !strings.EqualFold(a.To, acc.PayTo) || a.Value != acc.Amount {
		refuse("the payment is not for this resource's price and payee")
		return
	}
	st, err := s.settle(p, acc)
	if err != nil {
		w.Header().Set("PAYMENT-RESPONSE", b64JSON(x402Settlement{Success: false, Network: acc.Network, ErrorReason: err.Error()}))
		refuse(err.Error())
		return
	}
	w.Header().Set("PAYMENT-RESPONSE", b64JSON(st))
	w.Header().Set("X-PAYMENT-RESPONSE", b64JSON(st))
	s.content(w, r)
}

// settle: by the facilitator (verify, then settle), or here: the signature
// checked, then transferWithAuthorization sent from this service's account.
func (s *x402Server) settle(p x402Payload, acc x402Requirements) (x402Settlement, error) {
	if p.X402Version < 2 {
		p.Accepted = &acc
	}
	if s.facilitator != "" {
		body := struct { // the facilitator's field names
			X402Version         int              `json:"x402Version"`
			PaymentPayload      x402Payload      `json:"paymentPayload"`
			PaymentRequirements x402Requirements `json:"paymentRequirements"`
		}{p.X402Version, p, acc}
		var v struct {
			IsValid       bool   `json:"isValid"`
			InvalidReason string `json:"invalidReason"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := postJSON(ctx, s.facilitator+"/verify", body, &v); err != nil {
			return x402Settlement{}, err
		}
		if !v.IsValid {
			return x402Settlement{}, errors.New("invalid payment: " + v.InvalidReason)
		}
		var st x402Settlement
		if err := postJSON(ctx, s.facilitator+"/settle", body, &st); err != nil {
			return x402Settlement{}, err
		}
		if !st.Success {
			return st, errors.New("settlement failed: " + st.ErrorReason)
		}
		return st, nil
	}
	a := p.Payload.Authorization
	dn, dv := s.c.ch.tokenDomain(s.token, "USDC", "2")
	d, err := eip3009Digest(dn, dv, s.c.ch.chainID, s.token, a)
	if err != nil {
		return x402Settlement{}, err
	}
	sig := common.FromHex(p.Payload.Signature)
	if len(sig) != 65 {
		return x402Settlement{}, errors.New("invalid signature")
	}
	rs := append([]byte{}, sig...)
	if rs[64] >= 27 {
		rs[64] -= 27
	}
	pub, err := crypto.SigToPub(d[:], rs)
	if err != nil || !strings.EqualFold(crypto.PubkeyToAddress(*pub).Hex(), a.From) {
		return x402Settlement{}, errors.New("invalid signature")
	}
	big10 := func(x string) *big.Int { v, _ := new(big.Int).SetString(x, 10); return v }
	usdc, err := erc20With3009()
	if err != nil {
		return x402Settlement{}, err
	}
	data, err := usdc.Pack("transferWithAuthorization", common.HexToAddress(a.From), common.HexToAddress(a.To), big10(a.Value), big10(a.ValidAfter),
		big10(a.ValidBefore), common.HexToHash(a.Nonce), sig[64], common.BytesToHash(sig[:32]), common.BytesToHash(sig[32:64]))
	if err != nil {
		return x402Settlement{}, err
	}
	rc, err := s.c.ch.sendRaw(s.c.key, nil, &s.token, data)
	if err != nil {
		return x402Settlement{}, fmt.Errorf("settling: %w", err)
	}
	return x402Settlement{Success: true, Transaction: rc.TxHash.Hex(), Network: acc.Network, Payer: a.From}, nil
}

// erc20With3009: transferWithAuthorization(…, uint8 v, bytes32 r, bytes32 s), as USDC has it.
func erc20With3009() (abi.ABI, error) {
	a, err := loadArtifact("TestUSDC")
	if err != nil {
		return abi.ABI{}, err
	}
	for n, m := range a.abi.Methods {
		if m.RawName == "transferWithAuthorization" && len(m.Inputs) == 9 {
			a.abi.Methods["transferWithAuthorization"] = m
			_ = n
		}
	}
	return a.abi, nil
}

// postJSON: a small JSON POST with a timeout (an x402 facilitator).
func postJSON(ctx context.Context, url string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s: %s", url, resp.Status, strings.TrimSpace(string(rb)))
	}
	return json.Unmarshal(rb, out)
}

// outsideLabel: outside's fence and taint for text from outside the network's
// names (an x402 server).
func (c *Client) outsideLabel(label, text string) string {
	if c.isOwner() {
		return text
	}
	c.taintLabel(label)
	return fence(label, text)
}

func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return rawURL
}
