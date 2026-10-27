// Reading the registry's history. The chain is the log, so "who held this
// name, with which keys, at block B" is answered from events in their one
// global order. Nothing here trusts an indexer or a node.
package main

import (
	"crypto/ecdh"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

type ctrlChange struct {
	block      uint64
	controller common.Address
	version    uint32
	recovery   bool
}

type grantEvent struct {
	block      uint64
	agent      common.Address
	expires    uint64
	generation uint32
	spend      *big.Int
	scopes     string
}

type revokeEvent struct {
	block       uint64
	generation  uint32
	compromised bool
}

type keysEvent struct {
	block uint64
	first uint64
	view  []byte
	keys  []string
}

type Party struct {
	Name    string
	Owner   string // owner name (= Name for owners)
	IsAgent bool
	ctrl    []ctrlChange // owners
	grants  []grantEvent // agents
	revs    []revokeEvent
	keys    []keysEvent
}

type Resolver struct {
	ch       *Chain
	mu       sync.Mutex
	names    map[common.Hash]string // id -> name
	cache    map[string]*Party
	addrs    map[common.Address][]addrRec
	addrsAt  uint64 // the head when addrs was read: a miss on a later block reads it again
	namesAll bool   // names holds every name registered when loadNames ran
}

func newResolver(ch *Chain) *Resolver {
	return &Resolver{ch: ch, names: map[common.Hash]string{}, cache: map[string]*Party{}}
}

func (r *Resolver) forget() {
	r.mu.Lock()
	r.cache = map[string]*Party{}
	r.addrs = nil
	r.mu.Unlock()
}

// nameOf maps an id back to a name (from registration and grant events).
func (r *Resolver) nameOf(id common.Hash) (string, error) {
	r.mu.Lock()
	if n, ok := r.names[id]; ok {
		r.mu.Unlock()
		return n, nil
	}
	r.mu.Unlock()
	evs, err := r.ch.events("Registry", "OwnerRegistered", []common.Hash{id})
	if err != nil {
		return "", err
	}
	if len(evs) == 0 {
		if evs, err = r.ch.events("Registry", "AgentGranted", []common.Hash{id}); err != nil {
			return "", err
		}
	}
	if len(evs) == 0 {
		return "", fmt.Errorf("unknown id %s", id.Hex()[:10])
	}
	n := evs[0].Args["name"].(string)
	r.mu.Lock()
	r.names[id] = n
	r.mu.Unlock()
	return n, nil
}

func (r *Resolver) party(name string) (*Party, error) {
	r.mu.Lock()
	if p, ok := r.cache[name]; ok {
		r.mu.Unlock()
		return p, nil
	}
	r.mu.Unlock()
	id := idOf(name)
	p := &Party{Name: name}
	if label, owner, ok := strings.Cut(name, "."); ok && label != "" {
		p.IsAgent, p.Owner = true, owner
		gs, err := r.ch.events("Registry", "AgentGranted", []common.Hash{id})
		if err != nil {
			return nil, err
		}
		if len(gs) == 0 {
			return nil, fmt.Errorf("no agent %s", name)
		}
		for _, g := range gs {
			p.grants = append(p.grants, grantEvent{block: g.Block, agent: g.Args["agent"].(common.Address), expires: g.Args["expires"].(uint64),
				generation: g.Args["generation"].(uint32), spend: g.Args["spendPerEpoch"].(*big.Int), scopes: g.Args["scopes"].(string)})
		}
		rs, err := r.ch.events("Registry", "AgentRevoked", []common.Hash{id})
		if err != nil {
			return nil, err
		}
		for _, v := range rs {
			p.revs = append(p.revs, revokeEvent{block: v.Block, generation: v.Args["generation"].(uint32), compromised: v.Args["compromised"].(bool)})
		}
	} else {
		p.Owner = name
		regs, err := r.ch.events("Registry", "OwnerRegistered", []common.Hash{id})
		if err != nil {
			return nil, err
		}
		if len(regs) == 0 {
			return nil, fmt.Errorf("no owner %s", name)
		}
		p.ctrl = append(p.ctrl, ctrlChange{block: regs[0].Block, controller: regs[0].Args["controller"].(common.Address)})
	}
	// owner history (also for agents: their owner's key versions matter)
	if p.IsAgent {
		if _, err := r.party(p.Owner); err != nil {
			return nil, err
		}
	} else {
		cs, err := r.ch.events("Registry", "ControllerChanged", []common.Hash{id})
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			p.ctrl = append(p.ctrl, ctrlChange{block: c.Block, controller: c.Args["controller"].(common.Address),
				version: c.Args["version"].(uint32), recovery: c.Args["recovery"].(bool)})
		}
	}
	ks, err := r.ch.events("Registry", "KeysPublished", []common.Hash{id})
	if err != nil {
		return nil, err
	}
	for _, k := range ks {
		p.keys = append(p.keys, keysEvent{block: k.Block, first: k.Args["firstEpoch"].(uint64), view: k.Args["viewKey"].([]byte),
			keys: splitKeys(k.Args["epochKeys"].([]byte))})
	}
	r.mu.Lock()
	r.cache[name] = p
	r.names[id] = name
	r.mu.Unlock()
	return p, nil
}

