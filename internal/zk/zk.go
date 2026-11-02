// Zero knowledge: Poseidon2 hashing, fixed-depth Merkle trees, and the two
// circuits, proved with gnark Groth16 on BN254 and checked on chain by the
// verifiers gnark exports (contracts/PoolVerifier.sol, VoteVerifier.sol):
//
//   - withdraw (Privacy Pools, the 0xbow design): spend a note of the pool's
//     state tree, prove its label is in an association set the withdrawer
//     picked, reveal its nullifier hash, and leave the change as a new note;
//   - vote (Semaphore v4 shape): a citizen's identity is in the citizens'
//     tree both when a proposal opened and now; nullifier = H(scope, secret).
//
// Hashes (circuit, Go and contracts/Poseidon2.sol agree, a test checks it):
//
//	compress(l, r) = Poseidon2-BN254[t=2, rF=6, rP=50](l, r)[1] + r   (gnark-crypto's Compress)
//	H(x1..xn)      = compress(…compress(compress(n, x1), x2)…, xn)    (Merkle–Damgård, IV = arity)
//	tree node      = compress(left, right); empty leaf 0
//
// The keys in keys/ come from a local, single-party setup (zk_setup_test.go):
// fine for a devnet, NOT for production (docs/PRIVACY.md); they are refused off the
// devnet chain ids, and a network with value proves with a ceremony's keys
// (setup.go, UseKeys; ternchain ceremony).
package zk

import (
	"bytes"
	"crypto/ecdsa"
	"embed"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	p2 "github.com/consensys/gnark-crypto/ecc/bn254/fr/poseidon2"
	edbn254 "github.com/consensys/gnark-crypto/ecc/bn254/twistededwards"
	"github.com/consensys/gnark/backend/groth16"
	groth16bn254 "github.com/consensys/gnark/backend/groth16/bn254"
	"github.com/consensys/gnark/constraint"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/logger"
	_ "github.com/consensys/gnark/std/math/bits" // registers its solver hints (below)
	stdp2 "github.com/consensys/gnark/std/permutation/poseidon2"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	PoolDepth  = 20 // the pool's state tree: 2^20 notes
	ASPDepth   = 20 // an association set: 2^20 labels
	GroupDepth = 16 // the citizens' tree: 2^16 identities
)

var Modulus = ecc.BN254.ScalarField()

// gnark logs to stdout (the CLI prints JSON there). The vote circuit's
// solver hints (bits decomposition) are registered by the bits package's
// init: a CLI that loads the compiled circuit from its file never compiles
// it, so without that import every secret or --open citizen vote failed with
// "solver missing hint(s): bits.nBits".
func init() { logger.Disable() }

var perm = p2.NewDefaultPermutation()

func feOf(x *big.Int) fr.Element {
	var e fr.Element
	e.SetBigInt(x)
	return e
}

// Compress: the 2-to-1 Poseidon2 compression (gnark-crypto's Compress).
func Compress(l, r *big.Int) *big.Int {
	x := [2]fr.Element{feOf(l), feOf(r)}
	right := x[1]
	perm.Permutation(x[:])
	x[1].Add(&x[1], &right)
	return x[1].BigInt(new(big.Int))
}

// HashN: Merkle–Damgård over compress, the arity as IV (domain separation).
func HashN(xs ...*big.Int) *big.Int {
	s := big.NewInt(int64(len(xs)))
	for _, x := range xs {
		s = Compress(s, x)
	}
	return s
}

// mod reduces any 256-bit value into the field (keccak outputs, scopes).
func ModP(b []byte) *big.Int { return new(big.Int).Mod(new(big.Int).SetBytes(b), Modulus) }

// ---- Merkle trees: fixed depth, empty leaves 0 (Tornado / Semaphore v3 layout)

var (
	zerosOnce sync.Once
	zeros     []*big.Int // zeros[i]: the root of an empty subtree of height i
)

func ZeroAt(i int) *big.Int {
	zerosOnce.Do(func() {
		zeros = []*big.Int{big.NewInt(0)}
		for j := 1; j <= 32; j++ {
			zeros = append(zeros, Compress(zeros[j-1], zeros[j-1]))
		}
	})
	return zeros[i]
}

type Tree struct {
	depth  int
	leaves []*big.Int
	lv     [][]*big.Int // computed levels (nil: stale)
}

func NewTree(depth int, leaves []*big.Int) *Tree {
	return &Tree{depth: depth, leaves: leaves}
}

