//go:build redteam || redteam_fg

// Red-team scenario G: the supply chain (release signatures, signer sets,
// the install path). Run: go test -tags redteam_fg -run 'TestRedTeamG' -count=1 -v ./cmd/ternchain
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// G1: the textbook attacks against a properly set-up 2-of-3 signer set: all
// should be refused.
func TestRedTeamG_ThresholdAttacks(t *testing.T) {
	d := t.TempDir()
	oscar, nina, ada, mallory := relKeygen(t, d, "oscar"), relKeygen(t, d, "nina"), relKeygen(t, d, "ada"), relKeygen(t, d, "mallory")
	pin := filepath.Join(d, "pin.json")
	set := relSet(2, 2, nil, oscar, nina, ada)
	writeSignerSet(pin, set, 0o600)
	newRel := func(name, version string) string {
		dir := filepath.Join(d, name)
		os.MkdirAll(dir, 0o755)
		return relFiles(t, dir, version)
	}

	// Fewer signers than the threshold.
	s1 := newRel("one", "v1.0.0")
	relSign(t, oscar, s1)
	if _, _, err := relVerify(t, s1, pin, false); err == nil || !strings.Contains(err.Error(), "needs 2") {
		t.Fatalf("1 of 2: %v", err)
	}
	// The same signer twice (a duplicated entry in .sigs).
	var sigs releaseSigs
	readJSON(s1+".sigs", &sigs)
	sigs.Sigs = append(sigs.Sigs, sigs.Sigs[0], releaseSig{Signer: "nina", Key: sigs.Sigs[0].Key, Sig: sigs.Sigs[0].Sig})
	writeJSON(s1+".sigs", sigs)
	if _, _, err := relVerify(t, s1, pin, false); err == nil || !strings.Contains(err.Error(), "needs 2") {
		t.Fatalf("duplicate signer counted twice: %v", err)
	}
	// A key outside the set.
	relSign(t, mallory, s1)
	if _, _, err := relVerify(t, s1, pin, false); err == nil {
		t.Fatal("an outsider's signature counted")
	}
	t.Logf("RESULT G1a: HELD: 1-of-2, a duplicated signature, an outsider's key: refused")

	// Signatures over another SHA256SUMS: a valid 2-of-3 release, then its SHA256SUMS swapped.
	s2 := newRel("swap", "v1.1.0")
	relSign(t, oscar, s2)
	relSign(t, nina, s2)
	if m, _, err := relVerify(t, s2, pin, false); err != nil || m["signed"] != true {
		t.Fatalf("honest release: %v %v", m, err)
	}
	evil := []byte("binary evil")
	os.WriteFile(filepath.Join(filepath.Dir(s2), "ternchain-v1.1.0-linux-amd64"), evil, 0o755)
	h := sha256.Sum256(evil)
	b, _ := os.ReadFile(s2)
	lines := strings.SplitN(string(b), "\n", 2)
	os.WriteFile(s2, []byte(hex.EncodeToString(h[:])+"  ternchain-v1.1.0-linux-amd64\n"+lines[1]), 0o644)
	if _, _, err := relVerify(t, s2, pin, false); err == nil || !strings.Contains(err.Error(), "signs another SHA256SUMS") {
		t.Fatalf("swapped SHA256SUMS: %v", err)
	}
	// .sigs rewritten to name the new sums' hash: the signatures don't verify over it.
	readJSON(s2+".sigs", &sigs)
	nb, _ := os.ReadFile(s2)
	nh := sha256.Sum256(nb)
	sigs.Sums = hex.EncodeToString(nh[:])
	writeJSON(s2+".sigs", sigs)
	if _, _, err := relVerify(t, s2, pin, false); err == nil || !strings.Contains(err.Error(), "needs 2") {
		t.Fatalf("relabelled sigs: %v", err)
	}
	// A version relabel: the same signed sums claimed as another version.
	s3 := newRel("relabel", "v1.2.0")
	relSign(t, oscar, s3)
	relSign(t, nina, s3)
	readJSON(s3+".sigs", &sigs)
	sigs.Version = "v9.9.9"
	writeJSON(s3+".sigs", sigs)
	if _, _, err := relVerify(t, s3, pin, false); err == nil {
		t.Fatal("a relabelled version verified")
	}
	// A missing .sigs with threshold > 0.
	s4 := newRel("nosigs", "v1.3.0")
	if _, _, err := relVerify(t, s4, pin, false); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("no sigs: %v", err)
	}
	t.Logf("RESULT G1b: HELD: swapped SHA256SUMS, relabelled .sigs hash, relabelled version, missing .sigs: refused")

	// A changed signer set without approval: refused loudly, pin unchanged; garbage signatures in the set don't count.
	mset := relSet(3, 1, set, mallory)
	mset.Sigs = []releaseSig{{Signer: "oscar", Key: oscar.pub, Sig: base64.StdEncoding.EncodeToString(make([]byte, 64))}}
	mf := filepath.Join(filepath.Dir(s2), "signers.json")
	writeSignerSet(mf, mset, 0o644)
	if _, _, err := relVerify(t, s2, pin, false); err == nil || !strings.Contains(err.Error(), signerChangeLoud) {
		t.Fatalf("unapproved set: %v", err)
	}
	if p, _ := readSignerSet(pin); p.id() != set.id() {
		t.Fatal("the pin moved")
	}
	t.Logf("RESULT G1c: HELD: a new set without the old signers' approval: refused with %q", signerChangeLoud)

	// A rollback: the older release v1.1.0 (honest, signed) still verifies with no version asked.
	s5 := newRel("old", "v0.9.0")
	relSign(t, oscar, s5)
	relSign(t, nina, s5)
	if m, _, err := relVerify(t, s5, pin, false); err != nil || m["signed"] != true {
		t.Fatalf("old release: %v %v", m, err)
	}
	// FINDING G5: no anti-rollback: an attacker who can serve files offers an old, properly signed, vulnerable release.
	t.Logf("RESULT G1d: BROKEN (low): an old signed release (v0.9.0) verifies as 'signed'; no minimum version / no 'newer than the running binary' check")

	// A listed binary that isn't in the folder: reported "not here", verify still succeeds.
	s6 := newRel("missing", "v1.4.0")
	relSign(t, oscar, s6)
	relSign(t, nina, s6)
	os.Remove(filepath.Join(filepath.Dir(s6), "ternchain-v1.4.0-linux-amd64"))
	os.WriteFile(filepath.Join(filepath.Dir(s6), "ternchain"), []byte("binary evil"), 0o755) // renamed as install.sh names it
	m, _, err := relVerify(t, s6, pin, false)
	if err != nil || fmt.Sprint(m["files"]) == "" {
		t.Fatalf("missing file: %v %v", m, err)
	}
	t.Logf("RESULT G1e: BROKEN (low): files %v; a binary renamed 'ternchain' (as install.sh does) is silently not checked, verify exits 0", m["files"])
	// version --verify checks the running binary itself: the test binary isn't listed: refused (held).
	r, _ := verifyRelease(s6, set, "")
	if err := selfCheck(r, map[string]any{}); err == nil || !strings.Contains(err.Error(), "not in release") {
		t.Fatalf("selfCheck: %v", err)
	}
	t.Logf("RESULT G1f: HELD: version --verify refuses a binary that isn't in the verified SHA256SUMS")
}

