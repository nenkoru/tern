// A rating computed by the reader, from the chain, with an open formula
// (docs/PRIMITIVES.md, "Rating"): only reviews that cite a paid Bank lock between
// the reviewer and the subject count. The formula is the rating policy module
// in use (tern.policy/1, policy.go; the built-in one is below); which reviews
// count is not the module's to change.
//
//	attest NAME review TEXT --score S --lock ID   a review citing the deal (data: {"lock":ID,"text":TEXT})
//	rating NAME [--trust a,b] [--days N]          score, stars, deals, disputes, recent reviews
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/nenkoru/tern/internal/ui"
)

// The built-in rating module (internal/ui/builtin/policy-rating.json): a
// 180-day half-life; a prior of a quarter of an imagined neutral review (by a
// fully trusted reviewer): one review can't make a perfect score; a
// reviewer's weight from its own rating, (r+100)/200 within [0.25, 1], 0.5
// with no rating of its own.

// ReviewData is what a review carries (the claim's data).
type ReviewData struct {
	Lock *uint64 `json:"lock"`
	Text string  `json:"text"`
}

// dealLock: a Bank lock, with its parties' families (owner names).
type dealLock struct {
	ID                    uint64
	Payer, From, Payee    string
	Total, Paid, Refunded *big.Int
	RefundAfter           uint64
	Condition             string
	Demand                []byte
	Token                 common.Address
	payerSide, payeeSide  map[string]bool
}

func (c *Client) dealLock(id uint64) (*dealLock, error) {
	l, err := c.ch.call("Bank", "locks", u(id))
	if err != nil {
		return nil, fmt.Errorf("no lock %d", id)
	}
	if l[3].(*big.Int).Sign() == 0 {
		return nil, fmt.Errorf("no lock %d", id)
	}
	d := &dealLock{ID: id, Payer: c.nameOr(common.Hash(l[0].([32]byte))), From: c.nameOr(common.Hash(l[1].([32]byte))),
		Payee: c.nameOr(common.Hash(l[2].([32]byte))), Total: l[3].(*big.Int), Paid: l[4].(*big.Int), Refunded: l[5].(*big.Int),
		RefundAfter: l[6].(uint64), Condition: c.conditionName(l[7].(common.Address)), Demand: l[8].([]byte), Token: l[9].(common.Address)}
	d.payerSide = map[string]bool{ownerName(d.Payer): true, ownerName(d.From): true}
	d.payeeSide = map[string]bool{ownerName(d.Payee): true}
	return d, nil
}

// between: one family on each side of the lock, and not the same family.
func (d *dealLock) between(a, b string) bool {
	a, b = ownerName(a), ownerName(b)
	return a != b && (d.payerSide[a] && d.payeeSide[b] || d.payeeSide[a] && d.payerSide[b])
}

func (d *dealLock) involves(name string) bool {
	return d.payerSide[ownerName(name)] || d.payeeSide[ownerName(name)]
}

// dealAmount: what a deal paid, in whole units of its token (USDC 1.5, not
// 1.5e6): ratings weigh deals by size the same on every network.
func (c *Client) dealAmount(l *dealLock) float64 {
	f, _ := new(big.Rat).SetFrac(l.Paid, unitOf(c.ch.tokenDecimals(l.Token))).Float64()
	return f
}

func coinsFloat(w *big.Int) float64 {
	f, _ := new(big.Rat).SetFrac(w, coinUnit).Float64()
	return f
}

// reviewData encodes a review that cites a deal, after checking that the
// lock is one between the reviewer and the subject (else it would not count).
func (c *Client) reviewData(subject string, lockID uint64, text string) (string, error) {
	d, err := c.dealLock(lockID)
	if err != nil {
		return "", err
	}
	if !d.between(c.f.Name, subject) && !(d.payeeSide[ownerName(subject)] && c.actsForPayer(d, c.f.Name, uint64(c.chainNow()))) {
		return "", fmt.Errorf("lock %d is not a deal between %s and %s (payer %s, payee %s): a review citing it would not count", lockID, ownerName(c.f.Name), ownerName(subject), d.Payer, d.Payee)
	}
	b, _ := json.Marshal(ReviewData{Lock: &lockID, Text: text})
	return string(b), nil
}