func (t *Tree) build() {
	if t.lv != nil {
		return
	}
	cur := append([]*big.Int(nil), t.leaves...)
	t.lv = [][]*big.Int{cur}
	for d := 0; d < t.depth; d++ {
		next := make([]*big.Int, (len(cur)+1)/2)
		for i := range next {
			l, r := cur[2*i], ZeroAt(d)
			if 2*i+1 < len(cur) {
				r = cur[2*i+1]
			}
			next[i] = Compress(l, r)
		}
		cur = next
		t.lv = append(t.lv, cur)
	}
}

func (t *Tree) Root() *big.Int {
	if len(t.leaves) == 0 {
		return ZeroAt(t.depth)
	}
	t.build()
	return t.lv[t.depth][0]
}

// Proof: the siblings from the leaf up and the path bits (1: we are the right child).
func (t *Tree) Proof(i int) ([]*big.Int, []int, error) {
	if i < 0 || i >= len(t.leaves) {
		return nil, nil, fmt.Errorf("leaf %d not in a tree of %d", i, len(t.leaves))
	}
	t.build()
	path, bits := make([]*big.Int, t.depth), make([]int, t.depth)
	idx := i
	for d := 0; d < t.depth; d++ {
		sib := idx ^ 1
		if sib < len(t.lv[d]) {
			path[d] = t.lv[d][sib]
		} else {
			path[d] = ZeroAt(d)
		}
		bits[d] = idx & 1
		idx >>= 1
	}
	return path, bits, nil
}

func (t *Tree) IndexOf(leaf *big.Int) int {
	for i, l := range t.leaves {
		if l.Cmp(leaf) == 0 {
			return i
		}
	}
	return -1
}

// ---- circuits

func hashC(api frontend.API, h *stdp2.Permutation, xs ...frontend.Variable) frontend.Variable {
	var s frontend.Variable = len(xs)
	for _, x := range xs {
		s = h.Compress(s, x)
	}
	return s
}

func rootC(api frontend.API, h *stdp2.Permutation, leaf frontend.Variable, path, bits []frontend.Variable) frontend.Variable {
	cur := leaf
	for i := range path {
		api.AssertIsBoolean(bits[i])
		l := api.Select(bits[i], path[i], cur)
		r := api.Select(bits[i], cur, path[i])
		cur = h.Compress(l, r)
	}
	return cur
}

// WithdrawCircuit: privacy-pools-core's withdraw.circom, with fixed-depth trees.
// Public, in this order: the change note, the spent note's nullifier hash,
// the amount withdrawn, the state root, the association set root, and the
// context (a hash of the withdrawal's recipient, relayer and fee).
type WithdrawCircuit struct {
	NewCommitment frontend.Variable `gnark:",public"`
	NullifierHash frontend.Variable `gnark:",public"`
	Value         frontend.Variable `gnark:",public"`
	StateRoot     frontend.Variable `gnark:",public"`
	ASPRoot       frontend.Variable `gnark:",public"`
	Context       frontend.Variable `gnark:",public"`

	Label, ExistingValue, ExistingNullifier, ExistingSecret, NewNullifier, NewSecret frontend.Variable
	StatePath, StateBits                                                             [PoolDepth]frontend.Variable
	ASPPath, ASPBits                                                                 [ASPDepth]frontend.Variable
}

func (c *WithdrawCircuit) Define(api frontend.API) error {
	h, err := stdp2.NewPoseidon2(api)
	if err != nil {
		return err
	}
	pre := hashC(api, h, c.ExistingNullifier, c.ExistingSecret)
	note := hashC(api, h, c.ExistingValue, c.Label, pre)
	api.AssertIsEqual(c.NullifierHash, hashC(api, h, c.ExistingNullifier))
	api.AssertIsEqual(rootC(api, h, note, c.StatePath[:], c.StateBits[:]), c.StateRoot)
	api.AssertIsEqual(rootC(api, h, c.Label, c.ASPPath[:], c.ASPBits[:]), c.ASPRoot)
	remaining := api.Sub(c.ExistingValue, c.Value)
	api.ToBinary(c.Value, 128)   // 0 ≤ value < 2^128
	api.ToBinary(remaining, 128) // value ≤ the note's value: no coins from nothing
	api.AssertIsDifferent(c.NewNullifier, c.ExistingNullifier)
	api.AssertIsEqual(c.NewCommitment, hashC(api, h, remaining, c.Label, hashC(api, h, c.NewNullifier, c.NewSecret)))
	api.Mul(c.Context, c.Context) // binds the context: a relayer can't change recipient or fee
	return nil
}