// controllerAt and versionAt describe an owner at block b.
func (p *Party) controllerAt(b uint64) (common.Address, uint32) {
	var c common.Address
	var v uint32
	for _, x := range p.ctrl {
		if x.block <= b {
			c, v = x.controller, x.version
		}
	}
	return c, v
}

// verifySigner: did addr legitimately speak for name at block b?
func (r *Resolver) verifySigner(name string, addr common.Address, b uint64, blockTime int64) error {
	p, err := r.party(name)
	if err != nil {
		return err
	}
	if !p.IsAgent {
		if c, _ := p.controllerAt(b); c != addr {
			// A smart account holds the name: its mail key signs messages.
			if k, ok := r.ch.signerOf(c); !ok || k != addr {
				return errors.New("not signed by the owner's key at that block")
			}
		}
		return nil
	}
	op, err := r.party(p.Owner)
	if err != nil {
		return err
	}
	var g *grantEvent
	for i := range p.grants {
		if p.grants[i].block <= b {
			g = &p.grants[i]
		}
	}
	if g == nil {
		return errors.New("not signed by the agent's granted key at that block")
	}
	if g.agent != addr {
		// An agent account: its session key signs the agent's messages.
		if k, ok := r.ch.signerOf(g.agent); !ok || k != addr {
			return errors.New("not signed by the agent's granted key at that block")
		}
	}
	if uint64(blockTime) >= g.expires {
		return errors.New("the agent's grant had expired")
	}
	for _, v := range p.revs {
		if v.generation == g.generation && (v.compromised || v.block <= b) {
			return errors.New("the agent's grant was revoked")
		}
	}
	_, vGrant := op.controllerAt(g.block)
	_, vNow := op.controllerAt(b)
	if vGrant != vNow {
		return errors.New("the owner recovered or replaced a compromised key since the grant")
	}
	return nil
}

// keysAt returns the view key and the epoch recipient of name for epoch e.
func (r *Resolver) keysAt(name string, e uint64) (*ecdh.PublicKey, string, error) {
	p, err := r.party(name)
	if err != nil {
		return nil, "", err
	}
	for i := len(p.keys) - 1; i >= 0; i-- {
		k := p.keys[i]
		if e >= k.first && e < k.first+uint64(len(k.keys)) {
			v, err := ecdh.X25519().NewPublicKey(k.view)
			if err != nil {
				return nil, "", err
			}
			return v, k.keys[e-k.first], nil
		}
	}
	return nil, "", fmt.Errorf("%s has published no key for epoch %d", name, e)
}

// grantExpiry: when the current grant of an agent ends (false for owners).
func (c *Client) grantExpiry() (uint64, bool) {
	if c.isOwner() {
		return 0, false
	}
	c.res.forget() // a fresh grant may have arrived
	p, err := c.res.party(c.f.Name)
	if err != nil || len(p.grants) == 0 {
		return 0, false
	}
	return p.grants[len(p.grants)-1].expires, true
}

func (r *Resolver) scopesOf(name string) string {
	s, _ := r.scopesOfErr(name)
	return s
}

// scopesOfErr: the latest grant's scopes; an error if they can't be read now.
func (r *Resolver) scopesOfErr(name string) (string, error) {
	p, err := r.party(name)
	if err != nil {
		return "", err
	}
	if len(p.grants) == 0 {
		return "", nil
	}
	return p.grants[len(p.grants)-1].scopes, nil
}

// nameAt: which name's key was this address at block b (0 = now)? It looks
// at registrations, key changes and agent grants.
func (r *Resolver) nameAt(a common.Address, b uint64) string {
	if perfFixes {
		return r.nameAtIndexed(a, b)
	}
	r.mu.Lock()
	if r.addrs == nil {
		r.mu.Unlock()
		r.loadAddrs()
		r.mu.Lock()
	}
	recs, at := r.addrs[a], r.addrsAt
	r.mu.Unlock()
	if len(recs) == 0 && (b == 0 || b > at) { // a name registered since the index was read (a long-running client)
		if h, err := r.ch.b.BlockNumber(ctxb()); err == nil && h > at {
			r.loadAddrs()
			r.mu.Lock()
			recs = r.addrs[a]
			r.mu.Unlock()
		}
	}
	name := ""
	for _, x := range recs {
		if b == 0 || x.block <= b {
			name = x.name
		}
	}
	return name
}

type addrRec struct {
	block uint64
	name  string
}

