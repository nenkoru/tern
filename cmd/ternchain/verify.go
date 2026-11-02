// Spot checks: did a provider really do the work? Nobody can watch a GPU,
// but anyone can run the same job again and compare. A provider signs what
// it computed (a work statement: job, input hash, output hash, how the
// output was made comparable); a checker runs a sample of the jobs on
// another provider and compares the hashes. Two that disagree prove only
// that one of them is wrong; three or more decide by majority, and the
// signers outvoted have signed a wrong answer.
//
//	verify statement JOB INPUT OUTPUT --canon C [--resource R] [--engine E]   sign what you computed (tern.work/1)
//	verify sample JOB --seed SECRET --rate 0.05      is this job one to check? (the provider can't tell in advance)
//	verify compare STATEMENT STATEMENT [STATEMENT…]  match, mismatch (with the outvoted) or undecided
//	verify report RESULT [--group G] [--public]      sealed to the group, and/or a public claim (kind check)
//	verify score NAME [--group G]                    checks passed and failed, every one re-verified
//	verify bond PAYEE AMOUNT --panel a,b,c [--k N] [--days D] [--group G]   a provider's bond: a panel lock
//	verify escalate LOCK RESULT                      the result as evidence in the bond's case; the panel decides
//
// Hashes compare only what is deterministic: greedy decoding's token ids,
// a file's bytes, a rounded vector. canon names how (tokens/greedy,
// sha256/bytes, f16/round-3); statements with different canons are never
// compared. The work itself (the prompt, the activations) never leaves the
// group: statements carry hashes.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern/internal/ui"
)

const (
	tWork  = "tern.work/1"
	tCheck = "tern.check/1"
)

// WorkStatement: "I computed OUTPUT for JOB from INPUT", signed.
type WorkStatement struct {
	Schema   string                     `json:"schema"`
	By       string                     `json:"by"`
	Job      string                     `json:"job"`    // the requester's id for the job
	Input    string                     `json:"input"`  // 0x…: hash of the input
	Output   string                     `json:"output"` // 0x…: hash of the canonical output
	Canon    string                     `json:"canon"`  // how the output was made comparable
	Resource string                     `json:"resource,omitempty"`
	Engine   string                     `json:"engine,omitempty"`
	Vec      []float64                  `json:"vec,omitempty"` // numbers to compare within a tolerance (a few logits, a reading)
	At       int64                      `json:"at"`
	Ext      map[string]json.RawMessage `json:"ext,omitempty"` // what this version doesn't name (work/NAME), signed with it
	Sig      string                     `json:"sig,omitempty"`
}

func (s WorkStatement) body() []byte {
	s.Sig = ""
	return jsonOf(s)
}

var (
	hash32RE = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)
	jobRE    = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,80}$`)
	canonRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,47}$`)
)

func (s WorkStatement) check() error {
	switch {
	case s.Schema != tWork:
		return errors.New("not a tern.work/1 statement")
	case !jobRE.MatchString(s.Job):
		return errors.New("job: letters, digits and ._:-, at most 80")
	case !hash32RE.MatchString(s.Input) || !hash32RE.MatchString(s.Output):
		return errors.New("input and output: 0x and 32 bytes of hex (hashes, never the data)")
	case !canonRE.MatchString(s.Canon):
		return errors.New("canon: how the output was made comparable, like tokens/greedy")
	case len(s.Resource) > 120 || len(s.Engine) > 64:
		return errors.New("resource or engine too long")
	case len(s.Vec) > 256:
		return errors.New("vec: at most 256 numbers (a sample, not the data)")
	}
	for _, x := range s.Vec {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return errors.New("vec: finite numbers only")
		}
	}
	return nil
}

// CheckResult: what comparing statements of one job showed.
type CheckResult struct {
	Schema     string          `json:"schema"`
	Job        string          `json:"job"`
	Input      string          `json:"input"`
	Canon      string          `json:"canon"`
	Comparator string          `json:"comparator,omitempty"` // a compare module's line (default compare-default@1: exact)
	Verdict    string          `json:"verdict"`              // match | mismatch | undecided
	Majority   string          `json:"majority,omitempty"`
	Outvoted   []string        `json:"outvoted,omitempty"`
	Statements []WorkStatement `json:"statements"`
}