// G2: the shipped state: releases/signers.json has threshold 0.
// Before redteam/fix-signer-pin: (a) any forged release verified as
// "unsigned" with exit 0; (b) the first signer set a release folder offered
// was accepted on trust and PINNED, so (c) the real maintainers' set was then
// refused as a coerced change. Now: (a) unsigned fails without
// --allow-unsigned; (b) a first set from the release folder is refused loudly
// and not pinned; (c) the real set is still open to the owner's out-of-band
// acceptance.
func TestRedTeamG_ShippedThresholdZeroHijack(t *testing.T) {
	shipped, err := parseSignerSet(embeddedSigners)
	if err != nil || shipped.Threshold != 0 {
		t.Fatalf("shipped set: %v %v (re-check G2)", shipped, err)
	}
	d := t.TempDir()
	pin := filepath.Join(d, "config", "ternchain", "signers.json") // no pin yet: a fresh user

	// (a) A forged release (any bytes) with matching SHA256SUMS.
	fake := filepath.Join(d, "fake")
	os.MkdirAll(fake, 0o755)
	fs := relFiles(t, fake, "v1.0.0")
	m, _, err := relVerify(t, fs, filepath.Join(d, "other-pin.json"), false)
	if err == nil || !strings.Contains(err.Error(), "UNSIGNED") {
		t.Fatalf("unsigned now fails: %v %v", m, err)
	}
	t.Logf("RESULT G2a: HELD (fixed): an unsigned release fails unless --allow-unsigned: %v", err)

	// (b) The attacker's release folder carries its own signers.json chained from the shipped empty set.
	mallory := relKeygen(t, d, "mallory")
	evil := relSet(shipped.Version+1, 1, shipped, mallory)
	writeSignerSet(filepath.Join(fake, "signers.json"), evil, 0o644)
	relSign(t, mallory, fs)
	_, notes, err := relVerify(t, fs, pin, false)
	if err == nil || !strings.Contains(err.Error(), signerChangeLoud) || !strings.Contains(notes, "!!!") {
		t.Fatalf("attacker set must be refused loudly: %v\n%s", err, notes)
	}
	if p, _ := readSignerSet(pin); p != nil {
		t.Fatal("a refused set was pinned")
	}
	t.Logf("RESULT G2b: HELD (fixed): a first signer set from the release folder is refused loudly and not pinned")

	// (c) The maintainers publish their real set v2 (oscar, nina): the pin is untouched, so it is
	// accepted once the owner checked its id out of band.
	oscar, nina := relKeygen(t, d, "oscar"), relKeygen(t, d, "nina")
	real := relSet(shipped.Version+1, 2, shipped, oscar, nina)
	realDir := filepath.Join(d, "real")
	os.MkdirAll(realDir, 0o755)
	rs := relFiles(t, realDir, "v1.1.0")
	writeSignerSet(filepath.Join(realDir, "signers.json"), real, 0o644)
	relSign(t, oscar, rs)
	relSign(t, nina, rs)
	m, _, err = relVerify(t, rs, pin, true)
	if err != nil || m["signed"] != true {
		t.Fatalf("override: %v %v", m, err)
	}
	if p, _ := readSignerSet(pin); p == nil || p.id() != real.id() {
		t.Fatal("the real set wasn't pinned")
	}
	t.Logf("RESULT G2c: HELD (fixed): no hijacked pin; the real set is pinned after the owner's out-of-band check")
	t.Logf("RESULT G2d: by design: --accept-signer-change pins the offered set (one flag, the owner's call)")
}