// actsForPayer: the lock's payer is a community, and the reviewer speaks for
// it: an admin of it at the review's time, or a panelist who decided the lock.
// Such reviews count once per lock (the latest), weighed by the community's rating.
func (c *Client) actsForPayer(d *dealLock, reviewer string, at uint64) bool {
	if d.payeeSide[ownerName(reviewer)] || !c.ch.has("Commons") || !c.isCommunity(d.Payer) {
		return false
	}
	cid := idOf(d.Payer)
	for _, n := range []string{reviewer, ownerName(reviewer)} {
		if out, err := c.ch.call("Commons", "roleUntil", cid, idOf(n), uint8(roleAdmin)); err == nil && out[0].(uint64) > at {
			return true
		}
	}
	if d.Condition == "PanelCondition" {
		if pc, err := c.panelCase(d.ID); err == nil && pc.role(reviewer) == "panelist" {
			return true
		}
	}
	return false
}

type ratingOpts struct {
	trust map[string]bool // nil: everyone
	days  int             // 0: all time
}

type counted struct {
	cl     *Claim2
	forC   string // the community the reviewer spoke for, or ""
	text   string
	lock   *dealLock
	amount float64
	ageD   float64
}

// ratingPool: the chain's claims read once, checked as trustedClaims does.
type ratingPool struct {
	c       *Client
	pol     *ui.Policy // the rating module in use
	claims  []*Claim2
	locks   map[uint64]*dealLock
	now     int64
	base    map[string]float64 // a reviewer's own rating, one level deep
	hasBase map[string]bool
	checked map[common.Hash]bool // claims whose signer was checked (lazily: only those used)
}

func (c *Client) newRatingPool() (*ratingPool, error) {
	all, err := c.easClaims(nil)
	if err != nil {
		return nil, err
	}
	p := &ratingPool{c: c, pol: c.policyOf("rating"), locks: map[uint64]*dealLock{}, now: c.chainNow(), base: map[string]float64{}, hasBase: map[string]bool{}, checked: map[common.Hash]bool{}}
	for _, cl := range all {
		if cl.Revoked || cl.Issuer == "" || (cl.Expires != 0 && int64(cl.Expires) < p.now) {
			continue
		}
		p.claims = append(p.claims, cl)
	}
	return p, nil
}

// ok: the claim was made with its issuer's key of that time (checked when a
// claim is used: a rating uses tens of the network's thousands of claims,
// most of them community reports; checking each cost a call or two).
func (p *ratingPool) ok(cl *Claim2) bool {
	if v, done := p.checked[cl.UID]; done {
		return v
	}
	bt, _ := p.c.ch.blockTime(cl.Block)
	v := p.c.res.verifySigner(cl.Issuer, cl.Attester, cl.Block, bt) == nil
	p.checked[cl.UID] = v
	return v
}

func (p *ratingPool) lock(id uint64) *dealLock {
	if l, ok := p.locks[id]; ok {
		return l
	}
	l, _ := p.c.dealLock(id)
	p.locks[id] = l
	return l
}

