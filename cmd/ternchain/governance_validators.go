// The citizens' side of the validator set (docs/abuse/VALIDATORS.md):
//
//	gov propose param validators add DIR [CITIZEN] | remove V | cancel V   one change a proposal
//	gov validator show                          the set, who holds each, the cap, pending removals, alerts
//	gov validator offer ADDRESS|DIR             you consent to run this node key (an add needs it)
//	gov validator rotate NEW_ADDRESS|DIR        your validator to a new node key
//	gov validator lapse V | apply V             a lapsed citizen's validator; a removal that is due
//	gov validator challenge A B --why EVIDENCE  "same operator" (shared IP/ASN/keys), citizens vote
//	gov validator challenge-vote ID yes|no | challenge-close ID
//	gov validator report REPORT.json            a watcher's censorship report, on chain
//
// And the alarms every client's watch shows: governance-critical
// proposals, a governance contract changed (never followed by itself), the
// set's changes, churn past a quarter, challenges, censorship reports.
package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// governanceCritical: Directory keys whose upgrade needs a supermajority
// and the long exit window (Parliament._criticalKeys), and that clients
// never follow by themselves.
var governanceCritical = map[string]bool{"Validators": true, "Parliament": true, "Citizens": true, "CitizenGroup": true, "Directory": true,
	"TernTimelock": true, "TernGovernor": true, "WrappedTern": true, "VoteVerifier": true}

func governanceChangedText(key, current, using string) string {
	t := fmt.Sprintf("!!! GOVERNANCE CONTRACT CHANGED: %s is now %s", key, current)
	if using != "" {
		t += " (this client uses " + using + ")"
	}
	t += ". This client does NOT follow it: read 'gov versions " + key + "' (spec, audit) and the proposal that passed it; switch only by hand ('gov use " + key + "')."
	if key == "Validators" {
		t += " The chain's nodes don't follow it either: Besu reads the validator contract named in each node's genesis; moving it takes a chain upgrade every operator installs."
	}
	return t
}

func (c *Client) exitWindowText(id uint64) string {
	if w, err := c.ch.call("Parliament", "exitWindow", u(id)); err == nil {
		return dur(w[0].(*big.Int).Uint64())
	}
	return "the long exit window"
}

func (c *Client) validators() error {
	if !c.ch.has("Validators") {
		return errors.New("this network has no Validators contract (not a people's L1)")
	}
	return nil
}

// validatorAddCheck: what Validators.addValidator would refuse at execution, said before the vote.
func (c *Client) validatorAddCheck(v common.Address, who string) error {
	if err := c.validators(); err != nil {
		return err
	}
	if !c.isCitizen(who) {
		return fmt.Errorf("%s is not a citizen: a validator is a citizen's (one each)", who)
	}
	if out, err := c.ch.call("Validators", "validatorOf", idOf(who)); err == nil && out[0].(common.Address) != (common.Address{}) {
		return fmt.Errorf("one validator per citizen: %s already holds %s (rotate it instead: gov validator rotate)", who, out[0].(common.Address).Hex())
	}
	if out, err := c.ch.call("Validators", "isValidator", v); err == nil && out[0].(bool) {
		return fmt.Errorf("%s is already a validator", v.Hex())
	}
	if out, err := c.ch.call("Validators", "offered", idOf(who)); err == nil && out[0].(common.Address) != v {
		return fmt.Errorf("%s has not offered %s: the citizen consents first ('gov validator offer %s' with its own key)", who, v.Hex(), v.Hex())
	}
	return nil
}

