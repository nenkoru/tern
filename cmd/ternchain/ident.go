// Identities on this machine: one directory each (an owner or an agent).
//
//	identity.json   name, kind, owner
//	eth.key         secp256k1 key: signs datagrams and transactions
//	view.key        X25519 key for tags (the owner's, shared with its agents)
//	view.prev       the view key a rekey replaced, kept to find mail in flight (a day; rekey.go)
//	epochs/N.key    one age identity per epoch; destroyed after the retention
//	state.json      scan cursor, archive, contacts, taint, group keys
package main

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"filippo.io/age"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	defaultKeysAhead   = 7  // epochs of keys published in advance (rekey.go; strict privacy: strictKeysAhead)
	publicChannelAhead = 56 // a public group's or community's channel: its keys are public anyway
	defaultRetainEp    = 7  // epochs kept before keys are destroyed
)

type IdentityFile struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"` // owner | agent
	Owner  string `json:"owner"`
	Retain uint64 `json:"retain_epochs"`
	Ahead  uint64 `json:"ahead_epochs,omitempty"` // epochs of keys published in advance (rekey.go; 0: 7)
	KeepDays int `json:"keep_days,omitempty"` // days handled mail and group posts stay in state.json (retention.go; 0: 30)
	// Model: what reads this identity's mail: "local", or "cloud:PROVIDER" (localmodel.go); "" undeclared.
	Model         string   `json:"model,omitempty"`
	LocalOnly     []string `json:"local_only,omitempty"`      // names and group:G whose content is local-model only
	LocalOnlyMode string   `json:"local_only_mode,omitempty"` // withhold (default) | warn
	// A smart account (ERC-4337) holds this name on chain; the key above then
	// signs mail (owners) or is the account's session key (agents).
	PQ          bool   `json:"post_quantum,omitempty"` // publish post-quantum (hybrid) epoch keys: larger envelopes
	Account     string `json:"account,omitempty"`
	AccountInit string `json:"account_init,omitempty"` // factory call that deploys it (initCode)
	// AccountKey: the account is guarded by this identity's key (an owner's
	// key account, accounts.go enableKeyAccount), not a passkey.
	AccountKey bool `json:"account_key,omitempty"`
	// Home: the data nodes that keep this family's tag buckets (bucket.go);
	// the relay is one too. Empty: the relay.
	Home []string `json:"home,omitempty"`
	// Data nodes that let us down (dawithhold.go, 'da auto'): challenge a
	// committed batch that concerns us and no node serves (on, the default;
	// off; all: any batch we can't fetch), and re-send an envelope a node
	// signed for and didn't deliver (nil: daResendDefault times; 0: never).
	DAAutoChallenge string `json:"da_autochallenge,omitempty"`
	DAResend        *int   `json:"da_resend,omitempty"`
}

type Ident struct {
	dir  string
	f    IdentityFile
	key  *ecdsa.PrivateKey
	view *ecdh.PrivateKey
	// the view key a rekey replaced: finds (never opens) mail in flight
	// through epoch prevUntil, then is deleted (rekey.go)
	prevView  *ecdh.PrivateKey
	prevUntil uint64
}

type ethSigner struct{ k *ecdsa.PrivateKey }

func (s ethSigner) sign(d []byte) ([]byte, error) { return crypto.Sign(d, s.k) }

func (i *Ident) addr() common.Address { return crypto.PubkeyToAddress(i.key.PublicKey) }
func (i *Ident) id() common.Hash      { return idOf(i.f.Name) }
func (i *Ident) isOwner() bool        { return i.f.Kind == "owner" }

func newIdentDir(dir string, f IdentityFile) (*Ident, error) { return newIdentDirKey(dir, f, nil) }

// newIdentDirKey: a new identity folder holding key (nil: a fresh one).
func newIdentDirKey(dir string, f IdentityFile, k *ecdsa.PrivateKey) (*Ident, error) {
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err == nil {
		return nil, fmt.Errorf("%s already holds an identity (from an earlier run, or a network that was reset?): use another folder, or move this one away and start again", dir)
	}
	if err := os.MkdirAll(filepath.Join(dir, "epochs"), 0o700); err != nil {
		return nil, err
	}
	var err error
	if k == nil {
		if k, err = crypto.GenerateKey(); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "eth.key"), []byte(keyHex(k)), 0o600); err != nil {
		return nil, err
	}
	if f.Retain == 0 {
		f.Retain = defaultRetainEp
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), b, 0o600); err != nil {
		return nil, err
	}
	return loadIdent(dir)
}

