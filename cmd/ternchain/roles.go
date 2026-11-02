// roles: who holds every power over a network's contracts, read from the
// chain (ternchain roles). A public network's deployer holds none.
package main

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

type roleRow struct {
	Contract string `json:"contract"`
	Role     string `json:"role"`
	Holder   string `json:"holder"`
	Deployer bool   `json:"deployer_holds"`
}

// roleAudit: every governing role and its holder; deployer: the address to
// check (zero: the Inbox's recorded deployer).
func (ch *Chain) roleAudit(deployer common.Address) ([]roleRow, map[string]any, error) {
	if deployer == (common.Address{}) {
		if v, err := ch.call("Inbox", "deployer"); err == nil {
			deployer = v[0].(common.Address)
		}
	}
	name := func(a common.Address) string {
		for n, x := range ch.dep.Contracts {
			if common.HexToAddress(x) == a {
				return n
			}
		}
		if a == (common.Address{}) {
			return "anyone (address 0)"
		}
		if a == deployer {
			return "the deployer " + a.Hex()
		}
		return a.Hex()
	}
	var rows []roleRow
	add := func(c, r string, a common.Address) {
		rows = append(rows, roleRow{c, r, name(a), a == deployer && a != (common.Address{})})
	}
	tl := ch.addr("TernTimelock")
	held := func(role string, who common.Address) bool {
		r, err := ch.call("TernTimelock", role)
		if err != nil {
			return false
		}
		h, err := ch.call("TernTimelock", "hasRole", r[0].([32]byte), who)
		return err == nil && h[0].(bool)
	}
	whos := []common.Address{tl, ch.addr("Parliament"), {}}
	if deployer != (common.Address{}) {
		whos = append([]common.Address{deployer}, whos...)
	}
	for _, role := range []string{"DEFAULT_ADMIN_ROLE", "PROPOSER_ROLE", "EXECUTOR_ROLE", "CANCELLER_ROLE"} {
		for _, who := range whos {
			if held(role, who) {
				add("TernTimelock", role, who)
			}
		}
	}
	for _, x := range [][2]string{{"Directory", "owner"}, {"FeeOracle", "governance"}, {"Citizens", "governance"}, {"Treasury", "governance"},
		{"Validators", "governance"}, {"SponsorPaymaster", "operator"}} {
		if !ch.has(x[0]) {
			continue
		}
		if v, err := ch.call(x[0], x[1]); err == nil {
			add(x[0], x[1], v[0].(common.Address))
		}
	}
	if v, err := ch.call("Inbox", "bank"); err == nil {
		rows = append(rows, roleRow{"Inbox", "setBank (once, at deployment)", "spent: the Bank is " + name(v[0].(common.Address)), false})
	}
	facts := map[string]any{"deployer": deployer.Hex()}
	if deployer == (common.Address{}) {
		facts["deployer"] = "unknown: name it (ternchain roles 0xDEPLOYER)"
	}
	if v, err := ch.call("Citizens", "count"); err == nil {
		n := v[0].(uint32)
		facts["citizens"] = n
		if n == 0 {
			facts["governance"] = "no citizens and no founders: Parliament can pass nothing, so nobody can change these contracts (option C: a new version is a new deployment)"
		}
	}
	var noOwner []string
	for _, n := range []string{"Registry", "Bank", "Inbox", "EAS", "SchemaRegistry", "AccountFactory", "TernPaymaster", "TernJobs", "ERC8004Registrar",
		"ModuleRegistry", "InboxForwarder", "NoCoinHouse", "Random", "Clock", "Items"} {
		if ch.has(n) {
			noOwner = append(noOwner, n)
		}
	}
	facts["no_owner_by_design"] = noOwner
	holds := 0
	for _, r := range rows {
		if r.Deployer {
			holds++
		}
	}
	facts["deployer_roles"] = fmt.Sprint(holds)
	return rows, facts, nil
}
