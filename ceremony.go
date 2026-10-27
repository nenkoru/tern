package main

// The zk keys' trusted setup (audit 5 ZK-4). The keys embedded in the binary
// (internal/zk/keys) come from a single-party setup: whoever ran it can forge
// withdrawals from the pool and secret votes. They are the devnet keys, and
// they stay devnet keys: the verifier contracts carry CEREMONY = 0 and their
// constructor refuses to deploy off the devnet chain ids (zk.DevnetChainIDs),
// deploy refuses them before that, and a client refuses to prove with them
// on any other chain.
//
// A network with value runs a Groth16 MPC (gnark's mpcsetup: phase 1 powers of
// tau, then phase 2 per circuit; secure if one contributor of each phase
// deleted their randomness):
//
//	ternchain ceremony init --out t0.mpc                          phase 1 opens
//	ternchain ceremony contribute --in t0.mpc --out t1.mpc [--name WHO]   (each contributor, one after another)
//	ternchain ceremony init --phase2 --in tK.mpc --beacon HEX --out p0.mpc   phase 1 closes, phase 2 opens
//	ternchain ceremony contribute --in p0.mpc --out p1.mpc [--name WHO]   (both circuits in one go)
//	ternchain ceremony verify --in FILE [--keys DIR]               every contribution, the beacons, the keys
//	ternchain ceremony finalize --in pM.mpc --beacon HEX --keys DIR    the keys, verifiers and CEREMONY
//
// One file in, one file out: a contributor gets the latest transcript, runs
// contribute on their own machine and publishes the file it writes and the
// hash it prints; the next contributor starts from that file. The transcript
// keeps every contribution, so anyone can verify the whole chain. Each beacon
// is a public value nobody knows before the last contribution of its phase
// (say, the hash of a block announced in advance); it closes the phase.
//
// finalize writes DIR: NAME.pk and NAME.vk per circuit, PoolVerifier.sol and
// VoteVerifier.sol (zk.VerifierSolidity, CEREMONY = the final transcript's
// hash), CEREMONY and transcript.mpc. Then:
//
//	node contracts/build-zk.js DIR                      (DIR/PoolVerifier.bin, VoteVerifier.bin)
//	TERNCHAIN_ZK_KEYS=DIR ternchain deploy …            (net.json gets "ceremony")
//
// and the network hands out net.json with DIR beside it as zk-keys/ (or
// net.json "zk_keys": a path relative to it; TERNCHAIN_ZK_KEYS overrides).
//
// Phase 1 is our own: mpcsetup's Phase1 keeps its parameters unexported, so a
// public powers of tau (e.g. the PPoT .ptau files) can't be imported and
// verified through its API; importing one would mean parsing the .ptau into
// mpcsetup.SrsCommons ourselves and checking it with pairings. Our phase 1 is
// small (2^14) and every contributor can take part in both phases.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark/backend/groth16"
	"github.com/consensys/gnark/backend/groth16/bn254/mpcsetup"
	cs "github.com/consensys/gnark/constraint/bn254"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/frontend/cs/r1cs"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"

	"github.com/nenkoru/tern/internal/zk"
)

// mpcCircuit: a circuit the ceremony makes keys for.
type mpcCircuit struct {
	name, contract string
	r1cs           *cs.R1CS
	ccs            []byte // serialized
}

func mpcCompile(name, contract string, c frontend.Circuit) (*mpcCircuit, error) {
	ccs, err := frontend.Compile(ecc.BN254.ScalarField(), r1cs.NewBuilder, c)
	if err != nil {
		return nil, fmt.Errorf("%s circuit: %w", name, err)
	}
	r, ok := ccs.(*cs.R1CS)
	if !ok {
		return nil, errors.New("not a BN254 R1CS")
	}
	var b bytes.Buffer
	if _, err := r.WriteTo(&b); err != nil {
		return nil, err
	}
	return &mpcCircuit{name: name, contract: contract, r1cs: r, ccs: b.Bytes()}, nil
}

