//go:build ops

// The scale simulation's results: rating quality at the end of the days, and
// ROOT/results.{json,md} from everything measured (scale_sim.go).
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ratingQuality: does a good service rise? One honest reader looks every
// service up, once for everyone's reviews and once for reviews by citizens only
// (people vouched in as humans: the fake reviewers are not among them).
func (s *scaleSim) ratingQuality() {
	reader := ""
	for _, p := range s.w.People {
		if p.Founder && p.Abuse == "" {
			reader = p.Name
			break
		}
	}
	trust := map[string]bool{}
	for _, n := range s.w.Citizens {
		trust[n] = true
	}
	type row struct {
		Service string  `json:"service"`
		Quality float64 `json:"quality"`
		Crony   bool    `json:"crony,omitempty"`
		All     float64 `json:"score_all"`
		NAll    int     `json:"reviews_all"`
		Trusted float64 `json:"score_trusted"`
		NTr     int     `json:"reviews_trusted"`
		Deals   int     `json:"deals"`
	}
	var rows []row
	st := s.state()
	s.parN(len(s.w.Services), 8, func(i int) {
		v := s.w.Services[i]
		r := row{Service: v.Agent, Quality: v.Quality, Crony: v.Crony, All: math.NaN(), Trusted: math.NaN()}
		st.mu.Lock()
		r.Deals = st.dealsBy[v.Agent]
		st.mu.Unlock()
		sc := s.client(reader)
		var m1, m2 map[string]any
		s.m.time("rating.final", func() error {
			var err error
			m1, err = sc.c.rating(v.Agent, ratingOpts{})
			if err != nil {
				return err
			}
			m2, err = sc.c.rating(v.Agent, ratingOpts{trust: trust})
			return err
		})
		if f, ok := m1["score"].(float64); ok {
			r.All = f
		}
		if n, ok := m1["reviews_counted"].(int); ok {
			r.NAll = n
		}
		if f, ok := m2["score"].(float64); ok {
			r.Trusted = f
		}
		if n, ok := m2["reviews_counted"].(int); ok {
			r.NTr = n
		}
		st.mu.Lock()
		rows = append(rows, r)
		st.mu.Unlock()
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].Service < rows[j].Service })
	sp := func(get func(row) (float64, bool)) (float64, int) {
		var xs, ys []float64
		for _, r := range rows {
			if y, ok := get(r); ok {
				xs, ys = append(xs, r.Quality), append(ys, y)
			}
		}
		return spearman(xs, ys), len(xs)
	}
	rhoAll, nAll := sp(func(r row) (float64, bool) { return r.All, r.NAll > 0 && !math.IsNaN(r.All) })
	rhoTr, nTr := sp(func(r row) (float64, bool) { return r.Trusted, r.NTr > 0 && !math.IsNaN(r.Trusted) })
	rhoDeals, nD := sp(func(r row) (float64, bool) { return float64(r.Deals), true })
	// where the crony services rank among the rated ones (1 = top)
	rank := func(get func(row) float64, ok func(row) bool) []int {
		var rated []row
		for _, r := range rows {
			if ok(r) {
				rated = append(rated, r)
			}
		}
		sort.Slice(rated, func(i, j int) bool { return get(rated[i]) > get(rated[j]) })
		var out []int
		for i, r := range rated {
			if r.Crony {
				out = append(out, i+1)
			}
		}
		return out
	}
	cAll := rank(func(r row) float64 { return r.All }, func(r row) bool { return r.NAll > 0 && !math.IsNaN(r.All) })
	cTr := rank(func(r row) float64 { return r.Trusted }, func(r row) bool { return r.NTr > 0 && !math.IsNaN(r.Trusted) })
	// top 10 by rating: their mean true quality, against the mean of all
	topQ := func(get func(row) float64, ok func(row) bool) float64 {
		var rated []row
		for _, r := range rows {
			if ok(r) {
				rated = append(rated, r)
			}
		}
		sort.Slice(rated, func(i, j int) bool { return get(rated[i]) > get(rated[j]) })
		var q []float64
		for i := 0; i < len(rated) && i < 10; i++ {
			q = append(q, rated[i].Quality)
		}
		return mean(q)
	}
	var allQ []float64
	for _, r := range rows {
		allQ = append(allQ, r.Quality)
	}
	out := map[string]any{"reader": reader, "trusted_reviewers": len(trust), "spearman_all": r3(rhoAll), "rated_all": nAll, "spearman_trusted": r3(rhoTr),
		"rated_trusted": nTr, "spearman_quality_deals": r3(rhoDeals), "services": nD, "crony_rank_all": cAll, "crony_rank_trusted": cTr,
		"top10_quality_all":     r3(topQ(func(r row) float64 { return r.All }, func(r row) bool { return r.NAll > 0 && !math.IsNaN(r.All) })),
		"top10_quality_trusted": r3(topQ(func(r row) float64 { return r.Trusted }, func(r row) bool { return r.NTr > 0 && !math.IsNaN(r.Trusted) })),
		"mean_quality":          r3(mean(allQ)), "rows": rows}
	for i := range rows {
		if math.IsNaN(rows[i].All) {
			rows[i].All = -1000
		}
		if math.IsNaN(rows[i].Trusted) {
			rows[i].Trusted = -1000
		}
	}
	out["rows"] = rows
	for k, v := range out {
		if f, ok := v.(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
			out[k] = nil
		}
	}
	b, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		log.Printf("ratings: %v", err)
	}
	os.MkdirAll(filepath.Join(s.cfg.root, "metrics"), 0o755)
	os.WriteFile(filepath.Join(s.cfg.root, "metrics", "ratings.json"), b, 0o644)
}

