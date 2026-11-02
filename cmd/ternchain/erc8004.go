// ERC-8004 (Trustless Agents): ternchain names in the standard's registries.
//
// An identity registers itself in the Identity Registry (an ERC-721 agent
// id whose URI is a registration file naming its ternchain address), and a
// review that cites a paid deal (attest SUBJECT review TEXT --lock ID
// --score S) is also published as Reputation feedback for the subject's
// agent id: the deal's lock is the feedback's evidence (feedbackURI), so any
// ERC-8004 reader sees ternchain's deal-based ratings, and the review claim
// stays the ternchain record. On Base Sepolia the registries are the ones
// deployed there (net.json contracts ERC8004IdentityRegistry /
// ERC8004ReputationRegistry, version 2.0.0); devnets deploy look-alikes
// (contracts/ext).
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// The registries deployed on Base Sepolia (github.com/erc-8004/erc-8004-contracts; checked on chain: getVersion 2.0.0).
const (
	erc8004IdentityBaseSepolia   = "0x8004A818BFB912233c491871b3d84c89A494BD9e"
	erc8004ReputationBaseSepolia = "0x8004B663056A597Dffe9eCcC1965A193B7388713"
)

// erc8004Tag: the feedback tag ternchain's deal reviews carry (tag2: the deal's lock).
const erc8004Tag = "tern-deal"

// ERC8004File: this identity's agent id (erc8004.json in its folder).
type ERC8004File struct {
	AgentID  string `json:"agent_id"`
	Registry string `json:"registry"` // CAIP-10 style: eip155:CHAIN:ADDRESS
}

func (c *Client) erc8004Path() string { return filepath.Join(c.dir, "erc8004.json") }

func (ch *Chain) erc8004() bool {
	return ch.has("ERC8004IdentityRegistry") && ch.has("ERC8004ReputationRegistry")
}

// erc8004Services: what the registration names besides ternchain: an A2A
// agent (its Agent Card) and an x402 endpoint.
type erc8004Services struct {
	A2A, A2AVersion string // the card's URL; its A2A version ("1.0.0", "0.3.0")
	X402            string // an x402 endpoint (x402 serve)
}

// erc8004Registration: the agent's registration file (ERC-8004
// #registration-v1), carried in a data: URI so nothing off chain is needed.
func (c *Client) erc8004Registration(desc string, x402 bool, more erc8004Services) string {
	type service struct {
		Name     string `json:"name"`
		Endpoint string `json:"endpoint"`
		Version  string `json:"version,omitempty"`
		Address  string `json:"address,omitempty"`
	}
	svcs := []service{{"ternchain", fmt.Sprintf("tern:%s@eip155:%d", c.f.Name, c.ch.chainID.Int64()), "1", c.me().Hex()}}
	if more.A2A != "" {
		svcs = append(svcs, service{Name: "A2A", Endpoint: more.A2A, Version: more.A2AVersion})
	}
	if more.X402 != "" {
		svcs, x402 = append(svcs, service{Name: "x402", Endpoint: more.X402, Version: "2"}), true
	}
	reg := struct { // the standard's field names
		Type           string    `json:"type"`
		Name           string    `json:"name"`
		Description    string    `json:"description"`
		Services       []service `json:"services"`
		X402Support    bool      `json:"x402Support"`
		Active         bool      `json:"active"`
		SupportedTrust []string  `json:"supportedTrust"`
	}{"https://eips.ethereum.org/EIPS/eip-8004#registration-v1", c.f.Name, desc, svcs, x402, true, []string{"reputation"}}
	b, _ := json.Marshal(reg)
	return "data:application/json;base64," + base64.StdEncoding.EncodeToString(b)
}

// erc8004Register: this identity (an owner or an agent) as an ERC-8004
// agent; its ternchain name goes in the metadata ("tern.name").
func (c *Client) erc8004Register(desc string, x402 bool) (map[string]any, error) {
	return c.erc8004RegisterWith(desc, x402, erc8004Services{})
}