// validatorHandover (VA-8): the governance, Registry and citizens' house the
// Validators contract was handed to, against the network's own contracts
// (TernTimelock, Registry, Citizens). The bootstrap could have named any.
func validatorHandover(call func(string, ...any) ([]any, error), contracts map[string]string) map[string]any {
	res := map[string]any{}
	var bad []string
	for _, x := range [][2]string{{"governance", "TernTimelock"}, {"registry", "Registry"}, {"citizens", "Citizens"}} {
		out, err := call(x[0])
		if err != nil || len(out) == 0 {
			continue
		}
		got := out[0].(common.Address)
		res[x[0]] = got.Hex()
		if want, ok := contracts[x[1]]; ok && !strings.EqualFold(want, got.Hex()) {
			bad = append(bad, fmt.Sprintf("%s is %s, the network's %s is %s", x[0], got.Hex(), x[1], want))
		}
	}
	res["matches"] = len(bad) == 0
	if len(bad) > 0 {
		res["warning"] = "!!! THE VALIDATOR SET IS NOT HELD BY THIS NETWORK'S GOVERNANCE: " + strings.Join(bad, "; ")
	}
	return res
}

// validatorHandoverAt: validatorHandover over an RPC, against a network file (nil: no file to compare with).
func validatorHandoverAt(rpcURL, netPath string) map[string]any {
	var f DevnetFile
	if netPath == "" || readJSON(netPath, &f) != nil || len(f.Contracts) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cl, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil
	}
	defer cl.Close()
	a, err := loadArtifact("Validators")
	if err != nil {
		return nil
	}
	return validatorHandover(func(method string, args ...any) ([]any, error) {
		data, err := a.abi.Pack(method, args...)
		if err != nil {
			return nil, err
		}
		out, err := cl.CallContract(ctx, ethereum.CallMsg{To: &validatorsAddress, Data: data}, nil)
		if err != nil {
			return nil, err
		}
		return a.abi.Unpack(method, out)
	}, f.Contracts)
}

func (c *Client) validatorRemoveCheck(v common.Address) error {
	if err := c.validators(); err != nil {
		return err
	}
	if out, err := c.ch.call("Validators", "isValidator", v); err == nil && !out[0].(bool) {
		return fmt.Errorf("%s is not a validator", v.Hex())
	}
	return nil
}

// validatorCallNote: what a proposal to the Validators contract will do, in words.
func (c *Client) validatorCallNote(data []byte) string {
	if len(data) < 4 {
		return ""
	}
	va := c.ch.abis["Validators"]
	m, err := va.MethodById(data[:4])
	if err != nil {
		return ""
	}
	rules, _ := c.ch.call("Validators", "rules")
	left, _ := c.ch.call("Validators", "changesLeft")
	cap := ""
	if len(left) > 0 {
		cap = fmt.Sprintf("; the rolling cap allows %d more change(s) in this window now", left[0].(*big.Int).Int64())
	}
	switch m.Name {
	case "addValidator":
		return "adds one validator when executed (after the exit window), only the key its citizen offered ('gov validator offer')" + cap
	case "removeValidator":
		d := ""
		if len(rules) > 1 {
			d = dur(rules[1].(uint64))
		}
		return "schedules one removal when executed (a key its citizen rotated away from: the citizen's current key); it takes effect " + d + " later (anyone runs 'gov validator apply'), within the cap and never below the minimum set" + cap
	case "cancelRemoval":
		return "cancels a scheduled removal"
	case "setRules":
		return "changes the set's own safety rules (cap window, removal delay, minimum set): governance-critical"
	case "scheduleUpgrade":
		return "schedules a chain-software upgrade every operator must install: governance-critical"
	}
	return ""
}

