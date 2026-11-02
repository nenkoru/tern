// The shielded pool (Privacy Pools, contracts/PrivacyPool.sol): deposit coins
// as a note, later send any part of it to an address or a name with a zero
// knowledge proof that hides which deposit paid. Notes live encrypted in the
// identity folder (pool.notes) and can be rebuilt from the chain and the key:
// their secrets are derived (0xbow's scheme), so `private balance` finds
// deposits and change notes again on a new machine.
package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/nenkoru/tern/internal/zk"
)

// PoolNote: a note we can spend (or spent).
type PoolNote struct {
	Value      string `json:"value"` // wei
	Label      string `json:"label"`
	Nullifier  string `json:"nullifier"`
	Secret     string `json:"secret"`
	Commitment string `json:"commitment"`
	Index      int    `json:"index"`   // its leaf in the state tree
	Deposit    int    `json:"deposit"` // which of our deposits it descends from
	Step       int    `json:"step"`    // 0: the deposit, n: the n-th change note
	Block      uint64 `json:"block"`
	Spent      bool   `json:"spent,omitempty"`
}

func (n PoolNote) big(s string) *big.Int { v, _ := new(big.Int).SetString(s, 10); return v }
func (n PoolNote) value() *big.Int       { return n.big(n.Value) }
func (n PoolNote) label() *big.Int       { return n.big(n.Label) }
func (n PoolNote) nullifierHash() *big.Int {
	return zk.HashN(n.big(n.Nullifier))
}

type poolWallet struct {
	Deposits int        `json:"deposits"` // deposits made (the next derivation index)
	Notes    []PoolNote `json:"notes"`
}

func (c *Client) notesPath() string { return filepath.Join(c.dir, "pool.notes") }

func (c *Client) notesKey() []byte {
	return crypto.Keccak256([]byte("tern-pool-notes-key-v1"), crypto.FromECDSA(c.key))
}

func (c *Client) loadWallet() (*poolWallet, error) {
	w := &poolWallet{}
	b, err := os.ReadFile(c.notesPath())
	if errors.Is(err, os.ErrNotExist) {
		return w, nil
	}
	if err != nil {
		return nil, err
	}
	aead, _ := chacha20poly1305.NewX(c.notesKey())
	if len(b) < aead.NonceSize() {
		return nil, errors.New("pool.notes is damaged")
	}
	pt, err := aead.Open(nil, b[:aead.NonceSize()], b[aead.NonceSize():], []byte("pool.notes"))
	if err != nil {
		return nil, errors.New("pool.notes doesn't open with this identity's key")
	}
	return w, json.Unmarshal(pt, w)
}

func (c *Client) saveWallet(w *poolWallet) error {
	pt, _ := json.Marshal(w)
	aead, _ := chacha20poly1305.NewX(c.notesKey())
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	return os.WriteFile(c.notesPath(), aead.Seal(nonce, nonce, pt, []byte("pool.notes")), 0o600)
}

// Secrets (0xbow): a deposit's from (master, scope, i); the n-th change note's from (master, label, n).
func (c *Client) poolMaster() (*big.Int, *big.Int) {
	d := crypto.FromECDSA(c.key)
	return zk.ModP(crypto.Keccak256([]byte("tern-pool-nullifier-v1"), d)), zk.ModP(crypto.Keccak256([]byte("tern-pool-secret-v1"), d))
}

func (c *Client) depositSecrets(scope *big.Int, i int) (*big.Int, *big.Int) {
	mn, ms := c.poolMaster()
	return zk.HashN(mn, scope, big.NewInt(int64(i))), zk.HashN(ms, scope, big.NewInt(int64(i)))
}

func (c *Client) changeSecrets(label *big.Int, step int) (*big.Int, *big.Int) {
	mn, ms := c.poolMaster()
	return zk.HashN(mn, label, big.NewInt(int64(step))), zk.HashN(ms, label, big.NewInt(int64(step)))
}

