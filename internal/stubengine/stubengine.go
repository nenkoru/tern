// Package stubengine is a stand-in for a real split-model engine (llama.cpp's
// RPC servers, Petals, exo, ClusterChat's browsers): a model of Of pieces
// (layers) split across machines, each piece a deterministic step on a
// 32-byte state, so any two honest holders of the same pieces give the same
// answer and a checker can compare hashes. Tern carries none of this: it
// carries who is in, who is online with which pieces, the receipts and the
// credits. examples/cluster/stubengine is its command; the coop demo and
// its test drive it.
package stubengine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Step: piece i applied to a state.
func Step(state [32]byte, i int) [32]byte {
	var b [36]byte
	copy(b[:], state[:])
	b[32], b[33], b[34], b[35] = byte(i>>24), byte(i>>16), byte(i>>8), byte(i)
	return sha256.Sum256(b[:])
}

func Hex(b [32]byte) string { return "0x" + hex.EncodeToString(b[:]) }

func ParseState(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(b) != 32 {
		return out, errors.New("state: 0x and 32 bytes")
	}
	copy(out[:], b)
	return out, nil
}

func parsePieces(s string) (map[int]bool, error) {
	out := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		a, b, rng := strings.Cut(part, "-")
		lo, err1 := strconv.Atoi(a)
		hi, err2 := lo, error(nil)
		if rng {
			hi, err2 = strconv.Atoi(b)
		}
		if err1 != nil || err2 != nil || hi < lo {
			return nil, fmt.Errorf("pieces %q", s)
		}
		for i := lo; i <= hi; i++ {
			out[i] = true
		}
	}
	return out, nil
}

// ---- a node

// Node holds some pieces and runs segments through them. Lazy > 0 makes it
// dishonest: every Lazy-th job it sees, it claims the work and does none.
type Node struct {
	mu     sync.Mutex
	pieces map[int]bool
	of     int
	lazy   int
	jobs   map[string]bool // lazy jobs, by id
	seen   int
}

func NewNode(pieces string, of, lazy int) (*Node, error) {
	ps, err := parsePieces(pieces)
	if err != nil {
		return nil, err
	}
	return &Node{pieces: ps, of: of, lazy: lazy, jobs: map[string]bool{}}, nil
}

type fwdReq struct {
	Job   string `json:"job"`
	State string `json:"state"`
	From  int    `json:"from"`
	To    int    `json:"to"`
}

// Pass: one segment of one pass, as the node reports it.
type Pass struct {
	Input  string `json:"input"`
	Output string `json:"output"`
	Units  int    `json:"units"`
}

// Load: hold more pieces (someone else's laptop closed).
func (n *Node) Load(pieces string) error {
	ps, err := parsePieces(pieces)
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := range ps {
		if i < n.of {
			n.pieces[i] = true
		}
	}
	return nil
}

// Handler: POST /forward {job, state, from, to}; POST /load {pieces}; GET /info.
func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /forward", func(w http.ResponseWriter, r *http.Request) {
		var q fwdReq
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		st, err := ParseState(q.State)
		if err != nil || q.From < 0 || q.From > q.To || q.To >= n.of {
			http.Error(w, "bad segment", 400)
			return
		}
		n.mu.Lock()
		for i := q.From; i <= q.To; i++ {
			if !n.pieces[i] {
				n.mu.Unlock()
				http.Error(w, fmt.Sprintf("piece %d is not held here", i), 409)
				return
			}
		}
		lazy, ok := n.jobs[q.Job]
		if !ok {
			n.seen++
			lazy = n.lazy > 0 && n.seen%n.lazy == 0
			n.jobs[q.Job] = lazy
		}
		n.mu.Unlock()
		in := st
		if lazy {
			st = sha256.Sum256(append([]byte("lazy"), st[:]...))
		} else {
			for i := q.From; i <= q.To; i++ {
				st = Step(st, i)
			}
		}
		json.NewEncoder(w).Encode(Pass{Input: Hex(in), Output: Hex(st), Units: q.To - q.From + 1})
	})
	mux.HandleFunc("POST /load", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Pieces string `json:"pieces"`
		}
		json.NewDecoder(r.Body).Decode(&q)
		if err := n.Load(q.Pieces); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"of": n.of, "pieces": len(n.pieces), "jobs": len(n.jobs)})
	})
	return mux
}

// ---- a chat: route through the holders online