// validatorEvents: the set's changes and alarms, into watch.
func (c *Client) validatorEvents(from uint64, emit func(Event)) error {
	if !c.ch.has("Validators") {
		return nil
	}
	a := c.ch.abis["Validators"]
	head, err := c.ch.b.BlockNumber(context.Background())
	if err != nil || from > head {
		return nil
	}
	if from < c.ch.dep.Block {
		from = c.ch.dep.Block
	}
	logs, err := c.ch.b.FilterLogs(context.Background(), ethereum.FilterQuery{FromBlock: new(big.Int).SetUint64(from),
		Addresses: []common.Address{common.HexToAddress(c.ch.dep.Contracts["Validators"])}})
	if err != nil {
		return err
	}
	nameOf := func(h [32]byte) string {
		if h == ([32]byte{}) {
			return "unbound"
		}
		return c.nameOrHex(common.Hash(h))
	}
	types := map[string]string{"ValidatorAdded": "validator-added", "ValidatorRemoved": "validator-removed", "RemovalScheduled": "validator-removal",
		"RemovalCancelled": "validator-removal", "ValidatorRotated": "validator-rotated", "ChurnAlert": "validator-churn",
		"ChallengeOpened": "validator-challenge", "ChallengeClosed": "validator-challenge", "CensorshipReported": "censorship",
		"RulesSet": "governance-critical", "UpgradeScheduled": "chain-upgrade"}
	for _, l := range logs {
		if len(l.Topics) == 0 {
			continue
		}
		ev, err := a.EventByID(l.Topics[0])
		if err != nil || types[ev.Name] == "" {
			continue
		}
		if t := describeValidatorEvent(ev.Name, l, *ev, nameOf); t != "" {
			emit(Event{types[ev.Name], l.BlockNumber, t, map[string]any{"tx": l.TxHash.Hex()}})
		}
	}
	return nil
}

