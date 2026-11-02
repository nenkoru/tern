// Command scanner shows what is happening on a tern network: names and their
// agents, cards, payments, deals and how they ended, swaps, claims and the
// volume of mail, read from the chain's public events (internal/scan). It
// only reads: no key, no transactions. See README.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/nenkoru/tern/internal/scan"
)

const usage = `scanner [COMMAND] [flags]

  (none)        the summary
  names         every name: owners, their agents, cards
  name NAME     one name: its agents, card, payments, deals, claims
  deals         every lock (escrow, bond, swap, subscription…) and how it ended
  payments      every payment
  swaps         swaps, with the price when one side is money
  claims        claims (reviews, ratings, rulings…)
  mail          envelopes and bytes per day (mail is sealed: only its volume shows)
  serve         a web page with all of it, scanned again every --every

flags:
  --net FILE        the network file (default $TERNCHAIN_NET, ~/tern/net.json, or networks/tern-base-sepolia-1/net.json)
  --rpc URL         another RPC than the network file's
  --json            the same as JSON (with no command: everything scanned)
  --offline         show the cache, don't read the chain
  --cache DIR       where the scan is kept (default the user cache directory, ternscan/)
  --confirmations N stay N blocks behind the head (default 3)
  --limit N         at most N rows, newest first (default 50; 0: all)
  --listen ADDR     serve: where (default 127.0.0.1:8080)
  --every D         serve: how often to read new blocks (default 1m)
`

type opts struct {
	net, rpc, cache   string
	json, offline     bool
	confirmations     uint64
	limit             int
	listen            string
	every             time.Duration
	command, argument string
}

func main() {
	o, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprint(os.Stderr, err, "\n\n", usage)
		os.Exit(2)
	}
	if err := run(o, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "scanner:", err)
		os.Exit(1)
	}
}

// parseArgs takes flags before or after the command.
func parseArgs(args []string) (opts, error) {
	var o opts
	fs := flag.NewFlagSet("scanner", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.net, "net", "", "")
	fs.StringVar(&o.rpc, "rpc", "", "")
	fs.StringVar(&o.cache, "cache", "", "")
	fs.BoolVar(&o.json, "json", false, "")
	fs.BoolVar(&o.offline, "offline", false, "")
	fs.Uint64Var(&o.confirmations, "confirmations", 3, "")
	fs.IntVar(&o.limit, "limit", 50, "")
	fs.StringVar(&o.listen, "listen", "127.0.0.1:8080", "")
	fs.DurationVar(&o.every, "every", time.Minute, "")
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if err == flag.ErrHelp {
				fmt.Print(usage)
				os.Exit(0)
			}
			return o, err
		}
		if fs.NArg() == 0 {
			break
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
	if len(pos) > 0 {
		o.command = pos[0]
	}
	if len(pos) > 1 {
		o.argument = pos[1]
	}
	switch o.command {
	case "", "names", "deals", "payments", "swaps", "claims", "mail", "serve":
		if len(pos) > 1 {
			return o, fmt.Errorf("%s takes no argument", o.command)
		}
	case "name":
		if len(pos) != 2 {
			return o, fmt.Errorf("name takes one NAME")
		}
	case "help":
		fmt.Print(usage)
		os.Exit(0)
	default:
		return o, fmt.Errorf("unknown command %q", o.command)
	}
	return o, nil
}

