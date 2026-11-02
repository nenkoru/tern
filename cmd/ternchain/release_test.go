package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type relKey struct{ name, path, pub string }

func relKeygen(t *testing.T, dir, name string) relKey {
	t.Helper()
	p := filepath.Join(dir, name+".key")
	pub, err := releaseKeygen(p, name)
	if err != nil {
		t.Fatal(err)
	}
	return relKey{name, p, pub}
}

func relSet(version, threshold int, prev *signerSet, keys ...relKey) *signerSet {
	s := &signerSet{Version: version, Threshold: threshold}
	for _, k := range keys {
		s.Signers = append(s.Signers, releaseSigner{k.name, k.pub})
	}
	if prev != nil {
		s.Prev = prev.id()
	}
	return s
}

// relFiles: a fake release of VERSION in dir: two "binaries" and SHA256SUMS.
func relFiles(t *testing.T, dir, version string) string {
	t.Helper()
	var sums strings.Builder
	for _, n := range []string{"ternchain-" + version + "-linux-amd64", "ternchain-ops-" + version + "-darwin-arm64"} {
		b := []byte("binary " + n)
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o755); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(b)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(h[:]), n)
	}
	p := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(p, []byte(sums.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func relVerify(t *testing.T, sums, pin string, override bool, signers ...string) (map[string]any, string, error) {
	t.Helper()
	var notes bytes.Buffer
	_, m, err := verifyReleaseCLI(sums, releaseOpts{pin: pin, override: override, signers: strings.Join(signers, ","), notes: &notes})
	return m, notes.String(), err
}

func relSign(t *testing.T, k relKey, sums string) {
	t.Helper()
	if _, err := releaseSign(k.path, sums, ""); err != nil {
		t.Fatal(err)
	}
}

// 2 of 3 signers: one is not enough, the same one twice is still one, a key
// outside the set doesn't count, two distinct ones are; a changed binary or
// SHA256SUMS is refused.
func TestReleaseThresholdSignatures(t *testing.T) {
	d := t.TempDir()
	oscar, nina, ada, eve := relKeygen(t, d, "oscar"), relKeygen(t, d, "nina"), relKeygen(t, d, "ada"), relKeygen(t, d, "eve")
	pin := filepath.Join(d, "pin", "signers.json")
	set := relSet(2, 2, nil, oscar, nina, ada)
	if err := writeSignerSet(pin, set, 0o600); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(d, "rel")
	os.Mkdir(rel, 0o755)
	sums := relFiles(t, rel, "v0.9.0")

	if _, _, err := relVerify(t, sums, pin, false); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("no signatures: %v", err)
	}
	// --allow-unsigned (install.sh's TERN_ALLOW_UNSIGNED=1) is for a set with
	// threshold 0 only: with signers pinned, a missing .sigs is refused anyway.
	if _, _, err := verifyReleaseCLI(sums, releaseOpts{pin: pin, unsigned: true, notes: &bytes.Buffer{}}); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("no signatures, --allow-unsigned, a 2-of-3 set: %v", err)
	}
	relSign(t, oscar, sums)
	relSign(t, eve, sums) // not a signer of the set
	if _, _, err := relVerify(t, sums, pin, false); err == nil || !strings.Contains(err.Error(), "signed by 1 distinct") {
		t.Fatalf("1 of 3 (and eve): %v", err)
	}
	// oscar's signature twice (by hand: sign replaces its own): still one signer.
	var rs releaseSigs
	readJSON(sums+".sigs", &rs)
	rs.Sigs = append(rs.Sigs, rs.Sigs[0])
	b, _ := json.Marshal(rs)
	os.WriteFile(sums+".sigs", b, 0o644)
	if _, _, err := relVerify(t, sums, pin, false); err == nil || !strings.Contains(err.Error(), "signed by 1 distinct") {
		t.Fatalf("a duplicate counted twice: %v", err)
	}
	relSign(t, nina, sums)
	// Signed, then stripped: --allow-unsigned still doesn't pass it.
	signed, _ := os.ReadFile(sums + ".sigs")
	os.Remove(sums + ".sigs")
	if _, _, err := verifyReleaseCLI(sums, releaseOpts{pin: pin, unsigned: true, notes: &bytes.Buffer{}}); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("stripped .sigs, --allow-unsigned, a 2-of-3 set: %v", err)
	}
	os.WriteFile(sums+".sigs", signed, 0o644)
	m, _, err := relVerify(t, sums, pin, false)
	if err != nil || m["signed"] != true || fmt.Sprint(m["signed_by"]) != "[oscar nina]" {
		t.Fatalf("2 of 3: %v %v", m, err)
	}
	if fs := m["files"].(map[string]string); fs["ternchain-v0.9.0-linux-amd64"] != "ok" {
		t.Fatalf("files: %v", fs)
	}

	// A binary that isn't the one the signers rebuilt.
	bin := filepath.Join(rel, "ternchain-v0.9.0-linux-amd64")
	os.WriteFile(bin, []byte("backdoored"), 0o755)
	if _, _, err := relVerify(t, sums, pin, false); err == nil || !strings.Contains(err.Error(), "do not run") {
		t.Fatalf("a changed binary: %v", err)
	}
	if _, err := releaseSign(ada.path, sums, ""); err == nil || !strings.Contains(err.Error(), "refusing to sign") {
		t.Fatalf("signing beside a changed binary: %v", err)
	}
	// SHA256SUMS changed to the backdoored binary's hash: the signatures are over other bytes.
	h := sha256.Sum256([]byte("backdoored"))
	sb, _ := os.ReadFile(sums)
	lines := strings.SplitN(string(sb), "\n", 2)
	os.WriteFile(sums, []byte(hex.EncodeToString(h[:])+"  ternchain-v0.9.0-linux-amd64\n"+lines[1]), 0o644)
	if _, _, err := relVerify(t, sums, pin, false); err == nil || !strings.Contains(err.Error(), "signs another SHA256SUMS") {
		t.Fatalf("a changed SHA256SUMS: %v", err)
	}
	// Signatures of one version don't carry over to another (the version is signed).
	if _, _, err := verifyReleaseCLI(sums, releaseOpts{pin: pin, version: "v1.0.0", notes: &bytes.Buffer{}}); err == nil {
		t.Fatal("a wanted version that isn't the signed one")
	}
}

