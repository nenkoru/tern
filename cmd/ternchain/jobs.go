// ERC-8183 (Agentic Commerce) jobs: ternchain's escrow behind the standard's
// job interface (contracts/ext/TernJobs.sol).
//
// A client creates a job for a provider with an evaluator, sets its budget
// and funds it from a Bank lock: the client's name (or its agent, within
// the limit its owner set) locks the budget for the provider's name with
// TernJobs as the condition, and TernJobs holds the lock. The provider
// submits, the evaluator completes (the lock pays the provider) or rejects
// (it goes back), and after the expiry anyone refunds an unfinished job.
// Any ERC-8183 client or tool sees the same jobs, events and states; one
// that is not ternchain funds plainly (fund(jobId, "") pulls the budget).
//
// ternchain's other escrows map onto it: a tender is a job made without a
// provider (job create - EVALUATOR …, then job provider ID NAME picks the
// winning bid); a panel or arbiter is the evaluator (any address: a
// ternchain account, a multisig, a contract); a pledge (many funders, all
// or nothing) stays PledgeCondition: a job has one client.
package main

import (
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var jobStatusNames = []string{"open", "funded", "submitted", "completed", "rejected", "expired"}

// jobProviders: job id => the provider's ternchain name (jobs.json in the
// client's folder): a Bank lock pays a name, the job knows an address.
func (c *Client) jobProviders() map[string]string {
	m := map[string]string{}
	readJSON(filepath.Join(c.dir, "jobs.json"), &m)
	return m
}

func (c *Client) setJobProvider(id uint64, name string) error {
	m := c.jobProviders()
	m[fmt.Sprint(id)] = name
	return writeJSON(filepath.Join(c.dir, "jobs.json"), m)
}

// jobParty: a ternchain name's address, or an address as given; "-": none.
func (c *Client) jobParty(s string) (common.Address, string, error) {
	switch {
	case s == "-" || s == "":
		return common.Address{}, "", nil
	case common.IsHexAddress(s):
		return common.HexToAddress(s), "", nil
	}
	a, err := c.addressOf(s)
	return a, s, err
}

func (c *Client) jobsReady() error {
	if !c.ch.has("TernJobs") {
		return errors.New("no ERC-8183 jobs on this network (TernJobs, deployed with a settlement token)")
	}
	t, err := c.ch.call("TernJobs", "paymentToken")
	if err != nil {
		return err
	}
	if c.asset != t[0].(common.Address) {
		return fmt.Errorf("jobs here are in %s: pay in it (--token %s)", c.ch.assetSymbol(t[0].(common.Address)), t[0].(common.Address).Hex())
	}
	return nil
}

// jobCreate: a job for provider ("-": a tender), judged by evaluator, worth
// budget, expiring in `days`.
func (c *Client) jobCreate(provider, evaluator string, budget *big.Int, days int, desc string) (map[string]any, error) {
	if err := c.jobsReady(); err != nil {
		return nil, err
	}
	pa, pname, err := c.jobParty(provider)
	if err != nil {
		return nil, fmt.Errorf("provider: %w", err)
	}
	ea, _, err := c.jobParty(evaluator)
	if err != nil || ea == (common.Address{}) {
		return nil, fmt.Errorf("evaluator %q: a name or an address: %v", evaluator, err)
	}
	now := c.chainNow()
	exp := big.NewInt(now + int64(max(days, 1))*86400)
	rc, err := c.ch.send(c.key, nil, "TernJobs", "createJob", pa, ea, exp, desc, common.Address{})
	if err != nil {
		return nil, err
	}
	id, err := c.firstIndexed(rc, "TernJobs", "JobCreated")
	if err != nil {
		return nil, err
	}
	if _, err := c.ch.send(c.key, nil, "TernJobs", "setBudget", new(big.Int).SetUint64(id), budget, []byte{}); err != nil {
		return nil, err
	}
	if pname != "" {
		c.setJobProvider(id, pname)
	}
	r := map[string]any{"job": id, "client": c.me().Hex(), "provider": provider, "evaluator": evaluator,
		"budget": c.fmtMoney(budget) + " " + c.ch.assetSymbol(c.asset), "expires": time.Unix(exp.Int64(), 0).UTC().Format(time.RFC3339)}
	if pa == (common.Address{}) {
		r["next"] = fmt.Sprintf("a tender: pick the provider with job provider %d NAME, then job fund %d", id, id)
	} else {
		r["next"] = fmt.Sprintf("job fund %d (locks the budget in your bank for %s)", id, provider)
	}
	return r, nil
}

func (c *Client) jobProvider(id uint64, provider string) error {
	pa, pname, err := c.jobParty(provider)
	if err != nil || pa == (common.Address{}) {
		return fmt.Errorf("provider %q: %v", provider, err)
	}
	if _, err := c.ch.send(c.key, nil, "TernJobs", "setProvider", new(big.Int).SetUint64(id), pa); err != nil {
		return err
	}
	if pname != "" {
		return c.setJobProvider(id, pname)
	}
	return nil
}

type jobT struct {
	ID          *big.Int
	Client      common.Address
	Provider    common.Address
	Evaluator   common.Address
	Description string
	Budget      *big.Int
	ExpiredAt   *big.Int
	Status      uint8
	Hook        common.Address
}

func (c *Client) getJob(id uint64) (jobT, error) {
	out, err := c.ch.call("TernJobs", "getJob", new(big.Int).SetUint64(id))
	if err != nil {
		return jobT{}, fmt.Errorf("no job %d: %w", id, err)
	}
	j := *abi.ConvertType(out[0], new(jobT)).(*jobT)
	return j, nil
}

// jobFund: the budget into a Bank lock for the provider's name, held by the
// job (owner rules and the agent's limit apply as to any lock); a provider
// that is no ternchain name is paid plainly (the account's own tokens).
// price is the budget the client agreed to: the provider may change the
// budget on chain until the job is funded, and TernJobs.fund takes whatever
// it is then (audit ext M-6), so a different budget is refused, read again
// right before funding.
func (c *Client) jobFund(id uint64, price *big.Int, provider string) (map[string]any, error) {
	if err := c.jobsReady(); err != nil {
		return nil, err
	}
	if price == nil || price.Sign() <= 0 {
		return nil, errors.New("job fund ID PRICE: the price you agreed to (the job's budget must equal it)")
	}
	jid := new(big.Int).SetUint64(id)
	budgetIs := func() (jobT, error) { // the budget on chain, now
		j, err := c.getJob(id)
		if err == nil && j.Budget.Cmp(price) != 0 {
			err = fmt.Errorf("job %d's budget is %s, not the %s you agreed to: the provider (or you) changed it; not funding (job show %d)",
				id, c.fmtMoney(j.Budget), c.fmtMoney(price), id)
		}
		return j, err
	}
	j, err := budgetIs()
	if err != nil {
		return nil, err
	}
	if provider == "" {
		provider = c.jobProviders()[fmt.Sprint(id)]
	}
	if provider == "" { // not a ternchain name: the standard's own funding
		if _, ok := c.ch.routes[c.addr()]; !ok && c.me() != c.addr() {
			return nil, errors.New("plain funding pays from this identity's own tokens")
		}
		erc20, err := erc20ABI()
		if err != nil {
			return nil, err
		}
		data, _ := erc20.Pack("approve", c.ch.addr("TernJobs"), price) // the agreed price, not the budget: a raised one can't be pulled
		if _, err := c.ch.sendRaw(c.key, nil, &c.asset, data); err != nil {
			return nil, err
		}
		if _, err := budgetIs(); err != nil {
			zero, _ := erc20.Pack("approve", c.ch.addr("TernJobs"), new(big.Int))
			c.ch.sendRaw(c.key, nil, &c.asset, zero)
			return nil, err
		}
		if _, err := c.ch.send(c.key, nil, "TernJobs", "fund", jid, []byte{}); err != nil {
			return nil, err
		}
		return map[string]any{"job": id, "funded": c.fmtMoney(price), "from": "your account's tokens (the provider is no ternchain name)"}, nil
	}
	if a, err := c.addressOf(provider); err != nil || a != j.Provider {
		return nil, fmt.Errorf("job %d's provider is %s, not %s's address", id, j.Provider.Hex(), provider)
	}
	frees := time.Unix(int64(j.ExpiredAt.Uint64()+3600), 0).UTC().Format(time.RFC3339)
	demand, _ := abi.Arguments{{Type: abiType("uint256")}}.Pack(jid)
	lock, err := c.lockGuarded(provider, provider, price, j.ExpiredAt.Uint64()+3600, "TernJobs", demand)
	if err != nil {
		return nil, err
	}
	// The lock can only fund a job whose budget is its amount: should the
	// budget change now, the lock stays unbound until it refunds, at frees.
	if _, err := budgetIs(); err != nil {
		return nil, fmt.Errorf("%w. Lock %d was made and is frozen until %s (the job's expiry + 1h): then lock refund %d", err, lock, frees, lock)
	}
	opt, _ := abi.Arguments{{Type: abiType("uint256")}}.Pack(new(big.Int).SetUint64(lock))
	if _, err := c.ch.send(c.key, nil, "TernJobs", "fund", jid, opt); err != nil {
		return nil, fmt.Errorf("lock %d made, funding job %d failed (if its budget changed meanwhile, the lock is frozen until %s, the job's expiry + 1h: then lock refund %d): %w",
			lock, id, frees, lock, err)
	}
	return map[string]any{"job": id, "lock": lock, "funded": c.fmtMoney(price) + " " + c.ch.assetSymbol(c.asset), "provider": provider,
		"next": fmt.Sprintf("%s delivers and runs job submit %d DELIVERABLE; the evaluator then job complete %d (pays) or job reject %d (refunds)", provider, id, id, id)}, nil
}

// jobDeliverable: a 0x hash as given, else the hash of the text (an artifact's id, a URL).
func jobDeliverable(s string) common.Hash {
	if strings.HasPrefix(s, "0x") && len(s) == 66 {
		return common.HexToHash(s)
	}
	return crypto.Keccak256Hash([]byte(s))
}

func (c *Client) jobAct(id uint64, action, text string) (map[string]any, error) {
	jid := new(big.Int).SetUint64(id)
	var err error
	switch action {
	case "submit":
		_, err = c.ch.send(c.key, nil, "TernJobs", "submit", jid, jobDeliverable(text), []byte{})
	case "complete", "reject":
		_, err = c.ch.send(c.key, nil, "TernJobs", action, jid, crypto.Keccak256Hash([]byte(text)), []byte{})
	case "refund":
		_, err = c.ch.send(c.key, nil, "TernJobs", "claimRefund", jid)
	}
	if err != nil {
		return nil, err
	}
	return c.jobShow(id)
}

func (c *Client) jobShow(id uint64) (map[string]any, error) {
	j, err := c.getJob(id)
	if err != nil {
		return nil, err
	}
	st := fmt.Sprint(j.Status)
	if int(j.Status) < len(jobStatusNames) {
		st = jobStatusNames[j.Status]
	}
	r := map[string]any{"job": id, "status": st, "client": c.nameOrAddr(j.Client), "provider": c.nameOrAddr(j.Provider),
		"evaluator": c.nameOrAddr(j.Evaluator), "description": j.Description, "budget": c.fmtMoney(j.Budget) + " " + c.ch.assetSymbol(c.asset),
		"expires": time.Unix(j.ExpiredAt.Int64(), 0).UTC().Format(time.RFC3339), "standard": "ERC-8183"}
	if l, err := c.ch.call("TernJobs", "lockOf", new(big.Int).SetUint64(id)); err == nil && l[0].(*big.Int).Sign() > 0 {
		r["lock"] = new(big.Int).Sub(l[0].(*big.Int), big.NewInt(1)).Uint64()
	}
	if d, err := c.ch.call("TernJobs", "deliverableOf", new(big.Int).SetUint64(id)); err == nil && d[0].([32]byte) != [32]byte{} {
		r["deliverable"] = common.Hash(d[0].([32]byte)).Hex()
	}
	return r, nil
}

// nameOrAddr: the ternchain name an address holds, or the address.
func (c *Client) nameOrAddr(a common.Address) string {
	if a == (common.Address{}) {
		return "-"
	}
	if n := c.res.nameAt(a, 0); n != "" {
		return n
	}
	return a.Hex()
}
