// Secret votes in the citizens' house (Semaphore v4's scheme): an owner name
// registers an identity commitment once (derived from its key, nothing new
// to keep); while the name is a citizen its leaf is in the citizens' tree
// (CitizenGroup). A vote is a proof that some leaf was in the tree when the
// proposal opened and is in a recent root, with nullifier H(scope, secret): one
// vote per citizen, and nobody sees who voted what. A relayer submits it, so the
// voter's address never shows; the client then checks the vote is on chain.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	edbn254 "github.com/consensys/gnark-crypto/ecc/bn254/twistededwards"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern/internal/zk"
)

// identityTries: how many derived identities a name tries when the earlier
// ones are taken (audit 5 ZK-2: a squatter who copies a commitment from a
// pending register tx only pushes us to the next one).
var identityTries = 8

// identitySecret i: 0 is the key's own (zk.IdentitySecret, as before); i > 0
// is H(that, name, i) mod l, bound to the name.
func (c *Client) identitySecret(i int) *big.Int {
	s := zk.IdentitySecret(c.key)
	if i == 0 {
		return s
	}
	id := c.id()
	h := new(big.Int).SetBytes(crypto.Keccak256([]byte("tern-semaphore-identity-v2"), common.LeftPadBytes(s.Bytes(), 32), id[:],
		big.NewInt(int64(i)).Bytes()))
	ed := edbn254.GetEdwardsCurve()
	h.Mod(h, &ed.Order)
	if h.Sign() == 0 {
		h.SetInt64(1)
	}
	return h
}

// voteIdentity: which derived identity this name registered, and its
// registration number, cached so a vote never asks the RPC about our name
// (audit 5 PV-6: name -> nullifier -> yes/no in one session).
type voteIdentity struct {
	Group      string `json:"group"` // CitizenGroup's address
	Index      int    `json:"index"`
	Commitment string `json:"commitment"`
	Seq        string `json:"seq"`
}

func (c *Client) identityPath() string { return filepath.Join(c.dir, "vote.identity") }

// cachedIdentity: the cached identity, if it is this network's and this key's.
func (c *Client) cachedIdentity() *voteIdentity {
	if c.dir == "" || !c.ch.has("CitizenGroup") {
		return nil
	}
	b, err := os.ReadFile(c.identityPath())
	if err != nil {
		return nil
	}
	var v voteIdentity
	if json.Unmarshal(b, &v) != nil || !strings.EqualFold(v.Group, c.ch.addr("CitizenGroup").Hex()) || v.Index < 0 {
		return nil
	}
	if com, ok := new(big.Int).SetString(v.Commitment, 10); !ok || zk.IdentityCommitment(c.identitySecret(v.Index)).Cmp(com) != 0 {
		return nil // another key (a recovery): look again
	}
	return &v
}

func (c *Client) saveIdentity(i int, com *big.Int) error {
	if c.dir == "" {
		return nil
	}
	seq, err := c.bigCall("CitizenGroup", "seqOf", c.id())
	if err != nil {
		return err
	}
	b, _ := json.Marshal(voteIdentity{Group: c.ch.addr("CitizenGroup").Hex(), Index: i, Commitment: com.String(), Seq: seq.String()})
	return os.WriteFile(c.identityPath(), b, 0o600)
}

func (c *Client) dropIdentity() {
	if c.dir != "" {
		os.Remove(c.identityPath())
	}
}

// identityCommitment: the commitment this name votes with (the cached one, else the key's first).
func (c *Client) identityCommitment() *big.Int {
	if v := c.cachedIdentity(); v != nil {
		return zk.IdentityCommitment(c.identitySecret(v.Index))
	}
	return zk.IdentityCommitment(c.identitySecret(0))
}

