// Network self-governance: two houses decide upgrades, parameters and
// treasury grants (see GOVERNANCE.md).
//
//	coin house      wTERN votes, counted by TernGovernor (OpenZeppelin)
//	citizens' house one vote per citizen: an owner name vouched in by K
//	                citizens, N days old; vouchers answer for a sybil
//	Parliament      a proposal passes only with both houses, then waits in
//	                the timelock (the exit window), then anyone executes it
//	Directory       every version of every primitive; clients tell their
//	                owner about a new version and never switch funds alone
package main

import (
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

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// Parliament.State
var govStates = []string{"voting", "defeated", "passed", "queued", "ready", "executed", "expired"}
var govKinds = []string{"upgrade", "param", "grant"}

type parlProposal struct {
	Kind       uint8
	By         [32]byte
	Proposer   common.Address
	Target     common.Address
	Data       []byte
	CoinId     *big.Int
	Opened     uint64
	Ends       uint64
	Electorate uint32
	Yes        uint32
	No         uint32
	Queued     bool
	Title      string
}

type dirVersion struct {
	Value    common.Address
	Spec     [32]byte
	Audit    [32]byte
	At       uint64
	SpecURI  string
	AuditURI string
	Notes    string
}

type citizenView struct {
	Since, BarredUntil, TermFrom    uint64
	VouchesUsed                     uint8
	Active, Genesis, Confirmed, Sus bool
}

func (c *Client) ownerOnly() error {
	if fromAgent(c.f.Name) {
		return errors.New("agents don't vote, vouch or propose as citizens: citizenship is the owner's (an agent may tell its owner)")
	}
	return nil
}

// ---- citizens

func (c *Client) citizen(name string) (citizenView, error) {
	out, err := c.ch.call("Citizens", "citizens", idOf(name))
	if err != nil {
		return citizenView{}, err
	}
	return citizenView{out[0].(uint64), out[1].(uint64), out[2].(uint64), out[3].(uint8), out[4].(bool), out[5].(bool), out[6].(bool), out[7].(bool)}, nil
}

func (c *Client) isCitizen(name string) bool {
	out, err := c.ch.call("Citizens", "isCitizen", idOf(name))
	return err == nil && out[0].(bool)
}

func (c *Client) citizenParams() (citizenParams, error) {
	out, err := c.ch.call("Citizens", "p")
	if err != nil {
		return citizenParams{}, err
	}
	return citizenParams{out[0].(uint8), out[1].(uint8), out[2].(uint16), out[3].(uint16), out[4].(uint16), out[5].(uint64), out[6].(uint64),
		out[7].(uint64), out[8].(uint64), out[9].(common.Address)}, nil
}

func (c *Client) hashesOf(contract, method string, id common.Hash) []string {
	out, err := c.ch.call(contract, method, id)
	if err != nil {
		return nil
	}
	var names []string
	for _, h := range out[0].([][32]byte) {
		names = append(names, c.nameOrHex(common.Hash(h)))
	}
	return names
}

// govCitizens: the citizens now, the candidates with vouches, and the rules.
func (c *Client) govCitizens() (map[string]any, error) {
	ids := map[common.Hash]bool{}
	for _, ev := range []string{"GenesisListed", "Admitted", "Vouched"} {
		evs, err := c.ch.events("Citizens", ev)
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			ids[common.Hash(e.Args["who"].([32]byte))] = true
		}
	}
	var citizens, candidates []map[string]any
	now := uint64(c.chainNow())
	for id := range ids {
		name := c.nameOrHex(id)
		v, err := c.citizen(name)
		if err != nil {
			return nil, err
		}
		if c.isCitizen(name) {
			m := map[string]any{"name": name, "since": time.Unix(int64(v.Since), 0).UTC().Format(time.RFC3339)}
			if vb := c.hashesOf("Citizens", "vouchers", id); len(vb) > 0 {
				m["vouched_by"] = vb
			}
			if v.Genesis {
				m["genesis"] = true
				m["confirmed"] = v.Confirmed
				if !v.Confirmed {
					m["note"] = "a founder: needs confirming by other founders (max(K, founders/2) vouches) before the first term ends, or lapses; it vouches for newcomers after probation"
					if pv := c.hashesOf("Citizens", "pendingVouches", id); len(pv) > 0 {
						m["vouches"] = pv
					}
				}
			}
			citizens = append(citizens, m)
			continue
		}
		m := map[string]any{"name": name}
		if pv := c.hashesOf("Citizens", "pendingVouches", id); len(pv) > 0 {
			m["vouches"] = pv
		}
		if v.Sus {
			m["suspended_until"] = time.Unix(int64(v.BarredUntil), 0).UTC().Format(time.RFC3339)
		} else if v.BarredUntil > now {
			m["barred_until"] = time.Unix(int64(v.BarredUntil), 0).UTC().Format(time.RFC3339)
		}
		if out, err := c.ch.call("Citizens", "genesisKey", id); err == nil && out[0].(common.Address) != (common.Address{}) && v.Since == 0 {
			m["genesis"] = "listed, not claimed yet ('gov claim')"
		}
		if out, err := c.ch.call("Citizens", "eligible", id); err == nil && out[0].(bool) {
			m["eligible"] = "yes: anyone may 'gov admit " + name + "'"
		}
		candidates = append(candidates, m)
	}
	sort.Slice(citizens, func(i, j int) bool { return citizens[i]["name"].(string) < citizens[j]["name"].(string) })
	sort.Slice(candidates, func(i, j int) bool { return candidates[i]["name"].(string) < candidates[j]["name"].(string) })
	p, err := c.citizenParams()
	if err != nil {
		return nil, err
	}
	n, _ := c.ch.call("Citizens", "count")
	rules := fmt.Sprintf("a citizen: vouched by %d citizens, the name at least %s old", p.Vouches, dur(p.MinAge))
	if p.Deals != (common.Address{}) {
		rules += fmt.Sprintf(", %d settled deals with distinct counterparties", p.MinDeals)
	}
	rules += fmt.Sprintf("; a vote of citizens revokes (%d%% yes, %d%% turnout); a sybil's vouchers are out for %s", p.RevokeBps/100, p.QuorumBps/100, dur(p.Penalty))
	return map[string]any{"count": n[0].(uint32), "citizens": citizens, "candidates": candidates, "rules": rules}, nil
}