// reviews: the counted reviews of name (and of its owner, for an agent), and why the others don't count.
func (p *ratingPool) reviews(name string, o ratingOpts) ([]counted, map[string]int) {
	subs := map[common.Hash]bool{idOf(name): true, idOf(ownerName(name)): true}
	skipped := map[string]int{}
	latest := map[string]counted{} // one review per reviewer family and deal: the latest
	for _, cl := range p.claims {
		if cl.Kind != "review" || !subs[cl.Subject] || !p.ok(cl) {
			continue
		}
		if o.trust != nil && !o.trust[cl.Issuer] && !o.trust[ownerName(cl.Issuer)] {
			skipped["untrusted_reviewer"]++
			continue
		}
		if ownerName(cl.Issuer) == ownerName(name) {
			skipped["self_review"]++
			continue
		}
		age := float64(p.now-int64(cl.Time)) / 86400
		if o.days > 0 && age > float64(o.days) {
			skipped["older_than_window"]++
			continue
		}
		var rd ReviewData
		if json.Unmarshal([]byte(cl.Data), &rd) != nil || rd.Lock == nil {
			skipped["no_deal_cited"]++
			continue
		}
		l := p.lock(*rd.Lock)
		forC := ""
		switch {
		case l == nil:
			skipped["no_such_lock"]++
			continue
		case !l.between(cl.Issuer, name):
			if l.payeeSide[ownerName(name)] && p.c.actsForPayer(l, cl.Issuer, cl.Time) {
				forC = l.Payer
				break
			}
			skipped["lock_not_between_them"]++
			continue
		}
		if l.Paid.Sign() == 0 {
			skipped["lock_not_paid"]++
			continue
		}
		k := fmt.Sprintf("%s/%d", ownerName(cl.Issuer), l.ID)
		if forC != "" {
			k = fmt.Sprintf("for %s/%d", forC, l.ID) // one review per lock for the community
		}
		if prev, ok := latest[k]; ok {
			skipped["superseded"]++
			if prev.cl.Time > cl.Time {
				continue
			}
		}
		latest[k] = counted{cl: cl, forC: forC, text: rd.Text, lock: l, amount: p.c.dealAmount(l), ageD: max(age, 0)}
	}
	out := make([]counted, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].cl.Time > out[j].cl.Time })
	return out, skipped
}

func clampScore(s int64) float64 { return math.Max(-100, math.Min(100, float64(s))) }

// score: the rating module's score of the mean and the total trust; built
// in: (Σ w·s / Σ w) × R / (R + prior), w = log2(1 + coins paid) × t,
// t = 0.5^(age/180) × reviewer weight, R = Σ t: the deals' sizes weigh the
// reviews against each other; how many reviews, by whom, how fresh, says how
// far to trust the mean. It returns the score, Σ w, and the number of reviews
// used (0: no score).
func (p *ratingPool) score(rs []counted, withinDays float64, weighReviewers bool) (float64, float64, int) {
	var num, den, trust float64
	n := 0
	for _, r := range rs {
		if withinDays > 0 && r.ageD > withinDays {
			continue
		}
		rw := 1.0
		if weighReviewers {
			who := r.cl.Issuer
			if r.forC != "" {
				who = r.forC
			}
			rw = p.reviewerWeight(who)
		}
		t := p.pol.Num("review_trust", ui.Env{"review.age_days": r.ageD, "reviewer.weight": rw})
		w := p.pol.Num("review_weight", ui.Env{"review.coins": r.amount, "review.age_days": r.ageD, "review.trust": t, "reviewer.weight": rw})
		num += w * clampScore(r.cl.Score)
		den += w
		trust += t
		n++
	}
	if n == 0 || den == 0 {
		return 0, den, n
	}
	return p.pol.Num("score", ui.Env{"rating.mean": num / den, "rating.trust": trust, "rating.reviews": float64(n)}), den, n
}

// reviewerWeight: from the reviewer's own rating (computed without reviewer weights: one level deep), capped at 1.
func (p *ratingPool) reviewerWeight(reviewer string) float64 {
	fam := ownerName(reviewer)
	if !p.hasBase[fam] {
		rs, _ := p.reviews(fam, ratingOpts{})
		p.hasBase[fam] = true
		if len(rs) == 0 {
			p.base[fam] = math.NaN()
		} else {
			p.base[fam], _, _ = p.score(rs, 0, false)
		}
	}
	b := p.base[fam]
	if math.IsNaN(b) {
		return p.pol.Num("reviewer_weight", ui.Env{"reviewer.has_rating": false, "reviewer.score": 0.0})
	}
	return p.pol.Num("reviewer_weight", ui.Env{"reviewer.has_rating": true, "reviewer.score": b})
}