// The signer set rotates only with the old set's approval: a rotation signed
// by its threshold is accepted with a notice naming who was added and
// removed; one that isn't is refused loudly, unless overridden.
func TestReleaseSignerRotation(t *testing.T) {
	d := t.TempDir()
	oscar, nina, ada, eve := relKeygen(t, d, "oscar"), relKeygen(t, d, "nina"), relKeygen(t, d, "ada"), relKeygen(t, d, "eve")
	pin := filepath.Join(d, "signers.json")
	v2 := relSet(2, 2, nil, oscar, nina, ada)
	writeSignerSet(pin, v2, 0o600)

	rel := filepath.Join(d, "rel")
	os.Mkdir(rel, 0o755)
	sums := relFiles(t, rel, "v1.0.0")
	relSign(t, nina, sums)
	relSign(t, eve, sums) // eve: only a signer of the new set

	// A proper rotation: ada leaves, eve joins, oscar and nina approve.
	v3 := relSet(3, 2, v2, oscar, nina, eve)
	v3f := filepath.Join(d, "v3.json")
	writeSignerSet(v3f, v3, 0o644)
	if _, err := releaseSignSet(oscar.path, v3f); err != nil {
		t.Fatal(err)
	}
	if m, _ := releaseSignersInfo(v3f, pin); !strings.HasPrefix(fmt.Sprint(m["rotation"]), "not accepted") {
		t.Fatalf("1 of 2 approvals: %v", m["rotation"])
	}
	if _, _, err := relVerify(t, sums, pin, false, v3f); err == nil || !strings.Contains(err.Error(), signerChangeLoud) {
		t.Fatalf("a rotation one approval short: %v", err)
	}
	if _, err := releaseSignSet(nina.path, v3f); err != nil {
		t.Fatal(err)
	}
	m, notes, err := relVerify(t, sums, pin, false, v3f)
	if err != nil || m["signed"] != true || fmt.Sprint(m["signed_by"]) != "[nina eve]" {
		t.Fatalf("after a proper rotation: %v %v", m, err)
	}
	if !strings.Contains(notes, "added: [eve]; removed: [ada]") || m["signer_set_notice"] == nil || strings.Contains(notes, "!!!") {
		t.Fatalf("the notice: %q", notes)
	}
	if p, _ := readSignerSet(pin); p.id() != v3.id() {
		t.Fatal("the pin didn't move to v3")
	}

	// A set nobody of v3 approved (one person's new keys): refused loudly, the pin stays.
	mallory := relKeygen(t, d, "mallory")
	v4 := relSet(4, 1, v3, mallory)
	v4f := filepath.Join(rel, "signers.json") // shipped inside the release folder
	writeSignerSet(v4f, v4, 0o644)
	if _, err := releaseSignSet(mallory.path, v4f); err != nil {
		t.Fatal(err)
	}
	relSign(t, mallory, sums)
	_, notes, err = relVerify(t, sums, pin, false)
	if err == nil || !strings.Contains(err.Error(), signerChangeLoud) || !strings.Contains(notes, signerChangeLoud) || !strings.Contains(notes, "!!!!") {
		t.Fatalf("an unapproved set: %v\n%s", err, notes)
	}
	if p, _ := readSignerSet(pin); p.id() != v3.id() {
		t.Fatal("an unapproved set moved the pin")
	}
	// A set with the pinned version but other signers (a fork): refused too.
	fork := relSet(3, 1, v2, mallory)
	ff := filepath.Join(d, "fork.json")
	writeSignerSet(ff, fork, 0o644)
	os.Remove(v4f)
	if _, _, err := relVerify(t, sums, pin, false, ff); err == nil || !strings.Contains(err.Error(), signerChangeLoud) {
		t.Fatalf("a fork: %v", err)
	}

	// The override: checked against the offered set, the warning still printed.
	writeSignerSet(v4f, v4, 0o644)
	releaseSignSet(mallory.path, v4f)
	m, notes, err = relVerify(t, sums, pin, true)
	if err != nil || m["signed"] != true || m["signer_set_warning"] == nil || !strings.Contains(notes, "--accept-signer-change") {
		t.Fatalf("override: %v %v %q", m, err, notes)
	}
}