// Offer and Member: what a reader needs of `ternchain presence list` (the
// parts of a whole each member holds, and where its engine listens).
type Offer struct {
	Whole string `json:"whole"`
	Holds string `json:"holds"`
	Of    int    `json:"of"`
}

type Member struct {
	Name   string  `json:"name"`
	Load   float64 `json:"load"`
	Reach  string  `json:"reach"`
	Offers []Offer `json:"offers"`
}

// Segment: a run of pieces one member did for a chat.
type Segment struct {
	Name   string `json:"name"`
	Reach  string `json:"reach"`
	From   int    `json:"from"`
	To     int    `json:"to"`
	Units  int    `json:"units"`  // pieces × tokens: what this member did for the chat
	Passes []Pass `json:"passes"` // each pass through it: a spot check reruns one
}

// Route: for each piece in order, a holder online (not avoided, least
// loaded, then by name), staying with one holder while it holds the next.
func Route(online []Member, model string, avoid map[string]bool) ([]Segment, error) {
	of := 0
	holds := map[string]map[int]bool{}
	byName := map[string]Member{}
	for _, m := range online {
		if avoid[m.Name] || m.Reach == "" {
			continue
		}
		for _, c := range m.Offers {
			ps, err := parsePieces(c.Holds)
			if c.Whole != model || c.Of == 0 || err != nil {
				continue
			}
			of = c.Of
			if holds[m.Name] == nil {
				holds[m.Name] = map[int]bool{}
			}
			for i := range ps {
				holds[m.Name][i] = true
			}
			byName[m.Name] = m
		}
	}
	if of == 0 {
		return nil, fmt.Errorf("nobody online holds %s", model)
	}
	names := make([]string, 0, len(holds))
	for n := range holds {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if a, b := byName[names[i]].Load, byName[names[j]].Load; a != b {
			return a < b
		}
		return names[i] < names[j]
	})
	var segs []Segment
	for i := 0; i < of; i++ {
		if len(segs) > 0 && holds[segs[len(segs)-1].Name][i] {
			segs[len(segs)-1].To = i
			continue
		}
		found := false
		for _, n := range names {
			if holds[n][i] {
				segs = append(segs, Segment{Name: n, Reach: byName[n].Reach, From: i, To: i})
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("piece %d of %s: nobody online holds it (someone has to load it)", i, model)
		}
	}
	return segs, nil
}

var client = &http.Client{Timeout: 10 * time.Second}

// Forward: one segment at a node.
func Forward(reach, job string, state [32]byte, from, to int) (Pass, error) {
	var out Pass
	b, _ := json.Marshal(fwdReq{Job: job, State: Hex(state), From: from, To: to})
	resp, err := client.Post(strings.TrimRight(reach, "/")+"/forward", "application/json", bytes.NewReader(b))
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var msg bytes.Buffer
		msg.ReadFrom(resp.Body)
		return out, fmt.Errorf("%s: %s %s", reach, resp.Status, strings.TrimSpace(msg.String()))
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}

// LoadAt: ask the node at reach to hold more pieces.
func LoadAt(reach, pieces string) error {
	resp, err := client.Post(strings.TrimRight(reach, "/")+"/load", "application/json", strings.NewReader(fmt.Sprintf(`{"pieces":%q}`, pieces)))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", reach, resp.Status)
	}
	return nil
}

// Chat: what one chat did, and who did it.
type Chat struct {
	Job    string    `json:"job"`
	Model  string    `json:"model"`
	Input  string    `json:"input"`
	Output string    `json:"output"`
	Tokens []string  `json:"tokens"`
	Route  []Segment `json:"route"`
}

// Run: a chat of tokens passes through the members online.
func Run(online []Member, model, job, prompt string, tokens int, avoid map[string]bool) (Chat, error) {
	segs, err := Route(online, model, avoid)
	if err != nil {
		return Chat{}, err
	}
	input := sha256.Sum256([]byte(prompt))
	state := input
	ch := Chat{Job: job, Model: model, Input: Hex(input)}
	for t := 0; t < tokens; t++ {
		for i := range segs {
			p, err := Forward(segs[i].Reach, job, state, segs[i].From, segs[i].To)
			if err != nil {
				return ch, err
			}
			segs[i].Passes = append(segs[i].Passes, p)
			segs[i].Units += p.Units
			state, _ = ParseState(p.Output)
		}
		ch.Tokens = append(ch.Tokens, hex.EncodeToString(state[:2]))
	}
	ch.Output, ch.Route = Hex(state), segs
	return ch, nil
}
