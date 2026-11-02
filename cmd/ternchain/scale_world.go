//go:build ops

// The population of the scale simulation (scale_sim.go): people with 1–3
// agents, services with cards, communities, citizens, abusive actors, data nodes.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/big"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var firstNames = strings.Fields(`anna boris cara dima elena fedor galina igor katya lev masha nikita olga pavel rita sergei tanya ulyana vadim yana
ayse mert kemal elif hakan murat nur selin zeynep deniz burak ece okan lale melis dana omar lina sami noor
ivan petra jonas maria lucas sofia mateo emma liam mia noah ava leo zoe ravi priya arjun kiran mei chen wei yuki hana jin`)

var serviceKinds = []struct {
	Kind   string
	Weight int
	Caps   []string
	Price  float64 // a typical job, coins
	PerReq float64 // pay-per-request services (channels)
	Escrow string  // how its jobs are paid: escrow | optimistic | channel
}{
	{"freelancer", 30, []string{"translation", "copywriting", "code-review", "design", "bookkeeping"}, 2, 0, "escrow"},
	{"shop", 18, []string{"bread", "electronics", "clothes", "books", "spare-parts"}, 1, 0, "optimistic"},
	{"courier", 12, []string{"delivery", "same-day", "documents"}, 0.5, 0, "optimistic"},
	{"carrier", 6, []string{"freight", "pallets", "container"}, 5, 0, "optimistic"},
	{"gpu", 12, []string{"gpu-hours", "inference", "fine-tuning"}, 0, 0.002, "channel"},
	{"oracle", 12, []string{"price-feed", "weather", "sports-results", "fx-rates"}, 0, 0.001, "channel"},
	{"studio", 10, []string{"video-edit", "music-mix", "illustration", "3d-model"}, 3, 0, "escrow"},
}

func kindOf(k string) int {
	for i, sk := range serviceKinds {
		if sk.Kind == k {
			return i
		}
	}
	return 0
}

var communityTopics = strings.Fields(`neighbours makers cyclists parents gardeners chess runners photographers coders translators
bakers farmers students teachers nurses drivers musicians gamers readers hikers climbers cooks tenants freelancers
designers investors-club birders knitters woodworkers volunteers anime cinema football basketball poetry history
astronomy aquarists beekeepers`)

// makeFounders plans the population (deterministic in --seed) and makes the
// founding citizens' identities, whose addresses go into the genesis.
func (s *scaleSim) makeFounders() (string, error) {
	cfg := s.cfg
	rng := rand.New(rand.NewPCG(cfg.seed, 99))
	w := s.w
	for i := 0; i < cfg.owners; i++ {
		w.People = append(w.People, &simPerson{Name: fmt.Sprintf("%s%d", firstNames[i%len(firstNames)], i), Agents: []string{"a1"}, Node: i % max(cfg.nodes, 1)})
	}
	// extra agents up to --agents, at most 3 per owner
	extra := cfg.agents - cfg.owners
	for extra > 0 {
		p := w.People[rng.IntN(len(w.People))]
		if len(p.Agents) < 3 {
			p.Agents = append(p.Agents, fmt.Sprintf("a%d", len(p.Agents)+1))
			extra--
		}
	}
	// abusive owners at the end of the list: spam farms (3 agents each), sybils, fake reviewers, injectors
	nAb := int(math.Round(cfg.abuse * float64(cfg.agents)))
	quota := map[string]int{"spam": nAb * 40 / 100, "sybil": nAb * 25 / 100, "fakereview": nAb * 20 / 100}
	quota["inject"] = nAb - quota["spam"] - quota["sybil"] - quota["fakereview"]
	idx := len(w.People) - 1
	for _, kind := range []string{"spam", "inject", "sybil", "fakereview"} {
		per := 1
		if kind == "spam" || kind == "inject" {
			per = 3
		}
		for left := quota[kind]; left > 0 && idx > cfg.founders; idx-- {
			p := w.People[idx]
			n := min(per, left)
			p.Agents = nil
			for j := 1; j <= n; j++ {
				p.Agents = append(p.Agents, fmt.Sprintf("a%d", j))
			}
			p.Abuse = kind
			left -= n
		}
	}
	// the agent count again (abusers' agents replaced some)
	honest := []*simPerson{}
	for _, p := range w.People {
		if p.Abuse == "" {
			honest = append(honest, p)
		}
	}
	total := 0
	for _, p := range w.People {
		total += len(p.Agents)
	}
	for total < cfg.agents {
		p := honest[rng.IntN(len(honest))]
		if len(p.Agents) < 3 {
			p.Agents = append(p.Agents, fmt.Sprintf("a%d", len(p.Agents)+1))
			total++
		}
	}
	for total > cfg.agents {
		p := honest[rng.IntN(len(honest))]
		if len(p.Agents) > 1 {
			p.Agents = p.Agents[:len(p.Agents)-1]
			total--
		}
	}
	// founders: the first owners
	for i := 0; i < cfg.founders && i < len(w.People); i++ {
		w.People[i].Founder = true
	}
	// services: agents of honest owners (not founders' first agents), by kind
	wsum := 0
	for _, k := range serviceKinds {
		wsum += k.Weight
	}
	perm := rng.Perm(len(honest))
	for _, pi := range perm {
		if len(w.Services) >= cfg.services {
			break
		}
		p := honest[pi]
		if p.Founder && len(p.Agents) == 1 {
			continue
		}
		r := rng.IntN(wsum)
		ki := 0
		for r >= serviceKinds[ki].Weight {
			r -= serviceKinds[ki].Weight
			ki++
		}
		k := serviceKinds[ki]
		label := p.Agents[len(p.Agents)-1]
		agent := label + "." + p.Name
		caps := []string{k.Caps[rng.IntN(len(k.Caps))]}
		if rng.IntN(2) == 0 {
			if c2 := k.Caps[rng.IntN(len(k.Caps))]; c2 != caps[0] {
				caps = append(caps, c2)
			}
		}
		q := 0.15 + 0.85*rng.Float64()
		price := k.Price * (0.6 + 0.8*rng.Float64())
		w.Services = append(w.Services, &simService{Agent: agent, Kind: k.Kind, Caps: caps, Price: r3(price), Quality: r3(q), PerReq: k.PerReq})
	}
	// two bad services the fake reviewers push
	cr := 0
	for _, v := range w.Services {
		if cr < 2 && v.Kind == "freelancer" {
			v.Crony, v.Quality = true, 0.1
			cr++
		}
	}
	// arbiters and panelists: honest owners who are not services
	isSvcOwner := map[string]bool{}
	for _, v := range w.Services {
		isSvcOwner[ownerName(v.Agent)] = true
	}
	for _, p := range honest {
		if isSvcOwner[p.Name] {
			continue
		}
		if len(w.Arbiters) < 12 && !p.Founder {
			w.Arbiters = append(w.Arbiters, p.Name)
		} else if len(w.Panelists) < 15 && p.Founder {
			w.Panelists = append(w.Panelists, p.Name)
		}
	}
	// communities: founders among honest owners; 6 are citizens-only (founded by founders)
	for i := 0; i < cfg.communities; i++ {
		name := communityTopics[i%len(communityTopics)]
		if i >= len(communityTopics) {
			name += fmt.Sprint(i)
		}
		co := &simCommunity{Name: name}
		if i < cfg.communities*6/40 { // 6 of 40 are citizens only
			co.CitizensOnly = true
			co.Name = "citizens-" + name
			co.Founder = w.People[i].Name
		} else {
			co.Founder = honest[rng.IntN(len(honest))].Name
		}
		if i%5 == 1 {
			co.Fee = 0.01
		}
		w.Communities = append(w.Communities, co)
	}
	// memberships: popularity ~ 1/rank; each owner joins 1–4, abusers try a few too
	weights := make([]float64, len(w.Communities))
	wt := 0.0
	for i := range weights {
		weights[i] = 1 / float64(i%17+1)
		wt += weights[i]
	}
	for _, p := range w.People {
		n := 1 + rng.IntN(4)
		got := map[int]bool{}
		for j := 0; j < n*3 && len(got) < n; j++ {
			r := rng.Float64() * wt
			ci := 0
			for r > weights[ci] && ci < len(weights)-1 {
				r -= weights[ci]
				ci++
			}
			co := w.Communities[ci]
			if co.CitizensOnly && !p.Founder {
				continue // non-citizens can't join; citizens added later try again
			}
			got[ci] = true
		}
		for ci := range got {
			co := w.Communities[ci]
			if co.Founder != p.Name {
				co.Members = append(co.Members, p.Name)
			}
		}
	}
	for _, co := range w.Communities {
		sort.Strings(co.Members)
	}
	// a few owners are kept for outside (LLM) agents: honest, plain, one agent each
	for i := len(honest) - 1; i >= 0 && len(w.LLM) < 10; i-- {
		p := honest[i]
		if !p.Founder && !isSvcOwner[p.Name] && len(p.Agents) == 1 {
			w.LLM = append(w.LLM, p.Name)
		}
	}
	for _, n := range []string{} {
		_ = n
	}
	w.NodeNames = nil
	for i := 0; i < cfg.nodes; i++ {
		w.NodeNames = append(w.NodeNames, fmt.Sprintf("datanode%d", i+1))
	}
	s.index()
	// founders' identities now: the genesis names their addresses
	var gen []string
	for _, p := range w.People {
		if !p.Founder {
			continue
		}
		i, err := newIdentDir(s.dir(p.Name), IdentityFile{Name: p.Name, Kind: "owner", Owner: p.Name})
		if err != nil {
			return "", err
		}
		gen = append(gen, p.Name+"="+i.addr().Hex())
	}
	s.saveWorld()
	return strings.Join(gen, ","), nil
}

// ---- setup

func (s *scaleSim) setup() error {
	t0 := time.Now()
	w := s.w
	log.Printf("setup: %d owners, %d services, %d communities, %d data nodes", len(w.People), len(w.Services), len(w.Communities), len(w.NodeNames))
	// data nodes: funded, registered names, staked by their own process
	if err := s.setupNodes(); err != nil {
		return err
	}
	// identities and coins
	var names []string
	for _, p := range w.People {
		names = append(names, p.Name)
	}
	s.par(len(names), func(i int) {
		n := names[i]
		if _, err := os.Stat(filepath.Join(s.dir(n), "identity.json")); err != nil {
			if _, err := newIdentDir(s.dir(n), IdentityFile{Name: n, Kind: "owner", Owner: n}); err != nil {
				log.Printf("ident %s: %v", n, err)
			}
		}
		p := s.people[n]
		for _, a := range p.Agents {
			full := a + "." + n
			if _, err := os.Stat(filepath.Join(s.dir(full), "identity.json")); err != nil {
				if _, err := newIdentDir(s.dir(full), IdentityFile{Name: full, Kind: "agent", Owner: n}); err != nil {
					log.Printf("ident %s: %v", full, err)
				}
			}
		}
	})
	pay := map[common.Address]*big.Int{}
	for _, n := range names {
		i, err := loadIdent(s.dir(n))
		if err != nil {
			return err
		}
		amt := 60.0
		if s.people[n].Abuse != "" {
			amt = 40
		}
		pay[i.addr()] = simCoins(amt)
	}
	for _, a := range s.w.Arbiters {
		if i, err := loadIdent(s.dir(a)); err == nil {
			pay[i.addr()] = simCoins(100)
		}
	}
	if err := s.m.time("setup.fund", func() error { return s.fund(pay) }); err != nil {
		return err
	}
	log.Printf("setup: funded %d owners in %s", len(pay), time.Since(t0).Round(time.Second))
	// owners: register (name, keys, voting identity), bank deposit, a card with postage prices
	s.parN(len(names), max(s.cfg.conc, 192), func(i int) {
		n := names[i]
		p := s.people[n]
		s.with(n, func(c *Client) error {
			if s.ch.has(n) && c.view != nil {
				return nil
			}
			var g common.Address
			if k, err := crypto.GenerateKey(); err == nil {
				g = crypto.PubkeyToAddress(k.PublicKey)
			}
			if err := s.m.time("owner.register", func() error { return c.registerOwner(g) }); err != nil {
				return err
			}
			dep := 25.0
			if p.Abuse != "" {
				dep = 20
			}
			if err := s.m.time("bank.deposit", func() error { return c.deposit(simCoins(dep)) }); err != nil {
				return err
			}
			if rand.IntN(3) == 0 || p.Founder {
				card := fmt.Sprintf(`{"about":"%s","postage":{"default":"0.02","work":"0.05","ads":"1"}}`, n)
				s.m.time("card.set", func() error { return c.setCard(card) })
			}
			return nil
		})
	})
	log.Printf("setup: owners registered (%s)", time.Since(t0).Round(time.Second))
	// founders claim citizenship
	s.par(len(w.People), func(i int) {
		p := w.People[i]
		if !p.Founder {
			return
		}
		s.with(p.Name, func(c *Client) error {
			if c.isCitizen(p.Name) {
				return nil
			}
			return s.m.time("gov.claimGenesis", func() error { return c.govCitizenCall("claimGenesis", c.id()) })
		})
	})
	// agents: the owner grants (with a spend limit and scopes), funds its gas; the agent joins
	type job struct{ owner, label string }
	var jobs []job
	for _, p := range w.People {
		for _, a := range p.Agents {
			jobs = append(jobs, job{p.Name, a})
		}
	}
	s.parN(len(jobs), max(s.cfg.conc, 192), func(i int) {
		j := jobs[i]
		full := j.label + "." + j.owner
		if s.ch.has(full) {
			return
		}
		ac := s.client(full)
		if ac == nil {
			return
		}
		var b *GrantBundle
		spend, scopes := s.agentPolicy(full)
		err := s.with(j.owner, func(c *Client) error {
			return s.m.time("agent.grant", func() error {
				var err error
				b, err = c.grant(AgentRequest{Name: j.label, Owner: j.owner, Address: ac.c.addr().Hex()}, 60, simCoins(spend), scopes, simCoins(3))
				return err
			})
		})
		if err != nil {
			return
		}
		s.with(full, func(c *Client) error { return s.m.time("agent.join", func() error { return c.join(*b) }) })
	})
	log.Printf("setup: agents granted (%s)", time.Since(t0).Round(time.Second))
	// services publish their cards: capabilities and prices
	s.par(len(w.Services), func(i int) {
		v := w.Services[i]
		card := map[string]any{"about": v.Kind + " " + strings.Join(v.Caps, " "), "service": v.Kind, "capabilities": v.Caps,
			"accepts": []string{tWorkRequest, tMessage}, "postage": map[string]string{"work": "0.01", "default": "0.05", "ads": "1"}} // strangers' requests: read (work.go)
		if v.PerReq > 0 {
			card["price_per_request"] = fmt.Sprint(v.PerReq)
			card["pay"] = "channel"
		} else {
			card["price"] = fmt.Sprint(v.Price)
			card["pay"] = serviceKinds[kindOf(v.Kind)].Escrow
		}
		b, _ := json.Marshal(card)
		s.with(v.Agent, func(c *Client) error { return s.m.time("card.set", func() error { return c.setCard(string(b)) }) })
	})
	// communities: found, citizens-only, post fee; members ask and the founder admits
	p := CommonsParams{Term: 30 * 86400, VoteWindow: 3600, VetoWindow: 3600, VetoBps: 3000, PetitionBps: 2000, QuorumBps: 3000, Budget: simCoins(5)}
	s.par(len(w.Communities), func(i int) {
		co := w.Communities[i]
		s.with(co.Founder, func(c *Client) error {
			if c.isCommunity(co.Name) {
				return nil
			}
			if err := s.m.time("commons.found", func() error { return c.found(co.Name, p, co.Name+": be kind, no ads") }); err != nil {
				return err
			}
			if co.CitizensOnly {
				s.m.time("commons.citizensOnly", func() error { return c.makeCitizensOnly(co.Name) })
			}
			if co.Fee > 0 {
				s.m.time("commons.setPostFee", func() error {
					_, err := c.act("setPostFee", idOf(co.Name), c.id(), simCoins(co.Fee))
					return err
				})
			}
			return nil
		})
	})
	s.joinAll()
	log.Printf("setup: communities ready (%s)", time.Since(t0).Round(time.Second))
	w.SetupSec = time.Since(t0).Seconds()
	s.saveWorld()
	s.saveMetrics("setup")
	log.Printf("setup done in %s: %d RPC requests", time.Since(t0).Round(time.Second), s.rpcT.reqs.Load())
	return nil
}

// joinAll: every planned member asks to join, and the founder admits, one by
// one (the founder's transactions are in nonce order: a block each).
func (s *scaleSim) joinAll() {
	var mu sync.Mutex
	s.par(len(s.w.Communities), func(i int) {
		co := s.w.Communities[i]
		var joined []string
		for _, m := range co.Members {
			if co.CitizensOnly {
				ok := false
				s.with(m, func(c *Client) error { ok = c.isCitizen(m); return nil })
				if !ok {
					continue
				}
			}
			var err error
			s.with(m, func(c *Client) error {
				if c.commonsBool("isMember", idOf(co.Name), c.id()) {
					return errSkip
				}
				err = s.m.time("commons.join", func() error { _, err := c.joinGroup(co.Name); return err })
				return err
			})
			if err != nil {
				continue
			}
			s.with(co.Founder, func(c *Client) error {
				return s.m.time("commons.admit", func() error {
					_, err := c.act("admit", idOf(co.Name), c.id(), idOf(m))
					return err
				})
			})
			joined = append(joined, m)
		}
		mu.Lock()
		_ = joined
		mu.Unlock()
	})
}

// agentPolicy: an agent's spend limit per epoch and scopes. Services and
// shoppers may pay small amounts without asking (free_pay); abusers get
// what an owner running a farm would give.
func (s *scaleSim) agentPolicy(agent string) (float64, string) {
	p := s.people[ownerName(agent)]
	if p != nil && p.Abuse != "" {
		return 10, `{"free_pay":"2","send_freely":true,"fan_out":1000}`
	}
	if s.svc[agent] != nil { // a service answers strangers all day
		return 8, `{"free_pay":"1","send_freely":true,"fan_out":1000}`
	}
	return 8, `{"free_pay":"0.5","free_pay_day":"5","send_freely":true,"fan_out":50}`
}

// ---- data nodes

func (s *scaleSim) setupNodes() error {
	pay := map[common.Address]*big.Int{}
	for _, n := range s.w.NodeNames {
		i, err := newIdentDir(filepath.Join(s.cfg.root, "nodes", n), IdentityFile{Name: n, Kind: "owner", Owner: n})
		if err != nil {
			return err
		}
		pay[i.addr()] = simCoins(5000)
	}
	if err := s.fund(pay); err != nil {
		return err
	}
	var wg sync.WaitGroup
	errs := make([]error, len(s.w.NodeNames))
	for k, n := range s.w.NodeNames {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := openClient(filepath.Join(s.cfg.root, "nodes", n), s.ch)
			if err != nil {
				errs[k] = err
				return
			}
			gk, _ := crypto.GenerateKey()
			errs[k] = c.registerOwner(crypto.PubkeyToAddress(gk.PublicKey))
		}()
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return s.startNodes()
}

func (s *scaleSim) startNodes() error {
	s.w.Nodes = nil
	for k, n := range s.w.NodeNames {
		port := s.cfg.port + 10 + k
		url := fmt.Sprintf("http://127.0.0.1:%d", port)
		args := []string{"relay", "serve", "--da", "--listen", fmt.Sprintf("127.0.0.1:%d", port), "--public-url", url,
			"--store", filepath.Join(s.cfg.root, "nodes", n, "store"), "--price", "0.00001", "--dir", filepath.Join(s.cfg.root, "nodes", n), "--net", s.netPath(), "--trusted-proxy", "127.0.0.1"}
		if k == len(s.w.NodeNames)-1 && k > 0 || s.cfg.bucketBits > 0 { // with buckets every node is a home: it mirrors and indexes all
			args = append(args, "--mirror")
		}
		if _, err := s.spawn(n, args...); err != nil {
			return err
		}
		s.w.Nodes = append(s.w.Nodes, url)
	}
	for _, u := range s.w.Nodes {
		ok := false
		for i := 0; i < 120 && !ok; i++ {
			time.Sleep(500 * time.Millisecond)
			if resp, err := http.Get(u + "/info"); err == nil {
				resp.Body.Close()
				ok = resp.StatusCode == 200
			}
		}
		if !ok {
			return fmt.Errorf("data node %s did not come up (logs in %s/logs)", u, s.cfg.root)
		}
	}
	// staked as data nodes?
	for i := 0; i < 60; i++ {
		if len(stakedURLs(s.ch)) >= len(s.w.Nodes) {
			break
		}
		time.Sleep(time.Second)
	}
	s.saveWorld()
	log.Printf("data nodes: %v (staked: %d)", s.w.Nodes, len(stakedURLs(s.ch)))
	return nil
}
