// Anonymous groups' secrets: our identity in each group, random and kept
// apart from the account key (red team A-7, A-8).
//
// Our identity in a group is a scalar s: the leaf is H(s·G), the pseudonym
// and the unlinkable slots' nullifiers are H(scope, s). It used to be
// keccak(eth key, gid), so whoever imaged eth.key, even after the person
// left, recomputed the commitment, found it in the public Leaves events of
// every group she ever joined and recomputed the pseudonyms of her posts.
// Now s comes from crypto/rand when we create or join a group and lives in
// anon.json in the identity folder, beside eth.key. Not in state.json: a
// --state-dir starts from a copy of state.json, so a leave would leave the
// secret behind in the copy. Leaving deletes it: the device then holds
// nothing that finds the leaf. The cost: a new device restored from eth.key
// alone is in no anonymous group (copy anon.json with the key, or ask to be
// invited again).
//
// anon.json changes only under its own lock and is read fresh each time, so
// a long-running MCP server never writes back an old copy (losing a join's
// secret, or reviving one a leave destroyed).
//
// Groups joined before: no stored secret, so the old derivation, if its
// commitment is a leaf, stored marked legacy. group anon-migrate NAME sends
// a fresh commitment; the admin's group admit removes the old leaf and adds
// the new one (AnonGroups.remove, .add). What the legacy secret put on chain
// stays linkable to eth.key for good: the old leaf in the Leaves events and
// the nullifiers of every post made with it.
package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	edbn254 "github.com/consensys/gnark-crypto/ecc/bn254/twistededwards"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern/internal/zk"
)

const anonSecretsFile = "anon.json" // in the identity folder, 0600, like eth.key

// AnonSecret: our identity in one anonymous group (anon.json, by gid).
type AnonSecret struct {
	Secret string `json:"secret"`           // hex, s < l
	Legacy bool   `json:"legacy,omitempty"` // derived from eth.key (joined before random secrets): group anon-migrate NAME
	Next   string `json:"next,omitempty"`   // a migration's fresh secret, used once the admin adds its leaf
}

const anonLegacyWarning = "your identity in this group was derived from your account key (eth.key): anyone who gets eth.key " +
	"finds your leaf in the group's public history and recomputes the pseudonym and nullifiers of every post you made with it. " +
	"Migrating gives you a random identity for what you post from now on; it can't unlink what is already on chain"

func anonKey(gid common.Hash) string { return strings.ToLower(gid.Hex()) }

func hexBig(s string) *big.Int {
	if s == "" {
		return nil
	}
	b, ok := new(big.Int).SetString(s, 16)
	if !ok || b.Sign() <= 0 {
		return nil
	}
	return b
}

// newAnonSecret: a random identity scalar, 0 < s < l.
func newAnonSecret() *big.Int {
	ed := edbn254.GetEdwardsCurve()
	for {
		s, err := rand.Int(rand.Reader, &ed.Order)
		if err != nil {
			panic(err)
		}
		if s.Sign() > 0 {
			return s
		}
	}
}

// legacyAnonSecret: the identity derived from the account key, as before
// random secrets (groups joined then still use it until migrated).
func legacyAnonSecret(key *ecdsa.PrivateKey, gid common.Hash) *big.Int {
	ed := edbn254.GetEdwardsCurve()
	s := new(big.Int).SetBytes(crypto.Keccak256([]byte("tern-anon-group-v1"), crypto.FromECDSA(key), gid[:]))
	s.Mod(s, &ed.Order)
	if s.Sign() == 0 {
		s.SetInt64(1)
	}
	return s
}

// anonSecrets runs f on anon.json under its lock; f says whether it changed
// the map, which is then written (the file goes once empty).
func (c *Client) anonSecrets(f func(m map[string]*AnonSecret) bool) error {
	path := filepath.Join(c.dir, anonSecretsFile)
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	m := map[string]*AnonSecret{}
	if err := readJSON(path, &m); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%s: %w", path, err)
	}
	if m == nil {
		m = map[string]*AnonSecret{}
	}
	if !f(m) {
		return nil
	}
	if len(m) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeJSON(path, m)
}

// newAnonIdentity: our identity for a join or a group's creation: the one
// stored (a join asked again), else a fresh random one, stored first.
func (c *Client) newAnonIdentity(gid common.Hash) (*big.Int, error) {
	var s *big.Int
	err := c.anonSecrets(func(m map[string]*AnonSecret) bool {
		if e := m[anonKey(gid)]; e != nil && !e.Legacy {
			if s = hexBig(e.Secret); s != nil {
				return false
			}
		}
		s = newAnonSecret()
		m[anonKey(gid)] = &AnonSecret{Secret: s.Text(16)}
		return true
	})
	return s, err
}