func netPath(flagged string) (string, error) {
	if flagged != "" {
		return flagged, nil
	}
	if p := os.Getenv("TERNCHAIN_NET"); p != "" {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		if p := filepath.Join(home, "tern", "net.json"); exists(p) {
			return p, nil
		}
	}
	if p := filepath.Join("networks", "tern-base-sepolia-1", "net.json"); exists(p) {
		return p, nil
	}
	return "", scan.ErrNoNet
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func run(o opts, w io.Writer) error {
	p, err := netPath(o.net)
	if err != nil {
		return err
	}
	n, err := scan.ReadNet(p)
	if err != nil {
		return err
	}
	if o.rpc != "" {
		n.RPC = o.rpc
	}
	dir := o.cache
	if dir == "" {
		if dir, err = os.UserCacheDir(); err != nil {
			return err
		}
		dir = filepath.Join(dir, "ternscan")
	}
	cache := filepath.Join(dir, fmt.Sprintf("%s-%d.json", orDefault(n.Network, "network"), n.ChainID))
	s := scan.Load(cache, n)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var stale error // serve: a failed first update, shown on the page while the cache is served
	if !o.offline {
		if err := update(ctx, s, n, cache, o.confirmations); err != nil {
			if o.command != "serve" || s.ToTime == 0 || ctx.Err() != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "scanner: update:", err, "(serving the cache; trying again every", o.every.String()+")")
			stale = err
		}
	} else if s.ToTime == 0 {
		return fmt.Errorf("nothing cached for %s yet: run once without --offline", n.Network)
	}
	if o.command == "serve" {
		return serve(ctx, s, n, cache, o, stale)
	}
	return show(w, s, o)
}

func update(ctx context.Context, s *scan.State, n scan.Net, cache string, confirmations uint64) error {
	if n.RPC == "" {
		return fmt.Errorf("the network file names no rpc: pass --rpc")
	}
	c, err := ethclient.DialContext(ctx, n.RPC)
	if err != nil {
		return err
	}
	defer c.Close()
	start, shown := time.Now(), false
	err = s.Update(ctx, c, n, scan.Options{Confirmations: confirmations,
		Save: func(s *scan.State) error { return s.Save(cache) },
		Progress: func(from, to, head uint64) {
			if time.Since(start) > 2*time.Second { // only for a scan that takes a while (the first one)
				fmt.Fprintf(os.Stderr, "\rscanning block %d of %d (%d%%)   ", to, head, 100*(to-n.Block+1)/(head-n.Block+1))
				shown = true
			}
		}})
	if shown {
		fmt.Fprintln(os.Stderr)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("interrupted at block %d: the next run goes on from there", s.ToBlock)
	}
	return err
}

func show(w io.Writer, s *scan.State, o opts) error {
	switch o.command {
	case "":
		if o.json {
			return writeJSON(w, s)
		}
		summary(w, s)
	case "names":
		if o.json {
			return writeJSON(w, s.Names)
		}
		names(w, s, sortedNames(s))
	case "name":
		return oneName(w, s, o)
	case "deals":
		ls := newest(s.Locks, o.limit)
		if o.json {
			return writeJSON(w, ls)
		}
		deals(w, s, ls)
	case "payments":
		ps := newest(s.Payments, o.limit)
		if o.json {
			return writeJSON(w, ps)
		}
		payments(w, s, ps)
	case "swaps":
		rows := swapRows(s, newest(s.Swaps, o.limit))
		if o.json {
			return writeJSON(w, rows)
		}
		swaps(w, rows)
	case "claims":
		cs := newest(s.Claims, o.limit)
		if o.json {
			return writeJSON(w, cs)
		}
		claims(w, cs)
	case "mail":
		if o.json {
			return writeJSON(w, s.Mail)
		}
		mail(w, s)
	}
	return nil
}

// ---- the views