// ensureIdentity registers this owner's voting identity if it isn't yet (or rotated).
func (c *Client) ensureIdentity() (registered bool, err error) {
	if !c.ch.has("CitizenGroup") || !c.isOwner() {
		return false, nil
	}
	if c.cachedIdentity() != nil {
		return false, nil
	}
	have, err := c.bigCall("CitizenGroup", "identityOf", c.id())
	if err != nil {
		return false, err
	}
	for i := 0; i < identityTries && have.Sign() != 0; i++ {
		if zk.IdentityCommitment(c.identitySecret(i)).Cmp(have) == 0 {
			return false, c.saveIdentity(i, have)
		}
	}
	var last error
	for i := 0; i < identityTries; i++ {
		com := zk.IdentityCommitment(c.identitySecret(i))
		if out, err := c.ch.call("CitizenGroup", "nameOf", com); err == nil && out[0].([32]byte) != ([32]byte{}) {
			last = fmt.Errorf("identity %d is registered by another name", i)
			continue // squatted (copied from our pending tx): the next one
		}
		if _, err := c.ch.send(c.key, nil, "CitizenGroup", "register", c.id(), com); err != nil {
			if last = err; strings.Contains(err.Error(), "commitment") {
				continue // taken meanwhile
			}
			return false, err
		}
		return true, c.saveIdentity(i, com)
	}
	return false, fmt.Errorf("no voting identity could be registered (all %d derived ones are taken: %v); vote by name with --open", identityTries, last)
}

// groupLeaves: the citizens' tree's leaves after all MemberSet events
// before (block, index) (nil: all of them).
func (c *Client) groupLeaves(before *event) ([]*big.Int, error) {
	evs, err := c.ch.events("CitizenGroup", "MemberSet")
	if err != nil {
		return nil, err
	}
	sort.SliceStable(evs, func(i, j int) bool {
		return evs[i].Block < evs[j].Block || evs[i].Block == evs[j].Block && evs[i].Index < evs[j].Index
	})
	var leaves []*big.Int
	for _, e := range evs {
		if before != nil && (e.Block > before.Block || e.Block == before.Block && e.Index > before.Index) {
			break
		}
		i := int(e.Args["index"].(*big.Int).Int64())
		for len(leaves) <= i {
			leaves = append(leaves, big.NewInt(0))
		}
		leaves[i] = e.Args["commitment"].(*big.Int)
	}
	return leaves, nil
}

func (c *Client) bigCall(contract, method string, args ...any) (*big.Int, error) {
	out, err := c.ch.call(contract, method, args...)
	if err != nil {
		return nil, err
	}
	return out[0].(*big.Int), nil
}

// namedVoteMessage: voteNamed's message, keccak(abi.encode(yes, by)) >> 8 (audit 5 ZK-3).
func namedVoteMessage(yes bool, by common.Hash) *big.Int {
	b := byte(0)
	if yes {
		b = 1
	}
	h := crypto.Keccak256(common.LeftPadBytes([]byte{b}, 32), by[:])
	return new(big.Int).Rsh(new(big.Int).SetBytes(h), 8)
}

// votedAlready: our nullifier (or our name) in this proposal's vote events,
// scanned locally, so no query names the nullifier (audit 5 PV-6).
func (c *Client) votedAlready(id uint64, nullifier *big.Int) bool {
	topic := []common.Hash{common.BigToHash(u(id))}
	if evs, err := c.ch.events("Parliament", "SecretVoted", topic); err == nil {
		for _, e := range evs {
			if n, ok := e.Args["nullifier"].(*big.Int); ok && n.Cmp(nullifier) == 0 {
				return true
			}
		}
	}
	if evs, err := c.ch.events("Parliament", "CitizenVoted", topic); err == nil {
		for _, e := range evs {
			if by, ok := e.Args["by"].([32]byte); ok && common.Hash(by) == c.id() {
				return true
			}
		}
	}
	return false
}

// voteProof makes this citizen's proof for proposal id (a secret vote).
func (c *Client) voteProof(id uint64, yes bool) (nullifier, rootNow *big.Int, proof []byte, err error) {
	return c.voteProofFor(id, yes, nil)
}