func loadIdent(dir string) (*Ident, error) {
	b, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil {
		return nil, fmt.Errorf("no identity in %s: %w", dir, err)
	}
	i := &Ident{dir: dir}
	if err := json.Unmarshal(b, &i.f); err != nil {
		return nil, err
	}
	kb, err := os.ReadFile(filepath.Join(dir, "eth.key"))
	if err != nil {
		return nil, err
	}
	if i.key, err = crypto.HexToECDSA(strings.TrimSpace(string(kb))); err != nil {
		return nil, err
	}
	if vb, err := os.ReadFile(filepath.Join(dir, "view.key")); err == nil {
		raw, _ := hex.DecodeString(strings.TrimSpace(string(vb)))
		if i.view, err = ecdh.X25519().NewPrivateKey(raw); err != nil {
			return nil, err
		}
	}
	i.loadPrevView()
	return i, nil
}

func (i *Ident) setView(v *ecdh.PrivateKey) error {
	i.view = v
	return os.WriteFile(filepath.Join(i.dir, "view.key"), []byte(hex.EncodeToString(v.Bytes())), 0o600)
}

func newViewKey() (*ecdh.PrivateKey, error) { return ecdh.X25519().GenerateKey(rand.Reader) }

// ---- epoch keys

func (i *Ident) epochPath(e uint64) string {
	return filepath.Join(i.dir, "epochs", strconv.FormatUint(e, 10)+".key")
}

// ensureEpochs creates keys for epochs [from, from+n) and returns their recipients.
func (i *Ident) ensureEpochs(from uint64, n int) ([]string, error) {
	var out []string
	for e := from; e < from+uint64(n); e++ {
		p := i.epochPath(e)
		var rec string
		if b, err := os.ReadFile(p); err == nil {
			if _, rec, err = parseIdentity(string(b)); err != nil {
				return nil, err
			}
		} else {
			var id string
			if id, rec, err = newEpochIdentity(i.f.PQ); err != nil {
				return nil, err
			}
			// Created only if absent (a link is atomic): two processes topping
			// up at once (a scan here, a long-running server there) must not
			// each publish a key of their own for the same epoch.
			tmp := fmt.Sprintf("%s.%d.tmp", p, os.Getpid())
			if err := os.WriteFile(tmp, []byte(id+"\n"), 0o600); err != nil {
				return nil, err
			}
			err := os.Link(tmp, p)
			os.Remove(tmp)
			if os.IsExist(err) {
				b, rerr := os.ReadFile(p)
				if rerr != nil {
					return nil, rerr
				}
				if _, rec, err = parseIdentity(string(b)); err != nil {
					return nil, err
				}
			} else if err != nil {
				return nil, err
			}
		}
		out = append(out, rec)
	}
	return out, nil
}

// identities returns every epoch key still on disk.
func (i *Ident) identities() []age.Identity {
	var out []age.Identity
	ents, _ := os.ReadDir(filepath.Join(i.dir, "epochs"))
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(i.dir, "epochs", e.Name()))
		if err != nil {
			continue
		}
		if id, _, err := parseIdentity(string(b)); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// destroyOld deletes epoch keys older than the retention: after this, mail
// sealed in those epochs opens for nobody, wherever copies of it are kept.
func (i *Ident) destroyOld(current uint64) ([]uint64, error) {
	var gone []uint64
	ents, _ := os.ReadDir(filepath.Join(i.dir, "epochs"))
	for _, e := range ents {
		n, err := strconv.ParseUint(strings.TrimSuffix(e.Name(), ".key"), 10, 64)
		if err != nil {
			continue
		}
		if n+i.retain() < current {
			p := filepath.Join(i.dir, "epochs", e.Name())
			// overwrite before unlinking; real devices should keep these in hardware
			if st, err := os.Stat(p); err == nil {
				os.WriteFile(p, make([]byte, st.Size()), 0o600)
			}
			if err := os.Remove(p); err != nil {
				return gone, err
			}
			gone = append(gone, n)
		}
	}
	sort.Slice(gone, func(a, b int) bool { return gone[a] < gone[b] })
	return gone, nil
}

// ---- grant bundles: what the owner hands to an agent

type AgentRequest struct {
	Name    string `json:"name"` // the agent label, e.g. "ada"
	Owner   string `json:"owner"`
	Address string `json:"address"`
	Account bool   `json:"account,omitempty"` // Address is a smart account: gas comes from the owner's paymaster budget
}

type GrantBundle struct {
	Agent       string `json:"agent"` // full name, "ada.oscar"
	Owner       string `json:"owner"`
	ViewKey     string `json:"view_key"`               // hex, the owner's view private key
	StealthView string `json:"stealth_view,omitempty"` // detects the owner's private payments; can't spend
	PQ          bool   `json:"post_quantum,omitempty"` // the family's key kind (the owner's choice)
	Gas         string `json:"-"`                      // what grant did about the owner's gas deposit (shown, not kept)
}

// writeJSON writes atomically (a temporary file, then a rename): a command
// reading it while another writes never sees half a file.
func writeJSON(path string, v any) error {
	b, _ := json.MarshalIndent(v, "", "  ")
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

var errNotJoined = errors.New("agent has not joined yet: ask the owner to grant it, then run join")