func (r *Resolver) loadAddrs() {
	head, _ := r.ch.b.BlockNumber(ctxb())
	m := map[common.Address][]addrRec{}
	add := func(a common.Address, b uint64, n string) { m[a] = append(m[a], addrRec{b, n}) }
	if evs, err := r.ch.events("Registry", "OwnerRegistered"); err == nil {
		for _, e := range evs {
			add(e.Args["controller"].(common.Address), e.Block, e.Args["name"].(string))
		}
	}
	if evs, err := r.ch.events("Registry", "ControllerChanged"); err == nil {
		for _, e := range evs {
			if n, err := r.nameOf(common.Hash(e.Args["id"].([32]byte))); err == nil {
				add(e.Args["controller"].(common.Address), e.Block, n)
			}
		}
	}
	if evs, err := r.ch.events("Registry", "AgentGranted"); err == nil {
		for _, e := range evs {
			add(e.Args["agent"].(common.Address), e.Block, e.Args["name"].(string))
		}
	}
	r.mu.Lock()
	r.addrs, r.addrsAt = m, head
	r.mu.Unlock()
}

// addrIndex: address => the names it held, read once per process and then
// only from where it was read (perfFixes). Before, every reader loaded the
// whole registry history on its first claim, again after every forget(), and
// again whenever a claim's attester was not in it.
type addrIndex struct {
	mu sync.Mutex
	m  map[common.Address][]addrRec
	at uint64 // read up to this block
	ok bool
}

func (ch *Chain) addrIdx() *addrIndex {
	ch.aiOnce.Do(func() { ch.ai = &addrIndex{m: map[common.Address][]addrRec{}} })
	return ch.ai
}

// update reads what is new since the index was read.
func (ix *addrIndex) update(r *Resolver) {
	head, err := r.ch.b.BlockNumber(ctxb())
	if err != nil {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	from := r.ch.dep.Block
	if ix.ok {
		if head <= ix.at {
			return
		}
		from = ix.at + 1
	}
	add := func(a common.Address, b uint64, n string) { ix.m[a] = append(ix.m[a], addrRec{b, n}) }
	type ev struct {
		b uint64
		i uint
		f func()
	}
	var all []ev
	if evs, err := r.ch.eventsFrom(from, "Registry", "OwnerRegistered"); err == nil {
		for _, e := range evs {
			e := e
			all = append(all, ev{e.Block, e.Index, func() { add(e.Args["controller"].(common.Address), e.Block, e.Args["name"].(string)) }})
		}
	} else {
		return
	}
	if evs, err := r.ch.eventsFrom(from, "Registry", "ControllerChanged"); err == nil {
		for _, e := range evs {
			e := e
			all = append(all, ev{e.Block, e.Index, func() {
				if n, err := r.nameOf(common.Hash(e.Args["id"].([32]byte))); err == nil {
					add(e.Args["controller"].(common.Address), e.Block, n)
				}
			}})
		}
	} else {
		return
	}
	if evs, err := r.ch.eventsFrom(from, "Registry", "AgentGranted"); err == nil {
		for _, e := range evs {
			e := e
			all = append(all, ev{e.Block, e.Index, func() { add(e.Args["agent"].(common.Address), e.Block, e.Args["name"].(string)) }})
		}
	} else {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].b < all[j].b || all[i].b == all[j].b && all[i].i < all[j].i })
	for _, e := range all {
		e.f()
	}
	ix.at, ix.ok = head, true
}

func (r *Resolver) nameAtIndexed(a common.Address, b uint64) string {
	ix := r.ch.addrIdx()
	ix.mu.Lock()
	ok, at := ix.ok, ix.at
	ix.mu.Unlock()
	if !ok || b > at {
		ix.update(r)
	}
	ix.mu.Lock()
	recs := ix.m[a]
	ix.mu.Unlock()
	name := ""
	for _, x := range recs {
		if b == 0 || x.block <= b {
			name = x.name
		}
	}
	return name
}

// loadNames: every registered name's id, from two whole histories (perfFixes:
// find asked the node once per card, each a search from block 0; a one-shot
// CLI waited minutes at 300 cards).
func (r *Resolver) loadNames() {
	if !perfFixes {
		return
	}
	r.mu.Lock()
	done := r.namesAll
	r.mu.Unlock()
	if done {
		return
	}
	m := map[common.Hash]string{}
	for _, ev := range []string{"OwnerRegistered", "AgentGranted"} {
		evs, err := r.ch.events("Registry", ev)
		if err != nil {
			return
		}
		for _, e := range evs {
			if n, ok := e.Args["name"].(string); ok {
				m[idOf(n)] = n
			}
		}
	}
	r.mu.Lock()
	for id, n := range m {
		r.names[id] = n
	}
	r.namesAll = true
	r.mu.Unlock()
}