func (c *Client) workStatement(s WorkStatement) (WorkStatement, error) {
	s.Schema, s.By, s.At = tWork, c.f.Name, time.Now().Unix()
	s.Input, s.Output = strings.ToLower(s.Input), strings.ToLower(s.Output)
	if err := s.check(); err != nil {
		return s, err
	}
	var err error
	s.Sig, err = c.coopSign(tWork, s.body())
	return s, err
}

// compareWork: statements of one job, each by another family, decided by
// majority. It trusts nothing it is given: every signature is checked.
func (c *Client) compareWork(ss []WorkStatement, line string) (CheckResult, error) {
	return c.compareWorkAt(ss, line, 0)
}

// compareWorkAt: the same, signatures checked at block b (0: now): a result
// on chain is checked at the block that holds it.
func (c *Client) compareWorkAt(ss []WorkStatement, line string, b uint64) (CheckResult, error) {
	r := CheckResult{Schema: tCheck, Comparator: firstNonEmpty(line, "compare-default@1")}
	if len(ss) < 2 || len(ss) > 16 {
		return r, errors.New("compare 2 to 16 statements of one job")
	}
	p, err := c.compareModule(r.Comparator)
	if err != nil {
		return r, err
	}
	fams := map[string]bool{}
	for i, s := range ss {
		if err := s.check(); err != nil {
			return r, fmt.Errorf("statement %d: %v", i+1, err)
		}
		if err := c.coopVerifyAt(s.By, tWork, s.body(), s.Sig, b); err != nil {
			return r, fmt.Errorf("statement %d (%s): signature: %v", i+1, s.By, err)
		}
		if i == 0 {
			r.Job, r.Input, r.Canon = s.Job, strings.ToLower(s.Input), s.Canon
		} else if s.Job != r.Job || strings.ToLower(s.Input) != r.Input || s.Canon != r.Canon {
			return r, fmt.Errorf("statement %d is about another job, input or canon: not comparable", i+1)
		}
		if fams[ownerName(s.By)] {
			return r, fmt.Errorf("two statements by %s's family: one family is one vote", ownerName(s.By))
		}
		fams[ownerName(s.By)] = true
	}
	r.Statements = ss
	// who agrees with whom, by the comparator; the statement most agree with
	// anchors the majority
	n := len(ss)
	agree := make([][]bool, n)
	all, anchor, best := true, 0, -1
	for i := range ss {
		agree[i] = make([]bool, n)
	}
	for i := range ss {
		for j := i + 1; j < n; j++ {
			a := p.Bool("agree", pairEnv(ss[i], ss[j]))
			agree[i][j], agree[j][i] = a, a
			all = all && a
		}
	}
	if all {
		r.Verdict = "match"
		return r, nil
	}
	for i := range ss {
		k := 0
		for j := range ss {
			if agree[i][j] {
				k++
			}
		}
		if k > best {
			anchor, best = i, k
		}
	}
	if 2*(best+1) <= n {
		r.Verdict = "undecided"
		return r, nil
	}
	r.Verdict, r.Majority = "mismatch", strings.ToLower(ss[anchor].Output)
	for j, s := range ss {
		if j != anchor && !agree[anchor][j] {
			r.Outvoted = append(r.Outvoted, s.By)
		}
	}
	sort.Strings(r.Outvoted)
	return r, nil
}

// compareModule: the comparator, a policy module of kind compare: built in
// (compare-default@1 exact, compare-cosine@1, compare-topk@1, compare-abs@1)
// or one installed (policy install).
func (c *Client) compareModule(line string) (*ui.Policy, error) {
	if p := ui.BuiltinPolicyLine(line); p != nil && p.M.AppliesTo == "compare" {
		return p, nil
	}
	if c.dir != "" {
		if p, err := c.loadPolicyLine(line); err == nil && p.M.AppliesTo == "compare" {
			return p, nil
		}
	}
	return nil, fmt.Errorf("comparator %s: not built in and not installed (policy install it; built in: compare-default@1, compare-cosine@1, compare-topk@1, compare-abs@1)", line)
}