// G3: the install path agents are told to use (AGENTS.md: curl install.sh | sh).
// Static checks on scripts/install.sh and the release workflow.
func TestRedTeamG_InstallPath(t *testing.T) {
	b, err := os.ReadFile(repo("scripts/install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// G3a: before redteam/fix-install-sigs install.sh checked the binary only
	// against SHA256SUMS from the same release (no .sigs, no release verify).
	// Now it checks SHA256SUMS.sigs against the threshold of its embedded
	// signer set (OpenSSL 3, or a ternchain verified before), and refuses an
	// unsigned release unless TERN_ALLOW_UNSIGNED=1.
	for _, want := range []string{`"$base/SHA256SUMS.sigs"`, "pkeyutl -verify", `release verify "$rel"`, "TERN_ALLOW_UNSIGNED"} {
		if !strings.Contains(s, want) {
			t.Fatalf("install.sh no longer has %q: re-check G3a", want)
		}
	}
	// The attack: whoever serves the release's files serves a binary of
	// their own with a SHA256SUMS to match (and, with a key of their own,
	// a SHA256SUMS.sigs).
	f := newInstallFixture(t, "v6.6.6", nil) // the shipped install.sh
	if out, ok := f.run("v6.6.6"); ok || !strings.Contains(out, "UNSIGNED") {
		t.Fatalf("the shipped install.sh installed a forged, unsigned release: %v\n%s", ok, out)
	}
	if ossl := installOpenSSL(t); ossl != "" {
		d := t.TempDir()
		oscar, nina, mallory := relKeygen(t, d, "oscar"), relKeygen(t, d, "nina"), relKeygen(t, d, "mallory")
		f := newInstallFixture(t, "v6.6.6", relSet(2, 2, nil, oscar, nina))
		relSign(t, mallory, f.sums)
		if out, ok := f.run("v6.6.6", "TERN_OPENSSL="+ossl, "TERN_ALLOW_UNSIGNED=1"); ok || !strings.Contains(out, "signed by 0 distinct signers") {
			t.Fatalf("a release signed by an outsider: %v\n%s", ok, out)
		}
	} else {
		t.Logf("no OpenSSL 3 here: the signed-release attack is not run (TestInstallShVerifiesSignatures covers it where there is one)")
	}
	t.Logf("RESULT G3a: HELD (fixed): install.sh refuses a forged release: unsigned (unless TERN_ALLOW_UNSIGNED=1) or signed by fewer than the threshold of its embedded signer set")
	// FINDING G3b: net.json from the main branch, unpinned, unverified (networks/NAME/SHA256SUMS and the release's SHA256SUMS-NAME exist but aren't used).
	if !strings.Contains(s, "/main/networks/$network/net.json") || strings.Contains(s, "SHA256SUMS-") {
		t.Fatal("install.sh's net.json source changed: re-check G3b")
	}
	t.Logf("RESULT G3b: BROKEN (medium): net.json (contract addresses, RPCs) comes from raw main, checked only by grep chain_id")
	// G3c: runs the fresh binary and strips macOS quarantine before any verification.
	if !strings.Contains(s, `"$home/ternchain" version`) || !strings.Contains(s, "com.apple.quarantine") {
		t.Fatal("re-check G3c")
	}
	t.Logf("RESULT G3c: noted (low): install.sh strips quarantine and runs the downloaded binary right away")

	// Release workflow: a draft a person publishes (held); CI rebuilds twice (held).
	wf, _ := os.ReadFile(repo(".github/workflows/release.yml"))
	if !strings.Contains(string(wf), "--draft") {
		t.Fatal("release.yml publishes without a person now: re-check G4")
	}
	ci, _ := os.ReadFile(repo(".github/workflows/ci.yml"))
	if !strings.Contains(string(ci), "reproducible:") {
		t.Fatal("no reproducible job: re-check G4")
	}
	// FINDING G4: the release workflow builds once and never compares with releases/TAG/SHA256SUMS or a second build; ci.yml does, separately, and the draft doesn't wait for it.
	if strings.Contains(string(wf), "repro") || strings.Contains(string(wf), "needs:") {
		t.Fatal("release.yml gates on reproducibility now: re-check G4")
	}
	t.Logf("RESULT G4: HELD (draft for a person) / BROKEN (low): release.yml uploads binaries from one build, not gated on ci.yml's reproducible job; .sigs are never part of the release automation")
}