func summary(w io.Writer, s *scan.State) {
	fmt.Fprintf(w, "%s · blocks %d–%d · up to %s UTC\n\n", s.Network, s.FromBlock, s.ToBlock, time.Unix(s.ToTime, 0).UTC().Format("2006-01-02 15:04"))
	var owners, agents, revoked, cards int
	for _, n := range s.Names {
		if n.Kind == "owner" {
			owners++
		} else {
			agents++
			if n.Revoked {
				revoked++
			}
		}
		if n.Card != "" {
			cards++
		}
	}
	t := tab(w)
	fmt.Fprintf(t, "Names\t%d owners · %d agents (%d revoked) · %d cards\n", owners, agents, revoked, cards)
	sums := map[string]*big.Int{}
	for _, p := range s.Payments {
		add(sums, p.Token, p.AmountRaw)
	}
	fmt.Fprintf(t, "Payments\t%d · %s\n", len(s.Payments), totals(s, sums))
	status, kinds := map[string]int{}, map[string]int{}
	for _, l := range s.Locks {
		status[l.Status]++
		kinds[l.Kind]++
	}
	fmt.Fprintf(t, "Deals\t%d open · %d past their refund time · %d paid · %d refunded · %d split\n",
		status["open"]+status["partly paid"], status["refundable"], status["paid"], status["refunded"], status["split"])
	if len(kinds) > 0 {
		fmt.Fprintf(t, "\tby kind: %s\n", counts(kinds))
	}
	fmt.Fprintf(t, "Swaps\t%d\n", len(s.Swaps))
	ck := map[string]int{}
	for _, c := range s.Claims {
		ck[c.Kind]++
	}
	fmt.Fprintf(t, "Claims\t%d", len(s.Claims))
	if len(ck) > 0 {
		fmt.Fprintf(t, " · %s", counts(ck))
	}
	fmt.Fprintln(t)
	var env, bytes uint64
	for _, m := range s.Mail {
		env += m.Envelopes
		bytes += m.Bytes
	}
	fmt.Fprintf(t, "Mail\t%d envelopes · %s · last 14 days %s\n", env, size(bytes), spark(s, 14))
	t.Flush()
}