func ranks(xs []float64) []float64 {
	idx := make([]int, len(xs))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return xs[idx[a]] < xs[idx[b]] })
	r := make([]float64, len(xs))
	for i := 0; i < len(idx); {
		j := i
		for j+1 < len(idx) && xs[idx[j+1]] == xs[idx[i]] {
			j++
		}
		for k := i; k <= j; k++ {
			r[idx[k]] = float64(i+j)/2 + 1
		}
		i = j + 1
	}
	return r
}

func spearman(xs, ys []float64) float64 {
	if len(xs) < 3 {
		return math.NaN()
	}
	rx, ry := ranks(xs), ranks(ys)
	mx, my := mean(rx), mean(ry)
	var sxy, sxx, syy float64
	for i := range rx {
		sxy += (rx[i] - mx) * (ry[i] - my)
		sxx += (rx[i] - mx) * (rx[i] - mx)
		syy += (ry[i] - my) * (ry[i] - my)
	}
	if sxx == 0 || syy == 0 {
		return math.NaN()
	}
	return sxy / math.Sqrt(sxx*syy)
}

// ---- results.json / results.md

type metricsFile struct {
	Ops      []opSummary           `json:"ops"`
	Counters map[string]int        `json:"counters"`
	Samples  map[string][]float64  `json:"samples"`
	Blocks   []blockRec            `json:"blocks"`
	GasBy    map[string][2]float64 `json:"gas_by_method"`
	Reverted map[string]int        `json:"reverted_by_method"`
	RPC      int64                 `json:"rpc_requests"`
	RPCBy    map[string]int64      `json:"rpc_by_method"`
	NodeReq  map[string]int        `json:"node_requests"`
	NodeB    map[string]int64      `json:"node_bytes"`
	NodeFail map[string]int        `json:"node_failures"`
	Days     []dayReport           `json:"days"`
	Cfg      map[string]any        `json:"cfg"`
	Wall     float64               `json:"wall_s"`
}

