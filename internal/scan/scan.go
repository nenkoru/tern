// Package scan reads a tern network's public events into one summary: names
// and agents, cards, payments, locks and how they ended, swaps, claims and the
// volume of mail. It only reads (eth_getLogs and a few view calls), needs no
// key, and resumes from where its last run stopped. Mail stays sealed: it
// counts envelopes and bytes, nothing more. examples/scanner is its command.
package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern"
)

// Backend is what a scan needs from a chain connection (ethclient.Client and
// a simulated backend's client both are one).
type Backend interface {
	ethereum.LogFilterer
	ethereum.BlockNumberReader
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	CallContract(ctx context.Context, msg ethereum.CallMsg, block *big.Int) ([]byte, error)
}

// Net is the part of a network file (net.json) a scan uses.
type Net struct {
	Network   string            `json:"network"`
	RPC       string            `json:"rpc"`
	RPCs      []string          `json:"rpcs"` // the rpc and others for the same chain
	ChainID   int64             `json:"chain_id"`
	Contracts map[string]string `json:"contracts"`
	Block     uint64            `json:"block"`  // the first block with the network's contracts
	Schema    string            `json:"schema"` // the EAS claim schema
	LogRange  uint64            `json:"log_range"`
	Host      string            `json:"host"`
	Settle    *struct {
		Token    string `json:"token"`
		Symbol   string `json:"symbol"`
		Decimals int    `json:"decimals"`
	} `json:"settle"`
}

// ReadNet reads a net.json.
func ReadNet(path string) (Net, error) {
	var n Net
	b, err := os.ReadFile(path)
	if err != nil {
		return n, err
	}
	if err := json.Unmarshal(b, &n); err != nil {
		return n, fmt.Errorf("%s: %w", path, err)
	}
	if n.Contracts["Registry"] == "" || n.Contracts["Bank"] == "" {
		return n, fmt.Errorf("%s: no Registry or Bank: not a tern network file", path)
	}
	return n, nil
}

// cacheVersion: raise it when what a scan derives changes, so old caches are read again.
const cacheVersion = 2 // 2: PayeeSet, printable token symbols

// fingerprint changes when the network's contracts do (or cacheVersion), so a
// cache made for another deployment is thrown away.
func (n Net) fingerprint() string {
	ks := make([]string, 0, len(n.Contracts))
	for k, v := range n.Contracts {
		ks = append(ks, k+"="+strings.ToLower(v))
	}
	sort.Strings(ks)
	return crypto.Keccak256Hash([]byte(fmt.Sprint(cacheVersion, n.ChainID, n.Block, ks))).Hex()[:18]
}

// State is everything a scan learned, up to ToBlock. Amounts are kept as
// integers in the token's smallest unit (the *_raw fields) and shown in whole
// units next to them.
type State struct {
	Network     string            `json:"network"`
	ChainID     int64             `json:"chain_id"`
	Fingerprint string            `json:"fingerprint"`
	FromBlock   uint64            `json:"from_block"`
	ToBlock     uint64            `json:"to_block"` // scanned up to and including
	ToTime      int64             `json:"to_time"`
	Names       map[string]*Name  `json:"names"`
	IDs         map[string]string `json:"ids"`   // bytes32 id => name
	Addrs       map[string]string `json:"addrs"` // controller or agent address => name
	Payments    []*Payment        `json:"payments"`
	Locks       []*Lock           `json:"locks"`
	Swaps       []*Swap           `json:"swaps"`
	Claims      []*Claim          `json:"claims"`
	Mail        map[string]*Mail  `json:"mail"` // UTC day => envelopes
	Tokens      map[string]*Token `json:"tokens"`
}