// ratingFormula: the formula in words: the module's description (and line, unless built in).
func ratingFormula(p *ui.Policy) string {
	if p.Builtin {
		return "(Σ w·score / Σ w) × R / (R + 0.25); w = log2(1 + coins paid in the cited lock) × t, t = 0.5^(age days / 180) × reviewer weight, R = Σ t; see docs/PRIMITIVES.md, Rating"
	}
	return fmt.Sprintf("the rating module %s %s by %s (policy show rating): %s", p.M.Line, p.M.Version, p.M.Author, p.M.Description)
}

func stars(score float64) float64 { return math.Round((score+100)/200*50) / 10 }

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// rating: everything a reader needs to judge name, from the chain.
func (c *Client) rating(name string, o ratingOpts) (map[string]any, error) {
	if _, err := c.res.nameOf(idOf(name)); err != nil {
		return nil, fmt.Errorf("no such name %q", name)
	}
	p, err := c.newRatingPool()
	if err != nil {
		return nil, err
	}
	rs, skipped := p.reviews(name, o)
	sc, weight, n := p.score(rs, 0, true)
	sc90, _, n90 := p.score(rs, 90, true)
	m := map[string]any{"name": name, "score": round1(sc), "stars": stars(sc), "reviews_counted": len(rs), "weight": round1(weight),
		"last_90_days": map[string]any{"score": round1(sc90), "stars": stars(sc90), "reviews": n90},
		"formula":      ratingFormula(p.pol)}
	if len(skipped) > 0 {
		m["not_counted"] = skipped
	}
	if o.trust != nil {
		var ts []string
		for t := range o.trust {
			ts = append(ts, t)
		}
		sort.Strings(ts)
		m["trusting"] = ts
	}
	if o.days > 0 {
		m["window_days"] = o.days
	}
	deals, err := c.dealsOf(p, name, o)
	if err != nil {
		return nil, err
	}
	for k, v := range deals {
		m[k] = v
	}
	if n == 0 { // no score rather than a made-up middle
		m["score"], m["stars"] = nil, fmt.Sprintf("new: %v deals, no counted reviews", deals["deals"])
	}
	if n90 == 0 {
		m["last_90_days"] = map[string]any{"score": nil, "stars": "no counted reviews in 90 days", "reviews": 0}
	}
	var top []map[string]any
	for _, r := range rs {
		if len(top) == 3 {
			break
		}
		if !c.isOwner() && !c.family(r.cl.Issuer) {
			c.taint(r.cl.Issuer)
		}
		t := map[string]any{"by": r.cl.Issuer, "lock": r.lock.ID, "score": r.cl.Score, "paid": c.fmtIn(r.lock.Paid, r.lock.Token),
			"age_days": round1(r.ageD), "text": fence(r.cl.Issuer, r.text)}
		if r.forC != "" {
			t["for"] = r.forC
		}
		top = append(top, t)
	}
	m["recent_reviews"] = top
	// Owner accountability: what its names (agents included) did in communities.
	cd := c.ownerConduct(p, name)
	m["owner_conduct"] = cd
	if pen, _ := cd["penalty"].(float64); pen > 0 {
		base := 0.0
		if n > 0 {
			base = sc
		}
		m["score_with_conduct"] = round1(math.Max(-100, base-pen))
	} else if n > 0 {
		m["score_with_conduct"] = round1(sc)
	}
	return m, nil
}