// erc8004RegisterWith: erc8004Register naming more services. An identity
// registered already gets its registration file replaced (setAgentURI),
// its description kept unless a new one is given.
func (c *Client) erc8004RegisterWith(desc string, x402 bool, more erc8004Services) (map[string]any, error) {
	if !c.ch.erc8004() {
		return nil, errors.New("no ERC-8004 registries on this network (net.json contracts ERC8004IdentityRegistry, ERC8004ReputationRegistry)")
	}
	if more.A2A != "" { // only an Agent Card that answers: a registration pointing nowhere misleads every reader
		k, cu, _, err := a2aFetchCard(more.A2A)
		if err != nil {
			return nil, fmt.Errorf("--a2a: %w (start your A2A server first)", err)
		}
		e, _ := k.endpoint()
		more.A2A, more.A2AVersion = cu, e.Version+".0"
	}
	if more.X402 != "" {
		if u, err := url.Parse(more.X402); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("--x402: an http(s) URL, not %q", more.X402)
		}
	}
	var have ERC8004File
	if readJSON(c.erc8004Path(), &have) == nil && have.AgentID != "" {
		if more == (erc8004Services{}) {
			return map[string]any{"agent_id": have.AgentID, "registry": have.Registry, "done": "registered already"}, nil
		}
		id, _ := new(big.Int).SetString(have.AgentID, 10)
		if id == nil {
			return nil, fmt.Errorf("%s: bad agent id %q", c.erc8004Path(), have.AgentID)
		}
		if desc == "" { // keep what the registration said
			if _, _, reg, _, err := c.erc8004Read(id); err == nil && reg != nil {
				desc = reg.Description
			}
		}
		rc, err := c.ch.send(c.key, nil, "ERC8004IdentityRegistry", "setAgentURI", id, c.erc8004Registration(desc, x402, more))
		if err != nil {
			return nil, fmt.Errorf("ERC-8004 setAgentURI: %w", err)
		}
		r := map[string]any{"agent_id": have.AgentID, "registry": have.Registry, "updated": "registration file", "tx": rc.TxHash.Hex()}
		if more.A2A != "" {
			r["a2a"] = more.A2A
		}
		if more.X402 != "" {
			r["x402"] = more.X402
		}
		return r, nil
	}
	reg := c.ch.abis["ERC8004IdentityRegistry"]
	method := ""
	for n, m := range reg.Methods { // register(string agentURI, MetadataEntry[] metadata): one of three overloads
		if m.RawName == "register" && len(m.Inputs) == 2 {
			method = n
		}
	}
	type entry struct {
		MetadataKey   string
		MetadataValue []byte
	}
	uri, md := c.erc8004Registration(desc, x402, more), []entry{{"tern.name", []byte(c.f.Name)}}
	var rc *types.Receipt
	var err error
	if c.f.Account != "" && c.ch.has("ERC8004Registrar") {
		// A smart account takes no ERC-721 by safe transfer, and the registry
		// mints with _safeMint: the registrar registers and hands the id over.
		rc, err = c.ch.send(c.key, nil, "ERC8004Registrar", "register", uri, md)
	} else {
		rc, err = c.ch.send(c.key, nil, "ERC8004IdentityRegistry", method, uri, md)
	}
	if err != nil {
		return nil, fmt.Errorf("ERC-8004 register: %w", err)
	}
	ev := reg.Events["Transfer"]
	var id *big.Int
	for _, l := range rc.Logs { // the agent id that ended with us
		if len(l.Topics) == 4 && l.Topics[0] == ev.ID && l.Address == c.ch.addr("ERC8004IdentityRegistry") &&
			common.BytesToAddress(l.Topics[2].Bytes()) == c.me() {
			id = new(big.Int).SetBytes(l.Topics[3].Bytes())
		}
	}
	if id == nil {
		return nil, errors.New("ERC-8004 register: no agent id transferred to " + c.me().Hex())
	}
	f := ERC8004File{AgentID: id.String(), Registry: fmt.Sprintf("eip155:%d:%s", c.ch.chainID.Int64(), c.ch.addr("ERC8004IdentityRegistry").Hex())}
	if err := writeJSON(c.erc8004Path(), f); err != nil {
		return nil, err
	}
	return map[string]any{"agent_id": f.AgentID, "registry": f.Registry, "owner": c.me().Hex(), "tx": rc.TxHash.Hex()}, nil
}

// erc8004AgentOf: the agent id a ternchain name registered itself, or nil.
// Only ids minted for the name count: its own erc8004.json, a Registered
// event naming its address as owner, or the Registrar's RegisteredFor (an
// account's). An id merely transferred to the address doesn't: anyone can
// send one there, and reviews would go to theirs (audit ext M-7). The id
// must still be the address's and its tern.name (or its registration's
// name) must be this name. The newest such id wins.
func (c *Client) erc8004AgentOf(name string) (*big.Int, error) {
	a, err := c.addressOf(name)
	if err != nil {
		return nil, err
	}
	at := []common.Hash{common.BytesToHash(a.Bytes())}
	var ids []*big.Int
	if evs, err := c.ch.events("ERC8004IdentityRegistry", "Registered", nil, at); err == nil {
		for _, e := range evs {
			if v, ok := e.Args["agentId"].(*big.Int); ok {
				ids = append(ids, v)
			}
		}
	} else {
		return nil, err
	}
	if c.ch.has("ERC8004Registrar") {
		evs, err := c.ch.events("ERC8004Registrar", "RegisteredFor", nil, at)
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			if v, ok := e.Args["agentId"].(*big.Int); ok {
				ids = append(ids, v)
			}
		}
	}
	var mine ERC8004File // our own registration, as erc8004 register wrote it
	if name == c.f.Name && readJSON(c.erc8004Path(), &mine) == nil {
		if v, ok := new(big.Int).SetString(mine.AgentID, 10); ok {
			ids = append(ids, v)
		}
	}
	var id *big.Int
	for _, v := range ids {
		if (id == nil || v.Cmp(id) > 0) && c.erc8004Names(v, a, name) {
			id = v
		}
	}
	if id == nil {
		return nil, fmt.Errorf("%s has no ERC-8004 agent id of its own (they run: ternchain erc8004 register)", name)
	}
	return id, nil
}