// VoteCircuit: Semaphore v4's circuit, with two roots: the citizens' tree
// when the proposal opened (citizens from before it) and now (still a
// citizen: a revoked one can't vote once the root moves). Identity: secret s
// < l, public key s·G on Baby Jubjub (gnark's BN254 twisted Edwards curve),
// commitment H(pk.x, pk.y); nullifier H(scope, s).
type VoteCircuit struct {
	RootOpen  frontend.Variable `gnark:",public"`
	RootNow   frontend.Variable `gnark:",public"`
	Nullifier frontend.Variable `gnark:",public"`
	Scope     frontend.Variable `gnark:",public"`
	Message   frontend.Variable `gnark:",public"`

	Secret             frontend.Variable
	OpenPath, OpenBits [GroupDepth]frontend.Variable
	NowPath, NowBits   [GroupDepth]frontend.Variable
}

func (c *VoteCircuit) Define(api frontend.API) error {
	h, err := stdp2.NewPoseidon2(api)
	if err != nil {
		return err
	}
	ed := edbn254.GetEdwardsCurve()
	order := ed.Order
	api.AssertIsLessOrEqual(c.Secret, new(big.Int).Sub(&order, big.NewInt(1))) // s < l: one key, one nullifier
	bits := api.ToBinary(c.Secret, order.BitLen())
	// Fixed-base double-and-add with the multiples 2^i·G as constants.
	var x, y frontend.Variable = 0, 1
	a, d := ed.A.BigInt(new(big.Int)), ed.D.BigInt(new(big.Int))
	var g edbn254.PointAffine
	g.Set(&ed.Base)
	for i := range bits {
		gx, gy := g.X.BigInt(new(big.Int)), g.Y.BigInt(new(big.Int))
		u := api.Mul(x, y)
		t := api.Mul(u, new(big.Int).Mod(new(big.Int).Mul(new(big.Int).Mul(d, gx), gy), Modulus))
		nx := api.Div(api.Add(api.Mul(x, gy), api.Mul(y, gx)), api.Add(1, t))
		ny := api.Div(api.Sub(api.Mul(y, gy), api.Mul(x, new(big.Int).Mod(new(big.Int).Mul(a, gx), Modulus))), api.Sub(1, t))
		x, y = api.Select(bits[i], nx, x), api.Select(bits[i], ny, y)
		g.Double(&g)
	}
	commitment := hashC(api, h, x, y)
	api.AssertIsEqual(rootC(api, h, commitment, c.OpenPath[:], c.OpenBits[:]), c.RootOpen)
	api.AssertIsEqual(rootC(api, h, commitment, c.NowPath[:], c.NowBits[:]), c.RootNow)
	api.AssertIsEqual(c.Nullifier, hashC(api, h, c.Scope, c.Secret))
	api.Mul(c.Message, c.Message) // binds the vote
	return nil
}

// ---- identities (Semaphore)

// IdentitySecret: derived from the owner's key, so nothing new to keep; a
// new key (recovery) means a new identity, registered again.
func IdentitySecret(k *ecdsa.PrivateKey) *big.Int {
	ed := edbn254.GetEdwardsCurve()
	s := new(big.Int).SetBytes(crypto.Keccak256([]byte("tern-semaphore-identity-v1"), crypto.FromECDSA(k)))
	s.Mod(s, &ed.Order)
	if s.Sign() == 0 {
		s.SetInt64(1)
	}
	return s
}

func IdentityCommitment(secret *big.Int) *big.Int {
	ed := edbn254.GetEdwardsCurve()
	var pk edbn254.PointAffine
	pk.ScalarMultiplication(&ed.Base, secret)
	return HashN(pk.X.BigInt(new(big.Int)), pk.Y.BigInt(new(big.Int)))
}

// ---- keys and proofs

//go:embed keys
var zkFiles embed.FS

type zkKeys struct {
	ccs constraint.ConstraintSystem
	pk  groth16.ProvingKey
	vk  groth16.VerifyingKey
}

var (
	zkMu    sync.Mutex
	zkCache = map[string]*zkKeys{}
)