// anonIdentity: our secret in group gid, whose tree is s (nil: none of
// its leaves is ours). A migration's fresh secret takes over once its leaf
// is in; a group joined before random secrets gets the old derivation,
// stored marked legacy. legacy: the one returned is derived from eth.key.
func (c *Client) anonIdentity(gid common.Hash, s *anonState) (secret *big.Int, legacy bool, err error) {
	isLeaf := func(x *big.Int) bool {
		if x == nil {
			return false
		}
		cm := zk.IdentityCommitment(x)
		for _, l := range s.leaves {
			if l.Cmp(cm) == 0 {
				return true
			}
		}
		return false
	}
	found := false
	err = c.anonSecrets(func(m map[string]*AnonSecret) bool {
		k := anonKey(gid)
		e := m[k]
		if e == nil {
			ls := legacyAnonSecret(c.key, gid)
			if !isLeaf(ls) {
				return false
			}
			m[k] = &AnonSecret{Secret: ls.Text(16), Legacy: true}
			secret, legacy, found = ls, true, true
			return true
		}
		if n := hexBig(e.Next); n != nil && isLeaf(n) {
			e.Secret, e.Next, e.Legacy = e.Next, "", false
			secret = n
			return true
		}
		secret, legacy = hexBig(e.Secret), e.Legacy
		return false
	})
	if found {
		fmt.Fprintf(os.Stderr, "warning: anonymous group %s: %s. Run group anon-migrate NAME (docs/PRIVACY.md, Anonymous groups)\n", gid.Hex()[:10], anonLegacyWarning)
	}
	return secret, legacy, err
}

// dropAnonIdentity: we left group gid: its secrets go with its keys.
func (c *Client) dropAnonIdentity(gid common.Hash) error {
	return c.anonSecrets(func(m map[string]*AnonSecret) bool {
		if m[anonKey(gid)] == nil {
			return false
		}
		delete(m, anonKey(gid))
		return true
	})
}

// migrateAnon (group anon-migrate NAME): a fresh random identity for a
// group joined with the legacy one. A member sends the new commitment to
// the admin like a join; the admin's next group admit removes the old leaf
// and adds the new one. Until then we post with the old identity.
func (c *Client) migrateAnon(name string) (map[string]any, error) {
	g := c.st.Groups[name]
	if g == nil || g.Anon == nil {
		return nil, fmt.Errorf("%s is not an anonymous group you are in", name)
	}
	gid := g.Anon.gid()
	s, err := c.anonTree(gid)
	if err != nil {
		return nil, err
	}
	cur, legacy, err := c.anonIdentity(gid, s)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, fmt.Errorf("you are not a leaf of %s's tree (removed, or not admitted yet): nothing to migrate", name)
	}
	if !legacy {
		return map[string]any{"group": name, "status": "your identity in " + name + " is already a random one, not derived from your account key: nothing to do"}, nil
	}
	var next *big.Int
	if err := c.anonSecrets(func(m map[string]*AnonSecret) bool {
		e := m[anonKey(gid)]
		if e == nil {
			return false
		}
		if next = hexBig(e.Next); next != nil { // asked before: the same one again
			return false
		}
		next = newAnonSecret()
		e.Next = next.Text(16)
		return true
	}); err != nil {
		return nil, err
	}
	if next == nil {
		return nil, fmt.Errorf("your identity in %s is gone from this device (you left it)", name)
	}
	cm := zk.IdentityCommitment(next).String()
	r := map[string]any{"group": name, "warning": anonLegacyWarning + ". Your pseudonym changes; posts made with the old identity stay attributable to eth.key"}
	if g.Anon.Members != nil && g.Anon.Admin == c.f.Name { // our own group: swap our leaf now
		g.Anon.Pending[c.f.Name] = cm
		if _, err := c.admitAnon(g); err != nil {
			return nil, err
		}
		r["status"] = "migrated: your old leaf is removed and the new one added; group admit rotates the keys"
		return r, nil
	}
	if _, err := c.sendDatagram(tAnonJoin, g.Anon.Admin, AnonJoin{Gid: g.Anon.Gid, Commitment: cm}); err != nil {
		return nil, err
	}
	r["status"] = "requested: " + g.Anon.Admin + "'s next group admit removes your old leaf and adds the new one; until then you post with the old identity"
	return r, nil
}

// anonMember: name is a member of g the admin admitted (its own bookkeeping).
func (c *Client) anonMember(g *GroupKeys, name string) bool {
	for _, n := range g.Anon.Members {
		if n == name {
			return true
		}
	}
	return false
}

// anonSwapRefused: why the admin won't swap a member's leaf now ("": it
// will). A new identity is a new pseudonym, so in a pseudonymous group with
// bans it could shed one: the admin doesn't know whose pseudonym it banned.
func (c *Client) anonSwapRefused(g *GroupKeys) string {
	if g.Anon.Mode == anonUnlinkable {
		return ""
	}
	s, err := c.anonTree(g.Anon.gid())
	if err != nil {
		return "the group's tree can't be read now (" + err.Error() + "): it asks again with group anon-migrate"
	}
	if len(s.banned) > 0 {
		return fmt.Sprintf("%s has %d banned pseudonym(s) and a new identity would shed a ban on the old one: "+
			"to let it, unban them, or remove the member and invite it again", g.Name, len(s.banned))
	}
	return ""
}