// minTolVec: the fewest numbers a cosine or top-k comparison means anything
// on. With 5 or fewer, the top 5 are every entry (any two vectors "share" them);
// with 1, the cosine of any two positive numbers is 1. Shorter vectors leave
// pair.cosine and pair.topk_overlap at 0 (no agreement by tolerance), so a
// lazy provider can't pass with a few arbitrary numbers. compare-abs@1 (one
// reading, an odometer) still works on any length.
const minTolVec = 8

// pairEnv: what a comparator reads about two statements.
func pairEnv(a, b WorkStatement) ui.Env {
	env := ui.Env{"pair.same_hash": strings.EqualFold(a.Output, b.Output), "pair.has_vec": false,
		"pair.cosine": 0.0, "pair.max_abs": 1e9, "pair.topk_overlap": 0.0}
	if len(a.Vec) == 0 || len(a.Vec) != len(b.Vec) {
		return env
	}
	var dot, na, nb, maxAbs float64
	for i := range a.Vec {
		dot, na, nb = dot+a.Vec[i]*b.Vec[i], na+a.Vec[i]*a.Vec[i], nb+b.Vec[i]*b.Vec[i]
		maxAbs = max(maxAbs, math.Abs(a.Vec[i]-b.Vec[i]))
	}
	env["pair.has_vec"], env["pair.max_abs"] = true, maxAbs
	if len(a.Vec) < minTolVec {
		return env
	}
	if na > 0 && nb > 0 {
		env["pair.cosine"] = dot / math.Sqrt(na*nb)
	}
	top := func(v []float64) map[int]bool {
		idx := make([]int, len(v))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(i, j int) bool { return v[idx[i]] > v[idx[j]] })
		out := map[int]bool{}
		for _, i := range idx[:min(5, len(idx))] {
			out[i] = true
		}
		return out
	}
	ta, tb := top(a.Vec), top(b.Vec)
	common := 0
	for i := range ta {
		if tb[i] {
			common++
		}
	}
	env["pair.topk_overlap"] = float64(common) / float64(len(ta))
	return env
}

// recheck: a result as someone else reports it, decided again from its
// statements by the comparator it names (signatures checked at block b; 0: now).
func (c *Client) recheck(r CheckResult, b uint64) (CheckResult, error) {
	if r.Schema != tCheck {
		return r, errors.New("not a tern.check/1 result")
	}
	return c.compareWorkAt(r.Statements, r.Comparator, b)
}

// sampled: is job one of the fraction rate to check, under seed? The seed
// stays the checker's until it shows a result; its hash can be published
// first, so nobody picks the jobs after the fact.
func sampled(seed, job string, rate float64) bool {
	h := sha256.Sum256([]byte(seed + "\x00" + job))
	return float64(binary.BigEndian.Uint64(h[:8]))/float64(^uint64(0)) < rate
}

type checkPost struct {
	Check *CheckResult `json:"check"`
}

func (c *Client) checkReport(r CheckResult, group string, public bool) (map[string]any, error) {
	r, err := c.recheck(r, 0)
	if err != nil {
		return nil, err
	}
	if group == "" && !public {
		return nil, errors.New("report where: --group G (sealed to the group) and/or --public (a claim anyone reads)")
	}
	res := map[string]any{"verdict": r.Verdict, "job": r.Job}
	if group != "" {
		if c.st.Groups[group] == nil {
			return nil, fmt.Errorf("no keys for group %s here", group)
		}
		id, err := c.postGroup(group, fmt.Sprintf("spot check of job %s: %s", r.Job, r.Verdict), jsonOf(checkPost{&r}))
		if err != nil {
			return nil, err
		}
		res["posted"] = id
	}
	if public {
		// one claim about each signer other than ourselves: +100 matched the
		// majority, -100 outvoted; undecided says nothing about anyone
		var uids []string
		out := map[string]bool{}
		for _, n := range r.Outvoted {
			out[n] = true
		}
		for _, s := range r.Statements {
			if r.Verdict == "undecided" || c.family(s.By) {
				continue
			}
			score := int64(100)
			if out[s.By] {
				score = -100
			}
			uid, err := c.easAttest(subjectID(s.By), "check", jsonOf(r), score, 0, common.Hash{})
			if err != nil {
				return res, err
			}
			uids = append(uids, uid.Hex())
		}
		res["claims"] = uids
	}
	return res, nil
}

