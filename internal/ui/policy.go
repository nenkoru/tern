package ui

// Client policy modules (tern.policy/1), modelled on the ranking modules: a
// reader's social policies — inbox triage, the rating formula, report weights
// and thresholds, spam filters, look-alike names — as signed artifacts of
// small pure expressions (tern-expr/1), parameters and data tables, run on
// the reader's device. Today's formulas ship as the built-in modules (the
// manifests in builtin/); a builder publishes another, an owner installs it
// (pinned, never auto-updated) and switches to it. Each kind names its
// inputs, its outputs and the security floors no module changes: the client
// enforces those around the module, and a kind's parameters have hard
// bounds a module can't pass.

import (
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
)

const KindPolicy = "tern.policy/1"

// PolicyManifest: tern.policy/1.
type PolicyManifest struct {
	Schema      string             `json:"schema"`
	Name        string             `json:"name"`
	Version     string             `json:"version"`
	Line        string             `json:"line"`
	Description string             `json:"description,omitempty"`
	AppliesTo   string             `json:"applies_to"`
	Engine      string             `json:"engine"`
	Inputs      []string           `json:"inputs,omitempty"`
	Params      map[string]float64 `json:"params,omitempty"`
	Rules       map[string]string  `json:"rules,omitempty"`
	Reason      string             `json:"reason,omitempty"`
	Tables      map[string]string  `json:"tables,omitempty"`
	Limits      *struct {
		MaxCost int `json:"max_cost,omitempty"`
	} `json:"limits,omitempty"`
	License string            `json:"license,omitempty"`
	Author  string            `json:"author"`
	Payee   string            `json:"payee"`
	Parent  map[string]any    `json:"parent,omitempty"`
	Royalty map[string]any    `json:"royalty,omitempty"`
	Price   map[string]any    `json:"price,omitempty"`
	Files   map[string]string `json:"files"`
}

// PolicyOutput: one value a kind asks its module for.
type PolicyOutput struct {
	What     string   `json:"what"`
	Type     string   `json:"type"` // bool | number
	Required bool     `json:"required,omitempty"`
	Reads    []string `json:"reads"`         // the inputs it may read (a trailing "." is a prefix)
	Min      *float64 `json:"min,omitempty"` // the client clamps a number to [Min, Max]: a floor
	Max      *float64 `json:"max,omitempty"`
}