// loadKeys reads a circuit's constraint system and keys: embedded (the devnet
// setup) or from a ceremony's directory (UseKeys).
func loadKeys(name string) (*zkKeys, error) {
	zkMu.Lock()
	defer zkMu.Unlock()
	if k := zkCache[name]; k != nil {
		return k, nil
	}
	if name == "rep" { // not embedded, not in the MPC (rep.go)
		k, err := loadRep()
		if err == nil {
			zkCache[name] = k
		}
		return k, err
	}
	k := &zkKeys{ccs: groth16.NewCS(ecc.BN254), pk: groth16.NewProvingKey(ecc.BN254), vk: groth16.NewVerifyingKey(ecc.BN254)}
	b, err := readKeyFile(name, "ccs")
	if err != nil {
		return nil, err
	}
	if _, err := k.ccs.ReadFrom(bytes.NewReader(b)); err != nil {
		return nil, err
	}
	if b, err = readKeyFile(name, "pk"); err != nil {
		return nil, err
	}
	if keySrc.dir == "" {
		_, err = k.pk.UnsafeReadFrom(bytes.NewReader(b)) // our own build's
	} else {
		_, err = k.pk.ReadFrom(bytes.NewReader(b)) // a file: subgroup checks
	}
	if err != nil {
		return nil, err
	}
	if b, err = readKeyFile(name, "vk"); err != nil {
		return nil, err
	}
	if _, err := k.vk.ReadFrom(bytes.NewReader(b)); err != nil {
		return nil, err
	}
	// The proving key's setup elements are the verifying key's (a proving key
	// with another δ would make proofs that don't verify, or, with δ = 0,
	// proofs that don't hide the witness).
	pk, okp := k.pk.(*groth16bn254.ProvingKey)
	vk, okv := k.vk.(*groth16bn254.VerifyingKey)
	if !okp || !okv || pk.G1.Delta.IsInfinity() || !pk.G1.Delta.Equal(&vk.G1.Delta) || !pk.G2.Delta.Equal(&vk.G2.Delta) ||
		!pk.G1.Alpha.Equal(&vk.G1.Alpha) || !pk.G1.Beta.Equal(&vk.G1.Beta) || !pk.G2.Beta.Equal(&vk.G2.Beta) {
		return nil, fmt.Errorf("the %s proving key doesn't belong to its verifying key", name)
	}
	zkCache[name] = k
	return k, nil
}

// LastProveTime: how long the last proof took (for docs/PRIVACY.md's numbers and the CLI's output).
var LastProveTime time.Duration

// Prove makes a proof for a full assignment, checks it, and returns it in
// the 256 bytes gnark's Solidity verifier takes.
func Prove(name string, assignment frontend.Circuit) ([]byte, error) {
	pr, err := prove(name, assignment)
	if err != nil {
		return nil, err
	}
	p, ok := pr.(*groth16bn254.Proof)
	if !ok {
		return nil, errors.New("not a BN254 proof")
	}
	return p.MarshalSolidity(), nil
}

// ProveRaw: a proof in gnark's own (compressed) encoding, for proofs that
// readers check off chain with Verify: anonymous group posts, group rekeys.
func ProveRaw(name string, assignment frontend.Circuit) ([]byte, error) {
	pr, err := prove(name, assignment)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if _, err := pr.WriteTo(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Verify checks a ProveRaw proof; public holds the circuit's public inputs
// (the other fields are ignored).
func Verify(name string, proof []byte, public frontend.Circuit) error {
	k, err := loadKeys(name)
	if err != nil {
		return err
	}
	pr := groth16.NewProof(ecc.BN254)
	if _, err := pr.ReadFrom(bytes.NewReader(proof)); err != nil {
		return fmt.Errorf("bad proof: %w", err)
	}
	w, err := frontend.NewWitness(public, ecc.BN254.ScalarField(), frontend.PublicOnly())
	if err != nil {
		return err
	}
	return groth16.Verify(pr, k.vk, w)
}

func prove(name string, assignment frontend.Circuit) (groth16.Proof, error) {
	k, err := loadKeys(name)
	if err != nil {
		return nil, err
	}
	w, err := frontend.NewWitness(assignment, ecc.BN254.ScalarField())
	if err != nil {
		return nil, err
	}
	t := time.Now()
	pr, err := groth16.Prove(k.ccs, k.pk, w)
	if err != nil {
		return nil, fmt.Errorf("proving: the inputs don't satisfy the %s circuit (%v)", name, err)
	}
	LastProveTime = time.Since(t)
	pub, err := w.Public()
	if err != nil {
		return nil, err
	}
	if err := groth16.Verify(pr, k.vk, pub); err != nil {
		return nil, fmt.Errorf("own proof doesn't verify: %w", err)
	}
	return pr, nil
}

func Bigs(xs []*big.Int) []frontend.Variable {
	out := make([]frontend.Variable, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

func Ints(xs []int) []frontend.Variable {
	out := make([]frontend.Variable, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}