// CheckScore: what checks say about one name, each re-verified. Every
// result about a job counts, not only the first one reported. When the
// results agree (all say passed, or all failed), that is the job's outcome.
// When they conflict, each family that signed a statement in any of them
// takes a side once (NAME's own family on NAME's side), and the job passes
// or fails only if more than two thirds of the families are on one side;
// otherwise it is disputed. So a few Sybil owners who report first can't
// frame an honest provider: the honest checks reported later count too, and
// conflicting reports dispute the job instead of failing it.
type CheckScore struct {
	Name      string         `json:"name"`
	Passed    int            `json:"passed"`
	Failed    int            `json:"failed"`    // outvoted by two thirds of the families that checked the job
	Undecided int            `json:"undecided"` // disagreed with one other, no majority
	Disputed  int            `json:"disputed"`  // reports about the job conflict, no two-thirds side
	ByChecker map[string]int `json:"by_checker"`
	Jobs      []CheckJob     `json:"jobs,omitempty"`
	Ignored   []string       `json:"ignored,omitempty"`
}

// CheckJob: one job's tally: the families agreeing and disagreeing with NAME.
type CheckJob struct {
	Job      string   `json:"job"`
	Outcome  string   `json:"outcome"` // passed | failed | undecided | disputed
	Agree    []string `json:"agree,omitempty"`
	Disagree []string `json:"disagree,omitempty"`
	Reports  int      `json:"reports"`
}

func (c *Client) checkScore(name, group string) (*CheckScore, error) {
	sc := &CheckScore{Name: name, ByChecker: map[string]int{}}
	type tally struct {
		job     string
		side    map[string]int // family -> +1 agrees with name, -1 disagrees, 0 both (dropped)
		reports int
		pass    int // results in which name matched the majority
		fail    int // results in which name was outvoted
	}
	jobs := map[string]*tally{}
	var order []string
	self := ownerName(name)
	add := func(by string, r CheckResult, block uint64) {
		r, err := c.recheck(r, block)
		if err != nil {
			sc.Ignored = append(sc.Ignored, fmt.Sprintf("by %s: %v", by, err))
			return
		}
		in := false
		for _, s := range r.Statements {
			in = in || s.By == name
		}
		if !in {
			return
		}
		k := r.Job + "|" + r.Input
		tl := jobs[k]
		if tl == nil {
			tl = &tally{job: r.Job, side: map[string]int{}}
			jobs[k] = tl
			order = append(order, k)
		}
		tl.reports++
		sc.ByChecker[by]++
		switch {
		case r.Verdict == "match" || r.Verdict == "mismatch" && !hasStr(r.Outvoted, name):
			tl.pass++
		case r.Verdict == "mismatch":
			tl.fail++
		}
		out := map[string]bool{}
		for _, n := range r.Outvoted {
			out[n] = true
		}
		for _, s := range r.Statements {
			f := ownerName(s.By)
			if s.By == name || f == self {
				continue
			}
			var v int
			switch {
			case r.Verdict == "match":
				v = 1
			case r.Verdict == "mismatch" && out[name] && !out[s.By]:
				v = -1 // the majority name was outvoted by
			case r.Verdict == "mismatch" && !out[name]:
				v = map[bool]int{true: -1, false: 1}[out[s.By]]
			default: // undecided, or another outvoted signer: says nothing
				continue
			}
			if old, ok := tl.side[f]; ok && old != v {
				v = 0 // one family on both sides: it counts for neither
			}
			tl.side[f] = v
		}
	}
	claims, err := c.easClaims(ptrHash(subjectID(name)))
	if err != nil {
		return nil, err
	}
	for _, cl := range claims {
		if cl.Kind != "check" || cl.Revoked {
			continue
		}
		var r CheckResult
		if jsonStrict([]byte(cl.Data), &r) != nil {
			sc.Ignored = append(sc.Ignored, "a claim by "+cl.Issuer+" that isn't a tern.check/1 result")
			continue
		}
		add(cl.Issuer, r, cl.Block)
	}
	if group != "" {
		for _, it := range c.groupItems(group) {
			var p checkPost
			if it.Mail == nil || len(it.Mail.Data) == 0 || jsonStrict(it.Mail.Data, &p) != nil || p.Check == nil {
				continue
			}
			add(it.From, *p.Check, it.Block)
		}
	}
	for _, k := range order {
		tl := jobs[k]
		j := CheckJob{Job: tl.job, Reports: tl.reports}
		for f, v := range tl.side {
			switch v {
			case 1:
				j.Agree = append(j.Agree, f)
			case -1:
				j.Disagree = append(j.Disagree, f)
			}
		}
		sort.Strings(j.Agree)
		sort.Strings(j.Disagree)
		a, d := len(j.Agree)+1, len(j.Disagree) // name's own family is on its side
		switch {
		case tl.pass == 0 && tl.fail == 0:
			j.Outcome = "undecided"
			sc.Undecided++
		case tl.fail == 0:
			j.Outcome = "passed"
			sc.Passed++
		case tl.pass == 0:
			j.Outcome = "failed"
			sc.Failed++
		case 3*a > 2*(a+d):
			j.Outcome = "passed"
			sc.Passed++
		case 3*d > 2*(a+d):
			j.Outcome = "failed"
			sc.Failed++
		default:
			j.Outcome = "disputed"
			sc.Disputed++
		}
		sc.Jobs = append(sc.Jobs, j)
	}
	return sc, nil
}

func hasStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func ptrHash(h common.Hash) *common.Hash { return &h }

// checkBond: the provider locks a bond that a panel pays out to PAYEE
// (a checker, the group's treasurer) if its checks show wrong answers, and
// that comes back at the end of the term otherwise.
func (c *Client) checkBond(payee string, amount *big.Int, panel []string, k uint8, days int, group string) (uint64, error) {
	if days <= 0 {
		days = 30
	}
	if k == 0 {
		k = uint8(len(panel)/2 + 1)
	}
	now := uint64(c.chainNow())
	terms := fmt.Sprintf("spot-check bond of %s%s: the panel pays %s the share it finds the provider's checked answers wrong "+
		"(tern.check/1 results as evidence: verify escalate); otherwise it comes back at the end of the term",
		c.f.Name, map[bool]string{true: " in " + group, false: ""}[group != ""], payee)
	id, _, err := c.openPanel(payee, amount, now+uint64(days)*86400, k, panel, terms, now+60)
	return id, err
}

// checkEscalate: a result as evidence in a bond's case.
func (c *Client) checkEscalate(lock uint64, r CheckResult) (uint64, error) {
	r, err := c.recheck(r, 0)
	if err != nil {
		return 0, err
	}
	if r.Verdict == "match" {
		return 0, errors.New("the statements match: nothing to escalate")
	}
	dir := filepath.Join(c.dir, "evidence")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	path := filepath.Join(dir, fmt.Sprintf("check-%d-%s.json", lock, r.Job))
	if err := os.WriteFile(path, jsonOf(r), 0o600); err != nil {
		return 0, err
	}
	note := fmt.Sprintf("spot check of job %s: %s; outvoted: %s", r.Job, r.Verdict, strings.Join(r.Outvoted, ", "))
	return c.panelEvidence(lock, path, note)
}

// parseVec: "0.12,-3.5,7" as numbers.
func parseVec(s string) ([]float64, error) {
	if s == "" {
		return nil, nil
	}
	var out []float64
	for _, x := range strings.Split(s, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return nil, fmt.Errorf("--vec: numbers separated by commas: %v", err)
		}
		out = append(out, v)
	}
	return out, nil
}