func (c *Client) poolScope() (*big.Int, error) {
	out, err := c.ch.call("PrivacyPool", "SCOPE")
	if err != nil {
		return nil, err
	}
	return out[0].(*big.Int), nil
}

func (c *Client) needPool() error {
	if !c.ch.has("PrivacyPool") {
		return errors.New("this network has no shielded pool (its contracts predate it)")
	}
	return nil
}

// poolDeposit puts amount into the pool as a new note.
func (c *Client) poolDeposit(amount *big.Int) (*PoolNote, error) {
	if err := c.needPool(); err != nil {
		return nil, err
	}
	w, err := c.syncNotes()
	if err != nil {
		return nil, err
	}
	scope, err := c.poolScope()
	if err != nil {
		return nil, err
	}
	nul, sec := c.depositSecrets(scope, w.Deposits)
	r, err := c.ch.send(c.key, amount, "PrivacyPool", "deposit", zk.HashN(nul, sec))
	if err != nil {
		return nil, err
	}
	var n *PoolNote
	for _, l := range r.Logs {
		m := map[string]any{}
		if c.ch.c["PrivacyPool"].UnpackLogIntoMap(m, "Deposited", *l) == nil && m["commitment"] != nil {
			n = &PoolNote{Value: amount.String(), Label: m["label"].(*big.Int).String(), Nullifier: nul.String(), Secret: sec.String(),
				Commitment: m["commitment"].(*big.Int).String(), Index: int(m["index"].(*big.Int).Int64()), Deposit: w.Deposits, Block: r.BlockNumber.Uint64()}
		}
	}
	if n == nil {
		return nil, errors.New("deposit: no Deposited event")
	}
	w.Deposits++
	w.Notes = append(w.Notes, *n)
	return n, c.saveWallet(w)
}

type poolState struct {
	leaves  []*big.Int
	labels  []*big.Int       // deposits' labels, in order
	labelAt map[string]int64 // label => block time of its deposit
	byPre   map[string]event // precommitment => deposit
	byNull  map[string]event // nullifier hash => withdrawal
}

// poolEvents reads the state tree's leaves (deposits and change notes, by index).
func (c *Client) poolEvents() (*poolState, error) {
	deps, err := c.ch.events("PrivacyPool", "Deposited")
	if err != nil {
		return nil, err
	}
	wds, err := c.ch.events("PrivacyPool", "Withdrawn")
	if err != nil {
		return nil, err
	}
	s := &poolState{labelAt: map[string]int64{}, byPre: map[string]event{}, byNull: map[string]event{}}
	put := func(i int, v *big.Int) {
		for len(s.leaves) <= i {
			s.leaves = append(s.leaves, big.NewInt(0))
		}
		s.leaves[i] = v
	}
	for _, e := range deps {
		put(int(e.Args["index"].(*big.Int).Int64()), e.Args["commitment"].(*big.Int))
		l := e.Args["label"].(*big.Int)
		s.labels = append(s.labels, l)
		bt, _ := c.ch.blockTime(e.Block)
		s.labelAt[l.String()] = bt
		s.byPre[e.Args["precommitment"].(*big.Int).String()] = e
	}
	for _, e := range wds {
		put(int(e.Args["index"].(*big.Int).Int64()), e.Args["newCommitment"].(*big.Int))
		s.byNull[e.Args["nullifierHash"].(*big.Int).String()] = e
	}
	return s, nil
}

