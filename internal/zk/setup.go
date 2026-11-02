package zk

// Where the keys come from (audit 5 ZK-4). The keys embedded in keys/ come
// from a single-party setup (zk_setup_test.go): whoever ran it can forge
// withdrawals and secret votes. They are the devnet keys: the binary keeps
// them, and both the verifier contracts (their constructor) and the clients
// (the main package's zk gate) refuse them on any chain id but the devnet ones
// below. A network with value runs a multi-party ceremony (ternchain ceremony,
// ceremony.go): its keys directory holds the proving and verifying keys, the
// verifiers it generated and CEREMONY, the hash of the final transcript, which
// the verifier contracts carry as their CEREMONY constant and net.json as its
// "ceremony".

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark/backend/groth16"
)

// DevnetChainIDs: the chain ids the devnet keys may be used on, the one
// place that lists them (the verifier contracts get them from here, through
// VerifierSolidity): 1337, the devnet's default (devnet.go defaultChainID) and
// the simulated backend's (params.AllDevChainProtocolChanges), and 31337, the
// usual local test chain (Hardhat, Anvil). If the devnet's default changes,
// add it here and regenerate the verifiers (TERN_ZK_SETUP=verifiers go test
// -run TestZKSetup ./cmd/ternchain; node contracts/build.js). A launch must never use one of these.
var DevnetChainIDs = []uint64{1337, 31337}

// IsDevnetChain: may the devnet keys be used on this chain id?
func IsDevnetChain(id uint64) bool {
	for _, d := range DevnetChainIDs {
		if d == id {
			return true
		}
	}
	return false
}

// keySrc: "" dir is the embedded devnet keys.
var keySrc struct {
	dir      string
	ceremony [32]byte
}

// UseKeys proves with the keys of a ceremony: dir holds NAME.pk and NAME.vk
// per circuit and CEREMONY, which must say ceremony. Checked when a circuit's
// keys are first loaded (Prove).
func UseKeys(dir string, ceremony [32]byte) error {
	if dir == "" || ceremony == ([32]byte{}) {
		return errors.New("zk keys: a ceremony's keys need its directory and its (non-zero) transcript hash")
	}
	zkMu.Lock()
	defer zkMu.Unlock()
	keySrc.dir, keySrc.ceremony = dir, ceremony
	zkCache = map[string]*zkKeys{}
	return nil
}

// UseDevnetKeys: back to the embedded single-party devnet keys.
func UseDevnetKeys() {
	zkMu.Lock()
	defer zkMu.Unlock()
	keySrc.dir, keySrc.ceremony = "", [32]byte{}
	zkCache = map[string]*zkKeys{}
}

// Ceremony: the transcript hash of the keys in use, and whether they are the
// devnet keys (hash zero).
func Ceremony() (h [32]byte, devnet bool) {
	zkMu.Lock()
	defer zkMu.Unlock()
	return keySrc.ceremony, keySrc.dir == ""
}

// KeysDir: the ceremony keys' directory ("": the embedded devnet keys).
func KeysDir() string {
	zkMu.Lock()
	defer zkMu.Unlock()
	return keySrc.dir
}

// ReadCeremonyFile: the transcript hash a keys directory says it holds.
func ReadCeremonyFile(dir string) ([32]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, "CEREMONY"))
	if err != nil {
		return [32]byte{}, fmt.Errorf("%s is not a ceremony's keys directory (no CEREMONY file; ternchain ceremony finalize writes one): %w", dir, err)
	}
	return ParseCeremony(string(b))
}

// ParseCeremony: a transcript hash in hex (0x optional), non-zero.
func ParseCeremony(s string) ([32]byte, error) {
	var h [32]byte
	b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
	if err != nil || len(b) != 32 {
		return h, fmt.Errorf("ceremony %q: a 32-byte transcript hash in hex", strings.TrimSpace(s))
	}
	copy(h[:], b)
	if h == ([32]byte{}) {
		return h, errors.New("ceremony: zero is the devnet keys, not a ceremony")
	}
	return h, nil
}