// dealsOf: name's family's locks: deals paid, refunds, disputes and their outcomes, settled on time.
func (c *Client) dealsOf(p *ratingPool, name string, o ratingOpts) (map[string]any, error) {
	evs, err := c.ch.events("Bank", "Locked")
	if err != nil {
		return nil, err
	}
	// only the family's locks: by the lock's parties in the event (a lock read costs a call; the network has thousands)
	c.res.loadNames()
	fam := map[common.Hash]bool{}
	famOf := func(h common.Hash) bool {
		if v, ok := fam[h]; ok {
			return v
		}
		n, err := c.res.nameOf(h)
		v := err == nil && ownerName(n) == ownerName(name)
		fam[h] = v
		return v
	}
	var mine []event
	for _, e := range evs {
		if famOf(common.Hash(e.Args["from"].([32]byte))) || famOf(common.Hash(e.Args["payee"].([32]byte))) {
			mine = append(mine, e)
		}
	}
	evs = mine
	firstPay := map[uint64]uint64{} // lock -> block of its first claim or release
	for _, ev := range []string{"Claimed", "Released"} {
		es, err := c.ch.events("Bank", ev)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			id := e.Args["id"].(*big.Int).Uint64()
			if b, ok := firstPay[id]; !ok || e.Block < b {
				firstPay[id] = e.Block
			}
		}
	}
	rulings := map[common.Hash][]*Claim2{}
	for _, cl := range p.claims {
		if cl.Kind == "ruling" {
			rulings[cl.Subject] = append(rulings[cl.Subject], cl)
		}
	}
	verified := func(cls []*Claim2) []*Claim2 {
		var out []*Claim2
		for _, cl := range cls {
			if p.ok(cl) {
				out = append(out, cl)
			}
		}
		return out
	}
	paid, refunded, open, onTime, timed := 0, 0, 0, 0, 0
	volume := new(big.Int)
	var disputes []map[string]any
	won, lost, split := 0, 0, 0
	for _, e := range evs {
		id := e.Args["id"].(*big.Int).Uint64()
		if o.days > 0 {
			if bt, err := c.ch.blockTime(e.Block); err == nil && float64(p.now-bt)/86400 > float64(o.days) {
				continue
			}
		}
		l := p.lock(id)
		if l == nil || !l.involves(name) || l.payerSide[ownerName(name)] && l.payeeSide[ownerName(name)] {
			continue // not theirs, or money within one family
		}
		switch {
		case l.Paid.Sign() > 0:
			paid++
			volume.Add(volume, l.Paid)
			if b, ok := firstPay[id]; ok {
				timed++
				if bt, err := c.ch.blockTime(b); err == nil && uint64(bt) <= l.RefundAfter {
					onTime++
				}
			}
		case l.Refunded.Sign() > 0:
			refunded++
		default:
			open++
		}
		// A dispute: an arbiter's or a panel's ruling on this lock.
		var decider string
		var rs []*Claim2
		switch l.Condition {
		case "AttestationCondition":
			if d, ok := c.decodeAttestationDemand(l.Demand); ok && d["kind"] == "ruling" {
				att, _ := d["attester"].(string)
				decider = "arbiter " + att
				for _, r := range verified(rulings[common.HexToHash(d["subject"].(string))]) {
					if r.Issuer == att || ownerName(r.Issuer) == att {
						rs = append(rs, r)
					}
				}
			}
		case "PanelCondition":
			if pc, err := c.panelCase(id); err == nil {
				decider = "panel of " + strings.Join(pc.Panel, ",")
				for _, r := range verified(rulings[pc.Subject]) {
					if r.Time >= pc.HearingUntil && pc.role(r.Issuer) == "panelist" && !isParty(r.Issuer, pc.Payer, pc.Payee) {
						rs = append(rs, r)
					}
				}
			}
		}
		if decider == "" || len(rs) == 0 {
			continue
		}
		d := map[string]any{"lock": id, "decided_by": decider, "rulings": len(rs), "total": c.fmtIn(l.Total, l.Token)}
		closed := new(big.Int).Add(l.Paid, l.Refunded)
		if closed.Cmp(l.Total) == 0 {
			got := l.Refunded
			if l.payeeSide[ownerName(name)] {
				got = l.Paid
			}
			bps := new(big.Int).Div(new(big.Int).Mul(got, big.NewInt(10000)), l.Total).Int64()
			d["to_"+ownerName(name)+"_bps"] = bps
			switch {
			case bps >= 10000:
				won++
				d["outcome"] = "won"
			case bps == 0:
				lost++
				d["outcome"] = "lost"
			default:
				split++
				d["outcome"] = "split"
			}
		} else {
			d["outcome"] = "not settled yet"
		}
		disputes = append(disputes, d)
	}
	m := map[string]any{"deals": paid, "deals_volume": c.fmtMoney(volume), "refunded_deals": refunded, "open_locks": open,
		"disputes": len(disputes), "dispute_outcomes": map[string]int{"won": won, "lost": lost, "split": split}}
	if len(disputes) > 0 {
		sort.Slice(disputes, func(i, j int) bool { return disputes[i]["lock"].(uint64) > disputes[j]["lock"].(uint64) })
		if len(disputes) > 10 {
			disputes = disputes[:10]
		}
		m["recent_disputes"] = disputes
	}
	if timed > 0 {
		m["settled_before_deadline"] = round1(100*float64(onTime)/float64(timed)) / 100
	}
	return m, nil
}