// PolicyParam: a number the client itself reads from the module (a
// threshold), with its default and hard bounds.
type PolicyParam struct {
	What    string  `json:"what"`
	Default float64 `json:"default"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Floor   bool    `json:"floor,omitempty"` // a bound that protects people (🛡), not just sanity
}

// PolicyKind: what a kind of policy reads, answers, and may not change.
type PolicyKind struct {
	Name    string                  `json:"name"`
	What    string                  `json:"what"`
	Inputs  map[string]string       `json:"inputs,omitempty"`
	Outputs map[string]PolicyOutput `json:"outputs,omitempty"`
	Params  map[string]PolicyParam  `json:"params,omitempty"`
	Tables  map[string]string       `json:"tables,omitempty"`
	Reason  bool                    `json:"reason,omitempty"` // the module may word the reason
	Floors  []string                `json:"floors"`
}

func f64(x float64) *float64 { return &x }

// PolicyKinds: the replaceable client policies.
var PolicyKinds = map[string]*PolicyKind{
	"triage": {
		Name: "triage",
		What: "what of outside mail reaches the owner or agent (delivered, else quarantined: information only) and what wakes it",
		Inputs: map[string]string{
			"msg.contact":       "the sender (or its owner) is your contact, or (for an agent) one your owner approved (owner contact / owner approve AGENT NAME, or the grant's scopes.to)",
			"msg.owner_contact": "(an agent) your owner approved the sender (or its owner) as your correspondent, or your grant's scopes.to names it",
			"msg.paid_postage":  "the sender paid the postage your card asks for this letter's category (a bulk sender pays the ads price)",
			"msg.expected":      "a delivery you wait for: evidence in a case you are in, a key for a lock you paid or decide, a credential presentation",
			"msg.reply_to_ours": "an answer to something you sent",
			"msg.work_request":  "a work request (tern.work-request)",
			"msg.free_requests": "your card takes work requests from strangers without postage",
			"msg.from_agent":    "the sender is an agent, not a person",
			"msg.citizen":       "the sender is a verified citizen (human mode)",
			"msg.category":      "the category the sender says the letter is about (work, dating, ads, …; \"\" none): text",
			"msg.basis":         "what the default ladder matched, in words (the built-in reason): text",
		},
		Outputs: map[string]PolicyOutput{
			"deliver": {What: "delivered (true) or quarantined (false)", Type: "bool", Required: true, Reads: []string{"msg."}},
			"wake":    {What: "wakes the reader's watch (only a delivered item can)", Type: "bool", Required: true, Reads: []string{"msg."}},
		},
		Reason: true,
		Floors: []string{
			"the owner's own mail is instructions; nothing else is (a module only chooses delivered or quarantined: both are information)",
			"an approval counts only from your own owner",
			"a contact's agent tainted by outside mail stays quarantined",
			"the hop limit refuses long agent chains with strangers (grant scope hop_limit, at most 12); a trusted pair (same owner, or a lasting correspondent the owner approved) has a daily budget per thread instead (thread_per_day, at most 500)",
			"outside mail taints the agent; taint passes through siblings",
			"a look-alike of a known name never wakes anyone, and is flagged",
			"a quarantined item never wakes",
		},
	},
	"rating": {
		Name: "rating",
		What: "the rating formula: how reviews of paid deals weigh and add up",
		Inputs: map[string]string{
			"reviewer.has_rating": "the reviewer has a rating of its own (one level deep, without reviewer weights)",
			"reviewer.score":      "the reviewer's own score, -100..100 (0 when it has none)",
			"reviewer.weight":     "the reviewer's weight (reviewer_weight; 1 while computing reviewers' own ratings)",
			"review.age_days":     "days since the review",
			"review.coins":        "coins paid out in the cited lock",
			"review.trust":        "the review's trust (review_trust)",
			"rating.mean":         "the reviews' weighted mean score, -100..100",
			"rating.trust":        "the reviews' trust added up (Σ review_trust)",
			"rating.reviews":      "how many reviews counted",
		},
		Outputs: map[string]PolicyOutput{
			"reviewer_weight": {What: "a reviewer's weight, 0..1", Type: "number", Required: true, Reads: []string{"reviewer.has_rating", "reviewer.score"}, Min: f64(0), Max: f64(1)},
			"review_trust":    {What: "how far one review is trusted (freshness × reviewer)", Type: "number", Required: true, Reads: []string{"review.age_days", "reviewer.weight"}, Min: f64(0)},
			"review_weight":   {What: "a review's weight in the mean (the deal's size × its trust)", Type: "number", Required: true, Reads: []string{"review.", "reviewer.weight"}, Min: f64(0)},
			"score":           {What: "the score from the mean and the total trust, -100..100", Type: "number", Required: true, Reads: []string{"rating."}, Min: f64(-100), Max: f64(100)},
		},
		Floors: []string{
			"only reviews that cite a paid Bank lock between the reviewer and the subject count",
			"no self-reviews (the subject's own family); one review per reviewer family and deal, the latest",
			"a review's issuer key is checked against the name at the time",
			"the score stays within -100..100; weights are never negative",
		},
	},
	"reports": {
		Name: "reports",
		What: "members' reports: each reporter's weight, the weight that hides a post, and an author's conduct penalty",
		Inputs: map[string]string{
			"reporter.citizen":  "the reporter's owner is a verified citizen",
			"reporter.age_days": "days since the reporter's owner registered (0: unknown)",
			"reporter.penalty":  "the reporter's own conduct penalty",
			"reporter.record":   "how its reports in this community fared (record)",
			"record.upheld":     "its reports moderators upheld (hid the post after the report)",
			"record.dismissed":  "its reports moderators dismissed (or it revoked after a dismissal)",
			"record.stale":      "its reports that stood alone a day later",
			"community.members": "the community's members (the post's audience)",
			"author.penalty":    "the post author's conduct penalty",
			"author.citizen":    "the post author's owner is a verified citizen",
		},
		Outputs: map[string]PolicyOutput{
			"reporter_weight": {What: "one owner's weight as a reporter", Type: "number", Required: true, Reads: []string{"reporter."}, Min: f64(0)},
			"record":          {What: "the reporter's record factor", Type: "number", Required: true, Reads: []string{"record."}, Min: f64(0)},
			"threshold":       {What: "the reports' weight that hides a post in a community this size", Type: "number", Required: true, Reads: []string{"community."}, Min: f64(0)},
			"author_factor":   {What: "the threshold × this for the post's author (its standing)", Type: "number", Required: true, Reads: []string{"author."}, Min: f64(0)},
		},
		Params: map[string]PolicyParam{
			"min_owners":          {What: "owners that must report a post before it hides", Default: 2, Min: 2, Max: 100, Floor: true},
			"conduct_min_members": {What: "a community's voting members for its bans and hides to count against anyone", Default: 3, Min: 3, Max: 10000, Floor: true},
			"hide_at":             {What: "an author's conduct penalty from which its new posts are hidden until a moderator clears them", Default: 9, Min: 1, Max: 1000},
			"reported_at":         {What: "owners reporting a post that count it against its author's conduct", Default: 5, Min: 2, Max: 1000},
			"record_stale_hours":  {What: "hours after which a report standing alone counts against its reporter", Default: 24, Min: 1, Max: 720},
			"ban_penalty":         {What: "penalty per community that banned one of the owner's names", Default: 25, Min: 0, Max: 100},
			"name_hidden_penalty": {What: "penalty per name hidden by moderators", Default: 10, Min: 0, Max: 100},
			"post_hidden_penalty": {What: "penalty per post hidden by moderators", Default: 5, Min: 0, Max: 100},
			"reported_penalty":    {What: "penalty per post that reported_at owners reported (and no moderator cleared)", Default: 3, Min: 0, Max: 100},
		},
		Floors: []string{
			"never one reporter alone: min_owners is at least 2",
			"reports from the author's own family, from members on probation, and after a moderator's dismissal don't count",
			"only bans and hides from communities of at least 3 voting members, still standing, against a member there count",
		},
	},
	"spam": {
		Name: "spam",
		What: "reader-side spam hiding in communities and bulk-sender detection for postage",
		Params: map[string]PolicyParam{
			"near_dup_at":         {What: "word similarity (Jaccard) from which a post is a near-duplicate of its owner's earlier one", Default: 0.6, Min: 0.1, Max: 1},
			"near_dup_series":     {What: "one name's own near-duplicates shown before they are hidden", Default: 3, Min: 1, Max: 100},
			"posts_per_hour":      {What: "posts from one owner's names in a community within the window, beyond which they are hidden", Default: 6, Min: 1, Max: 10000},
			"rate_window_minutes": {What: "the window posts_per_hour counts over", Default: 60, Min: 1, Max: 1440},
			"bulk_postage_at":     {What: "different names an owner's names paid in a day, beyond which its letters are priced as ads", Default: 20, Min: 1, Max: 1000000},
		},
		Floors: []string{
			"a post repeating its owner's earlier post word for word is always hidden",
			"anonymous posts are checked against each other",
		},
	},
	"pool": {
		Name: "pool",
		What: "who in a pooled group is served first, and who waits (a group's admins pick the module; tern.pool/1)",
		Inputs: map[string]string{
			"member.balance":          "the member's credits in the group (served minus used, at the group's rates)",
			"member.served":           "credits the member earned by serving others",
			"member.used":             "credits the member used",
			"member.used_today":       "credits the member used this UTC day",
			"member.hours_since_used": "hours since the member last used the pool (1000000 if never)",
			"member.waiting_minutes":  "how long the member's request has waited, as the caller says (0 if unknown)",
			"pool.floor":              "the group's credit floor (credits policy)",
			"pool.quota_day":          "the group's daily quota in credits (0: none)",
		},
		Outputs: map[string]PolicyOutput{
			"priority": {What: "higher is served first", Type: "number", Required: true, Reads: []string{"member.", "pool."}},
			"waits":    {What: "served only when nobody else waits", Type: "bool", Required: true, Reads: []string{"member.", "pool."}},
		},
		Floors: []string{
			"only receipts both sides signed count toward any input; a module orders members, it never moves credits or money",
			"a module is pure: the same ledger gives every member the same order",
		},
	},
	"compare": {
		Name: "compare",
		What: "when two statements of the same work agree (verify compare): an exact hash, or numbers within a tolerance",
		Inputs: map[string]string{
			"pair.same_hash":    "the two outputs' hashes are equal",
			"pair.has_vec":      "both statements carry numbers (vec) of the same length",
			"pair.cosine":       "the cosine of the two vectors (0 without them)",
			"pair.max_abs":      "the largest absolute difference between the vectors (1e9 without them)",
			"pair.topk_overlap": "the share of the 5 largest entries' positions the two vectors have in common, 0..1",
		},
		Outputs: map[string]PolicyOutput{
			"agree": {What: "the two count as the same answer", Type: "bool", Required: true, Reads: []string{"pair."}},
		},
		Floors: []string{
			"every statement's signature is checked before any module sees it; one family is one vote",
			"statements of different canons are never compared",
		},
	},
	"lookalike": {
		Name: "lookalike",
		What: "look-alike names: the confusables table and the words that make a look-alike (\"support\", \"official\"…)",
		Tables: map[string]string{
			"confusables": "extra confusable characters, Unicode TR39 confusables.txt lines (SOURCE ; TARGET ; MA), hex code points",
			"words":       "extra words: [support] lines (a known name plus one is a look-alike) and [generic] lines",
		},
		Floors: []string{
			"the warning can't be turned off; a module only adds characters and words to the built-in tables, never removes any",
			"names mixing writing systems are refused at registration unless --force",
			"look-alikes of any registered name are flagged, not only of names you know",
		},
	},
}

// Policy: a compiled module.
type Policy struct {
	M       PolicyManifest
	Hash    string // the manifest's hash (what was signed and installed)
	Builtin bool
	Kind    *PolicyKind
	rules   map[string]*Expr
	reason  *ReasonTemplate
	tables  map[string][]byte
}

func reads(o PolicyOutput, in string) bool {
	for _, r := range o.Reads {
		if r == in || strings.HasSuffix(r, ".") && strings.HasPrefix(in, r) {
			return true
		}
	}
	return false
}

// CompilePolicy checks a manifest: its schema and kind, the engine, that
// every rule is an output of the kind and reads only declared inputs it may
// read and the module's params, that the kind's params stay in bounds.
func CompilePolicy(raw []byte) (*Policy, error) {
	if err := ValidateJSON(KindPolicy, "", raw); err != nil {
		return nil, err
	}
	var m PolicyManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	k := PolicyKinds[m.AppliesTo]
	if k == nil {
		return nil, fmt.Errorf("policy %s: no policy kind %q", m.Name, m.AppliesTo)
	}
	p := &Policy{M: m, Kind: k, rules: map[string]*Expr{}}
	declared := map[string]bool{}
	for _, in := range m.Inputs {
		if _, ok := k.Inputs[in]; !ok {
			return nil, fmt.Errorf("policy %s: unknown input %s (a %s policy reads %s)", m.Name, in, k.Name, strings.Join(keys(k.Inputs), ", "))
		}
		declared[in] = true
	}
	for name, v := range m.Params {
		if d, ok := k.Params[name]; ok && (v < d.Min || v > d.Max || math.IsNaN(v)) {
			fl := ""
			if d.Floor {
				fl = " (a security floor)"
			}
			return nil, fmt.Errorf("policy %s: params.%s = %v is outside %v..%v%s", m.Name, name, v, d.Min, d.Max, fl)
		}
	}
	check := func(what string, vars []string, may func(string) bool) error {
		for _, v := range vars {
			if pn, ok := strings.CutPrefix(v, "params."); ok {
				if _, ok := m.Params[pn]; !ok {
					return fmt.Errorf("policy %s: %s reads %s, which its params don't set", m.Name, what, v)
				}
				continue
			}
			if !declared[v] {
				return fmt.Errorf("policy %s: %s reads %s, which it doesn't declare in inputs", m.Name, what, v)
			}
			if !may(v) {
				return fmt.Errorf("policy %s: %s may not read %s", m.Name, what, v)
			}
		}
		return nil
	}
	for name, src := range m.Rules {
		o, ok := k.Outputs[name]
		if !ok {
			return nil, fmt.Errorf("policy %s: a %s policy has no rule %q (it has %s)", m.Name, k.Name, name, strings.Join(keys(k.Outputs), ", "))
		}
		e, err := Parse(src)
		if err != nil {
			return nil, fmt.Errorf("policy %s: rules.%s: %w", m.Name, name, err)
		}
		if err := check("rules."+name, e.Vars(), func(v string) bool { return reads(o, v) }); err != nil {
			return nil, err
		}
		p.rules[name] = e
	}
	for name, o := range k.Outputs {
		if o.Required && p.rules[name] == nil {
			return nil, fmt.Errorf("policy %s: a %s policy needs rules.%s (%s)", m.Name, k.Name, name, o.What)
		}
	}
	if m.Reason != "" {
		if !k.Reason {
			return nil, fmt.Errorf("policy %s: a %s policy has no reason to word", m.Name, k.Name)
		}
		r, err := ParseReason(m.Reason)
		if err != nil {
			return nil, fmt.Errorf("policy %s: %w", m.Name, err)
		}
		if err := check("reason", r.Vars(), func(string) bool { return true }); err != nil {
			return nil, err
		}
		p.reason = r
	}
	for name := range m.Tables {
		if _, ok := k.Tables[name]; !ok {
			return nil, fmt.Errorf("policy %s: a %s policy carries no table %q", m.Name, k.Name, name)
		}
	}
	return p, nil
}

// LoadPolicy: a checked bundle's module, with its tables (each re-checked
// against its hash as it is read).
func LoadPolicy(b *Bundle) (*Policy, error) {
	p, err := CompilePolicy(b.Raw)
	if err != nil {
		return nil, err
	}
	p.Hash = b.Hash
	p.tables = map[string][]byte{}
	for name, file := range p.M.Tables {
		data, err := b.File(file)
		if err != nil {
			return nil, err
		}
		p.tables[name] = data
	}
	return p, nil
}

//go:embed builtin/*.json
var builtinFS embed.FS

var (
	builtinOnce     sync.Once
	builtinPolicies map[string]*Policy // by kind: its default
	builtinLines    map[string]*Policy // every built-in module, by line (a kind may ship several: pool)
	builtinRankers  map[string]*Ranker // by name
)

func loadBuiltins() {
	builtinOnce.Do(func() {
		builtinPolicies, builtinLines, builtinRankers = map[string]*Policy{}, map[string]*Policy{}, map[string]*Ranker{}
		ents, _ := builtinFS.ReadDir("builtin")
		for _, e := range ents {
			raw, _ := builtinFS.ReadFile("builtin/" + e.Name())
			canon, err := canonical(raw)
			if err != nil {
				panic("builtin " + e.Name() + ": " + err.Error())
			}
			var h Header
			json.Unmarshal(canon, &h)
			switch h.Schema {
			case KindPolicy:
				p, err := CompilePolicy(canon)
				if err != nil {
					panic("builtin " + e.Name() + ": " + err.Error())
				}
				p.Builtin, p.Hash = true, "0x"+sha(canon)
				builtinLines[p.M.Line] = p
				if p.M.Name == p.M.AppliesTo+"-default" {
					builtinPolicies[p.M.AppliesTo] = p
				}
			case KindRanker:
				r, err := CompileRanker(canon)
				if err != nil {
					panic("builtin " + e.Name() + ": " + err.Error())
				}
				r.Hash = "0x" + sha(canon)
				builtinRankers[r.M.Name] = r
			}
		}
	})
}

// BuiltinPolicy: today's policy of that kind, as a module.
func BuiltinPolicy(kind string) *Policy {
	loadBuiltins()
	return builtinPolicies[kind]
}

// BuiltinPolicyLine: a built-in module by line (pool-quota@1), nil if none.
func BuiltinPolicyLine(line string) *Policy {
	loadBuiltins()
	return builtinLines[line]
}

// BuiltinRanker: a built-in ranking module by name (new, hot, trust).
func BuiltinRanker(name string) *Ranker {
	loadBuiltins()
	return builtinRankers[name]
}

// BuiltinRankers: the built-in ranking modules' names.
func BuiltinRankers() []string {
	loadBuiltins()
	return keys(builtinRankers)
}

// BuiltinManifest: a built-in module's manifest, to start a module of one's own from.
func BuiltinManifest(schema, name string) ([]byte, bool) {
	ents, _ := builtinFS.ReadDir("builtin")
	for _, e := range ents {
		raw, _ := builtinFS.ReadFile("builtin/" + e.Name())
		var h struct {
			Schema    string `json:"schema"`
			Name      string `json:"name"`
			AppliesTo string `json:"applies_to"`
		}
		json.Unmarshal(raw, &h)
		if h.Schema == schema && (h.Name == name || h.AppliesTo == name) {
			return raw, true
		}
	}
	return nil, false
}

func (p *Policy) maxCost() int {
	if p.M.Limits != nil && p.M.Limits.MaxCost > 0 {
		return p.M.Limits.MaxCost
	}
	return 10000
}

// Param: a kind parameter the client reads (the module's value within the
// kind's bounds, else the default), or one of the module's own params.
func (p *Policy) Param(name string) float64 {
	d, isKind := p.Kind.Params[name]
	v, ok := p.M.Params[name]
	switch {
	case !ok && isKind:
		return d.Default
	case isKind:
		return math.Max(d.Min, math.Min(d.Max, v))
	}
	return v
}

// Has: the module sets this rule.
func (p *Policy) Has(rule string) bool { return p.rules[rule] != nil }

func (p *Policy) env(in Env) Env {
	e := make(Env, len(in)+len(p.M.Params))
	for k, v := range in {
		e[k] = v
	}
	for k, v := range p.M.Params {
		e["params."+k] = v
	}
	return e
}

// fallback: the built-in module of the same kind, for a rule this module
// doesn't set or that failed to evaluate (a cost limit, a type error).
func (p *Policy) fallback() *Policy {
	if b := BuiltinPolicy(p.Kind.Name); b != nil && b != p {
		return b
	}
	return nil
}

// Value: a rule's value for these inputs.
func (p *Policy) Value(rule string, in Env) (any, error) {
	e := p.rules[rule]
	if e == nil {
		if b := p.fallback(); b != nil {
			return b.Value(rule, in)
		}
		return nil, fmt.Errorf("policy %s has no rule %s", p.M.Name, rule)
	}
	v, err := e.Eval(p.env(in), p.maxCost())
	if err != nil {
		if b := p.fallback(); b != nil {
			return b.Value(rule, in)
		}
		return nil, fmt.Errorf("policy %s rules.%s: %w", p.M.Name, rule, err)
	}
	return v, nil
}

// Bool: a yes/no rule (false on an error no fallback answers).
func (p *Policy) Bool(rule string, in Env) bool {
	v, err := p.Value(rule, in)
	if err != nil {
		return false
	}
	b, err := truthy(v)
	return err == nil && b
}

// Num: a number rule, clamped to the kind's floors (0 on an error no fallback answers).
func (p *Policy) Num(rule string, in Env) float64 {
	v, err := p.Value(rule, in)
	if err != nil {
		return 0
	}
	f, err := toNum(v)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	if o, ok := p.Kind.Outputs[rule]; ok {
		if o.Min != nil {
			f = math.Max(*o.Min, f)
		}
		if o.Max != nil {
			f = math.Min(*o.Max, f)
		}
	}
	return f
}

// Reason: the module's words for this case, or def.
func (p *Policy) Reason(in Env, def string) string {
	if p.reason == nil {
		return def
	}
	s, err := p.reason.Render(p.env(in), p.maxCost())
	if err != nil || strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// Table: a data table the module carries (nil: none).
func (p *Policy) Table(name string) []byte { return p.tables[name] }

// Line: how the module is named to people.
func (p *Policy) Line() string {
	if p.Builtin {
		return p.M.Line + " (built in)"
	}
	return p.M.Line
}

// Explain: everything a person needs to judge the module before using it:
// what it reads, its rules and parameters (with the kind's bounds), its
// tables, and the floors around it.
func (p *Policy) Explain() map[string]any {
	in := map[string]string{}
	for _, i := range p.M.Inputs {
		in[i] = p.Kind.Inputs[i]
	}
	params := map[string]any{}
	for name, d := range p.Kind.Params {
		x := map[string]any{"value": p.Param(name), "default": d.Default, "bounds": []float64{d.Min, d.Max}, "what": d.What}
		if d.Floor {
			x["floor"] = true
		}
		params[name] = x
	}
	for name, v := range p.M.Params {
		if _, ok := p.Kind.Params[name]; !ok {
			params[name] = map[string]any{"value": v, "what": "the module's own constant"}
		}
	}
	rules := map[string]any{}
	for name, o := range p.Kind.Outputs {
		src := "(not set: the built-in rule)"
		if r, ok := p.M.Rules[name]; ok {
			src = r
		}
		rules[name] = map[string]any{"what": o.What, "rule": src}
	}
	m := map[string]any{"kind": p.Kind.Name, "what": p.Kind.What, "module": p.Line(), "version": p.M.Version, "author": p.M.Author,
		"hash": p.Hash, "inputs": in, "params": params, "floors": p.Kind.Floors}
	if p.M.Description != "" {
		m["description"] = p.M.Description
	}
	if len(rules) > 0 {
		m["rules"] = rules
	}
	if p.M.Reason != "" {
		m["reason"] = p.M.Reason
	}
	if len(p.M.Tables) > 0 {
		t := map[string]any{}
		for name, file := range p.M.Tables {
			t[name] = map[string]any{"file": file, "what": p.Kind.Tables[name], "bytes": len(p.tables[name])}
		}
		m["tables"] = t
	}
	return m
}

// PolicyKindNames: the kinds, sorted.
func PolicyKindNames() []string {
	out := keys(PolicyKinds)
	sort.Strings(out)
	return out
}