type Name struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`            // owner | agent
	Owner   string   `json:"owner,omitempty"` // an agent's
	Since   int64    `json:"since"`
	Block   uint64   `json:"block"`
	Card    string   `json:"card,omitempty"`
	Agents  []string `json:"agents,omitempty"`
	Revoked bool     `json:"revoked,omitempty"`
	// Limits: an agent's spending limit per epoch (a day), per token ("coin" for the network coin)
	Limits map[string]string `json:"limits,omitempty"`
}

type Payment struct {
	Block     uint64 `json:"block"`
	Time      int64  `json:"time"`
	From      string `json:"from"`
	To        string `json:"to"`
	Token     string `json:"token"`
	Amount    string `json:"amount"`
	AmountRaw string `json:"amount_raw"`
}

type Lock struct {
	ID          uint64 `json:"id"`
	Block       uint64 `json:"block"`
	Time        int64  `json:"time"`
	From        string `json:"from"`
	Payee       string `json:"payee"` // "" while open to whoever the condition names
	Kind        string `json:"kind"`
	Condition   string `json:"condition"`
	Decider     string `json:"decider,omitempty"`
	Token       string `json:"token"`
	Item        string `json:"item,omitempty"` // an item lock: the item id in its collection
	Total       string `json:"total"`
	Paid        string `json:"paid"`
	Refunded    string `json:"refunded"`
	TotalRaw    string `json:"total_raw"`
	PaidRaw     string `json:"paid_raw"`
	RefundedRaw string `json:"refunded_raw"`
	RefundAfter int64  `json:"refund_after"`
	Status      string `json:"status"` // open | refundable | paid | refunded | split | partly paid
	Ended       int64  `json:"ended,omitempty"`
}

type Swap struct {
	Block  uint64 `json:"block"`
	Time   int64  `json:"time"`
	Offer  uint64 `json:"offer"`  // its lock
	Answer uint64 `json:"answer"` // its lock
}

type Claim struct {
	UID      string `json:"uid"`
	Block    uint64 `json:"block"`
	Time     int64  `json:"time"`
	Attester string `json:"attester"`
	Subject  string `json:"subject"`
	Kind     string `json:"kind"`
	Score    int64  `json:"score"`
	Data     string `json:"data,omitempty"` // the first bytes, when they are text
	Revoked  bool   `json:"revoked,omitempty"`
}

type Mail struct {
	Envelopes uint64 `json:"envelopes"`
	Bytes     uint64 `json:"bytes"`
}

type Token struct {
	Symbol   string `json:"symbol"`
	Decimals int    `json:"decimals"`
}

// Coin is the token key of the network's own coin (a lock or payment with token 0).
const Coin = "coin"

func newState(n Net) *State {
	return &State{Network: n.Network, ChainID: n.ChainID, Fingerprint: n.fingerprint(), FromBlock: n.Block,
		Names: map[string]*Name{}, IDs: map[string]string{}, Addrs: map[string]string{}, Mail: map[string]*Mail{}, Tokens: map[string]*Token{}}
}

// Load reads a cached state for this network, or starts an empty one.
func Load(path string, n Net) *State {
	s := newState(n)
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var c State
	if json.Unmarshal(b, &c) != nil || c.Fingerprint != s.Fingerprint || c.Names == nil {
		return s
	}
	for _, m := range []*map[string]string{&c.IDs, &c.Addrs} {
		if *m == nil {
			*m = map[string]string{}
		}
	}
	if c.Mail == nil {
		c.Mail = map[string]*Mail{}
	}
	if c.Tokens == nil {
		c.Tokens = map[string]*Token{}
	}
	return &c
}

// Save writes the state where Load finds it.
func (s *State) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	// A temp file of its own: a served page and a cron run may save the same cache.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// Options steer an update.
type Options struct {
	Confirmations uint64                      // stay this many blocks behind the head (reorgs)
	Progress      func(from, to, head uint64) // after each range
	Save          func(*State) error          // after each range, so an interrupted scan resumes
}

// Update scans from the block after the last one scanned up to the head.
func (s *State) Update(ctx context.Context, b Backend, n Net, o Options) error {
	b = retrying{b}
	sc, err := newScanner(b, n, s)
	if err != nil {
		return err
	}
	head, err := b.BlockNumber(ctx)
	if err != nil {
		return err
	}
	if head < o.Confirmations {
		return nil
	}
	head -= o.Confirmations
	from := s.FromBlock
	if s.ToBlock >= from && s.ToTime != 0 {
		from = s.ToBlock + 1
	}
	step := n.LogRange
	if step == 0 {
		step = 10_000
	}
	for from <= head {
		to := min(from+step-1, head)
		logs, err := sc.logs(ctx, from, to)
		if err != nil {
			if next, ok := narrower(err, step); ok {
				step = next // some RPCs refuse wide ranges or many results
				continue
			}
			return fmt.Errorf("logs %d-%d: %w", from, to, err)
		}
		if err := sc.clock(ctx, from, to); err != nil {
			return err
		}
		for _, l := range logs {
			if err := sc.apply(ctx, l); err != nil {
				return fmt.Errorf("block %d tx %s: %w", l.BlockNumber, l.TxHash.Hex(), err)
			}
		}
		s.ToBlock, s.ToTime = to, sc.t1
		s.refresh()
		if o.Save != nil {
			if err := o.Save(s); err != nil {
				return err
			}
		}
		if o.Progress != nil {
			o.Progress(from, to, head)
		}
		from = to + 1
	}
	s.refresh()
	return nil
}

// retrying: a public RPC drops a request now and then (a TLS handshake that
// times out, a 502): each call is tried again a few times, waiting longer
// each time. A rate limit is waited out longer still (and Dial's transport
// has already honoured any Retry-After). A refused range comes back at once,
// for Update to ask for a smaller one; a call that reverts comes back at once
// too (asking again gives the same answer).
type retrying struct{ Backend }

const (
	tries     = 4
	rateTries = 6
)

var (
	backoff     = time.Second     // the first wait; it doubles (tests shorten it)
	rateBackoff = 2 * time.Second // the first wait after a rate limit; it doubles, up to maxWait
	maxWait     = 30 * time.Second
)

func retry[T any](ctx context.Context, f func() (T, error)) (T, error) {
	var v T
	var err error
	for i := 0; ; i++ {
		if v, err = f(); err == nil || ctx.Err() != nil || tooWide(err) || reverted(err) {
			return v, err
		}
		n, wait := tries, backoff<<i
		if rateLimited(err) {
			n, wait = rateTries, min(rateBackoff<<i, maxWait)
		}
		if i >= n-1 {
			return v, err
		}
		select {
		case <-ctx.Done():
			return v, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (r retrying) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	return retry(ctx, func() ([]types.Log, error) { return r.Backend.FilterLogs(ctx, q) })
}

func (r retrying) BlockNumber(ctx context.Context) (uint64, error) {
	return retry(ctx, func() (uint64, error) { return r.Backend.BlockNumber(ctx) })
}

func (r retrying) HeaderByNumber(ctx context.Context, n *big.Int) (*types.Header, error) {
	return retry(ctx, func() (*types.Header, error) { return r.Backend.HeaderByNumber(ctx, n) })
}

func (r retrying) CallContract(ctx context.Context, msg ethereum.CallMsg, block *big.Int) ([]byte, error) {
	return retry(ctx, func() ([]byte, error) { return r.Backend.CallContract(ctx, msg, block) })
}

// minWindow: the narrowest range asked for before giving up.
const minWindow = 10

// narrower: the range to ask for after one failed, if a smaller one could
// help: the RPC's own limit when it names one (counted both ends), else half.
// A range refused as too wide, or one that timed out under its own weight;
// never after a rate limit (a narrower window would only mean more requests).
func narrower(err error, n uint64) (uint64, bool) {
	if n <= minWindow || rateLimited(err) || reverted(err) {
		return 0, false
	}
	s := err.Error()
	if m := rangeLimitRE.FindStringSubmatch(s); m != nil {
		if lim, _ := strconv.ParseUint(m[1], 10, 64); lim > 1 && lim-1 < n {
			return max(lim-1, minWindow), true
		}
	}
	if tooWide(err) || strings.Contains(strings.ToLower(s), "timeout") {
		return max(n/2, minWindow), true
	}
	return 0, false
}

// ---- the scanner: one pass over a range of blocks

type scanner struct {
	b      Backend
	n      Net
	s      *State
	abis   map[string]abi.ABI        // contract name => ABI
	byAddr map[common.Address]string // contract address => name
	events map[common.Hash]abi.Event // topic0 => event (names are unique across the contracts read)
	addrs  []common.Address
	topics []common.Hash
	schema common.Hash
	// block times: the range's ends, read once; blocks between are interpolated
	b0, b1 uint64
	t0, t1 int64
}

// what is read: contract => its events
var watched = map[string][]string{
	"Registry":      {"OwnerRegistered", "ControllerChanged", "AgentGranted", "AgentRevoked", "CardSet"},
	"Bank":          {"Paid", "TokenPaid", "Locked", "PayeeSet", "Claimed", "Refunded", "Released", "GivenBack", "TokenLimitSet"},
	"SwapCondition": {"Swapped"},
	"EAS":           {"Attested", "Revoked"},
	"Inbox":         {"Posted", "Committed"},
}

func loadABI(name string) (abi.ABI, error) {
	b, err := tern.Build.ReadFile("build/" + name + "_sol_" + name + ".abi")
	if err != nil {
		b, err = tern.BuildExt.ReadFile("build/ext/" + name + "_sol_" + name + ".abi")
	}
	if err != nil {
		return abi.ABI{}, err
	}
	return abi.JSON(strings.NewReader(string(b)))
}

func newScanner(b Backend, n Net, s *State) (*scanner, error) {
	sc := &scanner{b: b, n: n, s: s, abis: map[string]abi.ABI{}, byAddr: map[common.Address]string{}, events: map[common.Hash]abi.Event{},
		schema: common.HexToHash(n.Schema)}
	for _, c := range []string{"Registry", "Bank", "SwapCondition", "EAS", "Inbox"} {
		a, err := loadABI(c)
		if err != nil {
			return nil, err
		}
		sc.abis[c] = a
		addr, ok := n.Contracts[c]
		if !ok {
			continue // a network without it (no swaps yet)
		}
		sc.addrs = append(sc.addrs, common.HexToAddress(addr))
		for _, e := range watched[c] {
			ev, ok := a.Events[e]
			if !ok {
				return nil, fmt.Errorf("%s has no event %s", c, e)
			}
			sc.events[ev.ID] = ev
			sc.topics = append(sc.topics, ev.ID)
		}
	}
	for name, addr := range n.Contracts {
		sc.byAddr[common.HexToAddress(addr)] = name
	}
	return sc, nil
}

func (sc *scanner) logs(ctx context.Context, from, to uint64) ([]types.Log, error) {
	return sc.b.FilterLogs(ctx, ethereum.FilterQuery{FromBlock: new(big.Int).SetUint64(from), ToBlock: new(big.Int).SetUint64(to),
		Addresses: sc.addrs, Topics: [][]common.Hash{sc.topics}})
}

func (sc *scanner) clock(ctx context.Context, from, to uint64) error {
	h0, err := sc.b.HeaderByNumber(ctx, new(big.Int).SetUint64(from))
	if err != nil {
		return err
	}
	h1 := h0
	if to != from {
		if h1, err = sc.b.HeaderByNumber(ctx, new(big.Int).SetUint64(to)); err != nil {
			return err
		}
	}
	sc.b0, sc.b1, sc.t0, sc.t1 = from, to, int64(h0.Time), int64(h1.Time)
	return nil
}

// at: a block's time, interpolated between the range's ends (exact on chains
// with a fixed block time, such as Base's 2 seconds).
func (sc *scanner) at(block uint64) int64 {
	if sc.b1 == sc.b0 {
		return sc.t0
	}
	return sc.t0 + (sc.t1-sc.t0)*int64(block-sc.b0)/int64(sc.b1-sc.b0)
}

func (sc *scanner) decode(l types.Log) (string, map[string]any, error) {
	ev, ok := sc.events[l.Topics[0]]
	if !ok {
		return "", nil, nil
	}
	contract := sc.byAddr[l.Address]
	m := map[string]any{}
	if len(l.Data) > 0 {
		if err := sc.abis[contract].UnpackIntoMap(m, ev.Name, l.Data); err != nil {
			return "", nil, fmt.Errorf("%s.%s: %w", contract, ev.Name, err)
		}
	}
	var indexed abi.Arguments
	for _, in := range ev.Inputs {
		if in.Indexed {
			indexed = append(indexed, in)
		}
	}
	if err := abi.ParseTopicsIntoMap(m, indexed, l.Topics[1:]); err != nil {
		return "", nil, fmt.Errorf("%s.%s topics: %w", contract, ev.Name, err)
	}
	return contract + "." + ev.Name, m, nil
}

func (sc *scanner) apply(ctx context.Context, l types.Log) error {
	if len(l.Topics) == 0 || l.Removed {
		return nil
	}
	what, m, err := sc.decode(l)
	if err != nil || what == "" {
		return err
	}
	s, t := sc.s, sc.at(l.BlockNumber)
	switch what {
	case "Registry.OwnerRegistered":
		name := m["name"].(string)
		s.IDs[hexOf(m["id"])] = name
		s.Addrs[addrKey(m["controller"])] = name
		s.Names[name] = &Name{Name: name, Kind: "owner", Since: t, Block: l.BlockNumber}
	case "Registry.ControllerChanged":
		if name, ok := s.IDs[hexOf(m["id"])]; ok {
			s.Addrs[addrKey(m["controller"])] = name
		}
	case "Registry.AgentGranted":
		name, owner := m["name"].(string), s.IDs[hexOf(m["owner"])]
		s.IDs[hexOf(m["id"])] = name
		s.Addrs[addrKey(m["agent"])] = name
		a := s.Names[name]
		if a == nil {
			a = &Name{Name: name, Kind: "agent", Owner: owner, Since: t, Block: l.BlockNumber}
			s.Names[name] = a
			if o := s.Names[owner]; o != nil {
				o.Agents = append(o.Agents, name)
			}
		}
		a.Revoked = false // a new grant (generation) for the same name
		if v := m["spendPerEpoch"].(*big.Int); v.Sign() > 0 {
			a.setLimit(Coin, v.String())
		}
	case "Registry.AgentRevoked":
		if a := s.Names[s.IDs[hexOf(m["id"])]]; a != nil {
			a.Revoked = true
		}
	case "Registry.CardSet":
		if o := s.Names[s.IDs[hexOf(m["id"])]]; o != nil {
			o.Card = m["card"].(string)
		}
	case "Bank.TokenLimitSet":
		if a := s.Names[s.IDs[hexOf(m["agent"])]]; a != nil {
			tok, err := sc.token(ctx, m["token"].(common.Address))
			if err != nil {
				return err
			}
			a.setLimit(tok, m["perEpoch"].(*big.Int).String())
		}
	case "Bank.Paid", "Bank.TokenPaid":
		tok := Coin
		if a, ok := m["token"].(common.Address); ok {
			if tok, err = sc.token(ctx, a); err != nil {
				return err
			}
		}
		s.Payments = append(s.Payments, &Payment{Block: l.BlockNumber, Time: t, From: s.who(m["from"]), To: s.who(m["to"]), Token: tok,
			AmountRaw: m["amount"].(*big.Int).String()})
	case "Bank.Locked":
		return sc.locked(ctx, l, m, t)
	case "Bank.PayeeSet": // a lock opened with no payee: the claim named one (a bounty's winner, a swap's buyer)
		if k := s.lock(m["id"]); k != nil {
			k.Payee = s.who(m["payee"])
		}
	case "Bank.Claimed", "Bank.Released":
		if k := s.lock(m["id"]); k != nil {
			k.PaidRaw = addRaw(k.PaidRaw, m["amount"].(*big.Int))
			k.Ended = t
		}
	case "Bank.Refunded", "Bank.GivenBack":
		if k := s.lock(m["id"]); k != nil {
			k.RefundedRaw = addRaw(k.RefundedRaw, m["amount"].(*big.Int))
			k.Ended = t
		}
	case "SwapCondition.Swapped":
		s.Swaps = append(s.Swaps, &Swap{Block: l.BlockNumber, Time: t, Offer: m["offer"].(*big.Int).Uint64(), Answer: m["answer"].(*big.Int).Uint64()})
	case "EAS.Attested":
		if common.Hash(m["schemaUID"].([32]byte)) != sc.schema {
			return nil // only tern's claim schema
		}
		return sc.attested(ctx, l, common.Hash(m["uid"].([32]byte)), t)
	case "EAS.Revoked":
		uid := common.Hash(m["uid"].([32]byte)).Hex()
		for _, c := range s.Claims {
			if c.UID == uid {
				c.Revoked = true
			}
		}
	case "Inbox.Posted":
		s.mail(t, m["n"].(*big.Int).Uint64(), uint64(len(m["batch"].([]byte))))
	case "Inbox.Committed":
		s.mail(t, m["n"].(*big.Int).Uint64(), m["size"].(*big.Int).Uint64())
	}
	return nil
}

func (sc *scanner) locked(ctx context.Context, l types.Log, m map[string]any, t int64) error {
	s := sc.s
	id := m["id"].(*big.Int)
	k := &Lock{ID: id.Uint64(), Block: l.BlockNumber, Time: t, From: s.who(m["from"]), Payee: s.who(m["payee"]),
		TotalRaw: m["total"].(*big.Int).String(), PaidRaw: "0", RefundedRaw: "0", RefundAfter: int64(m["refundAfter"].(uint64))}
	cond := m["condition"].(common.Address)
	k.Condition = sc.byAddr[cond]
	if k.Condition == "" {
		k.Condition = cond.Hex()
	}
	k.Kind, k.Decider = sc.kindOf(k.Condition, m["demand"].([]byte))
	// The event doesn't carry the token or the item: the lock does (both fixed when it opened).
	bank := sc.abis["Bank"]
	out, err := sc.call(ctx, "Bank", bank, "locks", id)
	if err != nil {
		return fmt.Errorf("lock %d: %w", k.ID, err)
	}
	// An RPC behind a load balancer may answer from a node that hasn't seen
	// the lock yet (all zeros): an error, so the range is read again, rather
	// than a lock in the wrong token kept for good.
	if total, c := out[3].(*big.Int), out[7].(common.Address); total.Cmp(m["total"].(*big.Int)) != 0 || c != cond {
		return fmt.Errorf("lock %d: the rpc answered from a state without it (behind the logs); try again", k.ID)
	}
	if k.Token, err = sc.token(ctx, out[9].(common.Address)); err != nil {
		return err
	}
	item, err := sc.call(ctx, "Bank", bank, "itemOf", id)
	if err != nil {
		return fmt.Errorf("lock %d: itemOf: %w", k.ID, err)
	}
	if item[0].(*big.Int).Sign() > 0 {
		k.Item = new(big.Int).Sub(item[0].(*big.Int), big.NewInt(1)).String()
		k.Token += " item"
	}
	s.Locks = append(s.Locks, k)
	return nil
}

// kindOf names the recipe a lock was made with, as the client's `lock show` does.
func (sc *scanner) kindOf(cond string, demand []byte) (kind, decider string) {
	switch cond {
	case "AttestationCondition":
		t, _ := abi.NewType("tuple", "", []abi.ArgumentMarshaling{
			{Name: "attester", Type: "bytes32"}, {Name: "schema", Type: "bytes32"}, {Name: "subject", Type: "bytes32"},
			{Name: "kindHash", Type: "bytes32"}, {Name: "minScore", Type: "int64"}, {Name: "mode", Type: "uint8"}})
		v, err := abi.Arguments{{Type: t}}.Unpack(demand)
		if err != nil || len(v) != 1 {
			return "conditional", ""
		}
		d := *abi.ConvertType(v[0], new(struct {
			Attester, Schema, Subject, KindHash [32]byte
			MinScore                            int64
			Mode                                uint8
		})).(*struct {
			Attester, Schema, Subject, KindHash [32]byte
			MinScore                            int64
			Mode                                uint8
		})
		decider = sc.s.who(d.Attester)
		switch common.Hash(d.KindHash) {
		case crypto.Keccak256Hash([]byte("ruling")):
			return "escrow", decider
		case crypto.Keccak256Hash([]byte("slash")):
			return "bond", decider
		case crypto.Keccak256Hash([]byte("award")):
			return "bounty", decider
		}
		return "conditional", decider
	case "OptimisticCondition":
		return "optimistic", ""
	case "PanelCondition":
		return "panel", ""
	case "RulingCondition":
		return "ruling", ""
	case "RateCondition":
		return "subscription", ""
	case "VoucherCondition":
		return "channel", ""
	case "HashlockCondition":
		return "htlc", ""
	case "PledgeCondition":
		return "pledge", ""
	case "RevealCondition":
		return "reveal", ""
	case "SwapCondition":
		return "swap", ""
	case "Clock":
		return "game clock", ""
	case "TernJobs":
		return "job", "" // an ERC-8183 job: its evaluator decides (job show)
	}
	return "other", ""
}

func (sc *scanner) attested(ctx context.Context, l types.Log, uid common.Hash, t int64) error {
	out, err := sc.call(ctx, "EAS", sc.abis["EAS"], "getAttestation", uid)
	if err != nil {
		return fmt.Errorf("claim %s: %w", uid.Hex(), err)
	}
	a := abi.ConvertType(out[0], new(struct {
		Uid            [32]byte
		Schema         [32]byte
		Time           uint64
		ExpirationTime uint64
		RevocationTime uint64
		RefUID         [32]byte
		Recipient      common.Address
		Attester       common.Address
		Revocable      bool
		Data           []byte
	})).(*struct {
		Uid            [32]byte
		Schema         [32]byte
		Time           uint64
		ExpirationTime uint64
		RevocationTime uint64
		RefUID         [32]byte
		Recipient      common.Address
		Attester       common.Address
		Revocable      bool
		Data           []byte
	})
	if common.Hash(a.Uid) != uid {
		// the RPC answered from a state without it (an empty attestation): an
		// error, so the range is read again, rather than a claim lost for good
		return fmt.Errorf("claim %s: the rpc doesn't know it yet; try again", uid.Hex())
	}
	args := abi.Arguments{{Type: mustType("bytes32")}, {Type: mustType("string")}, {Type: mustType("bytes")}, {Type: mustType("int64")}}
	v, err := args.Unpack(a.Data)
	if err != nil {
		return nil // not tern's claim shape after all
	}
	c := &Claim{UID: uid.Hex(), Block: l.BlockNumber, Time: t, Attester: sc.s.byAddr(a.Attester), Subject: sc.s.who(v[0]),
		Kind: v[1].(string), Score: v[3].(int64), Revoked: a.RevocationTime != 0}
	if d := v[2].([]byte); isText(d) {
		c.Data = clip(string(d), 200)
	}
	sc.s.Claims = append(sc.s.Claims, c)
	return nil
}

func (sc *scanner) call(ctx context.Context, contract string, a abi.ABI, method string, args ...any) ([]any, error) {
	in, err := a.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	to := common.HexToAddress(sc.n.Contracts[contract])
	out, err := sc.b.CallContract(ctx, ethereum.CallMsg{To: &to, Data: in}, nil)
	if err != nil {
		return nil, err
	}
	return a.Unpack(method, out)
}

var erc20, _ = abi.JSON(strings.NewReader(`[
{"type":"function","name":"symbol","stateMutability":"view","inputs":[],"outputs":[{"type":"string"}]},
{"type":"function","name":"decimals","stateMutability":"view","inputs":[],"outputs":[{"type":"uint8"}]}]`))

// token: a token's key in State.Tokens (its address, or Coin), read once.
func (sc *scanner) token(ctx context.Context, a common.Address) (string, error) {
	if a == (common.Address{}) {
		if sc.s.Tokens[Coin] == nil {
			sym := "coin"
			if sc.n.Host != "" {
				sym = "ETH" // a network on a host chain: its coin is the host's
			}
			sc.s.Tokens[Coin] = &Token{Symbol: sym, Decimals: 18}
		}
		return Coin, nil
	}
	key := strings.ToLower(a.Hex())
	if sc.s.Tokens[key] != nil {
		return key, nil
	}
	tk := &Token{Symbol: a.Hex()[:8], Decimals: 18}
	if st := sc.n.Settle; st != nil && strings.EqualFold(st.Token, a.Hex()) {
		tk = &Token{Symbol: st.Symbol, Decimals: st.Decimals}
	} else {
		if out, err := sc.erc20(ctx, a, "symbol"); err == nil {
			tk.Symbol = clip(Printable(out[0].(string)), 24) // anyone's token: no terminal codes in its symbol
		}
		if out, err := sc.erc20(ctx, a, "decimals"); err == nil {
			tk.Decimals = int(out[0].(uint8))
		}
	}
	sc.s.Tokens[key] = tk
	return key, nil
}

func (sc *scanner) erc20(ctx context.Context, a common.Address, method string) ([]any, error) {
	in, _ := erc20.Pack(method)
	out, err := sc.b.CallContract(ctx, ethereum.CallMsg{To: &a, Data: in}, nil)
	if err != nil {
		return nil, err
	}
	return erc20.Unpack(method, out)
}

// ---- the state's helpers

func (n *Name) setLimit(token, raw string) {
	if n.Limits == nil {
		n.Limits = map[string]string{}
	}
	n.Limits[token] = raw
}

// who: a bytes32 id as a name, or a short hash when no name has it (a split, a deal subject).
func (s *State) who(v any) string {
	h := hexOf(v)
	if h == (common.Hash{}).Hex() {
		return ""
	}
	if n, ok := s.IDs[h]; ok {
		return n
	}
	return h[:10] + "…"
}

func (s *State) byAddr(a common.Address) string {
	if n, ok := s.Addrs[addrKey(a)]; ok {
		return n
	}
	return a.Hex()[:10] + "…"
}

func (s *State) lock(v any) *Lock {
	id := v.(*big.Int).Uint64()
	// Locks are numbered in order, so the id is nearly always the index.
	if id < uint64(len(s.Locks)) && s.Locks[id].ID == id {
		return s.Locks[id]
	}
	for _, k := range s.Locks {
		if k.ID == id {
			return k
		}
	}
	return nil
}

func (s *State) mail(t int64, n, size uint64) {
	d := Day(t)
	m := s.Mail[d]
	if m == nil {
		m = &Mail{}
		s.Mail[d] = m
	}
	m.Envelopes += n
	m.Bytes += size
}

// refresh recomputes what is derived: amounts in whole units, locks' status.
func (s *State) refresh() {
	for _, p := range s.Payments {
		p.Amount = s.Fmt(p.Token, p.AmountRaw)
	}
	for _, k := range s.Locks {
		tok := strings.TrimSuffix(k.Token, " item")
		if k.Item != "" {
			k.Total, k.Paid, k.Refunded = k.TotalRaw, k.PaidRaw, k.RefundedRaw // items count in units
		} else {
			k.Total, k.Paid, k.Refunded = s.Fmt(tok, k.TotalRaw), s.Fmt(tok, k.PaidRaw), s.Fmt(tok, k.RefundedRaw)
		}
		total, paid, refunded := bigOf(k.TotalRaw), bigOf(k.PaidRaw), bigOf(k.RefundedRaw)
		done := new(big.Int).Add(paid, refunded)
		switch {
		case done.Cmp(total) >= 0 && refunded.Sign() == 0:
			k.Status = "paid"
		case done.Cmp(total) >= 0 && paid.Sign() == 0:
			k.Status = "refunded"
		case done.Cmp(total) >= 0:
			k.Status = "split"
		case done.Sign() > 0:
			k.Status = "partly paid"
		case s.ToTime > k.RefundAfter:
			k.Status = "refundable" // past its refund time, nothing claimed
		default:
			k.Status = "open"
		}
	}
}

// Fmt shows a raw amount of a token in whole units.
func (s *State) Fmt(token, raw string) string {
	dec := 18
	if t := s.Tokens[token]; t != nil {
		dec = t.Decimals
	}
	return FmtUnits(bigOf(raw), dec)
}

// Symbol: a token's symbol ("" for an unknown token).
func (s *State) Symbol(token string) string {
	if t := s.Tokens[strings.TrimSuffix(token, " item")]; t != nil {
		return t.Symbol
	}
	return ""
}

// FmtUnits: v / 10^dec, without trailing zeros.
func FmtUnits(v *big.Int, dec int) string {
	r := new(big.Rat).SetFrac(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil))
	out := r.FloatString(dec)
	if strings.Contains(out, ".") {
		out = strings.TrimRight(strings.TrimRight(out, "0"), ".")
	}
	return out
}

// Day: a time's UTC date.
func Day(t int64) string { return time.Unix(t, 0).UTC().Format("2006-01-02") }

func hexOf(v any) string {
	switch x := v.(type) {
	case [32]byte:
		return common.Hash(x).Hex()
	case common.Hash:
		return x.Hex()
	}
	return ""
}

func addrKey(v any) string { return strings.ToLower(v.(common.Address).Hex()) }

func bigOf(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return new(big.Int)
	}
	return v
}

func addRaw(raw string, v *big.Int) string { return new(big.Int).Add(bigOf(raw), v).String() }

func mustType(t string) abi.Type {
	ty, err := abi.NewType(t, "", nil)
	if err != nil {
		panic(err)
	}
	return ty
}

func isText(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range string(b) {
		if c == '�' || (c < 0x20 && c != '\n' && c != '\t') {
			return false
		}
	}
	return true
}

// Printable: s without control characters (terminal escapes, line breaks:
// each becomes a space) or bidirectional overrides (which reorder what is
// shown). Chain strings are anyone's: print them through this.
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == 0x061c || r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069):
			return -1
		case unicode.IsControl(r) || r == utf8.RuneError:
			return ' '
		}
		return r
	}, s)
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// ErrNoNet: no network file was found.
var ErrNoNet = errors.New("no network file: pass --net or set TERNCHAIN_NET")
