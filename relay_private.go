// A relayer for private actions: it submits pool withdrawals (paid by a fee
// taken from the withdrawn amount, which the proof binds) and secret votes
// (free, rate limited), so neither the recipient nor the voter needs gas or
// shows an address. It learns nothing a chain observer doesn't, except the
// client's IP (use --proxy, e.g. Tor, to hide that too).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// poolRelayFee: what a relayer takes per withdrawal unless set (relay serve --pool-fee).
var poolRelayFee = new(big.Int).Div(coinUnit, big.NewInt(1000))

type poolRelayInfoT struct {
	Address common.Address `json:"address"`
	Fee     *big.Int       `json:"fee"`
}

type voteRequest struct {
	ID        uint64   `json:"id"`
	Yes       bool     `json:"yes"`
	Nullifier *big.Int `json:"nullifier"`
	Root      *big.Int `json:"root"` // the citizens' root the proof used (CitizenGroup.knownRoot)
	Proof     string   `json:"proof"`
}

func (r *Relay) privateRoutes(mux *http.ServeMux) {
	fee := func() *big.Int {
		if r.poolFee != nil {
			return r.poolFee
		}
		return poolRelayFee
	}
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /pool/info", func(w http.ResponseWriter, req *http.Request) {
		reply(w, poolRelayInfoT{Address: crypto.PubkeyToAddress(r.key.PublicKey), Fee: fee()})
	})
	mux.HandleFunc("POST /pool/withdraw", func(w http.ResponseWriter, req *http.Request) {
		if msg := r.admit(req, true); msg != "" {
			http.Error(w, msg, http.StatusTooManyRequests)
			return
		}
		var p poolRequest
		if err := json.NewDecoder(io.LimitReader(req.Body, 64<<10)).Decode(&p); err != nil || p.W.Fee == nil || p.W.AspId == nil {
			http.Error(w, "a withdrawal", 400)
			return
		}
		if p.W.Relayer != crypto.PubkeyToAddress(r.key.PublicKey) || p.W.Fee.Cmp(fee()) < 0 {
			http.Error(w, fmt.Sprintf("this relayer is %s and takes %s coins", crypto.PubkeyToAddress(r.key.PublicKey).Hex(), fmtCoins(fee())), http.StatusPaymentRequired)
			return
		}
		rc, err := r.ch.send(r.key, nil, "PrivacyPool", "withdraw", p.args()...) // estimated first: a bad proof costs us nothing
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		reply(w, map[string]string{"tx": rc.TxHash.Hex()})
	})
	mux.HandleFunc("POST /gov/vote", func(w http.ResponseWriter, req *http.Request) {
		if msg := r.admit(req, true); msg != "" {
			http.Error(w, msg, http.StatusTooManyRequests)
			return
		}
		var v voteRequest
		if err := json.NewDecoder(io.LimitReader(req.Body, 8<<10)).Decode(&v); err != nil || v.Nullifier == nil || v.Root == nil {
			http.Error(w, "a vote", 400)
			return
		}
		rc, err := r.ch.send(r.key, nil, "Parliament", "voteSecret", new(big.Int).SetUint64(v.ID), v.Yes, v.Nullifier, v.Root, common.FromHex(v.Proof))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		reply(w, map[string]string{"tx": rc.TxHash.Hex()})
	})
}

// poolMaxRelayFee: the most a relayer may take per withdrawal without the
// user raising the cap (TERNCHAIN_MAX_RELAY_FEE); also at most a tenth of the
// amount (but never below the usual fee). Audit 5 PV-2.
var poolMaxRelayFee = new(big.Int).Div(coinUnit, big.NewInt(100))

func relayFeeCap(amount *big.Int) (*big.Int, string) {
	if s := os.Getenv("TERNCHAIN_MAX_RELAY_FEE"); s != "" {
		if f, err := parseCoins(s); err == nil {
			return f, "TERNCHAIN_MAX_RELAY_FEE"
		}
	}
	limit := new(big.Int).Div(amount, big.NewInt(10))
	if limit.Cmp(poolMaxRelayFee) > 0 {
		limit.Set(poolMaxRelayFee)
	}
	if limit.Cmp(poolRelayFee) < 0 {
		limit.Set(poolRelayFee)
	}
	return limit, fmt.Sprintf("at most %s coins and a tenth of the amount", fmtCoins(poolMaxRelayFee))
}

// checkRelayURL: plain http only to this machine; elsewhere anyone on the path
// could set the fee and the relayer address (audit 5 PV-2).
func checkRelayURL(raw string) error {
	u, err := neturl.Parse(raw)
	if err != nil {
		return fmt.Errorf("relayer %q: %w", raw, err)
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && (loopback(u.Hostname()) || os.Getenv("TERNCHAIN_INSECURE_RELAY") != ""):
		return nil
	}
	return fmt.Errorf("relayer %s: use https (plain http to another machine lets anyone on the path set the fee and the relayer's address; TERNCHAIN_INSECURE_RELAY=1 on a network you trust)", raw)
}

// poolRelay: a relayer's address and fee, checked: https, the fee under the
// cap, and the address it named the first time we used it (pinned in relays.json).
func (c *Client) poolRelay(url string, amount *big.Int) (*poolRelayInfoT, error) {
	if err := checkRelayURL(url); err != nil {
		return nil, err
	}
	info, err := poolRelayInfo(url)
	if err != nil {
		return nil, fmt.Errorf("relayer %s: %w", url, err)
	}
	if limit, why := relayFeeCap(amount); info.Fee.Sign() < 0 || info.Fee.Cmp(limit) > 0 {
		return nil, fmt.Errorf("relayer %s asks %s coins per withdrawal, above your cap of %s (%s): set TERNCHAIN_MAX_RELAY_FEE=%s to accept it, or use another relayer or --direct",
			url, fmtCoins(info.Fee), fmtCoins(limit), why, fmtCoins(info.Fee))
	}
	if c.dir == "" {
		return info, nil
	}
	path := filepath.Join(c.dir, "relays.json")
	pins := map[string]string{}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &pins)
	}
	key := strings.TrimRight(url, "/")
	if was, ok := pins[key]; ok && !strings.EqualFold(was, info.Address.Hex()) {
		return nil, fmt.Errorf("relayer %s now names address %s, not %s as before: another operator, or someone in between. If it really moved, remove it from %s", url, info.Address.Hex(), was, path)
	} else if !ok {
		pins[key] = info.Address.Hex()
		b, _ := json.MarshalIndent(pins, "", "  ")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return nil, err
		}
	}
	return info, nil
}

func poolRelayInfo(url string) (*poolRelayInfoT, error) {
	resp, err := http.Get(strings.TrimRight(url, "/") + "/pool/info")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("it doesn't relay withdrawals (HTTP %d)", resp.StatusCode)
	}
	var i poolRelayInfoT
	if err := json.NewDecoder(resp.Body).Decode(&i); err != nil || i.Fee == nil {
		return nil, errors.New("a bad /pool/info")
	}
	return &i, nil
}

// poolRelaySubmit posts a request to a relayer and returns its transaction.
func poolRelaySubmit(url, path string, v any) (string, error) {
	b, _ := json.Marshal(v)
	resp, err := http.Post(strings.TrimRight(url, "/")+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("relayer: %s", strings.TrimSpace(string(body)))
	}
	var out map[string]string
	if json.Unmarshal(body, &out) != nil {
		return "", errors.New("relayer: a bad reply")
	}
	return out["tx"], nil
}