// readKeyFile: a circuit's file from the embedded devnet keys or the ceremony's directory.
func readKeyFile(name, ext string) ([]byte, error) {
	if keySrc.dir == "" {
		b, err := zkFiles.ReadFile("keys/" + name + "." + ext)
		if err != nil || len(b) == 0 {
			return nil, fmt.Errorf("no %s.%s in this build: run the zk setup (TERN_ZK_SETUP=1 go test -run TestZKSetup ./cmd/ternchain)", name, ext)
		}
		return b, nil
	}
	if ext == "ccs" { // the circuit is the binary's; the ceremony checked its keys are for it
		return zkFiles.ReadFile("keys/" + name + ".ccs")
	}
	got, err := ReadCeremonyFile(keySrc.dir)
	if err != nil {
		return nil, err
	}
	if got != keySrc.ceremony {
		return nil, fmt.Errorf("the zk keys in %s are from ceremony %x, but the network's is %x (net.json \"ceremony\")", keySrc.dir, got, keySrc.ceremony)
	}
	b, err := os.ReadFile(filepath.Join(keySrc.dir, name+"."+ext))
	if err != nil {
		return nil, fmt.Errorf("the ceremony's keys: %w", err)
	}
	return b, nil
}

// EmbeddedCCS: a circuit's constraint system as the binary proves with it.
func EmbeddedCCS(name string) ([]byte, error) {
	return zkFiles.ReadFile("keys/" + name + ".ccs")
}

// EmbeddedVK: the devnet verifying key of a circuit.
func EmbeddedVK(name string) (groth16.VerifyingKey, error) {
	b, err := zkFiles.ReadFile("keys/" + name + ".vk")
	if err != nil {
		return nil, err
	}
	vk := groth16.NewVerifyingKey(ecc.BN254)
	if _, err := vk.ReadFrom(bytes.NewReader(b)); err != nil {
		return nil, err
	}
	return vk, nil
}

// GnarkVersion: the gnark the verifiers are exported with (go.mod).
const GnarkVersion = "v0.16.3"

// VerifierSolidity: gnark's exported Solidity verifier for vk, renamed to
// contract, with the setup it comes from: CEREMONY, the ceremony's transcript
// hash (zero: the devnet keys), and a constructor that refuses zero on any
// chain id but DevnetChainIDs. The one generator of contracts/PoolVerifier.sol
// and VoteVerifier.sol: the devnet setup (zk_setup_test.go) and the ceremony
// (ternchain ceremony finalize) both use it.
func VerifierSolidity(vk groth16.VerifyingKey, circuit, contract string, ceremony [32]byte) (string, error) {
	var b bytes.Buffer
	if err := vk.ExportSolidity(&b); err != nil {
		return "", err
	}
	src := b.String()
	const decl = "contract Verifier {\n"
	if strings.Count(src, decl) != 1 {
		return "", errors.New("gnark's verifier template changed: no single \"contract Verifier {\"")
	}
	setup := "the ternchain ceremony " + fmt.Sprintf("0x%x", ceremony)
	if ceremony == ([32]byte{}) {
		setup = "the single-party devnet setup: NOT for production (refused off the devnet chain ids)"
	}
	conds := make([]string, len(DevnetChainIDs))
	ids := make([]string, len(DevnetChainIDs))
	for i, id := range DevnetChainIDs {
		conds[i] = fmt.Sprintf("block.chainid != %d", id)
		ids[i] = fmt.Sprint(id)
	}
	head := fmt.Sprintf(`/// gnark %s Groth16 verifier for the %s circuit (internal/zk), keys from %s.
contract %s {
    /// The trusted setup the keys below come from: the hash of the ternchain
    /// ceremony's final transcript (ternchain ceremony verify), or zero for the
    /// single-party devnet setup, whose runner can forge proofs (audit 5 ZK-4).
    bytes32 public constant CEREMONY = 0x%x;

    /// The devnet keys, on a chain that is not a devnet.
    error DevnetKeys();

    /// The devnet keys deploy only on the devnet chain ids (internal/zk DevnetChainIDs: %s).
    constructor() {
        if (CEREMONY == bytes32(0) && %s) revert DevnetKeys();
    }
`, GnarkVersion, circuit, setup, contract, ceremony, strings.Join(ids, ", "), strings.Join(conds, " && "))
	return strings.Replace(src, decl, head, 1), nil
}