// zkCircuits: the network's two circuits, compiled from their Define, which
// must give the constraint systems the binary proves with (audit 5 ZK-7).
func zkCircuits() ([]*mpcCircuit, error) {
	var out []*mpcCircuit
	for _, c := range []struct {
		name, contract string
		circuit        frontend.Circuit
	}{{"withdraw", "PoolVerifier", &zk.WithdrawCircuit{}}, {"vote", "VoteVerifier", &zk.VoteCircuit{}}} {
		m, err := mpcCompile(c.name, c.contract, c.circuit)
		if err != nil {
			return nil, err
		}
		emb, err := zk.EmbeddedCCS(c.name)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(m.ccs, emb) {
			return nil, fmt.Errorf("the %s circuit (internal/zk Define) doesn't compile to the embedded keys/%s.ccs: rerun the zk setup first", c.name, c.name)
		}
		out = append(out, m)
	}
	return out, nil
}

// ---- the transcript: one file, every contribution

const mpcMagic = "ternchain-mpc-v1\n"

type mpcCircuitInfo struct {
	Name        string `json:"name"`
	Contract    string `json:"contract"`
	CCS         string `json:"ccs_sha256"`
	Constraints int    `json:"constraints"`
	VK          string `json:"vk_sha256,omitempty"` // set by finalize
}

type mpcEntry struct {
	Name   string   `json:"name,omitempty"` // who says they made it (not checked)
	SHA256 []string `json:"sha256"`         // the contribution's bytes: one per circuit in phase 2
}

type mpcHeader struct {
	N        uint64           `json:"n"` // the FFT domain: the next power of two of the largest circuit
	Circuits []mpcCircuitInfo `json:"circuits"`
	Phase1   []mpcEntry       `json:"phase1"`
	Beacon1  string           `json:"beacon1,omitempty"` // closed phase 1
	Phase2   []mpcEntry       `json:"phase2,omitempty"`
	Beacon2  string           `json:"beacon2,omitempty"` // closed phase 2: final
}