func parseTrust(s string) map[string]bool {
	if s == "" {
		return nil
	}
	tr := map[string]bool{}
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			tr[x] = true
		}
	}
	return tr
}

// ---- the one-shot CLI: time-boxed, cached

// ratingCacheFor: how long a rating the CLI computed is shown again as is.
const ratingCacheFor = 5 * time.Minute

type ratingCached struct {
	At     int64          `json:"at"`
	Head   uint64         `json:"head"`
	Rating map[string]any `json:"rating"`
}

func (c *Client) ratingCachePath(name string, o ratingOpts) string {
	var ts []string
	for t := range o.trust {
		ts = append(ts, t)
	}
	sort.Strings(ts)
	k := fmt.Sprintf("%s|%s|%d", name, strings.Join(ts, ","), o.days)
	h := sha256.Sum256([]byte(k))
	return filepath.Join(c.dir, "cache", "rating-"+hex.EncodeToString(h[:6])+".json")
}

// ratingTimed: rating within timeout. A rating computed in the last minutes
// is answered from the cache (fresh: compute it again); one that doesn't finish
// in time answers with the last one computed, marked stale, or says so — the
// chain history it read meanwhile is kept for the next run (cache/), so a
// second try is faster. (A cold one-shot CLI used to read every claim and
// every lock on the network, one call each: more than 12 minutes at the
// scale simulation's 2000 agents.)
func (c *Client) ratingTimed(name string, o ratingOpts, timeout time.Duration, fresh bool) (map[string]any, error) {
	path := c.ratingCachePath(name, o)
	var old ratingCached
	haveOld := readJSON(path, &old) == nil && old.Rating != nil
	if haveOld && !fresh && time.Since(time.Unix(old.At, 0)) < ratingCacheFor {
		old.Rating["cached"] = fmt.Sprintf("computed %d s ago at block %d (rating --fresh to compute again)", int(time.Since(time.Unix(old.At, 0)).Seconds()), old.Head)
		return old.Rating, nil
	}
	type res struct {
		m   map[string]any
		err error
	}
	done := make(chan res, 1)
	go func() {
		m, err := c.rating(name, o)
		done <- res{m, err}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			head, _ := c.ch.b.BlockNumber(ctxb())
			os.MkdirAll(filepath.Dir(path), 0o700)
			writeJSON(path, ratingCached{At: time.Now().Unix(), Head: head, Rating: r.m})
		}
		return r.m, r.err
	case <-time.After(timeout):
		if haveOld {
			old.Rating["stale"] = fmt.Sprintf("computing it again took over %s: this one is from %s (block %d); run again to finish",
				timeout, time.Unix(old.At, 0).UTC().Format(time.RFC3339), old.Head)
			return old.Rating, nil
		}
		return nil, fmt.Errorf("rating %s didn't finish in %s (this identity is reading the network's history for the first time; what it read is kept in %s): run it again, or pass --timeout SECONDS",
			name, timeout, filepath.Join(c.dir, "cache"))
	}
}