func dur(s uint64) string {
	switch {
	case s >= 86400 && s%86400 == 0:
		return fmt.Sprintf("%d days", s/86400)
	case s >= 3600 && s%3600 == 0:
		return fmt.Sprintf("%d h", s/3600)
	case s >= 60 && s%60 == 0:
		return fmt.Sprintf("%d min", s/60)
	}
	return fmt.Sprintf("%d s", s)
}

// govVouch: we vouch for name (our standing answers for it).
func (c *Client) govVouch(name string) (map[string]any, error) {
	if err := c.ownerOnly(); err != nil {
		return nil, err
	}
	if fromAgent(name) {
		return nil, errors.New("only owner names become citizens, never agents")
	}
	if _, err := c.ch.send(c.key, nil, "Citizens", "vouch", c.id(), idOf(name)); err != nil {
		return nil, err
	}
	return c.candidateStatus(name)
}

func (c *Client) candidateStatus(name string) (map[string]any, error) {
	v, err := c.citizen(name)
	if err != nil {
		return nil, err
	}
	if c.isCitizen(name) && (!v.Genesis || v.Confirmed) {
		return map[string]any{"name": name, "citizen": true, "vouched_by": c.hashesOf("Citizens", "vouchers", idOf(name))}, nil
	}
	p, err := c.citizenParams()
	if err != nil {
		return nil, err
	}
	n, _ := c.ch.call("Citizens", "validVouches", idOf(name))
	need := uint64(p.Vouches)
	if v.Active && v.Genesis { // a founder's confirmation (audit 5 GV-2)
		if fv, err := c.ch.call("Citizens", "founderVouches"); err == nil {
			need = fv[0].(*big.Int).Uint64()
		}
	}
	m := map[string]any{"name": name, "citizen": false, "vouches": fmt.Sprintf("%d of %d", n[0].(*big.Int).Uint64(), need)}
	if at, err := c.ch.call("Registry", "registeredAt", idOf(name)); err == nil {
		if ok := uint64(c.chainNow()) >= at[0].(uint64)+p.MinAge; !ok {
			m["age"] = "the name becomes old enough at " + time.Unix(int64(at[0].(uint64)+p.MinAge), 0).UTC().Format(time.RFC3339) + "; then anyone may 'gov admit " + name + "'"
		}
	}
	if v.BarredUntil > uint64(c.chainNow()) {
		m["barred_until"] = time.Unix(int64(v.BarredUntil), 0).UTC().Format(time.RFC3339)
	}
	return m, nil
}

func (c *Client) govCitizenCall(method string, args ...any) error {
	if err := c.ownerOnly(); err != nil {
		return err
	}
	_, err := c.ch.send(c.key, nil, "Citizens", method, args...)
	return err
}

// govRevoke opens a citizens' vote to revoke name; sybil also punishes its vouchers.
func (c *Client) govRevoke(name string, sybil bool, reason string) (uint64, error) {
	if err := c.ownerOnly(); err != nil {
		return 0, err
	}
	r, err := c.ch.send(c.key, nil, "Citizens", "openRevocationVote", c.id(), idOf(name), sybil, reason)
	if err != nil {
		return 0, err
	}
	return c.firstIndexed(r, "Citizens", "RevocationOpened")
}

// ---- proposals

// govBy: who proposes: our name if we are a citizen, else ourselves as a coin holder.
func (c *Client) govBy() [32]byte {
	if !fromAgent(c.f.Name) && c.isCitizen(c.f.Name) {
		return c.id()
	}
	return [32]byte{}
}

// docRef: a document's hash and URI. A readable local file is hashed; a
// remote one needs its hash given (we never fetch on the proposer's behalf).
func docRef(ref, hash, what string) ([32]byte, string, error) {
	if ref == "" {
		return [32]byte{}, "", fmt.Errorf("--%s URI is required", what)
	}
	if hash != "" {
		b, err := hexutil.Decode(hash)
		if err != nil || len(b) != 32 {
			return [32]byte{}, "", fmt.Errorf("--%s-hash: 32 bytes of hex", what)
		}
		return [32]byte(b), ref, nil
	}
	if b, err := os.ReadFile(strings.TrimPrefix(ref, "file://")); err == nil {
		return crypto.Keccak256Hash(b), ref, nil
	}
	return [32]byte{}, "", fmt.Errorf("%s %q is not a local file: give its keccak256 with --%s-hash (voters check the document against it)", what, ref, what)
}

func (c *Client) govProposeUpgrade(key string, value common.Address, spec, specHash, audit, auditHash, notes string) (uint64, error) {
	sp, specURI, err := docRef(spec, specHash, "spec")
	if err != nil {
		return 0, err
	}
	au, auditURI, err := docRef(audit, auditHash, "audit")
	if err != nil {
		return 0, err
	}
	title := fmt.Sprintf("%s: version at %s", key, value.Hex())
	r, err := c.ch.send(c.key, nil, "Parliament", "proposeUpgrade", c.govBy(), key, value, sp, specURI, au, auditURI, notes, title)
	if err != nil {
		return 0, err
	}
	return c.firstIndexed(r, "Parliament", "Proposed")
}