type mpcTranscript struct {
	mpcHeader
	p1       [][]byte   // serialized mpcsetup.Phase1, in order
	p2       [][][]byte // [contribution][circuit] serialized mpcsetup.Phase2
	circuits []*mpcCircuit
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func newMPC(circuits []*mpcCircuit) *mpcTranscript {
	t := &mpcTranscript{circuits: circuits}
	most := 0
	for _, c := range circuits {
		most = max(most, c.r1cs.GetNbConstraints())
		t.Circuits = append(t.Circuits, mpcCircuitInfo{Name: c.name, Contract: c.contract, CCS: sha256Hex(c.ccs), Constraints: c.r1cs.GetNbConstraints()})
	}
	t.N = max(ecc.NextPowerOfTwo(uint64(most)), 2)
	return t
}

func (t *mpcTranscript) phase() int {
	switch {
	case t.Beacon1 == "":
		return 1
	case t.Beacon2 == "":
		return 2
	}
	return 3 // final
}

func (t *mpcTranscript) bytes() []byte {
	var b bytes.Buffer
	h, _ := json.Marshal(t.mpcHeader)
	b.WriteString(mpcMagic)
	binary.Write(&b, binary.BigEndian, uint32(len(h)))
	b.Write(h)
	blob := func(x []byte) {
		binary.Write(&b, binary.BigEndian, uint64(len(x)))
		b.Write(x)
	}
	for _, x := range t.p1 {
		blob(x)
	}
	for _, xs := range t.p2 {
		for _, x := range xs {
			blob(x)
		}
	}
	return b.Bytes()
}

// hash: the transcript's hash, what contributors publish; the final one is CEREMONY.
func (t *mpcTranscript) hash() [32]byte { return sha256.Sum256(t.bytes()) }

func (t *mpcTranscript) write(path string) ([32]byte, error) {
	b := t.bytes()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// readMPC: a transcript for these circuits; each contribution's bytes must
// match the hash its header lists.
func readMPC(b []byte, circuits []*mpcCircuit) (*mpcTranscript, error) {
	r := bytes.NewReader(b)
	magic := make([]byte, len(mpcMagic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != mpcMagic {
		return nil, errors.New("not a ternchain ceremony transcript")
	}
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil || int64(n) > int64(r.Len()) {
		return nil, errors.New("transcript: bad header")
	}
	hb := make([]byte, n)
	io.ReadFull(r, hb)
	t := &mpcTranscript{circuits: circuits}
	if err := json.Unmarshal(hb, &t.mpcHeader); err != nil {
		return nil, fmt.Errorf("transcript header: %w", err)
	}
	if len(t.Circuits) != len(circuits) {
		return nil, fmt.Errorf("transcript is for %d circuits, this binary has %d", len(t.Circuits), len(circuits))
	}
	for i, c := range circuits {
		if t.Circuits[i].Name != c.name || t.Circuits[i].CCS != sha256Hex(c.ccs) {
			return nil, fmt.Errorf("transcript's circuit %d is %s %s, this binary's is %s %s: not the same circuits", i,
				t.Circuits[i].Name, t.Circuits[i].CCS, c.name, sha256Hex(c.ccs))
		}
	}
	if fresh := newMPC(circuits); t.N != fresh.N {
		return nil, fmt.Errorf("transcript domain %d, the circuits need %d", t.N, fresh.N)
	}
	blob := func(want string) ([]byte, error) {
		var l uint64
		if err := binary.Read(r, binary.BigEndian, &l); err != nil || l > uint64(r.Len()) {
			return nil, errors.New("transcript: truncated")
		}
		x := make([]byte, l)
		io.ReadFull(r, x)
		if sha256Hex(x) != want {
			return nil, errors.New("transcript: a contribution doesn't match its listed hash")
		}
		return x, nil
	}
	for _, e := range t.Phase1 {
		if len(e.SHA256) != 1 {
			return nil, errors.New("transcript: phase 1 entry")
		}
		x, err := blob(e.SHA256[0])
		if err != nil {
			return nil, err
		}
		t.p1 = append(t.p1, x)
	}
	for _, e := range t.Phase2 {
		if len(e.SHA256) != len(circuits) {
			return nil, errors.New("transcript: phase 2 entry")
		}
		var xs [][]byte
		for _, h := range e.SHA256 {
			x, err := blob(h)
			if err != nil {
				return nil, err
			}
			xs = append(xs, x)
		}
		t.p2 = append(t.p2, xs)
	}
	if r.Len() != 0 {
		return nil, errors.New("transcript: trailing bytes")
	}
	if (len(t.Phase2) > 0 || t.Beacon2 != "") && t.Beacon1 == "" {
		return nil, errors.New("transcript: phase 2 without a closed phase 1")
	}
	return t, nil
}

func readMPCFile(path string, circuits []*mpcCircuit) (*mpcTranscript, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return readMPC(b, circuits)
}

func phase1Of(b []byte) (*mpcsetup.Phase1, error) {
	p := new(mpcsetup.Phase1)
	if _, err := p.ReadFrom(bytes.NewReader(b)); err != nil {
		return nil, fmt.Errorf("a phase 1 contribution: %w", err)
	}
	return p, nil
}

func phase2Of(b []byte) (*mpcsetup.Phase2, error) {
	p := new(mpcsetup.Phase2)
	if _, err := p.ReadFrom(bytes.NewReader(b)); err != nil {
		return nil, fmt.Errorf("a phase 2 contribution: %w", err)
	}
	return p, nil
}

func ser(w io.WriterTo) []byte {
	var b bytes.Buffer
	w.WriteTo(&b)
	return b.Bytes()
}

// commons: phase 1's result, its last contribution sealed with beacon 1.
func (t *mpcTranscript) commons() (*mpcsetup.SrsCommons, error) {
	last, err := phase1Of(t.p1[len(t.p1)-1])
	if err != nil {
		return nil, err
	}
	b, _ := hex.DecodeString(t.Beacon1)
	c := last.Seal(b)
	return &c, nil
}

// contribute adds one contribution (fresh randomness from crypto/rand, gone
// when this returns) to the open phase: phase 1, or phase 2 of every circuit.
func (t *mpcTranscript) contribute(name string) error {
	switch t.phase() {
	case 1:
		p := mpcsetup.NewPhase1(t.N)
		if len(t.p1) > 0 {
			var err error
			if p, err = phase1Of(t.p1[len(t.p1)-1]); err != nil {
				return err
			}
		}
		p.Contribute()
		x := ser(p)
		t.p1 = append(t.p1, x)
		t.Phase1 = append(t.Phase1, mpcEntry{Name: name, SHA256: []string{sha256Hex(x)}})
	case 2:
		var commons *mpcsetup.SrsCommons
		if len(t.p2) == 0 {
			var err error
			if commons, err = t.commons(); err != nil {
				return err
			}
		}
		e := mpcEntry{Name: name}
		var xs [][]byte
		for i, c := range t.circuits {
			p := new(mpcsetup.Phase2)
			if commons != nil {
				p.Initialize(c.r1cs, commons)
			} else {
				var err error
				if p, err = phase2Of(t.p2[len(t.p2)-1][i]); err != nil {
					return err
				}
			}
			p.Contribute()
			x := ser(p)
			xs = append(xs, x)
			e.SHA256 = append(e.SHA256, sha256Hex(x))
		}
		t.p2 = append(t.p2, xs)
		t.Phase2 = append(t.Phase2, e)
	default:
		return errors.New("the ceremony is finalized: no more contributions")
	}
	return nil
}

func parseBeacon(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
	if err != nil || len(b) < 16 {
		return nil, errors.New("--beacon: a public random value in hex, at least 16 bytes (say, the hash of a block announced before the last contribution)")
	}
	return b, nil
}

// closePhase1: phase 1 is verified and sealed with the beacon; phase 2 opens.
func (t *mpcTranscript) closePhase1(beacon string) error {
	if t.phase() != 1 {
		return errors.New("phase 1 is closed already")
	}
	if len(t.p1) == 0 {
		return errors.New("phase 1 has no contribution yet")
	}
	b, err := parseBeacon(beacon)
	if err != nil {
		return err
	}
	if err := t.verifyPhase1Chain(); err != nil {
		return err
	}
	t.Beacon1 = hex.EncodeToString(b)
	return nil
}

func (t *mpcTranscript) verifyPhase1Chain() error {
	prev := mpcsetup.NewPhase1(t.N)
	for i, x := range t.p1 {
		p, err := phase1Of(x)
		if err != nil {
			return err
		}
		if err := prev.Verify(p); err != nil {
			return fmt.Errorf("phase 1 contribution %d: %w", i+1, err)
		}
		prev = p
	}
	return nil
}

// verify checks every contribution against the one before (each proves it
// knew its randomness and applied it), and the beacons; for a final
// transcript it returns the keys (computed again, as finalize did).
func (t *mpcTranscript) verify() ([]groth16.ProvingKey, []groth16.VerifyingKey, error) {
	if t.phase() == 1 {
		return nil, nil, t.verifyPhase1Chain()
	}
	var ps []*mpcsetup.Phase1
	for _, x := range t.p1 {
		p, err := phase1Of(x)
		if err != nil {
			return nil, nil, err
		}
		ps = append(ps, p)
	}
	if len(ps) == 0 {
		return nil, nil, errors.New("phase 1 has no contribution")
	}
	b1, _ := hex.DecodeString(t.Beacon1)
	commons, err := mpcsetup.VerifyPhase1(t.N, b1, ps...)
	if err != nil {
		return nil, nil, fmt.Errorf("phase 1: %w", err)
	}
	var pks []groth16.ProvingKey
	var vks []groth16.VerifyingKey
	for i, c := range t.circuits {
		var qs []*mpcsetup.Phase2
		for _, xs := range t.p2 {
			q, err := phase2Of(xs[i])
			if err != nil {
				return nil, nil, err
			}
			qs = append(qs, q)
		}
		if t.phase() == 2 {
			prev := new(mpcsetup.Phase2)
			prev.Initialize(c.r1cs, &commons)
			for j, q := range qs {
				if err := prev.Verify(q); err != nil {
					return nil, nil, fmt.Errorf("phase 2 (%s) contribution %d: %w", c.name, j+1, err)
				}
				prev = q
			}
			continue
		}
		if len(qs) == 0 {
			return nil, nil, errors.New("phase 2 has no contribution")
		}
		b2, _ := hex.DecodeString(t.Beacon2)
		pk, vk, err := mpcsetup.VerifyPhase2(c.r1cs, &commons, b2, qs...)
		if err != nil {
			return nil, nil, fmt.Errorf("phase 2 (%s): %w", c.name, err)
		}
		if t.Circuits[i].VK != "" && t.Circuits[i].VK != sha256Hex(ser(vk)) {
			return nil, nil, fmt.Errorf("the %s verifying key isn't the one the transcript lists", c.name)
		}
		pks, vks = append(pks, pk), append(vks, vk)
	}
	return pks, vks, nil
}

// finalize closes phase 2 with the beacon and writes the keys directory;
// it returns CEREMONY, the final transcript's hash.
func (t *mpcTranscript) finalize(beacon, dir string) ([32]byte, error) {
	if t.phase() != 2 {
		return [32]byte{}, fmt.Errorf("finalize closes phase 2; this transcript is in phase %d", t.phase())
	}
	if len(t.p2) == 0 {
		return [32]byte{}, errors.New("phase 2 has no contribution yet")
	}
	b, err := parseBeacon(beacon)
	if err != nil {
		return [32]byte{}, err
	}
	t.Beacon2 = hex.EncodeToString(b)
	pks, vks, err := t.verify()
	if err != nil {
		t.Beacon2 = ""
		return [32]byte{}, err
	}
	for i := range t.Circuits {
		t.Circuits[i].VK = sha256Hex(ser(vks[i]))
	}
	h := t.hash()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return h, err
	}
	if err := t.writeKeys(dir, h, pks, vks); err != nil {
		return h, err
	}
	return h, nil
}

// keyFiles: what a keys directory holds, by file name.
func (t *mpcTranscript) keyFiles(h [32]byte, pks []groth16.ProvingKey, vks []groth16.VerifyingKey) (map[string][]byte, error) {
	f := map[string][]byte{"transcript.mpc": t.bytes(), "CEREMONY": []byte(fmt.Sprintf("0x%x\n", h))}
	for i, c := range t.circuits {
		var b bytes.Buffer
		pks[i].WriteRawTo(&b)
		f[c.name+".pk"] = b.Bytes()
		f[c.name+".vk"] = ser(vks[i])
		src, err := zk.VerifierSolidity(vks[i], c.name, c.contract, h)
		if err != nil {
			return nil, err
		}
		f[c.contract+".sol"] = []byte(src)
	}
	return f, nil
}

func (t *mpcTranscript) writeKeys(dir string, h [32]byte, pks []groth16.ProvingKey, vks []groth16.VerifyingKey) error {
	f, err := t.keyFiles(h, pks, vks)
	if err != nil {
		return err
	}
	for n, b := range f {
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// checkKeys: a keys directory holds exactly what this final transcript gives.
func (t *mpcTranscript) checkKeys(dir string, pks []groth16.ProvingKey, vks []groth16.VerifyingKey) error {
	f, err := t.keyFiles(t.hash(), pks, vks)
	if err != nil {
		return err
	}
	for n, want := range f {
		got, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("%s in %s isn't what the transcript gives", n, dir)
		}
	}
	return nil
}

// runCeremony: ternchain ceremony init|contribute|verify|finalize.
func runCeremony(args []string) error {
	return runCeremonyWith(args, nil)
}

// runCeremonyWith: the circuits given (tests: a small one), or the network's.
func runCeremonyWith(args []string, circuits []*mpcCircuit) error {
	const usage = "ternchain ceremony init --out FILE | init --phase2 --in FILE --beacon HEX --out FILE | contribute --in FILE --out FILE [--name WHO] | " +
		"verify --in FILE [--keys DIR] | finalize --in FILE --beacon HEX --keys DIR | solo --keys DIR (a test network's operator alone)"
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("ceremony "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	in := fs.String("in", "", "")
	outF := fs.String("out", "", "")
	beacon := fs.String("beacon", "", "")
	keys := fs.String("keys", "", "")
	name := fs.String("name", "", "")
	phase2 := fs.Bool("phase2", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("%v (%s)", err, usage)
	}
	if circuits == nil {
		var err error
		if circuits, err = zkCircuits(); err != nil {
			return err
		}
	}
	load := func() (*mpcTranscript, error) {
		if *in == "" {
			return nil, errors.New("--in FILE: the transcript")
		}
		return readMPCFile(*in, circuits)
	}
	save := func(t *mpcTranscript) error {
		if *outF == "" {
			return errors.New("--out FILE: the new transcript")
		}
		if *outF == *in {
			return errors.New("--out: a new file (the one you got stays as it was)")
		}
		h, err := t.write(*outF)
		if err != nil {
			return err
		}
		out(map[string]any{"transcript": *outF, "sha256": fmt.Sprintf("0x%x", h), "phase": t.phase(),
			"phase1_contributions": len(t.p1), "phase2_contributions": len(t.p2)})
		return nil
	}
	switch args[0] {
	case "solo": // the operator alone, both phases, in one go: a test network's keys (deploy --test-network only)
		if *keys == "" {
			return errors.New("--keys DIR: where the keys and verifiers go")
		}
		beaconOf := func() string {
			b := make([]byte, 32)
			rand.Read(b)
			return hex.EncodeToString(b)
		}
		who := *name
		if who == "" {
			who = "the operator (solo: a test network's keys)"
		}
		t := newMPC(circuits)
		if err := t.contribute(who); err != nil {
			return err
		}
		if err := t.closePhase1(beaconOf()); err != nil {
			return err
		}
		if err := t.contribute(who); err != nil {
			return err
		}
		h, err := t.finalize(beaconOf(), *keys)
		if err != nil {
			return err
		}
		out(map[string]any{"ceremony": fmt.Sprintf("0x%x", h), "keys": *keys, "contributions": "1 + 1 (the operator alone)",
			"warning": "keys from one party: whoever ran this can forge shielded withdrawals and secret votes. Deploy accepts them only with --test-network (never on the mainnet chain id), and net.json then says so",
			"next":    "node contracts/build-zk.js " + *keys + "; TERNCHAIN_ZK_KEYS=" + *keys + " ternchain deploy … --test-network"})
		return nil
	case "init":
		if !*phase2 {
			return save(newMPC(circuits))
		}
		t, err := load()
		if err != nil {
			return err
		}
		if err := t.closePhase1(*beacon); err != nil {
			return err
		}
		return save(t)
	case "contribute":
		t, err := load()
		if err != nil {
			return err
		}
		if err := t.contribute(*name); err != nil {
			return err
		}
		return save(t)
	case "verify":
		t, err := load()
		if err != nil {
			return err
		}
		pks, vks, err := t.verify()
		if err != nil {
			return err
		}
		res := map[string]any{"ok": true, "sha256": fmt.Sprintf("0x%x", t.hash()), "phase": t.phase(),
			"phase1_contributions": len(t.p1), "phase2_contributions": len(t.p2)}
		if t.phase() == 3 {
			res["ceremony"] = res["sha256"]
			if *keys != "" {
				if err := t.checkKeys(*keys, pks, vks); err != nil {
					return err
				}
				res["keys"] = *keys
			}
		}
		out(res)
		return nil
	case "finalize":
		t, err := load()
		if err != nil {
			return err
		}
		if *keys == "" {
			return errors.New("--keys DIR: where the keys and verifiers go")
		}
		h, err := t.finalize(*beacon, *keys)
		if err != nil {
			return err
		}
		out(map[string]any{"ceremony": fmt.Sprintf("0x%x", h), "keys": *keys,
			"next": "node contracts/build-zk.js " + *keys + "; TERNCHAIN_ZK_KEYS=" + *keys + " ternchain deploy … (net.json gets \"ceremony\"; hand out the keys as zk-keys/ beside it)"})
		return nil
	}
	return errors.New(usage)
}

// ---- the gate: which keys deploy and prove, on which chains

// errDevnetKeys: the single-party keys, on a chain that isn't a devnet.
func errDevnetKeys(chainID *big.Int) error {
	return fmt.Errorf("the zk keys in use are the single-party devnet keys (whoever ran that setup can forge withdrawals and secret votes), "+
		"and chain %d isn't a devnet (%v): this network needs a ceremony's keys (ternchain ceremony; TERNCHAIN_ZK_KEYS=DIR, net.json \"ceremony\")",
		chainID, zk.DevnetChainIDs)
}

// minCeremonyParties: contributions each phase of a ceremony needs before
// its keys deploy on the mainnet, or on any network not marked a test
// network (audit 6 ZK-4): one honest contributor per phase makes it sound,
// and one contributor alone is the operator's keys.
const minCeremonyParties = 3

// ceremonyParties: the contributions to each phase of the ceremony whose
// keys are in dir, from its transcript.mpc, which must hash to its CEREMONY.
func ceremonyParties(dir string) (p1, p2 int, err error) {
	h, err := zk.ReadCeremonyFile(dir)
	if err != nil {
		return 0, 0, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "transcript.mpc"))
	if err != nil {
		return 0, 0, fmt.Errorf("%s: no transcript.mpc (ternchain ceremony finalize writes it): %w", dir, err)
	}
	if sha256.Sum256(b) != h {
		return 0, 0, fmt.Errorf("%s: transcript.mpc isn't the ceremony its CEREMONY names", dir)
	}
	r := bytes.NewReader(b)
	magic := make([]byte, len(mpcMagic))
	var n uint32
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != mpcMagic || binary.Read(r, binary.BigEndian, &n) != nil || int64(n) > int64(r.Len()) {
		return 0, 0, fmt.Errorf("%s: transcript.mpc is not a ternchain ceremony transcript", dir)
	}
	hb := make([]byte, n)
	io.ReadFull(r, hb)
	var hd mpcHeader
	if err := json.Unmarshal(hb, &hd); err != nil {
		return 0, 0, fmt.Errorf("%s: transcript header: %w", dir, err)
	}
	if hd.Beacon2 == "" {
		return 0, 0, fmt.Errorf("%s: the ceremony isn't finalized", dir)
	}
	return len(hd.Phase1), len(hd.Phase2), nil
}

// ceremonyGate: may keys from a ceremony with p1/p2 contributions deploy on chainID?
func ceremonyGate(chainID uint64, testNetwork bool, p1, p2 int) error {
	if testNetwork && chainID == l1MainnetChainID {
		return fmt.Errorf("chain %d is the mainnet: it is never a test network", chainID)
	}
	if testNetwork || zk.IsDevnetChain(chainID) {
		return nil
	}
	if p1 < minCeremonyParties || p2 < minCeremonyParties {
		return fmt.Errorf("the zk keys come from a ceremony with %d + %d contributions: chain %d needs a multi-party ceremony (at least %d in each phase), "+
			"or, for an operator-run test network, deploy --test-network (net.json says the operator can see shielded transfers and secret votes)",
			p1, p2, chainID, minCeremonyParties)
	}
	return nil
}

// zkDeployGate: the keys deploy puts verifiers on chain for. TERNCHAIN_ZK_KEYS=DIR:
// a ceremony's, multi-party unless this is a test network (ceremonyGate);
// otherwise the devnet keys, on a devnet chain id only.
func zkDeployGate(ch *Chain, testNetwork bool) error {
	if dir := os.Getenv("TERNCHAIN_ZK_KEYS"); dir != "" {
		h, err := zk.ReadCeremonyFile(dir)
		if err != nil {
			return err
		}
		p1, p2, err := ceremonyParties(dir)
		if err != nil {
			return err
		}
		if err := ceremonyGate(ch.chainID.Uint64(), testNetwork, p1, p2); err != nil {
			return err
		}
		if err := zk.UseKeys(dir, h); err != nil {
			return err
		}
	}
	h, devnet := zk.Ceremony()
	if devnet {
		if !zk.IsDevnetChain(ch.chainID.Uint64()) {
			return errDevnetKeys(ch.chainID)
		}
		ch.dep.Ceremony = ""
		return nil
	}
	ch.dep.Ceremony = fmt.Sprintf("0x%x", h)
	return nil
}

// zkArtifact: the build deploy uses for a contract; a ceremony's verifiers
// come from its keys directory (node contracts/build-zk.js DIR).
func zkArtifact(name string) (contractBuild, error) {
	a, err := loadArtifact(name)
	dir := zk.KeysDir()
	if err != nil || dir == "" || (name != "PoolVerifier" && name != "VoteVerifier") {
		return a, err
	}
	b, err := os.ReadFile(filepath.Join(dir, name+".bin"))
	if err != nil {
		return a, fmt.Errorf("the ceremony's %s isn't compiled (node contracts/build-zk.js %s): %w", name, dir, err)
	}
	a.bin = common.FromHex(strings.TrimSpace(string(b)))
	return a, nil
}

// zkUseNet: the keys a client proves with, from its net.json: none named,
// the devnet keys; "ceremony" names a ceremony's, from TERNCHAIN_ZK_KEYS, or
// "zk_keys" (relative to net.json), or zk-keys/ beside net.json.
func zkUseNet(netPath string, dep Deployment) error {
	if dep.Ceremony == "" {
		zk.UseDevnetKeys()
		return nil
	}
	h, err := zk.ParseCeremony(dep.Ceremony)
	if err != nil {
		return fmt.Errorf("%s: %w", netPath, err)
	}
	dir := os.Getenv("TERNCHAIN_ZK_KEYS")
	if dir == "" {
		if dir = dep.ZKKeys; dir == "" {
			dir = "zk-keys"
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(filepath.Dir(netPath), dir)
		}
	}
	return zk.UseKeys(dir, h)
}

// zkHolders: the contract that keeps each circuit's verifier.
var zkHolders = map[string]string{"withdraw": "PrivacyPool", "vote": "CitizenGroup"}

// zkCheck, before proving (and after deploying): the keys in use may be used
// on this chain, and the verifier the network checks proofs with comes from
// the same setup (its CEREMONY), the one net.json names.
func (ch *Chain) zkCheck(circuit string) error {
	want, devnet := zk.Ceremony()
	if devnet && !zk.IsDevnetChain(ch.chainID.Uint64()) {
		return errDevnetKeys(ch.chainID)
	}
	if ch.dep.Ceremony != "" {
		if h, err := zk.ParseCeremony(ch.dep.Ceremony); err != nil || devnet || h != want {
			return fmt.Errorf("net.json names ceremony %s, but the zk keys in use are %s: get the network's keys (zk-keys/ beside net.json, or TERNCHAIN_ZK_KEYS=DIR)",
				ch.dep.Ceremony, ceremonyName(want, devnet))
		}
	}
	key := circuit + fmt.Sprintf("/%x", want)
	if _, ok := ch.zkOK.Load(key); ok {
		return nil
	}
	o, err := ch.call(zkHolders[circuit], "verifier")
	if err != nil {
		return err
	}
	addr := o[0].(common.Address)
	var got []any
	err = bind.NewBoundContract(addr, ch.abis["VoteVerifier"], ch.b, ch.b, ch.b).Call(&bind.CallOpts{}, &got, "CEREMONY")
	switch {
	case err != nil && devnet: // a verifier from before CEREMONY (an older devnet's), on a devnet chain
	case err != nil:
		return fmt.Errorf("the %s verifier at %s doesn't say which setup it is from (no CEREMONY): refusing to prove for it", circuit, addr.Hex())
	case got[0].([32]byte) != want:
		return fmt.Errorf("the network's %s verifier (%s) is from setup %s, but the zk keys in use are %s: refusing to prove (net.json \"ceremony\" and its keys must be the network's)",
			circuit, addr.Hex(), ceremonyName(got[0].([32]byte), got[0].([32]byte) == [32]byte{}), ceremonyName(want, devnet))
	}
	ch.zkOK.Store(key, true)
	return nil
}

func ceremonyName(h [32]byte, devnet bool) string {
	if devnet {
		return "the single-party devnet setup"
	}
	return fmt.Sprintf("ceremony 0x%x", h)
}