func parseStatement(arg string) (WorkStatement, error) {
	var s WorkStatement
	raw, err := coopInput(arg)
	if err == nil {
		err = jsonStrict(raw, &s)
	}
	return s, err
}

func parseResult(arg string) (CheckResult, error) {
	var r CheckResult
	raw, err := coopInput(arg)
	if err == nil {
		err = jsonStrict(raw, &r)
	}
	return r, err
}

func runVerify(c *Client, cmd string, pos []string, f coopFlags) error {
	switch cmd {
	case "verify statement":
		if len(pos) < 3 {
			return errors.New("usage: verify statement JOB INPUT-HASH OUTPUT-HASH --canon C [--resource R] [--engine E]")
		}
		vec, err := parseVec(f.vec)
		if err != nil {
			return err
		}
		s, err := c.workStatement(WorkStatement{Job: pos[0], Input: pos[1], Output: pos[2], Canon: f.canon, Resource: f.resource, Engine: f.engine, Vec: vec})
		if err != nil {
			return err
		}
		out(s)
		return nil
	case "verify sample":
		if len(pos) < 1 || f.seed == "" || f.rate <= 0 || f.rate > 1 {
			return errors.New("usage: verify sample JOB --seed SECRET --rate 0.05")
		}
		out(map[string]any{"job": pos[0], "check": sampled(f.seed, pos[0], f.rate),
			"seed_commit": crypto.Keccak256Hash([]byte(f.seed)).Hex()})
		return nil
	case "verify compare":
		var ss []WorkStatement
		for _, a := range pos {
			s, err := parseStatement(a)
			if err != nil {
				return fmt.Errorf("%s: not a tern.work/1 statement: %v", a, err)
			}
			ss = append(ss, s)
		}
		r, err := c.compareWork(ss, f.compare)
		if err != nil {
			return err
		}
		out(r)
		return nil
	case "verify report", "verify escalate":
		args := pos
		var lock uint64
		if cmd == "verify escalate" {
			if len(pos) < 2 {
				return errors.New("usage: verify escalate LOCK RESULT")
			}
			if _, err := fmt.Sscan(pos[0], &lock); err != nil {
				return errors.New("LOCK: the bond's lock id")
			}
			args = pos[1:]
		}
		if len(args) < 1 {
			return fmt.Errorf("usage: %s RESULT (a tern.check/1: JSON, @FILE or -)", cmd)
		}
		r, err := parseResult(args[0])
		if err != nil {
			return fmt.Errorf("not a tern.check/1 result: %v", err)
		}
		if cmd == "verify escalate" {
			art, err := c.checkEscalate(lock, r)
			if err != nil {
				return err
			}
			out(map[string]any{"evidence": art, "lock": lock, "next": "the panel reads it (panel case LOCK) and decides (lock decide)"})
			return nil
		}
		res, err := c.checkReport(r, f.group, f.public)
		if err != nil {
			return err
		}
		out(res)
		return nil
	case "verify score":
		if len(pos) < 1 {
			return errors.New("usage: verify score NAME [--group G]")
		}
		if f.group != "" {
			if _, err := c.scan(); err != nil {
				return err
			}
		}
		sc, err := c.checkScore(pos[0], f.group)
		if err != nil {
			return err
		}
		out(sc)
		return nil
	case "verify bond":
		if len(pos) < 2 || f.global("panel") == "" {
			return errors.New("usage: verify bond PAYEE AMOUNT --panel a,b,c [--k N] [--days D] [--group G]")
		}
		amount, err := parseCoins(pos[1])
		if err != nil {
			return err
		}
		var k, days int
		fmt.Sscan(f.global("k"), &k)
		fmt.Sscan(f.global("days"), &days)
		id, err := c.checkBond(pos[0], amount, splitList(f.global("panel")), uint8(k), days, f.group)
		if err != nil {
			return err
		}
		out(map[string]any{"bond": id, "payee": pos[0], "amount": fmtCoins(amount),
			"next": "checkers escalate a mismatch with 'verify escalate " + fmt.Sprint(id) + " RESULT'; the panel decides; unused, it comes back at the end of the term (lock refund)"})
		return nil
	}
	return unknownCommand(cmd)
}