func runGovValidator(c *Client, pos []string, f govFlags) error {
	if err := c.validators(); err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("gov validator show|offer|rotate|lapse|apply|challenge|challenge-vote|challenge-close|report")
	}
	sub, args := pos[0], pos[1:]
	addr := func(i int) (common.Address, error) {
		if len(args) <= i {
			return common.Address{}, fmt.Errorf("gov validator %s: an ADDRESS or validator DIR", sub)
		}
		return addrOrValidatorDir(args[i])
	}
	send := func(method string, a ...any) error {
		_, err := c.ch.send(c.key, nil, "Validators", method, a...)
		return err
	}
	switch sub {
	case "show":
		r, err := validatorStatus(c.ch.dep.RPC, 1)
		if err != nil {
			return err
		}
		m := map[string]any{}
		for _, k := range []string{"validators_contract", "rules", "churn", "bindings", "challenges", "alerts", "fault_tolerance", "head"} {
			if v, ok := r[k]; ok {
				m[k] = v
			}
		}
		if b, ok := m["bindings"].([]map[string]any); ok {
			for _, x := range b {
				if s, ok := x["citizen"].(string); ok && strings.HasPrefix(s, "0x") {
					x["citizen"] = c.nameOrHex(common.HexToHash(s))
				}
			}
		}
		m["handover"] = validatorHandover(func(method string, a ...any) ([]any, error) { return c.ch.call("Validators", method, a...) }, c.ch.dep.Contracts)
		out(m)
	case "offer":
		v, err := addr(0)
		if err != nil {
			return err
		}
		if err := send("offerValidator", c.id(), v); err != nil {
			return err
		}
		out(map[string]any{"citizen": c.f.Name, "offered": v.Hex(),
			"next": "a proposal adds it: gov propose param validators add " + v.Hex() + " " + c.f.Name + " (only this key; 'offer 0x0000000000000000000000000000000000000000' withdraws)"})
	case "rotate":
		to, err := addr(0)
		if err != nil {
			return err
		}
		was, _ := c.ch.call("Validators", "validatorOf", c.id())
		if err := send("rotate", c.id(), to); err != nil {
			return err
		}
		m := map[string]any{"citizen": c.f.Name, "validator": to.Hex(), "note": "from the next block the old key no longer seals; stop the old node"}
		if len(was) > 0 {
			m["was"] = was[0].(common.Address).Hex()
		}
		out(m)
	case "lapse", "apply":
		v, err := addr(0)
		if err != nil {
			return err
		}
		method := map[string]string{"lapse": "lapse", "apply": "applyRemoval"}[sub]
		if err := send(method, v); err != nil {
			return err
		}
		still, _ := c.ch.call("Validators", "isValidator", v)
		at, _ := c.ch.call("Validators", "removalAt", v)
		m := map[string]any{"validator": v.Hex(), "in_set": still[0].(bool)}
		if len(at) > 0 && at[0].(uint64) != 0 {
			m["removal_at"] = time.Unix(int64(at[0].(uint64)), 0).UTC().Format(time.RFC3339)
		}
		out(m)
	case "challenge":
		a, err := addr(0)
		if err != nil {
			return err
		}
		b, err := addr(1)
		if err != nil {
			return err
		}
		ev, uri, err := docRef(f.why, "", "why")
		if err != nil {
			return fmt.Errorf("--why EVIDENCE_FILE (shared IP/ASN, key reuse, the same hoster account…): %w", err)
		}
		r, err := c.ch.send(c.key, nil, "Validators", "challenge", c.id(), a, b, ev, uri)
		if err != nil {
			return err
		}
		id, err := c.firstIndexed(r, "Validators", "ChallengeOpened")
		if err != nil {
			return err
		}
		out(map[string]any{"challenge": id, "a": a.Hex(), "b": b.Hex(), "evidence": common.Hash(ev).Hex(), "uri": uri,
			"note": fmt.Sprintf("citizens vote 'gov validator challenge-vote %d yes|no'; 2/3 yes is a finding: the removal of %s (the second) is then a Parliament proposal citing it (gov propose param validators remove %s)", id, b.Hex(), b.Hex())})
	case "challenge-vote":
		if len(args) != 2 {
			return errors.New("gov validator challenge-vote ID yes|no")
		}
		id, err := strconv.ParseUint(args[0], 10, 64)
		if err != nil {
			return err
		}
		if err := send("voteChallenge", u(id), c.id(), args[1] == "yes"); err != nil {
			return err
		}
		out(map[string]any{"challenge": id, "vote": args[1]})
	case "challenge-close":
		if len(args) != 1 {
			return errors.New("gov validator challenge-close ID")
		}
		id, err := strconv.ParseUint(args[0], 10, 64)
		if err != nil {
			return err
		}
		if err := send("closeChallenge", u(id)); err != nil {
			return err
		}
		x, err := c.ch.call("Validators", "challengeAt", u(id))
		if err != nil {
			return err
		}
		ch := abiConvert[valChallenge](x[0])
		m := map[string]any{"challenge": id, "passed": ch.Passed, "yes": ch.Yes, "no": ch.No}
		if ch.Passed {
			m["next"] = fmt.Sprintf("a finding, not a removal: propose it to both houses, citing challenge %d: gov propose param validators remove %s --why …", id, ch.B.Hex())
		}
		out(m)
	case "report":
		if len(args) != 1 {
			return errors.New("gov validator report CENSORSHIP_REPORT.json (from validator watch --report-dir)")
		}
		var rep CensorshipReport
		if err := readJSON(args[0], &rep); err != nil {
			return err
		}
		uri := args[0]
		if f.why != "" {
			uri = f.why
		}
		if err := send("reportCensorship", c.id(), [32]byte(rep.Tx), rep.FirstSeen, rep.BlocksPending, rep.Proposers, uri); err != nil {
			return err
		}
		out(map[string]any{"reported": rep.Tx.Hex(), "blocks_pending": rep.BlocksPending, "proposers": rep.Proposers, "unverified": true,
			"note": "a watcher's claim, unverified by the contract (check the blocks yourself); on chain as evidence (Validators.CensorshipReported): every citizen's watch shows it; propose the removals, and if those are censored too, the social fork (fork-from-state)"})
	default:
		return fmt.Errorf("gov validator %s? (show|offer|rotate|lapse|apply|challenge|challenge-vote|challenge-close|report)", sub)
	}
	return nil
}