func (s *scaleSim) writeReport() error {
	dir := filepath.Join(s.cfg.root, "metrics")
	var m metricsFile
	for _, n := range []string{"final", "stress"} {
		if readJSON(filepath.Join(dir, n+".json"), &m) == nil {
			break
		}
	}
	if m.Ops == nil {
		ms, _ := filepath.Glob(filepath.Join(dir, "day*.json"))
		sort.Strings(ms)
		if len(ms) == 0 {
			return fmt.Errorf("nothing measured in %s", dir)
		}
		readJSON(ms[len(ms)-1], &m)
	}
	var stress []map[string]any
	readJSON(filepath.Join(dir, "stress.json"), &stress)
	var ratings map[string]any
	readJSON(filepath.Join(dir, "ratings.json"), &ratings)
	var w scaleWorld
	readJSON(filepath.Join(s.cfg.root, "world.json"), &w)
	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	p("# Scale simulation: measured results")
	p("")
	p("Config: %v", m.Cfg)
	p("")
	p("## Days")
	p("")
	p("| day | wall s | mail | posts+reactions | vouchers | deals done | txs | tx/s | blocks | Mgas | max fill | full blocks | base fee max gwei | chain MB | nodes MB | ids MB | RPC | sim RSS MB |")
	p("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
	for _, d := range m.Days {
		c := d.Counters
		p("| %d | %.0f | %d | %d | %d | %d | %d | %.1f | %d | %.0f | %.2f | %d | %.3f | %.0f | %.0f | %.0f | %d | %.0f |", d.Day, d.WallS, c["mail.sent"], c["post.sent"]+c["reaction.sent"],
			c["voucher.sent"], c["deal.done"], d.Txs, float64(d.Txs)/math.Max(d.WallS, 1), d.Blocks, d.GasUsed/1e6, d.MaxFill, d.FullBlocks, d.BaseFeeMax,
			d.ChainMB, d.NodesMB, d.IdsMB, d.RPC, d.SimRSSMB)
	}
	p("")
	p("Processes per day (CPU seconds / max RSS MB):")
	p("")
	for _, d := range m.Days {
		var ks []string
		for k := range d.Procs {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		var parts []string
		for _, k := range ks {
			parts = append(parts, fmt.Sprintf("%s %.0f s / %.0f MB", k, d.Procs[k][0], d.Procs[k][1]))
		}
		p("- day %d: %s", d.Day, strings.Join(parts, "; "))
	}
	p("")
	p("## Operations (all phases)")
	p("")
	p("| op | ok | failed | fail rate | p50 s | p95 s | p99 s | max s | top error |")
	p("|---|---|---|---|---|---|---|---|---|")
	for _, o := range m.Ops {
		top, tn := "", 0
		for e, n := range o.Errors {
			if n > tn {
				top, tn = e, n
			}
		}
		if len(top) > 80 {
			top = top[:80]
		}
		top = strings.ReplaceAll(top, "|", "/")
		p("| %s | %d | %d | %.3f | %.3f | %.3f | %.3f | %.3f | %s |", o.Op, o.OK, o.Failed, o.FailRate, o.P50, o.P95, o.P99, o.Max, top)
	}
	p("")
	p("## Samples")
	p("")
	var sk []string
	for k := range m.Samples {
		sk = append(sk, k)
	}
	sort.Strings(sk)
	for _, k := range sk {
		v := m.Samples[k]
		p("- %s: n %d, p50 %.2f, p95 %.2f, p99 %.2f, max %.2f", k, len(v), pct(v, 50), pct(v, 95), pct(v, 99), pct(v, 100))
	}
	p("")
	p("## Counters")
	p("")
	var ck []string
	for k := range m.Counters {
		ck = append(ck, k)
	}
	sort.Strings(ck)
	for _, k := range ck {
		p("- %s: %d", k, m.Counters[k])
	}
	p("")
	p("## Gas by method")
	p("")
	p("| method | txs | avg gas | total Mgas | reverted |")
	p("|---|---|---|---|---|")
	type gm struct {
		k    string
		n, g float64
	}
	var gs []gm
	tot := 0.0
	for k, v := range m.GasBy {
		gs = append(gs, gm{k, v[0], v[1]})
		tot += v[1]
	}
	sort.Slice(gs, func(i, j int) bool { return gs[i].g > gs[j].g })
	for _, g := range gs {
		p("| %s | %.0f | %.0f | %.1f | %d |", g.k, g.n, g.g/math.Max(g.n, 1), g.g/1e6, m.Reverted[g.k])
	}
	p("")
	p("Total gas %.0f M over %d blocks.", tot/1e6, len(m.Blocks))
	p("")
	p("## RPC and data nodes")
	p("")
	p("- RPC requests: %d", m.RPC)
	var rk []string
	for k := range m.RPCBy {
		rk = append(rk, k)
	}
	sort.Slice(rk, func(i, j int) bool { return m.RPCBy[rk[i]] > m.RPCBy[rk[j]] })
	for _, k := range rk {
		p("  - %s: %d", k, m.RPCBy[k])
	}
	p("- data node requests by path: %v", m.NodeReq)
	p("- data node response bytes by path: %v", m.NodeB)
	p("- data node failures: %v", m.NodeFail)
	p("")
	if len(stress) > 0 {
		p("## Stress")
		p("")
		p("| kind | target/s | sent | ok | achieved/s | blocks | fill | full blocks |")
		p("|---|---|---|---|---|---|---|---|")
		for _, r := range stress {
			p("| %v | %v | %v | %v | %v | %v | %v | %v |", r["kind"], r["target_per_s"], r["sent"], r["ok"], r["achieved_per_s"], r["blocks"], r["block_fill"], r["full_blocks"])
		}
		p("")
	}
	if ratings != nil {
		delete(ratings, "rows")
		rb, _ := json.MarshalIndent(ratings, "", " ")
		p("## Rating quality")
		p("")
		p("```")
		p("%s", rb)
		p("```")
	}
	os.WriteFile(filepath.Join(s.cfg.root, "results.md"), []byte(b.String()), 0o644)
	jb, _ := json.MarshalIndent(map[string]any{"days": m.Days, "ops": m.Ops, "counters": m.Counters, "gas_by_method": m.GasBy, "rpc_by_method": m.RPCBy,
		"node_requests": m.NodeReq, "node_bytes": m.NodeB, "stress": stress, "ratings": ratings, "cfg": m.Cfg, "setup_s": w.SetupSec}, "", " ")
	os.WriteFile(filepath.Join(s.cfg.root, "results.json"), jb, 0o644)
	fmt.Println("wrote", filepath.Join(s.cfg.root, "results.md"))
	return nil
}
