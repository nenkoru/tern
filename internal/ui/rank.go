package ui

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// RankerManifest: tern.ranker/1 (docs/FEED.md, research/generative-ui.md 3.3).
type RankerManifest struct {
	Schema      string             `json:"schema"`
	Name        string             `json:"name"`
	Version     string             `json:"version"`
	Line        string             `json:"line"`
	Description string             `json:"description,omitempty"`
	AppliesTo   string             `json:"applies_to"`
	Engine      string             `json:"engine"`
	Inputs      []string           `json:"inputs"`
	Params      map[string]float64 `json:"params,omitempty"`
	Filter      string             `json:"filter,omitempty"`
	Score       string             `json:"score"`
	Reason      string             `json:"reason"`
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

// FeedInputs: every input a ranker may declare, and what it means. A module
// gets only what it declares; the client shows the list before install.
var FeedInputs = map[string]string{
	"item.age_hours":        "hours since the post",
	"item.votes_up":         "members' up reactions (one per owner)",
	"item.votes_down":       "members' down reactions",
	"item.circle_up":        "up reactions by owners in your trust circle",
	"item.circle_down":      "down reactions by owners in your trust circle",
	"item.replies":          "comments by other owners",
	"item.hidden":           "hidden by the community's moderators or by members' reports",
	"item.reports":          "members' open reports",
	"item.has_card":         "carries a typed card (vote, job, invitation…)",
	"author.trust_distance": "steps from you in your trust graph (0 you, 1 your contacts, … 9 outside)",
	"author.agent":          "the author is an agent, not a person",
	"author.citizen":        "the author is a verified citizen (human mode)",
	"author.age_days":       "days since the author's name was registered",
	"author.posts_hour":     "posts by the author's owner in this community in the last hour",
}

// Ranker: a compiled module.
type Ranker struct {
	M      RankerManifest
	Hash   string // the manifest's hash (what was signed and installed)
	filter *Expr
	score  *Expr
	reason *ReasonTemplate
	inputs map[string]bool
}

// CompileRanker checks a manifest: the schema, the engine, and that the
// expressions read only declared inputs and params.
func CompileRanker(raw []byte) (*Ranker, error) {
	if err := ValidateJSON("tern.ranker/1", "", raw); err != nil {
		return nil, err
	}
	var m RankerManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	r := &Ranker{M: m, inputs: map[string]bool{}}
	for _, in := range m.Inputs {
		if _, ok := FeedInputs[in]; !ok {
			return nil, fmt.Errorf("ranker %s: unknown input %s (the feed offers %s)", m.Name, in, strings.Join(keys(FeedInputs), ", "))
		}
		r.inputs[in] = true
	}
	var err error
	if r.score, err = Parse(m.Score); err != nil {
		return nil, fmt.Errorf("ranker %s: score: %w", m.Name, err)
	}
	if m.Filter != "" {
		if r.filter, err = Parse(m.Filter); err != nil {
			return nil, fmt.Errorf("ranker %s: filter: %w", m.Name, err)
		}
	}
	if r.reason, err = ParseReason(m.Reason); err != nil {
		return nil, fmt.Errorf("ranker %s: %w", m.Name, err)
	}
	vars := r.score.Vars()
	if r.filter != nil {
		vars = append(vars, r.filter.Vars()...)
	}
	vars = append(vars, r.reason.Vars()...)
	for _, v := range vars {
		if p, ok := strings.CutPrefix(v, "params."); ok {
			if _, ok := m.Params[p]; !ok {
				return nil, fmt.Errorf("ranker %s reads %s, which its params don't set", m.Name, v)
			}
			continue
		}
		if !r.inputs[v] {
			return nil, fmt.Errorf("ranker %s reads %s, which it doesn't declare in inputs", m.Name, v)
		}
	}
	return r, nil
}

func (r *Ranker) maxCost() int {
	if r.M.Limits != nil && r.M.Limits.MaxCost > 0 {
		return r.M.Limits.MaxCost
	}
	return 10000
}

// Candidate: one post as the client knows it (all inputs; the module sees only its own).
type Candidate struct {
	ID     string
	Inputs map[string]any
}

// Ranked: a post's place, score and the module's "why".
type Ranked struct {
	ID     string  `json:"id"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

// Rank runs the module over the candidates: filter, score, reason; ties by id
// (deterministic: anyone can check a claimed ranking). Dropped: filtered out.
func (r *Ranker) Rank(cs []Candidate) (out []Ranked, dropped int, err error) {
	for _, c := range cs {
		env := Env{}
		for k := range r.inputs {
			v, ok := c.Inputs[k]
			if !ok {
				return nil, 0, fmt.Errorf("the client has no %s for %s", k, c.ID)
			}
			env[k] = v
		}
		for k, v := range r.M.Params {
			env["params."+k] = v
		}
		if r.filter != nil {
			v, err := r.filter.Eval(env, r.maxCost())
			if err != nil {
				return nil, 0, fmt.Errorf("%s filter: %w", r.M.Name, err)
			}
			if keep, err := truthy(v); err != nil || !keep {
				dropped++
				continue
			}
		}
		v, err := r.score.Eval(env, r.maxCost())
		if err != nil {
			return nil, 0, fmt.Errorf("%s score: %w", r.M.Name, err)
		}
		s, err := toNum(v)
		if err != nil || math.IsNaN(s) || math.IsInf(s, 0) {
			s = 0
		}
		why, err := r.reason.Render(env, r.maxCost())
		if err != nil {
			return nil, 0, fmt.Errorf("%s reason: %w", r.M.Name, err)
		}
		out = append(out, Ranked{ID: c.ID, Score: math.Round(s*1e6) / 1e6, Reason: why})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out, dropped, nil
}