// voteProofFor: by != nil makes a named vote's proof (its message names by).
func (c *Client) voteProofFor(id uint64, yes bool, by *common.Hash) (nullifier, rootNow *big.Int, proof []byte, err error) {
	v := c.cachedIdentity()
	if v == nil {
		if _, err := c.ensureIdentity(); err != nil {
			return nil, nil, nil, err
		}
		if v = c.cachedIdentity(); v == nil {
			return nil, nil, nil, errors.New("no voting identity (gov identity)")
		}
	}
	evs, err := c.ch.events("Parliament", "Proposed", []common.Hash{common.BigToHash(new(big.Int).SetUint64(id))})
	if err != nil || len(evs) == 0 {
		return nil, nil, nil, fmt.Errorf("no proposal %d", id)
	}
	rootOpen, err := c.bigCall("Parliament", "citizenRootAt", u(id))
	if err != nil {
		return nil, nil, nil, err
	}
	openLeaves, err := c.groupLeaves(&evs[0])
	if err != nil {
		return nil, nil, nil, err
	}
	nowLeaves, err := c.groupLeaves(nil)
	if err != nil {
		return nil, nil, nil, err
	}
	openT, nowT := zk.NewTree(zk.GroupDepth, openLeaves), zk.NewTree(zk.GroupDepth, nowLeaves)
	rootNow, err = c.bigCall("CitizenGroup", "root")
	if err != nil {
		return nil, nil, nil, err
	}
	if openT.Root().Cmp(rootOpen) != 0 || nowT.Root().Cmp(rootNow) != 0 {
		return nil, nil, nil, errors.New("the citizens' tree doesn't match its events: try again")
	}
	secret := c.identitySecret(v.Index)
	com := zk.IdentityCommitment(secret)
	oi, ni := openT.IndexOf(com), nowT.IndexOf(com)
	if oi < 0 && ni < 0 {
		c.dropIdentity() // rotated elsewhere? read it again next time
	}
	if oi < 0 {
		return nil, nil, nil, fmt.Errorf("your voting identity wasn't in the citizens' tree when proposal %d opened: only citizens from before it vote on it", id)
	}
	if ni < 0 {
		return nil, nil, nil, errors.New("your voting identity isn't in the citizens' tree now: you are not a citizen (gov citizens), or your status changed and the tree wasn't synced (gov sync NAME)")
	}
	scope, err := c.bigCall("Parliament", "voteScope", u(id))
	if err != nil {
		return nil, nil, nil, err
	}
	var msg *big.Int
	if by != nil {
		msg = namedVoteMessage(yes, *by)
	} else if msg, err = c.bigCall("Parliament", "voteMessage", yes); err != nil {
		return nil, nil, nil, err
	}
	nullifier = zk.HashN(scope, secret)
	if c.votedAlready(id, nullifier) {
		return nil, nil, nil, errors.New("voted already (this identity's nullifier is used for this proposal)")
	}
	op, ob, _ := openT.Proof(oi)
	np, nb, _ := nowT.Proof(ni)
	a := &zk.VoteCircuit{RootOpen: rootOpen, RootNow: rootNow, Nullifier: nullifier, Scope: scope, Message: msg, Secret: secret}
	copy(a.OpenPath[:], zk.Bigs(op))
	copy(a.OpenBits[:], zk.Ints(ob))
	copy(a.NowPath[:], zk.Bigs(np))
	copy(a.NowBits[:], zk.Ints(nb))
	if err := c.ch.zkCheck("vote"); err != nil { // the keys and the network's verifier: one setup (ceremony.go)
		return nil, nil, nil, err
	}
	proof, err = zk.Prove("vote", a)
	return nullifier, rootNow, proof, err
}

// hasVotingIdentity: this citizen's identity was registered before proposal id opened.
func (c *Client) hasVotingIdentity(id uint64) bool {
	if !c.ch.has("CitizenGroup") {
		return false
	}
	var seq *big.Int
	if v := c.cachedIdentity(); v != nil {
		seq, _ = new(big.Int).SetString(v.Seq, 10)
	}
	if seq == nil || seq.Sign() == 0 {
		var err error
		if seq, err = c.bigCall("CitizenGroup", "seqOf", c.id()); err != nil {
			return false
		}
	}
	at, err := c.bigCall("Parliament", "registrationsAt", u(id))
	return err == nil && seq.Sign() > 0 && seq.Cmp(at) <= 0
}