// erc8004Names: the agent id is held by a and registered for name: its
// tern.name metadata, or without one its registration file's name.
func (c *Client) erc8004Names(id *big.Int, a common.Address, name string) bool {
	if o, err := c.ch.call("ERC8004IdentityRegistry", "ownerOf", id); err != nil || o[0].(common.Address) != a {
		return false
	}
	if m, err := c.ch.call("ERC8004IdentityRegistry", "getMetadata", id, "tern.name"); err == nil && len(m[0].([]byte)) > 0 {
		return string(m[0].([]byte)) == name
	}
	u, err := c.ch.call("ERC8004IdentityRegistry", "tokenURI", id)
	if err != nil {
		return false
	}
	b64, ok := strings.CutPrefix(u[0].(string), "data:application/json;base64,")
	if !ok {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return false
	}
	var reg struct {
		Name     string `json:"name"`
		Services []struct {
			Name     string `json:"name"`
			Endpoint string `json:"endpoint"`
		} `json:"services"`
	}
	if json.Unmarshal(raw, &reg) != nil || reg.Name != name {
		return false
	}
	for _, s := range reg.Services {
		if s.Name == "ternchain" && strings.HasPrefix(s.Endpoint, "tern:"+name+"@") {
			return true
		}
	}
	return false
}

// erc8004Feedback: a deal review as Reputation feedback: value = the score
// (-100..100, 0 decimals), tag1 tern-deal, tag2 the lock, feedbackURI the
// lock on this chain, feedbackHash the review's hash. The reviewer's
// address gives it (the registry refuses the agent's own owner).
func (c *Client) erc8004Feedback(subject string, lockID uint64, score int64, review string) (map[string]any, error) {
	if !c.ch.erc8004() {
		return nil, errors.New("no ERC-8004 registries on this network")
	}
	id, err := c.erc8004AgentOf(subject)
	if err != nil {
		return nil, err
	}
	uri := fmt.Sprintf("tern:lock/eip155:%d:%s/%d", c.ch.chainID.Int64(), c.ch.addr("Bank").Hex(), lockID)
	h := crypto.Keccak256Hash([]byte(review))
	rc, err := c.ch.send(c.key, nil, "ERC8004ReputationRegistry", "giveFeedback", id, big.NewInt(score), uint8(0), erc8004Tag,
		fmt.Sprint(lockID), "", uri, h)
	if err != nil {
		if strings.Contains(err.Error(), "Self-feedback") {
			return nil, fmt.Errorf("ERC-8004 feedback: %s's agent id is held by this address (the registry refuses feedback from its owner)", subject)
		}
		return nil, fmt.Errorf("ERC-8004 feedback: %w", err)
	}
	return map[string]any{"agent_id": id.String(), "value": score, "tag1": erc8004Tag, "tag2": fmt.Sprint(lockID), "feedback_uri": uri, "tx": rc.TxHash.Hex()}, nil
}

// erc8004Summary: what the ERC-8004 registry says about a name from the
// given reviewers' feedback (tern-deal only).
func (c *Client) erc8004Summary(name string, reviewers []string) (map[string]any, error) {
	id, err := c.erc8004AgentOf(name)
	if err != nil {
		return nil, err
	}
	var from []common.Address
	for _, r := range reviewers {
		a, err := c.addressOf(r)
		if err != nil {
			return nil, err
		}
		from = append(from, a)
	}
	if len(from) == 0 {
		return nil, errors.New("name the reviewers whose feedback counts (the registry sums only the clients asked for: ERC-8004 leaves sybils to the reader)")
	}
	out, err := c.ch.call("ERC8004ReputationRegistry", "getSummary", id, from, erc8004Tag, "")
	if err != nil {
		return nil, err
	}
	n, v, d := out[0].(uint64), out[1].(*big.Int), out[2].(uint8)
	mean, _ := new(big.Rat).SetFrac(v, unitOf(d)).Float64()
	return map[string]any{"agent_id": id.String(), "feedback": n, "mean_score": mean, "tag": erc8004Tag}, nil
}