// syncNotes brings pool.notes up to date with the chain: deposits made with
// our derived secrets (also from another machine), spent notes, and the
// change notes they left.
func (c *Client) syncNotes() (*poolWallet, error) {
	w, err := c.loadWallet()
	if err != nil {
		return nil, err
	}
	if !c.ch.has("PrivacyPool") {
		return w, nil
	}
	s, err := c.poolEvents()
	if err != nil {
		return nil, err
	}
	scope, err := c.poolScope()
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, n := range w.Notes {
		have[n.Commitment] = true
	}
	for i := 0; ; i++ { // our deposits: the derived precommitments, until one is missing
		nul, sec := c.depositSecrets(scope, i)
		e, ok := s.byPre[zk.HashN(nul, sec).String()]
		if !ok {
			if i >= w.Deposits {
				break
			}
			continue
		}
		cm := e.Args["commitment"].(*big.Int).String()
		if !have[cm] {
			w.Notes = append(w.Notes, PoolNote{Value: e.Args["value"].(*big.Int).String(), Label: e.Args["label"].(*big.Int).String(),
				Nullifier: nul.String(), Secret: sec.String(), Commitment: cm, Index: int(e.Args["index"].(*big.Int).Int64()), Deposit: i, Block: e.Block})
			have[cm] = true
		}
		if i+1 > w.Deposits {
			w.Deposits = i + 1
		}
	}
	for k := 0; k < len(w.Notes); k++ { // spent notes and their change
		n := &w.Notes[k]
		e, ok := s.byNull[n.nullifierHash().String()]
		if !ok {
			continue
		}
		n.Spent = true
		rest := new(big.Int).Sub(n.value(), e.Args["value"].(*big.Int))
		nul, sec := c.changeSecrets(n.label(), n.Step+1)
		cm := zk.HashN(rest, n.label(), zk.HashN(nul, sec))
		if cm.Cmp(e.Args["newCommitment"].(*big.Int)) != 0 || have[cm.String()] {
			continue // someone else's change (a note we gave away), or known
		}
		w.Notes = append(w.Notes, PoolNote{Value: rest.String(), Label: n.Label, Nullifier: nul.String(), Secret: sec.String(),
			Commitment: cm.String(), Index: int(e.Args["index"].(*big.Int).Int64()), Deposit: n.Deposit, Step: n.Step + 1, Block: e.Block})
		have[cm.String()] = true
	}
	return w, c.saveWallet(w)
}

func (c *Client) unspent(w *poolWallet) []PoolNote {
	var out []PoolNote
	for _, n := range w.Notes {
		if !n.Spent && n.value().Sign() > 0 {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].value().Cmp(out[j].value()) < 0 })
	return out
}

// ---- association sets

type aspSet struct {
	ID        *big.Int
	Name      string
	Publisher common.Address
	Root      *big.Int
	Labels    []*big.Int
	URI       string
	Updated   uint64
	OK        bool   // usable: the labels rebuild the root, none repeats, and each is a deposit's
	Size      int    // distinct labels that are deposits: the anonymity set it really gives (audit 5 PV-1)
	Problem   string // why it isn't OK
}

func (c *Client) aspSets() ([]*aspSet, error) {
	if !c.ch.has("AssociationSets") {
		return nil, errors.New("this network has no association sets list")
	}
	evs, err := c.ch.events("AssociationSets", "Published")
	if err != nil {
		return nil, err
	}
	byID := map[string]*aspSet{}
	var order []*aspSet
	for _, e := range evs {
		id := e.Args["id"].(*big.Int)
		s := byID[id.String()]
		if s == nil {
			s = &aspSet{ID: id, Name: e.Args["name"].(string), Publisher: e.Args["publisher"].(common.Address)}
			byID[id.String()] = s
			order = append(order, s)
		}
		if e.Args["reset"].(bool) {
			s.Labels = nil
		}
		s.Labels = append(s.Labels, e.Args["labels"].([]*big.Int)...)
		s.Root, s.URI, s.Updated = e.Args["root"].(*big.Int), e.Args["uri"].(string), e.Block
	}
	deposited := map[string]bool{}
	if c.ch.has("PrivacyPool") {
		deps, err := c.ch.events("PrivacyPool", "Deposited")
		if err != nil {
			return nil, err
		}
		for _, e := range deps {
			deposited[e.Args["label"].(*big.Int).String()] = true
		}
	}
	for _, s := range order {
		seen := map[string]bool{}
		dup, unknown := 0, 0
		for _, l := range s.Labels {
			k := l.String()
			if seen[k] {
				dup++
				continue
			}
			seen[k] = true
			if deposited[k] {
				s.Size++
			} else {
				unknown++
			}
		}
		switch {
		case len(s.Labels) == 0 || len(s.Labels) > 1<<zk.ASPDepth || zk.NewTree(zk.ASPDepth, s.Labels).Root().Cmp(s.Root) != 0:
			s.Problem = "its labels don't rebuild its root"
		case dup > 0:
			s.Problem = fmt.Sprintf("%d repeated labels: it looks bigger than it is", dup)
		case unknown > 0:
			s.Problem = fmt.Sprintf("%d labels that are no deposit: it looks bigger than it is", unknown)
		}
		s.OK = s.Problem == ""
	}
	return order, nil
}