// voteRelays: the relayers a secret vote may go through, in order: --relay,
// then TERNCHAIN_VOTE_RELAYS (comma-separated), the fallbacks when one drops it.
func (c *Client) voteRelays() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range append([]string{c.relay}, strings.Split(os.Getenv("TERNCHAIN_VOTE_RELAYS"), ",")...) {
		if r = strings.TrimSpace(r); r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// voteLandWait: how long a relayed vote may take to show on chain.
var voteLandWait = 2 * time.Second

// secretVoteLanded: our nullifier is in this proposal's SecretVoted events
// (scanned locally, audit 5 PV-5: a relayer that answers "ok" and drops the
// vote is caught).
func (c *Client) secretVoteLanded(id uint64, nullifier *big.Int) bool {
	deadline := time.Now().Add(voteLandWait)
	for {
		if evs, err := c.ch.events("Parliament", "SecretVoted", []common.Hash{common.BigToHash(u(id))}); err == nil {
			for _, e := range evs {
				if n, ok := e.Args["nullifier"].(*big.Int); ok && n.Cmp(nullifier) == 0 {
					return true
				}
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// voteCitizens: secret by default (a proof, through a relayer); open: by name.
func (c *Client) voteCitizens(id uint64, yes, open bool) (string, error) {
	if err := c.ownerOnly(); err != nil {
		return "", err
	}
	word := map[bool]string{true: "yes", false: "no"}[yes]
	if !c.ch.has("CitizenGroup") { // an older network: open votes only
		_, err := c.ch.send(c.key, nil, "Parliament", "voteCitizen", u(id), c.id(), yes)
		return word + " (open: this network has no secret votes)", err
	}
	cached := c.cachedIdentity() != nil
	new, ierr := c.ensureIdentity()
	if ierr != nil && !open {
		return "", ierr
	} else if ierr == nil && new && !open {
		return "", fmt.Errorf("your voting identity is registered now; it counts for proposals opened from now on. For proposal %d vote by name: --open", id)
	}
	if ierr != nil || !c.hasVotingIdentity(id) { // no identity, or registered after it opened: by name, the old way (audit 5 ZK-2: never blocked by the identity)
		if !open {
			return "", fmt.Errorf("your voting identity was registered after proposal %d opened: vote by name with --open", id)
		}
		_, err := c.ch.send(c.key, nil, "Parliament", "voteCitizen", u(id), c.id(), yes)
		return word + " (open, by name)", err
	}
	if open {
		me := c.id()
		nullifier, rootNow, proof, err := c.voteProofFor(id, yes, &me)
		if err != nil {
			return "", err
		}
		_, err = c.ch.send(c.key, nil, "Parliament", "voteNamed", u(id), me, yes, nullifier, rootNow, proof)
		return word + " (open, by name, counted by proof)", err
	}
	relays := c.voteRelays()
	if len(relays) == 0 {
		return "", errors.New("a secret vote goes through a relayer (your own transaction would show who voted): set --relay URL, or vote by name with --open")
	}
	if !cached { // the identity was read from the RPC in this session: don't follow it with the vote (audit 5 PV-6)
		return "", errors.New("your voting identity was just looked up on chain (it is cached now): vote again in a new session, so the RPC can't link your name to your vote (or run gov identity first next time)")
	}
	var problems []string
	for attempt := 0; attempt < 2; attempt++ { // once more if the tree lost a leaf meanwhile
		nullifier, rootNow, proof, err := c.voteProof(id, yes)
		if err != nil {
			return "", err
		}
		moved := false
		for _, r := range relays {
			_, err := poolRelaySubmit(r, "/gov/vote", voteRequest{ID: id, Yes: yes, Nullifier: nullifier, Root: rootNow, Proof: common.Bytes2Hex(proof)})
			if err == nil && c.secretVoteLanded(id, nullifier) {
				return word + " (secret)", nil
			}
			if err != nil && strings.Contains(err.Error(), "prove again") {
				moved = true
				break
			}
			if err == nil {
				err = errors.New("it answered ok, but the vote isn't on chain")
			}
			problems = append(problems, r+": "+err.Error())
		}
		if !moved {
			break
		}
	}
	return "", fmt.Errorf("your secret vote is not counted: %s. Try another relayer (--relay, or TERNCHAIN_VOTE_RELAYS=URL1,URL2), or vote by name with --open", strings.Join(problems, "; "))
}