func names(w io.Writer, s *scan.State, ns []*scan.Name) {
	t := tab(w)
	fmt.Fprintln(t, "NAME\tKIND\tSINCE\tAGENTS / LIMIT\tCARD")
	for _, n := range ns {
		extra := strings.Join(n.Agents, ", ")
		if n.Kind == "agent" {
			extra = limits(s, n)
			if n.Revoked {
				extra = "revoked"
			}
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", n.Name, n.Kind, scan.Day(n.Since), extra, cardLine(n.Card))
	}
	t.Flush()
}

func oneName(w io.Writer, s *scan.State, o opts) error {
	n := s.Names[o.argument]
	if n == nil {
		return fmt.Errorf("no name %q on %s (as of block %d)", o.argument, s.Network, s.ToBlock)
	}
	me := func(x string) bool { return x == n.Name }
	var ps []*scan.Payment
	for _, p := range s.Payments {
		if me(p.From) || me(p.To) {
			ps = append(ps, p)
		}
	}
	var ls []*scan.Lock
	for _, l := range s.Locks {
		if me(l.From) || me(l.Payee) || me(l.Decider) {
			ls = append(ls, l)
		}
	}
	var cs []*scan.Claim
	for _, c := range s.Claims {
		if me(c.Attester) || me(c.Subject) {
			cs = append(cs, c)
		}
	}
	ps, ls, cs = newest(ps, o.limit), newest(ls, o.limit), newest(cs, o.limit)
	if o.json {
		return writeJSON(w, map[string]any{"name": n, "payments": ps, "deals": ls, "claims": cs})
	}
	fmt.Fprintf(w, "%s · %s since %s (block %d)\n", n.Name, n.Kind, scan.Day(n.Since), n.Block)
	if n.Owner != "" {
		fmt.Fprintf(w, "owner: %s · limit: %s", n.Owner, limits(s, n))
		if n.Revoked {
			fmt.Fprint(w, " · revoked")
		}
		fmt.Fprintln(w)
	}
	if len(n.Agents) > 0 {
		fmt.Fprintf(w, "agents: %s\n", strings.Join(n.Agents, ", "))
	}
	if n.Card != "" {
		fmt.Fprintf(w, "card: %s\n", n.Card)
	}
	section := func(title string, k int, f func()) {
		fmt.Fprintf(w, "\n%s (%d)\n", title, k)
		if k > 0 {
			f()
		}
	}
	section("Payments", len(ps), func() { payments(w, s, ps) })
	section("Deals", len(ls), func() { deals(w, s, ls) })
	section("Claims", len(cs), func() { claims(w, cs) })
	return nil
}

func deals(w io.Writer, s *scan.State, ls []*scan.Lock) {
	t := tab(w)
	fmt.Fprintln(t, "LOCK\tOPENED\tKIND\tFROM → TO\tAMOUNT\tDECIDER\tSTATUS")
	for _, l := range ls {
		to := orDefault(l.Payee, "(open)")
		st := l.Status
		switch st {
		case "split", "partly paid":
			st += fmt.Sprintf(" (%s paid, %s back)", l.Paid, l.Refunded)
		case "open":
			st += " until " + scan.Day(l.RefundAfter)
		}
		fmt.Fprintf(t, "#%d\t%s\t%s\t%s → %s\t%s\t%s\t%s\n", l.ID, scan.Day(l.Time), l.Kind, l.From, to, lockAmount(s, l),
			orDefault(l.Decider, "-"), st)
	}
	t.Flush()
}

func payments(w io.Writer, s *scan.State, ps []*scan.Payment) {
	t := tab(w)
	fmt.Fprintln(t, "DAY\tFROM → TO\tAMOUNT")
	for _, p := range ps {
		fmt.Fprintf(t, "%s\t%s → %s\t%s %s\n", scan.Day(p.Time), p.From, p.To, p.Amount, s.Symbol(p.Token))
	}
	t.Flush()
}

type swapRow struct {
	Day    string `json:"day"`
	Offer  uint64 `json:"offer_lock"`
	Answer uint64 `json:"answer_lock"`
	A      string `json:"a"` // name: what
	B      string `json:"b"`
	Price  string `json:"price,omitempty"` // of one unit of the side that isn't money
}

func swapRows(s *scan.State, sw []*scan.Swap) []swapRow {
	byID := map[uint64]*scan.Lock{}
	for _, l := range s.Locks {
		byID[l.ID] = l
	}
	var out []swapRow
	for _, x := range sw {
		a, b := byID[x.Offer], byID[x.Answer]
		r := swapRow{Day: scan.Day(x.Time), Offer: x.Offer, Answer: x.Answer}
		if a == nil || b == nil {
			out = append(out, r)
			continue
		}
		r.A, r.B = a.From+": "+lockAmount(s, a), b.From+": "+lockAmount(s, b)
		// The price: money per unit of the other side. An item is never the
		// money; between two tokens, the answer (the buyer's side) is.
		goods, money := a, b
		if a.Item == "" && b.Item != "" {
			goods, money = b, a
		}
		if money.Item == "" && goods.Total != "0" && (goods.Item != "" || goods.Token != money.Token) {
			r.Price = ratio(money.Total, goods.Total) + " " + s.Symbol(money.Token) + " per " + unit(s, goods)
		}
		out = append(out, r)
	}
	return out
}

func swaps(w io.Writer, rows []swapRow) {
	t := tab(w)
	fmt.Fprintln(t, "DAY\tLOCKS\tGIVES\tGIVES\tPRICE")
	for _, r := range rows {
		fmt.Fprintf(t, "%s\t#%d ↔ #%d\t%s\t%s\t%s\n", r.Day, r.Offer, r.Answer, r.A, r.B, orDefault(r.Price, "-"))
	}
	t.Flush()
}

func claims(w io.Writer, cs []*scan.Claim) {
	t := tab(w)
	fmt.Fprintln(t, "DAY\tBY\tABOUT\tKIND\tSCORE\tDATA")
	for _, c := range cs {
		data := strings.ReplaceAll(c.Data, "\n", " ")
		if c.Revoked {
			data = "(revoked) " + data
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%d\t%s\n", scan.Day(c.Time), c.Attester, c.Subject, c.Kind, c.Score, clip(data, 60))
	}
	t.Flush()
}

func mail(w io.Writer, s *scan.State) {
	days := make([]string, 0, len(s.Mail))
	for d := range s.Mail {
		days = append(days, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	t := tab(w)
	fmt.Fprintln(t, "DAY\tENVELOPES\tBYTES")
	for _, d := range days {
		fmt.Fprintf(t, "%s\t%d\t%s\n", d, s.Mail[d].Envelopes, size(s.Mail[d].Bytes))
	}
	t.Flush()
}

// ---- helpers

func tab(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

func writeJSON(w io.Writer, v any) error {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(v)
}

// newest: the last n of a list in chain order, newest first (n 0: all).
func newest[T any](xs []T, n int) []T {
	out := make([]T, 0, len(xs))
	for i := len(xs) - 1; i >= 0 && (n <= 0 || len(out) < n); i-- {
		out = append(out, xs[i])
	}
	return out
}

func sortedNames(s *scan.State) []*scan.Name {
	// Owners in the order they came, each followed by its agents.
	var owners []*scan.Name
	for _, n := range s.Names {
		if n.Kind == "owner" {
			owners = append(owners, n)
		}
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i].Block < owners[j].Block })
	var out []*scan.Name
	for _, o := range owners {
		out = append(out, o)
		for _, a := range o.Agents {
			if n := s.Names[a]; n != nil {
				out = append(out, n)
			}
		}
	}
	return out
}

func limits(s *scan.State, n *scan.Name) string {
	var parts []string
	for tok, raw := range n.Limits {
		parts = append(parts, s.Fmt(tok, raw)+" "+s.Symbol(tok)+"/day")
	}
	sort.Strings(parts)
	return orDefault(strings.Join(parts, ", "), "-")
}

// cardLine: a card's "offer", or its first characters.
func cardLine(card string) string {
	var m map[string]any
	if json.Unmarshal([]byte(card), &m) == nil {
		if o, ok := m["offer"].(string); ok {
			return clip(o, 50)
		}
	}
	return clip(card, 50)
}

func lockAmount(s *scan.State, l *scan.Lock) string {
	if l.Item != "" {
		return l.Total + " × " + unit(s, l)
	}
	return l.Total + " " + s.Symbol(l.Token)
}

func unit(s *scan.State, l *scan.Lock) string {
	if l.Item != "" {
		return "item " + l.Item + " of " + s.Symbol(l.Token)
	}
	return s.Symbol(l.Token)
}

func ratio(a, b string) string {
	x, ok1 := new(big.Rat).SetString(a)
	y, ok2 := new(big.Rat).SetString(b)
	if !ok1 || !ok2 || y.Sign() == 0 {
		return "?"
	}
	out := new(big.Rat).Quo(x, y).FloatString(6)
	return strings.TrimRight(strings.TrimRight(out, "0"), ".")
}

func add(m map[string]*big.Int, token, raw string) {
	v, _ := new(big.Int).SetString(raw, 10)
	if v == nil {
		return
	}
	if m[token] == nil {
		m[token] = new(big.Int)
	}
	m[token].Add(m[token], v)
}

func totals(s *scan.State, m map[string]*big.Int) string {
	var parts []string
	for tok, v := range m {
		parts = append(parts, s.Fmt(tok, v.String())+" "+s.Symbol(tok))
	}
	sort.Strings(parts)
	return orDefault(strings.Join(parts, " + "), "nothing")
}

func counts(m map[string]int) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return m[ks[i]] > m[ks[j]] || (m[ks[i]] == m[ks[j]] && ks[i] < ks[j]) })
	var parts []string
	for i, k := range ks {
		if i == 6 {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// spark: envelopes per day for the last n days up to the scan's end.
func spark(s *scan.State, n int) string {
	bars := []rune("▁▂▃▄▅▆▇█")
	end := time.Unix(s.ToTime, 0).UTC()
	vals := make([]uint64, n)
	var top uint64
	for i := range vals {
		if m := s.Mail[end.AddDate(0, 0, i-n+1).Format("2006-01-02")]; m != nil {
			vals[i] = m.Envelopes
		}
		top = max(top, vals[i])
	}
	var b strings.Builder
	for _, v := range vals {
		if v == 0 {
			b.WriteRune('·')
			continue
		}
		b.WriteRune(bars[int(v*uint64(len(bars)-1)/max(top, 1))])
	}
	return b.String()
}

func size(b uint64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