func (s *aspSet) has(label *big.Int) bool {
	for _, l := range s.Labels {
		if l.Cmp(label) == 0 {
			return true
		}
	}
	return false
}

// aspRule computes a set by a rule anyone can rerun:
//
//	all       every deposit
//	clean24h  deposits at least 24 hours old that no citizen flagged (claim kind pool.flag on the label)
func (c *Client) aspRule(rule string) ([]*big.Int, error) {
	s, err := c.poolEvents()
	if err != nil {
		return nil, err
	}
	switch rule {
	case "all":
		return s.labels, nil
	case "clean24h":
		flagged := map[common.Hash]bool{}
		if cls, err := c.easClaims(nil); err == nil {
			for _, cl := range cls {
				if cl.Kind == "pool.flag" && !cl.Revoked && cl.Issuer != "" && c.isCitizen(ownerName(cl.Issuer)) {
					flagged[cl.Subject] = true
				}
			}
		}
		now := c.chainNow()
		var out []*big.Int
		for _, l := range s.labels {
			if now-s.labelAt[l.String()] >= 86400 && !flagged[common.BigToHash(l)] {
				out = append(out, l)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("unknown rule %q: all or clean24h", rule)
}

// aspPublish publishes labels under name (appending when the set only grew).
func (c *Client) aspPublish(name string, labels []*big.Int, uri string) (map[string]any, error) {
	if len(labels) == 0 {
		return nil, errors.New("an empty set: nothing to publish")
	}
	root := zk.NewTree(zk.ASPDepth, labels).Root()
	sets, err := c.aspSets()
	if err != nil {
		return nil, err
	}
	delta, reset := labels, true
	for _, s := range sets {
		if s.Name != name {
			continue
		}
		if s.Publisher != c.key2addr() {
			return nil, fmt.Errorf("the set %q belongs to %s", name, s.Publisher.Hex())
		}
		if s.Root.Cmp(root) == 0 {
			return map[string]any{"asp": name, "root": rootHex(root), "labels": len(labels), "unchanged": true}, nil
		}
		if len(s.Labels) <= len(labels) && sameLabels(s.Labels, labels[:len(s.Labels)]) {
			delta, reset = labels[len(s.Labels):], false
		}
	}
	if _, err := c.ch.send(c.key, nil, "AssociationSets", "publish", name, root, delta, reset, uri); err != nil {
		return nil, err
	}
	return map[string]any{"asp": name, "root": rootHex(root), "labels": len(labels), "sent": len(delta), "reset": reset}, nil
}

func (c *Client) key2addr() common.Address { return crypto.PubkeyToAddress(c.key.PublicKey) }

func sameLabels(a, b []*big.Int) bool {
	for i := range a {
		if a[i].Cmp(b[i]) != 0 {
			return false
		}
	}
	return true
}

func rootHex(x *big.Int) string { return common.BigToHash(x).Hex() }

// ---- sending from the pool

// poolWithdrawal is PrivacyPool.Withdrawal.
type poolWithdrawal struct {
	Recipient       common.Address
	Name            [32]byte
	Relayer         common.Address
	Fee             *big.Int
	AspId           *big.Int
	EphemeralPubKey []byte
	ViewTag         [1]byte
}

// poolRequest: what a relayer submits (relay POST /pool/withdraw).
type poolRequest struct {
	W             poolWithdrawal `json:"w"`
	Value         *big.Int       `json:"value"`
	StateRoot     *big.Int       `json:"state_root"`
	ASPRoot       *big.Int       `json:"asp_root"`
	NullifierHash *big.Int       `json:"nullifier_hash"`
	NewCommitment *big.Int       `json:"new_commitment"`
	Proof         string         `json:"proof"`
}

func (r *poolRequest) args() []any {
	return []any{r.W, r.Value, r.StateRoot, r.ASPRoot, r.NullifierHash, r.NewCommitment, common.FromHex(r.Proof)}
}

// SendOpts for the pool.
type poolSendOpts struct {
	asp    string          // association set NAME or NAME@PUBLISHER ("" = the largest valid one holding the note, a trusted publisher's first)
	direct bool            // submit from our own key (no relayer): the gas payer shows
	dry    bool            // prove only: the request is left in lastPool, not submitted (tests)
	relay  *poolRelayInfoT // the relayer, checked (poolSend fills it)
}

// poolSend pays amount from a note to `to` (a name: its stealth address when
// it has one, else its Bank balance; or a 0x address). Guarded like any payment.
func (c *Client) poolSend(to string, amount *big.Int, o poolSendOpts) (map[string]any, error) {
	if err := c.needPool(); err != nil {
		return nil, err
	}
	if amount.Sign() <= 0 {
		return nil, errors.New("private send: the amount must be above zero")
	}
	guardTo := to
	if common.IsHexAddress(to) {
		guardTo = strings.ToLower(to)
	}
	fee := new(big.Int)
	if !o.direct { // the relayer's fee leaves too: guard amount + fee (audit 5 PV-2)
		if err := c.pickPoolRelay(&o, amount); err != nil {
			return nil, err
		}
		fee = o.relay.Fee
	}
	done, err := c.guardPay(guardTo, new(big.Int).Add(amount, fee))
	if err != nil {
		return nil, fmt.Errorf("private send: %w", err)
	}
	res, err := c.poolSendRaw(to, amount, o)
	done(err == nil)
	return res, err
}

func (c *Client) poolSendRaw(to string, amount *big.Int, o poolSendOpts) (map[string]any, error) {
	wl, err := c.syncNotes()
	if err != nil {
		return nil, err
	}
	// The relayer and its fee.
	w := poolWithdrawal{Fee: new(big.Int), AspId: new(big.Int)}
	via := "you (direct: your address paid the gas and shows on chain)"
	if !o.direct {
		if err := c.pickPoolRelay(&o, amount); err != nil {
			return nil, err
		}
		w.Relayer, w.Fee, via = o.relay.Address, o.relay.Fee, "relayer "+c.relay
	}
	need := new(big.Int).Add(amount, w.Fee)
	var note *PoolNote
	for _, n := range c.unspent(wl) {
		if n.value().Cmp(need) >= 0 {
			n := n
			note = &n
			break
		}
	}
	if note == nil {
		return nil, fmt.Errorf("no note holds %s coins (amount + relayer fee %s): private deposit first, or send less", fmtCoins(need), fmtCoins(w.Fee))
	}
	// The association set.
	sets, err := c.aspSets()
	if err != nil {
		return nil, err
	}
	// Trusted publishers: us, and our relayer (relay serve --asp publishes the
	// network's set with its key). A set named without its publisher must be
	// one of theirs (audit 5 PV-9: names are first-come).
	wantName, wantPub := o.asp, common.Address{}
	if i := strings.LastIndex(o.asp, "@"); i >= 0 && common.IsHexAddress(o.asp[i+1:]) {
		wantName, wantPub = o.asp[:i], common.HexToAddress(o.asp[i+1:])
	}
	trusted := func(s *aspSet) bool {
		return s.Publisher == c.key2addr() || o.relay != nil && s.Publisher == o.relay.Address
	}
	var asp, foreign *aspSet
	for _, s := range sets {
		if !s.OK || !s.has(note.label()) {
			continue
		}
		if wantName != "" && s.Name != wantName {
			continue
		}
		if wantName != "" && (wantPub != common.Address{} && s.Publisher != wantPub || wantPub == common.Address{} && !trusted(s)) {
			foreign = s
			continue
		}
		if asp == nil || trusted(s) && !trusted(asp) || trusted(s) == trusted(asp) && s.Size > asp.Size {
			asp = s
		}
	}
	if asp == nil {
		if foreign != nil {
			return nil, fmt.Errorf("association set %q is published by %s, not by you or your relayer: if that is the publisher you mean, --asp %s@%s",
				foreign.Name, foreign.Publisher.Hex(), foreign.Name, foreign.Publisher.Hex())
		}
		if o.asp != "" {
			return nil, fmt.Errorf("association set %q doesn't hold this note's deposit (yet), or its labels don't match its root: asp list", o.asp)
		}
		return nil, errors.New("no association set holds this note's deposit yet: wait for one (asp list), or publish one (asp publish --rule all --name NAME)")
	}
	w.AspId = asp.ID
	// The recipient.
	kind := ""
	switch {
	case common.IsHexAddress(to):
		w.Recipient, kind = common.HexToAddress(to), "address"
	default:
		if sp, vp, err := c.stealthMeta(ownerName(to)); err == nil {
			addr, eph, tag, err := deriveStealth(sp, vp)
			if err != nil {
				return nil, err
			}
			w.Recipient, w.EphemeralPubKey, w.ViewTag, kind = addr, eph, [1]byte{tag}, "stealth address of "+ownerName(to)
		} else if !errors.Is(err, errNoStealthMeta) { // only "no stealth keys" falls back to the name (audit 5 PV-8)
			return nil, fmt.Errorf("private send: %s's stealth keys can't be read (%v); not falling back to its bank balance, which would name it on chain", ownerName(to), err)
		} else {
			if _, err := c.res.party(ownerName(to)); err != nil {
				return nil, fmt.Errorf("no name %s: %w", to, err)
			}
			w.Name, kind = idOf(ownerName(to)), "bank balance of "+ownerName(to)+" (it has no stealth keys: the name shows)"
		}
	}
	// The trees.
	st, err := c.poolEvents()
	if err != nil {
		return nil, err
	}
	tree := zk.NewTree(zk.PoolDepth, st.leaves)
	stateRoot := tree.Root()
	if out, err := c.ch.call("PrivacyPool", "root"); err != nil || out[0].(*big.Int).Cmp(stateRoot) != 0 {
		return nil, errors.New("the pool's root doesn't match its events: try again")
	}
	sp, sb, err := tree.Proof(note.Index)
	if err != nil {
		return nil, err
	}
	at := zk.NewTree(zk.ASPDepth, asp.Labels)
	ai := at.IndexOf(note.label())
	ap, ab, err := at.Proof(ai)
	if err != nil {
		return nil, err
	}
	ctxOut, err := c.ch.call("PrivacyPool", "context", w)
	if err != nil {
		return nil, err
	}
	rest := new(big.Int).Sub(note.value(), need) // the recipient gets amount; the relayer its fee on top
	nn, ns := c.changeSecrets(note.label(), note.Step+1)
	newC := zk.HashN(rest, note.label(), zk.HashN(nn, ns))
	a := &zk.WithdrawCircuit{NewCommitment: newC, NullifierHash: note.nullifierHash(), Value: need, StateRoot: stateRoot,
		ASPRoot: asp.Root, Context: ctxOut[0].(*big.Int), Label: note.label(), ExistingValue: note.value(),
		ExistingNullifier: note.big(note.Nullifier), ExistingSecret: note.big(note.Secret), NewNullifier: nn, NewSecret: ns}
	copy(a.StatePath[:], zk.Bigs(sp))
	copy(a.StateBits[:], zk.Ints(sb))
	copy(a.ASPPath[:], zk.Bigs(ap))
	copy(a.ASPBits[:], zk.Ints(ab))
	if err := c.ch.zkCheck("withdraw"); err != nil { // the keys and the network's verifier: one setup, not the devnet's off a devnet (ceremony.go)
		return nil, err
	}
	proof, err := zk.Prove("withdraw", a)
	if err != nil {
		return nil, err
	}
	req := poolRequest{W: w, Value: need, StateRoot: stateRoot, ASPRoot: asp.Root, NullifierHash: note.nullifierHash(),
		NewCommitment: newC, Proof: common.Bytes2Hex(proof)}
	c.lastPool = &req
	if o.dry {
		return map[string]any{"prepared": true}, nil
	}
	tx := ""
	if o.direct {
		r, err := c.ch.send(c.key, nil, "PrivacyPool", "withdraw", req.args()...)
		if err != nil {
			return nil, err
		}
		tx = r.TxHash.Hex()
	} else if tx, err = poolRelaySubmit(c.relay, "/pool/withdraw", req); err != nil {
		return nil, err
	}
	// The relayer's answer isn't proof: the note must be spent on chain (audit 5 PV-5).
	spent := false
	for try := 0; try < 3 && !spent; try++ {
		if try > 0 {
			time.Sleep(time.Second)
		}
		wl, err := c.syncNotes()
		if err != nil {
			return nil, err
		}
		for _, n := range wl.Notes {
			spent = spent || n.Commitment == note.Commitment && n.Spent
		}
	}
	if !spent {
		return nil, fmt.Errorf("the relayer answered (tx %s) but the withdrawal isn't on chain: your note is still unspent. Try another relayer (--relay), or --direct", tx)
	}
	res := map[string]any{"sent_privately": fmtCoins(amount), "to": to, "paid_into": kind, "relayer_fee": fmtCoins(w.Fee),
		"via": via, "association_set": asp.Name, "association_set_publisher": asp.Publisher.Hex(), "anonymity_set": asp.Size,
		"change_left": fmtCoins(rest), "tx": tx, "proof_ms": zk.LastProveTime.Milliseconds()}
	if rest.Sign() == 0 { // audit 5 PV-12
		res["warning"] = "this spent a whole note: its amount is a deposit's (or an earlier change's) and links to it; send part of a note, or deposit round amounts"
	}
	return res, nil
}

// pickPoolRelay checks --relay once per send (poolRelay: https, fee cap, pinned address).
func (c *Client) pickPoolRelay(o *poolSendOpts, amount *big.Int) error {
	if o.relay != nil {
		return nil
	}
	if c.relay == "" {
		return errors.New("no relayer: set --relay URL (or the network's relay in net.json), or --direct to submit it yourself (your address then shows as the sender)")
	}
	info, err := c.poolRelay(c.relay, amount)
	if err != nil {
		return err
	}
	o.relay = info
	return nil
}

// poolBalance: unspent notes and which association sets hold them.
func (c *Client) poolBalance() (map[string]any, error) {
	if err := c.needPool(); err != nil {
		return nil, err
	}
	w, err := c.syncNotes()
	if err != nil {
		return nil, err
	}
	sets, _ := c.aspSets()
	total := new(big.Int)
	var rows []map[string]any
	for _, n := range c.unspent(w) {
		total.Add(total, n.value())
		var in []string
		for _, s := range sets {
			if s.OK && s.has(n.label()) {
				in = append(in, fmt.Sprintf("%s (%d)", s.Name, s.Size))
			}
		}
		rows = append(rows, map[string]any{"coins": fmtCoins(n.value()), "label": rootHex(n.label())[:18] + "…", "block": n.Block,
			"association_sets": in})
	}
	return map[string]any{"private_balance": fmtCoins(total), "notes": rows,
		"note": "a note pays one send at a time; its change stays a note. A set's size (in brackets) is how many deposits yours hides among"}, nil
}
