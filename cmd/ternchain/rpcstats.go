package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// TERNCHAIN_RPC_STATS=1: every JSON-RPC call this process makes over HTTP
// (the network's RPC, the bundler) counted by method, with the wall time spent
// in it, printed to stderr at exit. "logs" also breaks eth_getLogs down by
// contract and event. For measuring, not for every day: off, it costs nothing.
var rpcStatsMode = os.Getenv("TERNCHAIN_RPC_STATS")

// rpcStatsName: "Contract.method" for an eth_call's address and selector (main sets it).
var rpcStatsName func(to, selector string) string

type rpcStat struct {
	n, retries int
	d          time.Duration
}

var rpcStats = struct {
	sync.Mutex
	m     map[string]*rpcStat
	start time.Time
}{m: map[string]*rpcStat{}, start: time.Now()}

// rpcMethods: the methods (and, for eth_getLogs with mode "logs", the filter)
// of one request body, a single call or a batch.
func rpcMethods(body []byte) []string {
	type call struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	var calls []call
	if b := strings.TrimSpace(string(body)); strings.HasPrefix(b, "[") {
		json.Unmarshal(body, &calls)
	} else {
		var c call
		json.Unmarshal(body, &c)
		calls = []call{c}
	}
	var out []string
	for _, c := range calls {
		m := c.Method
		if m == "" {
			m = "?"
		}
		if m == "eth_call" && rpcStatsMode == "logs" && len(c.Params) >= 1 {
			var tx struct {
				To    string `json:"to"`
				Input string `json:"input"`
				Data  string `json:"data"`
			}
			json.Unmarshal(c.Params[0], &tx)
			in := tx.Input + tx.Data
			if len(in) >= 10 {
				in = in[:10]
			}
			name := tx.To + " " + in
			if rpcStatsName != nil {
				name = rpcStatsName(tx.To, in)
			}
			m += " " + name
		}
		if m == "eth_getLogs" && rpcStatsMode == "logs" && len(c.Params) == 1 {
			var q struct {
				Address any   `json:"address"`
				Topics  []any `json:"topics"`
			}
			json.Unmarshal(c.Params[0], &q)
			t0 := ""
			if len(q.Topics) > 0 {
				t0 = fmt.Sprint(q.Topics[0])
				if len(t0) > 14 {
					t0 = t0[:14]
				}
			}
			m += fmt.Sprintf(" %v %s", q.Address, t0)
		}
		out = append(out, m)
	}
	return out
}

// countRPC: one request's calls (a batch shares its time among them).
func countRPC(body []byte, d time.Duration, retries int) {
	ms := rpcMethods(body)
	if len(ms) == 0 {
		return
	}
	rpcStats.Lock()
	defer rpcStats.Unlock()
	for _, m := range ms {
		s := rpcStats.m[m]
		if s == nil {
			s = &rpcStat{}
			rpcStats.m[m] = s
		}
		s.n++
		s.retries += retries
		s.d += d / time.Duration(len(ms))
	}
}

// statsRPC: counts what goes through next (the bundler's transport).
type statsRPC struct{ next http.RoundTripper }

func (s statsRPC) RoundTrip(req *http.Request) (*http.Response, error) {
	body := reqBody(req)
	t := time.Now()
	resp, err := s.next.RoundTrip(req)
	countRPC(body, time.Since(t), 0)
	return resp, err
}

func reqBody(req *http.Request) []byte {
	if rpcStatsMode == "" || req.GetBody == nil {
		return nil
	}
	r, err := req.GetBody()
	if err != nil {
		return nil
	}
	b, _ := io.ReadAll(r)
	return b
}

// rpcHTTP: the HTTP client for JSON-RPC that isn't the network's RPC (the
// bundler): counted when measuring, no retries (a resent user operation is
// the bundler's to refuse, not ours to repeat).
func rpcHTTP() *http.Client {
	if rpcStatsMode == "" {
		return http.DefaultClient
	}
	return &http.Client{Transport: statsRPC{defaultTransport{}}}
}

// defaultTransport: http.DefaultTransport at call time (--proxy replaces its Proxy).
type defaultTransport struct{}

func (defaultTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return http.DefaultTransport.RoundTrip(req)
}

// printRPCStats: the table, to stderr, when TERNCHAIN_RPC_STATS is set.
func printRPCStats() {
	if rpcStatsMode == "" {
		return
	}
	rpcStats.Lock()
	defer rpcStats.Unlock()
	keys := make([]string, 0, len(rpcStats.m))
	n, retries := 0, 0
	var d time.Duration
	for k, s := range rpcStats.m {
		keys = append(keys, k)
		n, retries, d = n+s.n, retries+s.retries, d+s.d
	}
	sort.Slice(keys, func(i, j int) bool { return rpcStats.m[keys[i]].d > rpcStats.m[keys[j]].d })
	w := os.Stderr
	fmt.Fprintf(w, "rpc stats: %d calls, %.1f s in calls, %d retried, %.1f s wall\n", n, d.Seconds(), retries, time.Since(rpcStats.start).Seconds())
	for _, k := range keys {
		s := rpcStats.m[k]
		fmt.Fprintf(w, "  %6d  %8.2f s  %s", s.n, s.d.Seconds(), k)
		if s.retries > 0 {
			fmt.Fprintf(w, "  (%d retried)", s.retries)
		}
		fmt.Fprintln(w)
	}
}

// callName: an eth_call's contract and method, by the ABIs this client has.
func (ch *Chain) callName(to, sel string) string {
	for n, c := range ch.c {
		if !strings.EqualFold(c.Address().Hex(), to) {
			continue
		}
		for _, m := range ch.abis[n].Methods {
			if len(sel) == 10 && strings.EqualFold(fmt.Sprintf("0x%x", m.ID), sel) {
				return n + "." + m.Name
			}
		}
		return n + " " + sel
	}
	return to + " " + sel
}
