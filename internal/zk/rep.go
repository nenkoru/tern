package zk

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark/backend/groth16"
	"github.com/consensys/gnark/constraint"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/frontend/cs/r1cs"
	stdp2 "github.com/consensys/gnark/std/permutation/poseidon2"
)

const (
	RepDepth = 16  // the private reviews' tree (PrivateReviews.sol): 2^16 reviews
	RepSlots = 8   // deals one proof counts
	RepScore = 200 // scores are -100..100, stored +100
)

// RepCircuit: "Count distinct paid deals of the private reviews' tree are
// mine, and their scores (each +100) add up to at least MinTotal", with a
// nullifier H(Scope, s) for the verifier that chose Scope. A review's leaf is
// H(H(s, lock), score+100): s is the payee's reputation secret, given to
// nobody; H(s, lock) is what the payee gave the payer for that one deal.
// Which leaves, which locks and which counterparts stay hidden.
type RepCircuit struct {
	Root      frontend.Variable `gnark:",public"`
	Count     frontend.Variable `gnark:",public"`
	MinTotal  frontend.Variable `gnark:",public"`
	Scope     frontend.Variable `gnark:",public"`
	Nullifier frontend.Variable `gnark:",public"`
	Message   frontend.Variable `gnark:",public"`

	Secret frontend.Variable
	Active [RepSlots]frontend.Variable
	Lock   [RepSlots]frontend.Variable
	Score  [RepSlots]frontend.Variable
	Path   [RepSlots][RepDepth]frontend.Variable
	Bits   [RepSlots][RepDepth]frontend.Variable
}

func (c *RepCircuit) Define(api frontend.API) error {
	h, err := stdp2.NewPoseidon2(api)
	if err != nil {
		return err
	}
	count, total := frontend.Variable(0), frontend.Variable(0)
	for i := 0; i < RepSlots; i++ {
		api.AssertIsBoolean(c.Active[i])
		api.AssertIsLessOrEqual(c.Score[i], RepScore)
		leaf := hashC(api, h, hashC(api, h, c.Secret, c.Lock[i]), c.Score[i])
		root := rootC(api, h, leaf, c.Path[i][:], c.Bits[i][:])
		api.AssertIsEqual(api.Select(c.Active[i], root, c.Root), c.Root) // an inactive slot proves nothing
		count = api.Add(count, c.Active[i])
		total = api.Add(total, api.Mul(c.Active[i], c.Score[i]))
		for j := 0; j < i; j++ { // distinct deals: one lock counts once
			both := api.Mul(c.Active[i], c.Active[j])
			api.AssertIsDifferent(api.Select(both, api.Sub(c.Lock[i], c.Lock[j]), 1), 0)
		}
	}
	api.AssertIsEqual(count, c.Count)
	api.AssertIsLessOrEqual(c.MinTotal, total)
	api.AssertIsEqual(c.Nullifier, hashC(api, h, c.Scope, c.Secret))
	api.Mul(c.Message, c.Message) // binds the presentation
	return nil
}

// The rep circuit's keys are not in the binary (19 MB) nor in the network's
// MPC ceremony (its phase 1 is too small for 37k constraints): a network's
// operator makes them alone (SetupRep: ternchain-ops ceremony rep, or solo),
// like a test network's other keys, and hands them out in zk-keys/; a devnet
// makes them on first use, in TERNCHAIN_ZK_DEVNET_DIR or the user's cache
// (one machine's devnet: proofs check where the keys are).

var (
	repMu  sync.Mutex
	repCS  constraint.ConstraintSystem
	repErr error
)

func repConstraints() (constraint.ConstraintSystem, error) {
	repMu.Lock()
	defer repMu.Unlock()
	if repCS == nil && repErr == nil {
		repCS, repErr = frontend.Compile(ecc.BN254.ScalarField(), r1cs.NewBuilder, &RepCircuit{})
	}
	return repCS, repErr
}

// SetupRep writes rep.pk and rep.vk into dir: a single-party setup (whoever
// runs it can forge reputation proofs for the network that uses its keys).
func SetupRep(dir string) error {
	cs, err := repConstraints()
	if err != nil {
		return err
	}
	pk, vk, err := groth16.Setup(cs)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var b bytes.Buffer
	pk.WriteRawTo(&b)
	if err := os.WriteFile(filepath.Join(dir, "rep.pk"), b.Bytes(), 0o644); err != nil {
		return err
	}
	b.Reset()
	vk.WriteTo(&b)
	return os.WriteFile(filepath.Join(dir, "rep.vk"), b.Bytes(), 0o644)
}

// DevnetRepDir: where a devnet's rep keys are made on first use.
func DevnetRepDir() string {
	if d := os.Getenv("TERNCHAIN_ZK_DEVNET_DIR"); d != "" {
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "ternchain", "zk-devnet")
}

// loadRep: the rep circuit's keys, from the ceremony's directory, or a devnet's (made if missing).
func loadRep() (*zkKeys, error) {
	cs, err := repConstraints()
	if err != nil {
		return nil, err
	}
	dir := keySrc.dir
	if dir == "" {
		dir = DevnetRepDir()
		if _, err := os.Stat(filepath.Join(dir, "rep.vk")); err != nil {
			if err := SetupRep(dir); err != nil {
				return nil, err
			}
		}
	}
	k := &zkKeys{ccs: cs, pk: groth16.NewProvingKey(ecc.BN254), vk: groth16.NewVerifyingKey(ecc.BN254)}
	pb, err := os.ReadFile(filepath.Join(dir, "rep.pk"))
	if err != nil {
		return nil, fmt.Errorf("this network's zk keys have no rep circuit (%s): private reputation is off here; its operator makes them with ternchain-ops ceremony rep --keys DIR", dir)
	}
	if _, err := k.pk.UnsafeReadFrom(bytes.NewReader(pb)); err != nil {
		return nil, err
	}
	vb, err := os.ReadFile(filepath.Join(dir, "rep.vk"))
	if err != nil {
		return nil, err
	}
	if _, err := k.vk.ReadFrom(bytes.NewReader(vb)); err != nil {
		return nil, err
	}
	return k, nil
}
