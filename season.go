// Season 2 of the shared devnet: founders' keys made before the network,
// the shared-network mode, the notice, and the approval round trip
// (ask-owner carries what was refused; owner approve --ask approves exactly
// that).
package main

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// keygen writes a new key to path (0600, never over an existing file) and
// returns its address. No network needed: founders send NAME=ADDRESS to the
// operator before the network exists ('devnet --genesis').
func keygen(path string) (common.Address, error) {
	if path == "" {
		return common.Address{}, errors.New("--out FILE is required (where the key goes; keep it like an id/ folder)")
	}
	k, err := crypto.GenerateKey()
	if err != nil {
		return common.Address{}, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return common.Address{}, fmt.Errorf("%s exists: not overwriting a key", path)
		}
		return common.Address{}, err
	}
	if _, err := f.WriteString(keyHex(k) + "\n"); err != nil {
		f.Close()
		return common.Address{}, err
	}
	if err := f.Close(); err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(k.PublicKey), nil
}

// readKeyFile: a key written by keygen (hex, optional 0x).
func readKeyFile(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(string(b)), "0x"))
	if err != nil {
		return nil, fmt.Errorf("%s: not a key made by owner keygen: %w", path, err)
	}
	return k, nil
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`) // Registry.validLabel

// parseGenesisStrict reads --genesis NAME=0xADDRESS,… and refuses anything
// else: a typo would otherwise drop a founder silently, and with fewer
// founders than K+1 no vote could ever pass.
func parseGenesisStrict(s string) ([]GenesisCitizen, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []GenesisCitizen
	seen := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		n, a, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || !nameRE.MatchString(n) || !common.IsHexAddress(a) {
			return nil, fmt.Errorf("--genesis: %q is not NAME=0xADDRESS (each founder runs 'owner keygen --out FILE --name NAME' and sends the line it prints)", p)
		}
		if seen[n] {
			return nil, fmt.Errorf("--genesis: %s twice", n)
		}
		seen[n] = true
		out = append(out, GenesisCitizen{Name: n, Key: common.HexToAddress(a)})
	}
	if len(out) <= 3 {
		return nil, fmt.Errorf("--genesis: %d founders; the citizens' house needs more than K=3 (at least 4)", len(out))
	}
	return out, nil
}

// sharedNetwork: this process runs against a shared test network (set by
// main from net.json), so MCP tools get the same refusals as the CLI.
var sharedNetwork bool

// defaultSharedRefusals: what a shared network's clients refuse when its
// net.json names nothing ("refuse"): minting, randomness rounds, game pots,
// passkey accounts, and fiat in a currency other than TEST.
var defaultSharedRefusals = []string{"mint", "random", "pot", "passkey", "fiat-real"}

// sharedRefusals: net.json's "refuse" list (set by main; nil: the defaults).
var sharedRefusals []string

// sharedRefusal: what clients of a shared test network ("mode": "shared" in
// net.json, set by 'devnet --shared') refuse. A client rule, like the guard:
// the contracts still allow it, but the network's own client doesn't do it.
// The name pot is refused in any case (lock open lowercases kinds).
func sharedRefusal(cmd string, pos []string, passkey bool) error {
	off := func(what string) error {
		return fmt.Errorf("%s is off on this shared test network (net.json \"mode\": \"shared\"; see TERMS.md)", what)
	}
	group, _, _ := strings.Cut(cmd, " ")
	rules := sharedRefusals
	if rules == nil {
		rules = defaultSharedRefusals
	}
	on := map[string]bool{}
	for _, r := range rules {
		on[r] = true
	}
	switch {
	case on["mint"] && (cmd == "token mint" || cmd == "item mint"):
		return off("minting")
	case on["random"] && group == "random":
		return off("randomness rounds")
	case on["pot"] && (strings.EqualFold(group, "pot") || cmd == "lock open" && len(pos) > 0 && strings.EqualFold(pos[0], "pot")):
		return off("game pots")
	case on["passkey"] && cmd == "owner init" && passkey:
		return off("--passkey")
	case on["fiat-real"] && cmd == "fiat" && len(pos) >= 4:
		switch pos[0] {
		case "paid", "request", "claim", "invoice":
			if !strings.EqualFold(pos[3], "TEST") {
				return fmt.Errorf("fiat on this shared test network uses the currency TEST only (got %q): no real currencies, invoices, bank details or amounts", pos[3])
			}
		}
	}
	return nil
}

// networkNotice: the notice for a test network, with who runs it and how to
// reach them when the operator said (devnet --operator, --contact).
func networkNotice(operator, contact string) string {
	who := "a test network for invited people"
	if operator != "" {
		who = "a test network run by " + operator + " for invited people"
	}
	if contact != "" {
		who += " (contact: " + contact + ")"
	}
	return strings.ToUpper(who[:1]) + who[1:] + ". Coins are test units: no value, not for sale or exchange, never converted into anything, " +
		"deleted at each reset. Nothing agreed here (deals, campaigns, grants, votes) is a real contract, purchase, investment or promise. " +
		"No real money, bank transfers, invoices or physical tasks tied to anything here. No illegal content and no one else's private data; " +
		"public posts, reviews and claims stay visible until the reset. Mail is encrypted, but the operator sees who sends how much and when, " +
		"and your AI agent's provider reads what your agent reads. Terms: TERMS.md, next to net.json."
}

// noticeOf: the network's notice from its net.json, or the generic one.
func noticeOf(df *DevnetFile) string {
	if df != nil && df.Notice != "" {
		return df.Notice
	}
	return testNotice
}

// epochWords: what "per epoch" means on this network, with the next reset.
func (c *Client) epochWords() string {
	n, err := c.ch.epochLength()
	if err != nil || n == 0 {
		return "per epoch"
	}
	now := c.chainNowU()
	next := (now/n + 1) * n
	length := (time.Duration(n) * time.Second).String()
	if n == 86400 {
		length = "a UTC day (00:00 to 00:00 UTC)"
	}
	return fmt.Sprintf("an epoch is %s; limits reset at %s (in %s)", length,
		time.Unix(int64(next), 0).UTC().Format("2006-01-02 15:04 UTC"), (time.Duration(next-now) * time.Second).String())
}

// askOwner: an agent's question to its owner, carrying the action its guard
// refused last (if any, within a day); it wakes the owner's watch.
func (c *Client) askOwner(subject, text string) (map[string]any, error) {
	if c.isOwner() {
		return nil, errors.New("ask-owner is for agents, and you are an owner: your agents ask you (inbox shows their asks first)")
	}
	req := c.pendingAsk()
	id, err := c.send(SendOpts{To: c.f.Owner, Intent: "question", Subject: subject, Text: text, Ask: req})
	if err != nil {
		return nil, err
	}
	r := map[string]any{"asked": c.f.Owner, "sent": id, "note": "the answer arrives as a signed message from your owner; the ask wakes your owner's watch"}
	if req.To != "" {
		r["carries"] = strings.TrimSpace("approval for: " + req.What)
		r["owner_runs"] = "ternchain owner approve --dir " + c.ownerDirWords() + " --ask " + id[:18]
	}
	return r, nil
}

// approveAsk: the owner approves exactly what one of its agents' asks
// carries: its recipient and amount, nothing its text says.
func (c *Client) approveAsk(id string) (map[string]any, error) {
	if !c.isOwner() {
		return nil, errors.New("only an owner approves its agents' actions")
	}
	it := c.find1(id)
	if it == nil {
		if _, err := c.scan(); err != nil {
			return nil, err
		}
		it = c.find1(id)
	}
	if it == nil {
		return nil, fmt.Errorf("no ask %s in your inbox (inbox --dir … lists asks first)", id)
	}
	if it.Mail == nil || it.Mail.Ask == nil || !fromAgent(it.From) || ownerName(it.From) != c.f.Name || it.To != c.f.Name ||
		!strings.HasPrefix(it.Verified, "ok") {
		return nil, fmt.Errorf("%s is not an ask from one of your agents", id)
	}
	a := it.Mail.Ask
	if a.To == "" {
		return nil, errors.New("this ask carries no refused action (a plain question): answer it with reply, or approve explicitly: owner approve AGENT TO [AMOUNT]")
	}
	var amt *big.Int
	if a.Amount != "" {
		v, err := parseCoins(a.Amount)
		if err != nil {
			return nil, fmt.Errorf("the ask's amount %q: %w", a.Amount, err)
		}
		amt = v
	}
	if err := c.approveAction(it.From, a.To, amt); err != nil {
		return nil, err
	}
	it.Done = true
	c.save()
	r := map[string]any{"approved": it.From, "to": a.To, "for": a.What, "ask": it.ID[:18]}
	if amt != nil {
		r["amount"], r["valid"] = fmtCoins(amt), "one payment of at most that, 24 hours"
	} else if strings.HasPrefix(a.To, "thread:") {
		r["allows"] = "this thread goes on: " + a.What + " (once, nothing else)"
	} else {
		r["contact"] = a.To + " is now a lasting correspondent of " + it.From + " (owner contact " + it.From + " " + a.To + " --off withdraws it)"
	}
	return r, nil
}

// runAttest: attest SUBJECT KIND DATA [--score S] [--lock ID], for the CLI and MCP alike.
func runAttest(c *Client, pos []string, lockS, scoreS string) (map[string]any, error) {
	if len(pos) < 3 {
		return nil, errors.New("usage: attest SUBJECT KIND DATA")
	}
	data := strings.Join(pos[2:], " ")
	if lockS != "" { // a review that cites the deal: only those count in a rating
		d, err := c.reviewData(pos[0], atoi(lockS), data)
		if err != nil {
			return nil, err
		}
		data = d
	}
	if scoreS != "" {
		var sc int64
		fmt.Sscan(scoreS, &sc)
		// One scale for reviews: -100..100. Basis points are for rulings, awards and slashes only.
		if k := pos[1]; k != "ruling" && k != "award" && k != "slash" && (sc < -100 || sc > 100) {
			return nil, errors.New("scores are -100..100 (100 = perfect); basis points are only for rulings, awards and slashes")
		}
		uid, err := c.attestScored(pos[0], pos[1], data, sc)
		if err != nil {
			return nil, err
		}
		return map[string]any{"attestation": uid.Hex(), "subject": pos[0], "kind": pos[1], "score": sc}, nil
	}
	if err := c.attest(pos[0], pos[1], data, 0); err != nil {
		return nil, err
	}
	return map[string]any{"attested": pos[0], "kind": pos[1]}, nil
}