// threshold 0 (the shipped placeholder): the release is reported unsigned,
// and its hashes are still checked.
func TestReleaseUnsignedPlaceholder(t *testing.T) {
	if s, err := parseSignerSet(embeddedSigners); err != nil || s.Threshold != 0 || len(s.Signers) != 0 {
		t.Fatalf("releases/signers.json: %v %v", s, err)
	}
	d := t.TempDir()
	pin := filepath.Join(d, "signers.json")
	sums := relFiles(t, d, "v0.1.0")
	// Unsigned fails unless the caller accepts a hash-only check (red team G2a).
	if _, _, err := relVerify(t, sums, pin, false); err == nil || !strings.Contains(err.Error(), "UNSIGNED") || !strings.Contains(err.Error(), "--allow-unsigned") {
		t.Fatalf("unsigned without --allow-unsigned: %v", err)
	}
	var notes bytes.Buffer
	_, m, err := verifyReleaseCLI(sums, releaseOpts{pin: pin, unsigned: true, notes: &notes})
	if err != nil || m["signed"] != false || !strings.HasPrefix(fmt.Sprint(m["status"]), "UNSIGNED") || m["pin"] != pin || !strings.Contains(notes.String(), "UNSIGNED") {
		t.Fatalf("unsigned: %v %v", m, err)
	}
	os.WriteFile(filepath.Join(d, "ternchain-v0.1.0-linux-amd64"), []byte("other"), 0o755)
	if _, _, err := verifyReleaseCLI(sums, releaseOpts{pin: pin, unsigned: true, notes: &bytes.Buffer{}}); err == nil || !strings.Contains(err.Error(), "do not run") {
		t.Fatalf("unsigned, a changed binary: %v", err)
	}
	// Malformed sets are refused: threshold above the signers, a duplicate key.
	k := relKeygen(t, d, "oscar")
	for _, s := range []*signerSet{relSet(2, 2, nil, k), {Version: 2, Threshold: 1, Signers: []releaseSigner{{"oscar", k.pub}, {"nina", k.pub}}}} {
		b, _ := json.Marshal(s)
		if _, err := parseSignerSet(b); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
}

// keygen writes 0600 and prints only the public key; version --verify finds
// this binary in a signed SHA256SUMS; the CLI wires release verify.
func TestReleaseKeygenSelfCheckCLI(t *testing.T) {
	d := t.TempDir()
	var buf bytes.Buffer
	outW = &buf
	defer func() { outW = nil }()
	kp := filepath.Join(d, "oscar.key")
	if err := run([]string{"release", "keygen", "--out", kp, "--name", "oscar"}); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(kp)
	var kf signerKeyFile
	readJSON(kp, &kf)
	if fi.Mode().Perm() != 0o600 || strings.Contains(buf.String(), kf.Seed) || !strings.Contains(buf.String(), kf.PublicKey) {
		t.Fatalf("keygen: %v %s", fi.Mode(), buf.String())
	}
	if err := run([]string{"release", "keygen", "--out", kp}); err == nil {
		t.Fatal("keygen overwrote a key")
	}
	nina := relKeygen(t, d, "nina")
	pin := filepath.Join(d, "signers.json")
	writeSignerSet(pin, relSet(2, 2, nil, relKey{"oscar", kp, kf.PublicKey}, nina), 0o600)

	// A release whose SHA256SUMS lists this very binary.
	exe, _ := os.Executable()
	h, _ := fileSHA256(exe)
	rel := filepath.Join(d, "rel")
	os.Mkdir(rel, 0o755)
	sums := filepath.Join(rel, "SHA256SUMS")
	os.WriteFile(sums, []byte(h+"  ternchain-v2.0.0-"+runtime.GOOS+"-"+runtime.GOARCH+"\n"), 0o644)
	for _, k := range []string{kp, nina.path} {
		if err := run([]string{"release", "sign", "--key", k, rel}); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	r, m, err := verifyReleaseCLI(rel, releaseOpts{pin: pin, notes: &bytes.Buffer{}})
	if err == nil {
		err = selfCheck(r, m)
	}
	t.Logf("verify + self-check of a %d MB binary: %v", fileSize(exe)>>20, time.Since(start))
	if err != nil || m["self"].(map[string]any)["listed_as"] == nil {
		t.Fatalf("self check: %v %v", m, err)
	}
	os.Remove(sums + ".sigs")
	os.WriteFile(sums, []byte(strings.Repeat("0", 64)+"  ternchain-v2.0.0-linux-amd64\n"), 0o644)
	run([]string{"release", "sign", "--key", kp, sums})
	run([]string{"release", "sign", "--key", nina.path, sums})
	r, m, err = verifyReleaseCLI(rel, releaseOpts{pin: pin, notes: &bytes.Buffer{}})
	if err != nil || selfCheck(r, m) == nil {
		t.Fatalf("a SHA256SUMS without this binary: %v", err)
	}
	buf.Reset()
	if err := run([]string{"release", "verify", rel, "--pin", pin}); err != nil || !strings.Contains(buf.String(), `"signed": true`) {
		t.Fatalf("release verify: %v %s", err, buf.String())
	}
	if h := helpFor("release verify"); !strings.Contains(h, "--accept-signer-change") {
		t.Fatalf("release verify --help: %s", h)
	}
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// Red team G2b/c: while the pinned set has no signers, a first set read from
// the release folder (or --signers) is not taken on trust and not pinned: an
// attacker serving one fake release would otherwise own the pin and the real
// maintainers' set would be refused as "coerced". The set built into the
// binary is still taken on first use; any other needs --accept-signer-change.
func TestReleaseFirstSetNotFromReleaseFolder(t *testing.T) {
	shipped, err := parseSignerSet(embeddedSigners)
	if err != nil || shipped.Threshold != 0 {
		t.Skip("the shipped set has signers: this case is moot")
	}
	d := t.TempDir()
	pin := filepath.Join(d, "pin", "signers.json")
	rel := filepath.Join(d, "rel")
	os.Mkdir(rel, 0o755)
	sums := relFiles(t, rel, "v1.0.0")
	mallory := relKeygen(t, d, "mallory")
	evil := relSet(shipped.Version+1, 1, shipped, mallory)
	writeSignerSet(filepath.Join(rel, "signers.json"), evil, 0o644)
	relSign(t, mallory, sums)
	_, notes, err := relVerify(t, sums, pin, false)
	if err == nil || !strings.Contains(err.Error(), signerChangeLoud) || !strings.Contains(notes, "threshold 0") {
		t.Fatalf("a first set from the release folder: %v\n%s", err, notes)
	}
	if _, err := os.Stat(pin); err == nil {
		t.Fatal("a refused set was pinned")
	}
	// The same via --signers.
	ef := filepath.Join(d, "evil.json")
	writeSignerSet(ef, evil, 0o644)
	os.Remove(filepath.Join(rel, "signers.json"))
	if _, _, err := relVerify(t, sums, pin, false, ef); err == nil || !strings.Contains(err.Error(), signerChangeLoud) {
		t.Fatalf("a first set by --signers: %v", err)
	}
	// The owner checked it out of band: the override takes and pins it.
	if m, _, err := relVerify(t, sums, pin, true, ef); err != nil || m["signed"] != true {
		t.Fatalf("override: %v %v", m, err)
	}
	if p, _ := readSignerSet(pin); p == nil || p.id() != evil.id() {
		t.Fatal("the accepted set wasn't pinned")
	}
	// A set built into the binary is taken on first use.
	oscar := relKeygen(t, d, "oscar")
	emb := relSet(shipped.Version+1, 1, shipped, oscar)
	emb.builtIn = true
	res, err := resolveSigners(shipped, []*signerSet{emb}, false)
	if err != nil || res.set != emb || !res.changed {
		t.Fatalf("the built-in set on first use: %+v %v", res, err)
	}
}

// ternchain-ops is listed in SHA256SUMS-ops (release.yml): its version
// --verify DIR checks that file, against the same set and threshold.
func TestReleaseOpsSumsSameThreshold(t *testing.T) {
	d := t.TempDir()
	rel := filepath.Join(d, "rel")
	os.Mkdir(rel, 0o755)
	if got := selfSumsPath(rel, true); got != rel {
		t.Fatalf("no SHA256SUMS-ops: %s", got)
	}
	sums := relFiles(t, rel, "v1.0.0")
	ops := filepath.Join(rel, "SHA256SUMS-ops")
	os.Rename(sums, ops)
	if got := selfSumsPath(rel, true); got != ops {
		t.Fatalf("ops build: %s", got)
	}
	if got := selfSumsPath(rel, false); got != rel {
		t.Fatalf("people's build: %s", got)
	}
	oscar, nina := relKeygen(t, d, "oscar"), relKeygen(t, d, "nina")
	pin := filepath.Join(d, "pin.json")
	writeSignerSet(pin, relSet(2, 2, nil, oscar, nina), 0o600)
	relSign(t, oscar, ops)
	if _, _, err := relVerify(t, selfSumsPath(rel, true), pin, false); err == nil || !strings.Contains(err.Error(), "needs 2") {
		t.Fatalf("SHA256SUMS-ops, 1 of 2: %v", err)
	}
	relSign(t, nina, ops)
	if m, _, err := relVerify(t, selfSumsPath(rel, true), pin, false); err != nil || m["signed"] != true {
		t.Fatalf("SHA256SUMS-ops, 2 of 2: %v %v", m, err)
	}
}