// govProposeParam: one call to a governed contract, by its Directory key.
func (c *Client) govProposeParam(key, method string, args []string, why string) (uint64, error) {
	a, ok := c.ch.abis[key]
	if !ok {
		return 0, fmt.Errorf("no ABI for %q here (governed contracts: FeeOracle, Citizens, Parliament, TernGovernor, TernTimelock…)", key)
	}
	m, ok := a.Methods[method]
	if !ok {
		var names []string
		for n, mm := range a.Methods {
			if !mm.IsConstant() {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		return 0, fmt.Errorf("%s has no method %q (it has: %s)", key, method, strings.Join(names, ", "))
	}
	if len(args) != len(m.Inputs) {
		return 0, fmt.Errorf("%s.%s takes %d arguments: %s", key, method, len(m.Inputs), m.Sig)
	}
	vals := make([]any, len(args))
	for i, in := range m.Inputs {
		v, err := c.parseABIArg(in.Type, args[i])
		if err != nil {
			return 0, fmt.Errorf("argument %d (%s %s): %w", i+1, in.Type, in.Name, err)
		}
		vals[i] = v
	}
	data, err := a.Pack(method, vals...)
	if err != nil {
		return 0, err
	}
	target, err := c.dirCurrent(key)
	if pinned, ok := c.ch.dep.Contracts[key]; ok && governanceCritical[key] {
		// A governance contract: the one this client was given (and its owner
		// chose), never whatever the Directory names now (F6: a hostile version).
		target, err = common.HexToAddress(pinned), nil
	}
	if err != nil { // the Directory itself is not listed in it
		a, ok := c.ch.dep.Contracts[key]
		if !ok {
			return 0, err
		}
		target = common.HexToAddress(a)
	}
	title := fmt.Sprintf("%s.%s(%s)", key, method, strings.Join(args, ", "))
	if why != "" {
		title += ": " + why
	}
	r, err := c.ch.send(c.key, nil, "Parliament", "proposeParam", c.govBy(), target, data, title)
	if err != nil {
		return 0, err
	}
	return c.firstIndexed(r, "Parliament", "Proposed")
}

// govProposeGrant: treasury money for work, into a Bank lock that reviewers
// decide (k of them, after the work's time), refundable to the treasury.
func (c *Client) govProposeGrant(to string, amount *big.Int, reviewers []string, k uint8, days uint64, terms string) (uint64, map[string]any, error) {
	if len(reviewers) == 0 {
		return 0, nil, errors.New("--reviewers a,b,c: who rules on the work (the money stays in a lock until they do)")
	}
	judges := 0
	for _, r := range reviewers {
		if !isParty(r, "tern-treasury", to) {
			judges++
		}
	}
	if k == 0 {
		k = uint8(len(reviewers)/2 + 1)
	}
	if judges < int(k) {
		return 0, nil, fmt.Errorf("k=%d but only %d reviewers are not the payee's side", k, judges)
	}
	if terms == "" {
		return 0, nil, errors.New("--why TEXT: the work the grant pays for (its terms)")
	}
	opens, err := c.govEarliestEffect()
	if err != nil {
		return 0, nil, err
	}
	workEnd := opens + max(days, 1)*86400
	refundAfter := workEnd + 14*86400 // reviewers rule within two weeks after the work's time
	subj := dealSubject("tern-treasury", to, "grant", terms, fmt.Sprint(refundAfter))
	ids := make([][32]byte, len(reviewers))
	for i, r := range reviewers {
		ids[i] = idOf(r)
	}
	d, err := panelDemandType().Pack(panelDemand{ids, k, c.schema(), subj, crypto.Keccak256Hash([]byte("ruling")), workEnd})
	if err != nil {
		return 0, nil, err
	}
	r, err := c.ch.send(c.key, nil, "Parliament", "proposeGrant", c.govBy(), payeeID(to), amount, refundAfter,
		common.HexToAddress(c.ch.dep.Contracts["PanelCondition"]), d, terms)
	if err != nil {
		return 0, nil, err
	}
	id, err := c.firstIndexed(r, "Parliament", "Proposed")
	return id, map[string]any{"reviewers": reviewers, "k": k, "work_until": time.Unix(int64(workEnd), 0).UTC().Format(time.RFC3339),
		"refund_to_treasury_after": time.Unix(int64(refundAfter), 0).UTC().Format(time.RFC3339)}, err
}

// govEarliestEffect: when a proposal made now could take effect at the earliest.
func (c *Client) govEarliestEffect() (uint64, error) {
	d, err := c.ch.call("TernGovernor", "votingDelay")
	if err != nil {
		return 0, err
	}
	p, err := c.ch.call("TernGovernor", "votingPeriod")
	if err != nil {
		return 0, err
	}
	x, err := c.ch.call("TernTimelock", "getMinDelay")
	if err != nil {
		return 0, err
	}
	return uint64(c.chainNow()) + d[0].(*big.Int).Uint64() + p[0].(*big.Int).Uint64() + x[0].(*big.Int).Uint64() + 60, nil
}

func (c *Client) proposal(id uint64) (*parlProposal, error) {
	out, err := c.ch.call("Parliament", "proposal", u(id))
	if err != nil {
		return nil, err
	}
	return abiConvert[parlProposal](out[0]), nil
}

func (c *Client) govState(id uint64) (string, error) {
	out, err := c.ch.call("Parliament", "state", u(id))
	if err != nil {
		return "", err
	}
	return govStates[out[0].(uint8)], nil
}

// govVote: in the coin house (wTERN at the proposal's snapshot), the
// citizens' house (one vote, citizens from before it opened), or both ("").
// The citizens' vote is secret (a proof through a relayer) unless open.
func (c *Client) govVote(id uint64, yes bool, house string, open ...bool) (map[string]any, error) {
	x, err := c.proposal(id)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"proposal": id}
	var errs []string
	if house == "" || house == "coin" {
		support := uint8(0)
		if yes {
			support = 1
		}
		if _, err := c.ch.send(c.key, nil, "TernGovernor", "castVote", x.CoinId, support); err != nil {
			errs = append(errs, "coin house: "+err.Error())
		} else {
			res["coin"] = map[bool]string{true: "yes", false: "no"}[yes]
		}
	}
	if house == "" || house == "citizens" {
		if how, err := c.voteCitizens(id, yes, len(open) > 0 && open[0]); err != nil {
			errs = append(errs, "citizens' house: "+err.Error())
		} else {
			res["citizens"] = how
		}
	}
	if house != "" && house != "coin" && house != "citizens" {
		return nil, errors.New("--house coin|citizens (or neither: both you can)")
	}
	if len(res) == 1 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	if len(errs) > 0 {
		res["not_counted"] = errs
	}
	return res, nil
}

func (c *Client) govQueue(id uint64) (map[string]any, error) {
	if _, err := c.ch.send(c.key, nil, "Parliament", "queue", u(id)); err != nil {
		if st, e := c.govState(id); e == nil && st != "passed" {
			return nil, fmt.Errorf("proposal %d is %s, not passed by both houses: %w", id, st, err)
		}
		return nil, err
	}
	eta, _ := c.ch.call("Parliament", "eta", u(id))
	return map[string]any{"proposal": id, "state": "queued", "runs_after": time.Unix(eta[0].(*big.Int).Int64(), 0).UTC().Format(time.RFC3339),
		"note": "the exit window: anyone who disagrees has until then to move their funds"}, nil
}

func (c *Client) govExecute(id uint64) error {
	if _, err := c.ch.send(c.key, nil, "Parliament", "execute", u(id)); err != nil {
		if st, e := c.govState(id); e == nil && st != "ready" {
			return fmt.Errorf("proposal %d is %s, not ready: %w", id, st, err)
		}
		return err
	}
	return nil
}

// govShow: a proposal, both houses' tallies, and what it does.
func (c *Client) govShow(id uint64) (map[string]any, error) {
	x, err := c.proposal(id)
	if err != nil {
		return nil, err
	}
	st, err := c.govState(id)
	if err != nil {
		return nil, err
	}
	m := map[string]any{"proposal": id, "kind": govKinds[x.Kind], "title": x.Title, "state": st,
		"voting_ends": time.Unix(int64(x.Ends), 0).UTC().Format(time.RFC3339)}
	if crit, err := c.ch.call("Parliament", "critical", u(id)); err == nil && crit[0].(bool) {
		m["governance_critical"] = "GOVERNANCE-CRITICAL: passes only with 2/3 of the votes cast in both houses, and waits " +
			c.exitWindowText(id) + " in the exit window"
		if evs, err := c.ch.events("Parliament", "Critical", []common.Hash{common.BigToHash(u(id))}); err == nil && len(evs) > 0 {
			m["critical_because"] = evs[0].Args["why"]
		}
	}
	if x.By != ([32]byte{}) {
		m["by"] = c.nameOrHex(common.Hash(x.By))
	} else {
		m["by"] = x.Proposer.Hex() + " (a coin holder)"
	}
	// The coin house.
	if v, err := c.ch.call("TernGovernor", "proposalVotes", x.CoinId); err == nil {
		coin := map[string]any{"yes": fmtCoins(v[1].(*big.Int)), "no": fmtCoins(v[0].(*big.Int))}
		if snap, err := c.ch.call("TernGovernor", "proposalSnapshot", x.CoinId); err == nil {
			if q, err := c.ch.call("TernGovernor", "quorum", snap[0].(*big.Int)); err == nil {
				coin["quorum"] = fmtCoins(q[0].(*big.Int))
			}
		}
		if ok, err := c.ch.call("Parliament", "coinPassed", u(id)); err == nil && uint64(c.chainNow()) > x.Ends {
			coin["passed"] = ok[0].(bool)
		}
		m["coin_house"] = coin
	}
	// The citizens' house.
	bps, _ := c.ch.call("Parliament", "citizenQuorumBps")
	need := (uint64(x.Electorate)*uint64(bps[0].(uint16)) + 9999) / 10000
	cit := map[string]any{"yes": x.Yes, "no": x.No, "citizens": x.Electorate, "turnout_needed": max(need, 1)}
	if ok, err := c.ch.call("Parliament", "citizensPassed", u(id)); err == nil && uint64(c.chainNow()) > x.Ends {
		cit["passed"] = ok[0].(bool)
	}
	m["citizens_house"] = cit
	if st == "queued" || st == "ready" {
		if eta, err := c.ch.call("Parliament", "eta", u(id)); err == nil {
			m["runs_after"] = time.Unix(eta[0].(*big.Int).Int64(), 0).UTC().Format(time.RFC3339)
		}
	}
	// What it does.
	switch govKinds[x.Kind] {
	case "upgrade":
		if evs, err := c.ch.events("Parliament", "UpgradeProposed", []common.Hash{common.BigToHash(u(id))}); err == nil && len(evs) > 0 {
			a := evs[0].Args
			m["upgrade"] = map[string]any{"key": a["key"], "address": a["value"].(common.Address).Hex(),
				"spec": common.Hash(a["spec"].([32]byte)).Hex(), "spec_uri": a["specURI"], "audit": common.Hash(a["audit"].([32]byte)).Hex(),
				"audit_uri": a["auditURI"], "notes": a["notes"]}
			if st != "executed" {
				m["upgrade"].(map[string]any)["replaces"] = c.dirCurrentOr(a["key"].(string))
			}
		}
	case "grant":
		if evs, err := c.ch.events("Parliament", "GrantProposed", []common.Hash{common.BigToHash(u(id))}); err == nil && len(evs) > 0 {
			a := evs[0].Args
			g := map[string]any{"to": c.nameOrHex(common.Hash(a["payee"].([32]byte))), "amount": fmtCoins(a["amount"].(*big.Int)),
				"condition": c.conditionName(a["condition"].(common.Address)), "refund_after": time.Unix(int64(a["refundAfter"].(uint64)), 0).UTC().Format(time.RFC3339),
				"terms": a["terms"]}
			if gr, err := c.ch.events("Treasury", "Granted", []common.Hash{common.BigToHash(u(id))}); err == nil && len(gr) > 0 {
				g["lock"] = gr[0].Args["lock"].(*big.Int).String()
			}
			m["grant"] = g
		}
	case "param":
		m["call"] = c.describeCall(x.Target, x.Data)
		if call := m["call"].(map[string]any); call["target"] == "Validators" {
			m["validators_note"] = c.validatorCallNote(x.Data)
		}
	}
	return m, nil
}

// describeCall decodes a call to a contract this client knows.
func (c *Client) describeCall(target common.Address, data []byte) map[string]any {
	m := map[string]any{"target": target.Hex(), "data": hexutil.Encode(data)}
	for k, a := range c.ch.dep.Contracts {
		if common.HexToAddress(a) != target {
			continue
		}
		m["target"] = k
		if parsed, ok := c.ch.abis[k]; ok && len(data) >= 4 {
			if meth, err := parsed.MethodById(data[:4]); err == nil {
				args, _ := meth.Inputs.Unpack(data[4:])
				m["method"] = meth.Sig
				var shown []string
				for _, a := range args {
					switch v := a.(type) {
					case [32]byte:
						shown = append(shown, common.Hash(v).Hex())
					case []byte:
						shown = append(shown, hexutil.Encode(v))
					default:
						b, _ := json.Marshal(v)
						shown = append(shown, string(b))
					}
				}
				m["args"] = shown
			}
		}
	}
	return m
}

func (c *Client) govProposals(all bool) ([]map[string]any, error) {
	n, err := c.ch.call("Parliament", "proposalCount")
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for i := uint64(0); i < n[0].(*big.Int).Uint64(); i++ {
		x, err := c.proposal(i)
		if err != nil {
			return nil, err
		}
		st, _ := c.govState(i)
		if !all && (st == "executed" || st == "defeated" || st == "expired") {
			continue
		}
		out = append(out, map[string]any{"proposal": i, "kind": govKinds[x.Kind], "title": x.Title, "state": st,
			"voting_ends": time.Unix(int64(x.Ends), 0).UTC().Format(time.RFC3339)})
	}
	return out, nil
}

// ---- versions: the Directory's history, and which one this client uses

func (c *Client) dirCurrent(key string) (common.Address, error) {
	out, err := c.ch.call("Directory", "current", key)
	if err != nil {
		return common.Address{}, err
	}
	if out[0].(common.Address) == (common.Address{}) {
		return common.Address{}, fmt.Errorf("no %q in the Directory", key)
	}
	return out[0].(common.Address), nil
}

func (c *Client) dirCurrentOr(key string) string {
	a, err := c.dirCurrent(key)
	if err != nil {
		return "nothing (a new primitive)"
	}
	return a.Hex()
}

func (c *Client) dirVersions(key string) ([]dirVersion, error) {
	out, err := c.ch.call("Directory", "versions", key)
	if err != nil {
		return nil, err
	}
	return *abiConvert[[]dirVersion](out[0]), nil
}

// govVersions: every version of key (or a summary of all keys), marking the
// current one and the one this client uses.
func (c *Client) govVersions(key string) (any, error) {
	if key != "" {
		vs, err := c.dirVersions(key)
		if err != nil {
			return nil, err
		}
		using := c.ch.dep.Contracts[key]
		var out []map[string]any
		for i, v := range vs {
			m := map[string]any{"version": i + 1, "address": v.Value.Hex(), "since": time.Unix(int64(v.At), 0).UTC().Format(time.RFC3339)}
			if v.Spec != ([32]byte{}) {
				m["spec"], m["spec_uri"] = common.Hash(v.Spec).Hex(), v.SpecURI
			}
			if v.Audit != ([32]byte{}) {
				m["audit"], m["audit_uri"] = common.Hash(v.Audit).Hex(), v.AuditURI
			} else {
				m["audit"] = "none (a launch version)"
			}
			if v.Notes != "" {
				m["migration_notes"] = v.Notes
			}
			if i == len(vs)-1 {
				m["current"] = true
			}
			if strings.EqualFold(using, v.Value.Hex()) {
				m["you_use"] = true
			}
			out = append(out, m)
		}
		return out, nil
	}
	return c.govUpdates(true)
}

// govUpdates: keys where this client uses another version than the current one.
func (c *Client) govUpdates(all bool) ([]map[string]any, error) {
	ks, err := c.ch.call("Directory", "keys")
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, k := range ks[0].([]string) {
		cur, err := c.ch.call("Directory", "current", k)
		if err != nil {
			return nil, err
		}
		n, _ := c.ch.call("Directory", "versionCount", k)
		m := map[string]any{"key": k, "current": fmt.Sprintf("v%d %s", n[0].(*big.Int).Uint64(), cur[0].(common.Address).Hex())}
		if governanceCritical[k] {
			m["governance_critical"] = true
		}
		using, ok := c.ch.dep.Contracts[k]
		if ok && !strings.EqualFold(using, cur[0].(common.Address).Hex()) {
			vn, _ := c.ch.call("Directory", "versionOf", k, common.HexToAddress(using))
			m["you_use"] = fmt.Sprintf("v%d %s", vn[0].(*big.Int).Uint64(), using)
			m["update"] = fmt.Sprintf("read 'gov versions %s' (spec, audit, migration notes), move what you hold yourself, then 'gov use %s'", k, k)
			if governanceCritical[k] {
				m["warning"] = governanceChangedText(k, cur[0].(common.Address).Hex(), using)
			}
		} else if !all {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// Pins: which version of a key this identity uses, kept in its folder. The
// network's file names the launch versions; nothing switches by itself.
func (c *Client) pinsPath() string { return filepath.Join(c.dir, "versions.json") }

func (c *Client) applyPins() {
	pins := map[string]string{}
	if readJSON(c.pinsPath(), &pins) != nil {
		return
	}
	for k, a := range pins {
		if parsed, ok := c.ch.abis[k]; ok && common.IsHexAddress(a) {
			c.ch.c[k] = bind.NewBoundContract(common.HexToAddress(a), parsed, c.ch.b, c.ch.b, c.ch.b)
			c.ch.dep.Contracts[k] = common.HexToAddress(a).Hex()
		}
	}
}

// govUse switches this identity to a version of key (the current one by default).
func (c *Client) govUse(key, addr string) (map[string]any, error) {
	var a common.Address
	var err error
	if addr == "" || addr == "current" || addr == "latest" {
		if a, err = c.dirCurrent(key); err != nil {
			return nil, err
		}
	} else if strings.HasPrefix(addr, "v") {
		vs, err := c.dirVersions(key)
		if err != nil {
			return nil, err
		}
		n, _ := strconv.Atoi(addr[1:])
		if n < 1 || n > len(vs) {
			return nil, fmt.Errorf("%s has versions v1..v%d", key, len(vs))
		}
		a = vs[n-1].Value
	} else {
		a = common.HexToAddress(addr)
	}
	if n, err := c.ch.call("Directory", "versionOf", key, a); err != nil || n[0].(*big.Int).Sign() == 0 {
		return nil, fmt.Errorf("%s is not a version of %s in the Directory", a.Hex(), key)
	}
	if _, ok := c.ch.abis[key]; !ok {
		return nil, fmt.Errorf("this client has no ABI for %s: update ternchain to use it", key)
	}
	pins := map[string]string{}
	readJSON(c.pinsPath(), &pins)
	was := c.ch.dep.Contracts[key]
	pins[key] = a.Hex()
	if err := writeJSON(c.pinsPath(), pins); err != nil {
		return nil, err
	}
	c.applyPins()
	return map[string]any{"key": key, "using": a.Hex(), "was": was,
		"note": "only this identity switched; what you hold in the old version stays there until you move it"}, nil
}

// ---- push: proposals and versions

func (c *Client) govEvents(from uint64, now uint64, w *WatchState, emit func(Event), once func(string, Event)) error {
	if !c.ch.has("Parliament") || !c.ch.has("Directory") {
		return nil
	}
	if w.Proposals == nil {
		w.Proposals = map[string]string{}
	}
	opened, err := c.ch.eventsFrom(from, "Parliament", "Proposed")
	if err != nil {
		return err
	}
	for _, ev := range opened {
		id := ev.Args["id"].(*big.Int).Uint64()
		emit(Event{"proposal-opened", ev.Block, fmt.Sprintf("proposal %d (%s): %s; both houses vote until %s: 'gov show %d', 'gov vote %d yes|no'",
			id, govKinds[ev.Args["kind"].(uint8)], ev.Args["title"], time.Unix(int64(ev.Args["ends"].(uint64)), 0).UTC().Format(time.RFC3339), id, id),
			map[string]any{"proposal": id}})
		if _, ok := w.Proposals[fmt.Sprint(id)]; !ok {
			w.Proposals[fmt.Sprint(id)] = "voting"
		}
	}
	for sid, was := range w.Proposals {
		if was == "executed" || was == "defeated" || was == "expired" {
			continue
		}
		id, _ := strconv.ParseUint(sid, 10, 64)
		x, err := c.proposal(id)
		if err != nil {
			continue
		}
		st, err := c.govState(id)
		if err != nil {
			continue
		}
		if st == "voting" && x.Ends > now && (x.Ends-now <= 86400 || x.Ends-now <= (x.Ends-x.Opened)/4) {
			once("vote-ending:"+sid, Event{"vote-ending", 0, fmt.Sprintf("voting on proposal %d ends in %s: 'gov vote %d yes|no'", id, dur(x.Ends-now), id),
				map[string]any{"proposal": id}})
		}
		if st != was {
			switch st {
			case "passed":
				emit(Event{"proposal-passed", 0, fmt.Sprintf("proposal %d passed both houses: anyone may 'gov queue %d' (then the exit window)", id, id), map[string]any{"proposal": id}})
			case "defeated":
				emit(Event{"proposal-failed", 0, fmt.Sprintf("proposal %d failed: a house said no or missed its quorum ('gov show %d')", id, id), map[string]any{"proposal": id}})
			case "ready":
				emit(Event{"proposal-ready", 0, fmt.Sprintf("proposal %d's exit window is over: anyone may 'gov execute %d'", id, id), map[string]any{"proposal": id}})
			case "executed":
				emit(Event{"proposal-executed", 0, fmt.Sprintf("proposal %d took effect", id), map[string]any{"proposal": id}})
			}
			w.Proposals[sid] = st
		}
	}
	// Governance-critical proposals, loudly.
	if crit, err := c.ch.eventsFrom(from, "Parliament", "Critical"); err == nil {
		for _, ev := range crit {
			id := ev.Args["id"].(*big.Int).Uint64()
			emit(Event{"governance-critical", ev.Block, fmt.Sprintf("GOVERNANCE-CRITICAL PROPOSAL %d (%s): it needs 2/3 of the votes in both houses and waits %s "+
				"after the vote. Read it ('gov show %d') and vote ('gov vote %d yes|no')", id, ev.Args["why"], c.exitWindowText(id), id, id),
				map[string]any{"proposal": id, "why": ev.Args["why"]}})
		}
	}
	if err := c.validatorEvents(from, emit); err != nil {
		return err
	}
	// A new version of a primitive this client uses: tell, don't switch.
	pub, err := c.ch.eventsFrom(from, "Directory", "Published")
	if err != nil {
		return err
	}
	for _, ev := range pub {
		k := ev.Args["key"].(string)
		using, ok := c.ch.dep.Contracts[k]
		v := ev.Args["value"].(common.Address)
		if governanceCritical[k] && ev.Args["version"].(*big.Int).Uint64() > 1 { // not the launch listing
			emit(Event{"governance-contract-changed", ev.Block, governanceChangedText(k, v.Hex(), using),
				map[string]any{"key": k, "current": v.Hex(), "using": using}})
			continue
		}
		if !ok || strings.EqualFold(using, v.Hex()) {
			continue
		}
		vn, _ := c.ch.call("Directory", "versionOf", k, common.HexToAddress(using))
		used := "?"
		if len(vn) > 0 {
			used = vn[0].(*big.Int).String()
		}
		emit(Event{"new-version", ev.Block, fmt.Sprintf("%s v%s is current (%s); you use v%s. Read 'gov versions %s' (spec, audit, migration notes); "+
			"move what you hold yourself, then 'gov use %s'. Nothing was switched for you", k, ev.Args["version"].(*big.Int), v.Hex(), used, k, k),
			map[string]any{"key": k, "current": v.Hex(), "using": using}})
	}
	return nil
}

// ---- ABI arguments from the command line

// parseABIArg reads one argument: numbers (decimal or 0x), true/false,
// addresses (or a Directory key), bytes32 (hex, or any text: its keccak256),
// bytes (hex), strings, lists (a,b,c) and tuples (JSON objects).
func (c *Client) parseABIArg(t abi.Type, s string) (any, error) {
	switch t.T {
	case abi.UintTy, abi.IntTy:
		n, ok := new(big.Int).SetString(s, 0)
		if !ok {
			return nil, fmt.Errorf("not a number: %q", s)
		}
		if t.Size > 64 {
			return n, nil
		}
		v := reflect.New(t.GetType()).Elem()
		if t.T == abi.UintTy {
			if n.Sign() < 0 || n.BitLen() > t.Size {
				return nil, fmt.Errorf("%s out of range for uint%d", s, t.Size)
			}
			v.SetUint(n.Uint64())
		} else {
			v.SetInt(n.Int64())
		}
		return v.Interface(), nil
	case abi.BoolTy:
		return strconv.ParseBool(s)
	case abi.AddressTy:
		if common.IsHexAddress(s) {
			return common.HexToAddress(s), nil
		}
		if a, ok := c.ch.dep.Contracts[s]; ok {
			return common.HexToAddress(a), nil
		}
		return nil, fmt.Errorf("not an address or a contract key: %q", s)
	case abi.FixedBytesTy:
		if b, err := hexutil.Decode(s); err == nil && len(b) == t.Size {
			v := reflect.New(t.GetType()).Elem()
			reflect.Copy(v, reflect.ValueOf(b))
			return v.Interface(), nil
		}
		if t.Size == 32 {
			return [32]byte(crypto.Keccak256Hash([]byte(s))), nil
		}
		return nil, fmt.Errorf("bytes%d: hex of that length", t.Size)
	case abi.BytesTy:
		return hexutil.Decode(s)
	case abi.StringTy:
		return s, nil
	case abi.SliceTy, abi.ArrayTy:
		parts := []string{}
		if s != "" {
			parts = strings.Split(s, ",")
		}
		v := reflect.MakeSlice(reflect.SliceOf(t.Elem.GetType()), len(parts), len(parts))
		for i, p := range parts {
			e, err := c.parseABIArg(*t.Elem, strings.TrimSpace(p))
			if err != nil {
				return nil, err
			}
			v.Index(i).Set(reflect.ValueOf(e))
		}
		if t.T == abi.ArrayTy {
			if len(parts) != t.Size {
				return nil, fmt.Errorf("%d items needed", t.Size)
			}
			arr := reflect.New(t.GetType()).Elem()
			reflect.Copy(arr, v)
			return arr.Interface(), nil
		}
		return v.Interface(), nil
	case abi.TupleTy:
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s), &obj); err != nil {
			return nil, fmt.Errorf("a tuple as JSON {\"name\": value, …}: %w", err)
		}
		v := reflect.New(t.GetType()).Elem()
		for i, name := range t.TupleRawNames {
			raw, ok := obj[name]
			if !ok {
				return nil, fmt.Errorf("tuple field %q missing", name)
			}
			str := strings.Trim(string(raw), `"`)
			e, err := c.parseABIArg(*t.TupleElems[i], str)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			v.Field(i).Set(reflect.ValueOf(e))
		}
		return v.Interface(), nil
	}
	return nil, fmt.Errorf("unsupported type %s", t)
}

// ---- the command line

type govFlags struct {
	spec, specHash, audit, auditHash, notes, house, reviewers, why string
	k, days                                                        uint64
	sybil, all, open                                               bool
}

func runGov(c *Client, cmd string, pos []string, f govFlags) error {
	id := func() (uint64, error) {
		if len(pos) == 0 {
			return 0, errors.New("usage: " + cmd + " ID")
		}
		return strconv.ParseUint(pos[0], 10, 64)
	}
	switch cmd {
	case "gov lock", "gov unlock":
		amt, err := parseCoins(firstOr(pos, 0, ""))
		if err != nil {
			return err
		}
		if cmd == "gov lock" {
			err = c.lockVotes(amt)
		} else {
			err = c.unlockVotes(amt)
		}
		if err != nil {
			return err
		}
		out(map[string]any{strings.TrimPrefix(cmd, "gov "): pos[0], "note": "wTERN votes from the next proposal's snapshot on"})
	case "gov citizens":
		m, err := c.govCitizens()
		if err != nil {
			return err
		}
		out(m)
	case "gov vouch", "gov unvouch", "gov admit", "gov reinstate", "gov lapse":
		if err := need(pos, 1, cmd+" NAME"); err != nil {
			return err
		}
		var m map[string]any
		var err error
		switch cmd {
		case "gov vouch":
			m, err = c.govVouch(pos[0])
		case "gov unvouch":
			err = c.govCitizenCall("unvouch", c.id(), idOf(pos[0]))
			m = map[string]any{"unvouched": pos[0]}
		default:
			if _, err = c.ch.send(c.key, nil, "Citizens", strings.TrimPrefix(cmd, "gov "), idOf(pos[0])); err == nil {
				m, err = c.candidateStatus(pos[0])
			}
		}
		if err != nil {
			return err
		}
		out(m)
	case "gov validator":
		return runGovValidator(c, pos, f)
	case "gov claim":
		if err := c.govCitizenCall("claimGenesis", c.id()); err != nil {
			return err
		}
		out(map[string]any{"genesis": c.f.Name, "note": "a founder until the first term ends: other founders must confirm you by then ('gov vouch " + c.f.Name + "'); you vouch for newcomers once confirmed and past probation"})
	case "gov leave":
		if err := c.govCitizenCall("leave", c.id()); err != nil {
			return err
		}
		out(map[string]any{"left": c.f.Name})
	case "gov revoke":
		if err := need(pos, 2, "gov revoke NAME [--sybil] REASON…"); err != nil {
			return err
		}
		r, err := c.govRevoke(pos[0], f.sybil, strings.Join(pos[1:], " "))
		if err != nil {
			return err
		}
		out(map[string]any{"revocation": r, "name": pos[0], "sybil": f.sybil, "note": "citizens vote: 'gov revoke-vote " + fmt.Sprint(r) + " yes|no'; after the window, 'gov revoke-close " + fmt.Sprint(r) + "'"})
	case "gov revoke-vote":
		if err := need(pos, 2, "gov revoke-vote ID yes|no"); err != nil {
			return err
		}
		if err := c.govCitizenCall("voteRevocation", bigID(pos[0]), c.id(), pos[1] == "yes"); err != nil {
			return err
		}
		out(map[string]any{"revocation": pos[0], "vote": pos[1]})
	case "gov revoke-close":
		if err := need(pos, 1, "gov revoke-close ID"); err != nil {
			return err
		}
		r, err := c.ch.send(c.key, nil, "Citizens", "closeRevocation", bigID(pos[0]))
		if err != nil {
			return err
		}
		passed := false
		ev := c.ch.abis["Citizens"].Events["RevocationClosed"]
		for _, l := range r.Logs {
			if len(l.Topics) > 0 && l.Topics[0] == ev.ID {
				if v, err := ev.Inputs.NonIndexed().Unpack(l.Data); err == nil {
					passed = v[0].(bool)
				}
			}
		}
		out(map[string]any{"revocation": pos[0], "revoked": passed})
	case "gov propose":
		if err := need(pos, 1, "gov propose upgrade|param|grant …"); err != nil {
			return err
		}
		var pid uint64
		var err error
		extra := map[string]any{}
		switch pos[0] {
		case "upgrade":
			if err := need(pos, 3, "gov propose upgrade KEY ADDRESS --spec URI|FILE --audit URI|FILE [--spec-hash H] [--audit-hash H] [--notes TEXT]"); err != nil {
				return err
			}
			if !common.IsHexAddress(pos[2]) {
				return fmt.Errorf("not an address: %q", pos[2])
			}
			pid, err = c.govProposeUpgrade(pos[1], common.HexToAddress(pos[2]), f.spec, f.specHash, f.audit, f.auditHash, f.notes)
		case "param":
			if err := need(pos, 3, "gov propose param KEY METHOD [ARGS…] [--why TEXT]   (e.g. FeeOracle setTarget envelope 12)"); err != nil {
				return err
			}
			key, method, args := pos[1], pos[2], pos[3:]
			if strings.EqualFold(key, "validators") { // the chain's validator set, in its own words
				key = "Validators"
				switch method {
				case "add", "remove", "cancel":
					if len(args) < 1 || len(args) > 2 || (method != "add" && len(args) != 1) {
						return errors.New("gov propose param validators add ADDRESS|VALIDATOR_DIR [CITIZEN] | remove|cancel ADDRESS|VALIDATOR_DIR")
					}
					a, err := addrOrValidatorDir(args[0])
					if err != nil {
						return err
					}
					if method == "add" {
						who := c.f.Name
						if len(args) == 2 {
							who = args[1]
						}
						if err := c.validatorAddCheck(a, who); err != nil {
							return err
						}
						method, args = "addValidator", []string{a.Hex(), idOf(who).Hex()}
					} else {
						if err := c.validatorRemoveCheck(a); err != nil {
							return err
						}
						method, args = map[string]string{"remove": "removeValidator", "cancel": "cancelRemoval"}[method], []string{a.Hex()}
					}
				case "upgrade":
					if len(args) != 3 {
						return errors.New("gov propose param validators upgrade BLOCK RELEASE_HASH URI")
					}
					method = "scheduleUpgrade"
				}
			}
			pid, err = c.govProposeParam(key, method, args, f.why)
		case "grant":
			if err := need(pos, 3, "gov propose grant TO AMOUNT --reviewers a,b,c [--k N] [--days D] --why TERMS"); err != nil {
				return err
			}
			amt, perr := parseCoins(pos[2])
			if perr != nil {
				return perr
			}
			var revs []string
			for _, r := range strings.Split(f.reviewers, ",") {
				if r = strings.TrimSpace(r); r != "" {
					revs = append(revs, r)
				}
			}
			terms := f.why
			if terms == "" && len(pos) > 3 {
				terms = strings.Join(pos[3:], " ")
			}
			pid, extra, err = c.govProposeGrant(pos[1], amt, revs, uint8(f.k), f.days, terms)
		default:
			return errors.New("gov propose upgrade|param|grant …")
		}
		if err != nil {
			return err
		}
		m, err := c.govShow(pid)
		if err != nil {
			return err
		}
		for k, v := range extra {
			m[k] = v
		}
		out(m)
	case "gov identity":
		if err := c.ownerOnly(); err != nil {
			return err
		}
		reg, err := c.ensureIdentity()
		if err != nil {
			return err
		}
		com := c.identityCommitment()
		inTree, _ := c.bigCall("CitizenGroup", "leafOf", c.id())
		out(map[string]any{"identity": rootHex(com), "registered_now": reg, "in_citizens_tree": inTree != nil && inTree.Sign() > 0,
			"note": "secret votes count for proposals opened after your identity is in the tree; it is derived from your key (a new key: run this again)"})
	case "gov sync":
		if err := need(pos, 1, "gov sync NAME"); err != nil {
			return err
		}
		if _, err := c.ch.send(c.key, nil, "CitizenGroup", "sync", idOf(pos[0])); err != nil {
			return err
		}
		inTree, _ := c.bigCall("CitizenGroup", "leafOf", idOf(pos[0]))
		out(map[string]any{"synced": pos[0], "in_citizens_tree": inTree != nil && inTree.Sign() > 0})
	case "gov vote":
		if err := need(pos, 2, "gov vote ID yes|no [--house coin|citizens] [--open]"); err != nil {
			return err
		}
		n, err := id()
		if err != nil {
			return err
		}
		if pos[1] != "yes" && pos[1] != "no" {
			return fmt.Errorf("%s: vote yes or no (got %q)", cmd, pos[1])
		}
		m, err := c.govVote(n, pos[1] == "yes", f.house, f.open)
		if err != nil {
			return err
		}
		out(m)
	case "gov queue":
		n, err := id()
		if err != nil {
			return err
		}
		m, err := c.govQueue(n)
		if err != nil {
			return err
		}
		out(m)
	case "gov execute":
		n, err := id()
		if err != nil {
			return err
		}
		if err := c.govExecute(n); err != nil {
			return err
		}
		m, _ := c.govShow(n)
		out(m)
	case "gov show":
		n, err := id()
		if err != nil {
			return err
		}
		m, err := c.govShow(n)
		if err != nil {
			return err
		}
		out(m)
	case "gov proposals":
		ps, err := c.govProposals(f.all)
		if err != nil {
			return err
		}
		out(ps)
	case "gov versions":
		v, err := c.govVersions(firstOr(pos, 0, ""))
		if err != nil {
			return err
		}
		out(v)
	case "gov updates":
		v, err := c.govUpdates(false)
		if err != nil {
			return err
		}
		out(v)
	case "gov use":
		if err := need(pos, 1, "gov use KEY [ADDRESS|vN|current]"); err != nil {
			return err
		}
		m, err := c.govUse(pos[0], firstOr(pos, 1, ""))
		if err != nil {
			return err
		}
		out(m)
	case "gov current":
		if err := need(pos, 1, "gov current KEY"); err != nil {
			return err
		}
		a, err := c.dirCurrent(pos[0])
		if err != nil {
			return err
		}
		out(map[string]any{pos[0]: a.Hex(), "you_use": c.ch.dep.Contracts[pos[0]]})
	case "gov primitives":
		ps, err := c.ch.refresh()
		if err != nil {
			return err
		}
		out(ps)
	default:
		return unknownCommand(cmd)
	}
	return nil
}
