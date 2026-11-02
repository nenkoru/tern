package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// scripts/install.sh carries the release signer set (its public keys and
// threshold) so that it checks SHA256SUMS.sigs with a verifier that isn't
// the binary it downloads (red team G3a). The block is generated from
// releases/signers.json: UPDATE_INSTALL_SH=1 go test -run TestInstallShSignerSet ./cmd/ternchain

const (
	installShBegin = "# --- the release signer set: generated from releases/signers.json, do not edit by hand ---\n"
	installShEnd   = "# --- end of the release signer set ---\n"
)

var installSignerName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// installShSignerBlock: install.sh's lines for the set s.
func installShSignerBlock(s *signerSet) (string, error) {
	var b strings.Builder
	b.WriteString(installShBegin)
	b.WriteString("# (UPDATE_INSTALL_SH=1 go test -run TestInstallShSignerSet ./cmd/ternchain rewrites it; the test fails when they differ)\n")
	fmt.Fprintf(&b, "signers_version=%d\nsigners_id=%s\nsigners_threshold=%d\nsigners_keys='\n", s.Version, s.id(), s.Threshold)
	for _, x := range s.Signers {
		k, err := parseSignerKey(x.Key)
		if err != nil {
			return "", err
		}
		if !installSignerName.MatchString(x.Name) {
			return "", fmt.Errorf("signer name %q: install.sh takes letters, digits, '.', '_' and '-' only", x.Name)
		}
		fmt.Fprintf(&b, "%s %s\n", fmtSignerKey(k), x.Name)
	}
	b.WriteString("'\n" + installShEnd)
	return b.String(), nil
}

// withSignerBlock: install.sh's text with its signer set block replaced.
func withSignerBlock(sh string, s *signerSet) (string, error) {
	i, j := strings.Index(sh, installShBegin), strings.Index(sh, installShEnd)
	if i < 0 || j < i {
		return "", fmt.Errorf("install.sh has no signer set block (%q … %q)", strings.TrimSpace(installShBegin), strings.TrimSpace(installShEnd))
	}
	blk, err := installShSignerBlock(s)
	if err != nil {
		return "", err
	}
	return sh[:i] + blk + sh[j+len(installShEnd):], nil
}

// install.sh's embedded set is releases/signers.json's.
func TestInstallShSignerSet(t *testing.T) {
	set, err := readSignerSet(repo("releases/signers.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(repo("scripts/install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := withSignerBlock(string(b), set)
	if err != nil {
		t.Fatal(err)
	}
	if want == string(b) {
		return
	}
	if os.Getenv("UPDATE_INSTALL_SH") == "1" {
		if err := os.WriteFile(repo("scripts/install.sh"), []byte(want), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Logf("scripts/install.sh: the signer set block now matches releases/signers.json (v%d, %s)", set.Version, set.id())
		return
	}
	t.Fatalf("scripts/install.sh's signer set differs from releases/signers.json (v%d, threshold %d, %s): "+
		"run UPDATE_INSTALL_SH=1 go test -run TestInstallShSignerSet ./cmd/ternchain and commit both", set.Version, set.Threshold, set.id())
}

// installOpenSSL: an openssl that verifies ed25519 (OpenSSL 3 or later), as install.sh looks for one.
func installOpenSSL(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	pub, _ := hex.DecodeString("3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c") // RFC 8032, test 2
	sig, _ := hex.DecodeString("92a009a9f0d4cab8720e820b5f642540a2b27b5416503f8fb3762223ebdb69da085ac1e43e15996e458f3613d0f11d8c387b2eaeb4302aeeb00d291612bb0c00")
	pem := "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEA" + base64.StdEncoding.EncodeToString(pub) + "\n-----END PUBLIC KEY-----\n"
	os.WriteFile(filepath.Join(d, "k.pem"), []byte(pem), 0o644)
	os.WriteFile(filepath.Join(d, "m"), []byte{0x72}, 0o644)
	os.WriteFile(filepath.Join(d, "s"), sig, 0o644)
	for _, o := range []string{os.Getenv("TERN_OPENSSL"), "openssl", "/opt/homebrew/opt/openssl@3/bin/openssl", "/usr/local/opt/openssl@3/bin/openssl",
		"/opt/homebrew/bin/openssl", "/usr/local/bin/openssl", "/usr/bin/openssl"} {
		if o == "" {
			continue
		}
		p, err := exec.LookPath(o)
		if err != nil {
			continue
		}
		out, err := exec.Command(p, "pkeyutl", "-verify", "-pubin", "-inkey", filepath.Join(d, "k.pem"), "-rawin",
			"-in", filepath.Join(d, "m"), "-sigfile", filepath.Join(d, "s")).CombinedOutput()
		if err == nil && bytes.Contains(out, []byte("Verified Successfully")) {
			return p
		}
	}
	return ""
}

// installFixture: a release of VERSION for this machine served over HTTP
// (the "binary" is a script printing its version), a net.json, and
// install.sh with the signer set set (nil: the one shipped).
type installFixture struct {
	t                      *testing.T
	dir, rel, sums, binary string
	other, script, net     string
	base, terms            string
}

func newInstallFixture(t *testing.T, version string, set *signerSet) *installFixture {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("install.sh is for macOS and Linux")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		if _, err := exec.LookPath("wget"); err != nil {
			t.Skip("install.sh needs curl or wget")
		}
	}
	m, err := exec.Command("uname", "-m").Output()
	if err != nil {
		t.Skip("no uname")
	}
	arch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "arm64": "arm64", "aarch64": "arm64"}[strings.TrimSpace(string(m))]
	if arch == "" {
		t.Skipf("install.sh has no release for %s", m)
	}
	f := &installFixture{t: t, dir: t.TempDir()}
	f.rel = filepath.Join(f.dir, "srv", "rel")
	os.MkdirAll(f.rel, 0o755)
	f.binary = filepath.Join(f.rel, "ternchain-"+version+"-"+runtime.GOOS+"-"+arch)
	if err := os.WriteFile(f.binary, []byte("#!/bin/sh\necho '{\"version\": \""+version+"\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.other = filepath.Join(f.rel, "ternchain-"+version+"-windows-amd64.exe")
	os.WriteFile(f.other, []byte("another platform"), 0o644)
	f.writeSums()
	os.WriteFile(filepath.Join(f.dir, "srv", "TERMS.md"), []byte("terms\n"), 0o644)
	f.net = filepath.Join(f.dir, "net.json")
	os.WriteFile(f.net, []byte(`{"network": "fixture-net", "chain_id": 1}`+"\n"), 0o644)
	sh, err := os.ReadFile(repo("scripts/install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(sh)
	if set != nil {
		if s, err = withSignerBlock(s, set); err != nil {
			t.Fatal(err)
		}
	}
	f.script = filepath.Join(f.dir, "install.sh")
	os.WriteFile(f.script, []byte(s), 0o755)
	srv := httptest.NewServer(http.FileServer(http.Dir(filepath.Join(f.dir, "srv"))))
	t.Cleanup(srv.Close)
	f.base, f.terms = srv.URL+"/rel", srv.URL+"/TERMS.md"
	return f
}

// writeSums: SHA256SUMS of the release's files as they are now.
func (f *installFixture) writeSums() {
	var b strings.Builder
	for _, p := range []string{f.binary, f.other} {
		h, err := fileSHA256(p)
		if err != nil {
			f.t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s  %s\n", h, filepath.Base(p))
	}
	f.sums = filepath.Join(f.rel, "SHA256SUMS")
	if err := os.WriteFile(f.sums, []byte(b.String()), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// run: install.sh into a fresh folder; its output and whether it installed.
func (f *installFixture) run(version string, env ...string) (string, bool) {
	f.t.Helper()
	home := filepath.Join(f.t.TempDir(), "tern")
	shell := os.Getenv("INSTALL_TEST_SHELL") // another POSIX shell to run it with (dash, busybox sh)
	if shell == "" {
		shell = "sh"
	}
	cmd := exec.Command(shell, f.script)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "TERN_") && !strings.HasPrefix(e, "TERNCHAIN_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+f.dir, "TERN_HOME="+home, "TERN_VERSION="+version, "TERN_BASE_URL="+f.base,
		"TERN_NET_URL="+f.net, "TERN_TERMS_URL="+f.terms, "TERN_VERIFIER="+filepath.Join(f.dir, "no-verifier"))
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	_, statErr := os.Stat(filepath.Join(home, "ternchain"))
	if (err == nil) != (statErr == nil) {
		f.t.Fatalf("install.sh exit %v but the binary installed: %v\n%s", err, statErr == nil, out)
	}
	return string(out), err == nil
}

func (f *installFixture) sigs() releaseSigs {
	var rs releaseSigs
	readJSON(f.sums+".sigs", &rs)
	return rs
}

func (f *installFixture) writeSigs(rs releaseSigs) {
	b, _ := json.MarshalIndent(rs, "", "  ")
	if err := os.WriteFile(f.sums+".sigs", b, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// sumsHash: the hex sha256 of the fixture's SHA256SUMS, as .sigs names it.
func (f *installFixture) sumsHash() string {
	b, err := os.ReadFile(f.sums)
	if err != nil {
		f.t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Red team G3a: install.sh installs only a release whose SHA256SUMS the
// threshold of distinct signers of its embedded set signed (checked by
// OpenSSL 3, not by the downloaded binary), and only a binary matching it.
func TestInstallShVerifiesSignatures(t *testing.T) {
	ossl := installOpenSSL(t)
	if ossl == "" {
		t.Skip("no OpenSSL 3 or later (with ed25519 pkeyutl -rawin) here: install.sh's signature check can't run; macOS: brew install openssl@3")
	}
	const v = "v9.9.9"
	d := t.TempDir()
	oscar, nina, ada, mallory := relKeygen(t, d, "oscar"), relKeygen(t, d, "nina"), relKeygen(t, d, "ada"), relKeygen(t, d, "mallory")
	set := relSet(2, 2, nil, oscar, nina, ada)
	f := newInstallFixture(t, v, set)
	env := "TERN_OPENSSL=" + ossl
	mustFail := func(what, want string, extra ...string) {
		t.Helper()
		out, ok := f.run(v, append([]string{env}, extra...)...)
		if ok || !strings.Contains(out, want) {
			t.Fatalf("%s: installed=%v, want a refusal with %q:\n%s", what, ok, want, out)
		}
	}

	// Unsigned: refused, and with signers pinned TERN_ALLOW_UNSIGNED=1 doesn't
	// change that (TestInstallShAllowUnsignedOnlyAtThresholdZero).
	mustFail("unsigned", "UNSIGNED")
	mustFail("unsigned with TERN_ALLOW_UNSIGNED=1", "does not apply here", "TERN_ALLOW_UNSIGNED=1")

	// Below the threshold; the same signer twice; an outsider.
	relSign(t, oscar, f.sums)
	mustFail("1 of 2", "signed by 1 distinct signers [oscar]")
	rs := f.sigs()
	rs.Sigs = append(rs.Sigs, rs.Sigs[0])
	f.writeSigs(rs)
	mustFail("oscar twice", "signed by 1 distinct signers [oscar]")
	relSign(t, mallory, f.sums)
	mustFail("oscar and an outsider", "signed by 1 distinct signers [oscar]")
	// An outsider's signature under an insider's name and key label.
	rs = f.sigs()
	for i := range rs.Sigs {
		if rs.Sigs[i].Signer == "mallory" {
			rs.Sigs[i].Signer, rs.Sigs[i].Key = "nina", nina.pub
		}
	}
	f.writeSigs(rs)
	mustFail("mallory as nina", "signed by 1 distinct signers [oscar]")

	// The threshold: installed.
	os.Remove(f.sums + ".sigs")
	relSign(t, oscar, f.sums)
	relSign(t, nina, f.sums)
	out, ok := f.run(v, env)
	if !ok || !strings.Contains(out, "signed: 2 of the 2 needed [oscar nina]") {
		t.Fatalf("2 of 2: %v\n%s", ok, out)
	}
	good, _ := os.ReadFile(f.sums + ".sigs")

	// Another version's signatures don't count for this one (the version is signed).
	f2 := newInstallFixture(t, "v9.9.8", set)
	os.WriteFile(f2.sums+".sigs", good, 0o644)
	if out, ok := f2.run("v9.9.8", env); ok || !strings.Contains(out, "signs another SHA256SUMS") {
		t.Fatalf("v9.9.9's signatures for v9.9.8: %v\n%s", ok, out)
	}
	rs = f2.sigs()
	rs.Sums = f2.sumsHash()
	f2.writeSigs(rs)
	if out, ok := f2.run("v9.9.8", env); ok || !strings.Contains(out, "signed by 0 distinct signers") {
		t.Fatalf("v9.9.9's signatures relabelled for v9.9.8: %v\n%s", ok, out)
	}

	// Forged: another binary with a SHA256SUMS to match, the old signatures
	// (as is, then with sums_sha256 rewritten to the new file).
	os.WriteFile(f.binary, []byte("#!/bin/sh\necho evil\n"), 0o755)
	mustFail("tampered binary", "does not match its SHA256SUMS line")
	f.writeSums()
	mustFail("forged SHA256SUMS", "signs another SHA256SUMS")
	rs = f.sigs()
	rs.Sums = f.sumsHash()
	f.writeSigs(rs)
	mustFail("forged SHA256SUMS, sums_sha256 rewritten", "signed by 0 distinct signers")
	// A garbage signature file.
	os.WriteFile(f.sums+".sigs", []byte("not json"), 0o644)
	mustFail("garbage .sigs", "signs another SHA256SUMS")
}

// The shipped install.sh: its set has threshold 0, so any release is
// refused as UNSIGNED unless TERN_ALLOW_UNSIGNED=1 (no openssl needed).
func TestInstallShShippedSetRefusesUnsigned(t *testing.T) {
	shipped, err := parseSignerSet(embeddedSigners)
	if err != nil {
		t.Fatal(err)
	}
	if shipped.Threshold != 0 {
		t.Skip("the shipped set has signers: TestInstallShVerifiesSignatures covers it")
	}
	f := newInstallFixture(t, "v9.9.9", nil)
	if out, ok := f.run("v9.9.9"); ok || !strings.Contains(out, "UNSIGNED") || !strings.Contains(out, "TERN_ALLOW_UNSIGNED=1") {
		t.Fatalf("the shipped install.sh, an unsigned release: %v\n%s", ok, out)
	}
	if out, ok := f.run("v9.9.9", "TERN_ALLOW_UNSIGNED=1"); !ok || !strings.Contains(out, "!!!") {
		t.Fatalf("the shipped install.sh with TERN_ALLOW_UNSIGNED=1: %v\n%s", ok, out)
	}
	// TERN_ALLOW_UNSIGNED=1 still checks the hash.
	os.WriteFile(f.binary, []byte("#!/bin/sh\necho evil\n"), 0o755)
	if out, ok := f.run("v9.9.9", "TERN_ALLOW_UNSIGNED=1"); ok || !strings.Contains(out, "does not match") {
		t.Fatalf("unsigned and tampered: %v\n%s", ok, out)
	}
}

// TERN_ALLOW_UNSIGNED=1 is for release candidates, before signers exist: it
// works only while install.sh's pinned signer set has threshold 0. With
// signers pinned, a release whose SHA256SUMS.sigs is missing or was stripped
// is refused whatever the flag says, by every path install.sh can take:
// OpenSSL, a ternchain installed before (even one whose own pin has no
// signers yet), or neither.
func TestInstallShAllowUnsignedOnlyAtThresholdZero(t *testing.T) {
	const v = "v9.9.9"
	d := t.TempDir()
	oscar, nina := relKeygen(t, d, "oscar"), relKeygen(t, d, "nina")
	f := newInstallFixture(t, v, relSet(2, 2, nil, oscar, nina))
	// Stripped: the release was signed, then its .sigs removed.
	relSign(t, oscar, f.sums)
	relSign(t, nina, f.sums)
	if err := os.Remove(f.sums + ".sigs"); err != nil {
		t.Fatal(err)
	}
	refused := func(what string, out string, ok bool) {
		t.Helper()
		if ok || !strings.Contains(out, "UNSIGNED") && !strings.Contains(out, "does not apply") {
			t.Fatalf("%s, stripped .sigs, TERN_ALLOW_UNSIGNED=1: installed=%v, want a refusal:\n%s", what, ok, out)
		}
		if strings.Contains(out, "!!!") {
			t.Fatalf("%s: the hash-only warning ran, so the flag was honoured:\n%s", what, out)
		}
	}
	if ossl := installOpenSSL(t); ossl != "" {
		out, ok := f.run(v, "TERN_OPENSSL="+ossl, "TERN_ALLOW_UNSIGNED=1")
		refused("OpenSSL", out, ok)
		if !strings.Contains(out, "needs 2 signers") || !strings.Contains(out, "does not apply here") {
			t.Fatalf("OpenSSL: the refusal doesn't say why:\n%s", out)
		}
	}

	// The same script with no OpenSSL to find: the verifier path, and then neither.
	sh, _ := os.ReadFile(f.script)
	noSSL := strings.Replace(string(sh), "find_openssl() {", "find_openssl() { return 1; }\nfind_openssl_unused() {", 1)
	if noSSL == string(sh) {
		t.Fatal("install.sh has no find_openssl")
	}
	os.WriteFile(f.script, []byte(noSSL), 0o755)
	out, ok := f.run(v, "TERN_ALLOW_UNSIGNED=1")
	if ok || !strings.Contains(out, "can't check release") {
		t.Fatalf("no OpenSSL, no verifier, TERN_ALLOW_UNSIGNED=1: installed=%v\n%s", ok, out)
	}
	if testing.Short() {
		return
	}
	bin := filepath.Join(d, "ternchain")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	// A verifier installed before signers existed: its own pin has threshold 0,
	// so it would pass the release hash-only; install.sh's pinned set decides.
	out, ok = f.run(v, "TERN_VERIFIER="+bin, "TERNCHAIN_SIGNERS_PIN="+filepath.Join(d, "pin0.json"), "TERN_ALLOW_UNSIGNED=1")
	refused("a threshold-0 verifier", out, ok)
	// A verifier pinning the same 2-of-2 set.
	pin := filepath.Join(d, "pin.json")
	writeSignerSet(pin, relSet(2, 2, nil, oscar, nina), 0o600)
	out, ok = f.run(v, "TERN_VERIFIER="+bin, "TERNCHAIN_SIGNERS_PIN="+pin, "TERN_ALLOW_UNSIGNED=1")
	refused("a 2-of-2 verifier", out, ok)
	if !strings.Contains(out, "does not apply") {
		t.Fatalf("a 2-of-2 verifier: the refusal doesn't say the flag doesn't apply:\n%s", out)
	}
}

// With no OpenSSL to use, install.sh checks the signatures with a ternchain
// installed before (TERN_VERIFIER): release verify against its pinned set.
func TestInstallShVerifierFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("builds ternchain")
	}
	shipped, _ := parseSignerSet(embeddedSigners)
	if shipped == nil || shipped.Threshold != 0 {
		t.Skip("the shipped set has signers: install.sh takes OpenSSL first")
	}
	const v = "v9.9.9"
	d := t.TempDir()
	bin := filepath.Join(d, "ternchain")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	oscar, nina := relKeygen(t, d, "oscar"), relKeygen(t, d, "nina")
	pin := filepath.Join(d, "pin.json")
	writeSignerSet(pin, relSet(2, 2, nil, oscar, nina), 0o600)
	f := newInstallFixture(t, v, nil) // the shipped install.sh: no keys of its own
	env := []string{"TERN_VERIFIER=" + bin, "TERNCHAIN_SIGNERS_PIN=" + pin}
	if out, ok := f.run(v, env...); ok || !strings.Contains(out, "refused") || !strings.Contains(out, "UNSIGNED") {
		t.Fatalf("unsigned, by the verifier: %v\n%s", ok, out)
	}
	if out, ok := f.run(v, append(env, "TERN_ALLOW_UNSIGNED=1")...); ok {
		t.Fatalf("unsigned under a pinned 2-of-2 set isn't a threshold-0 release: --allow-unsigned doesn't pass it\n%s", out)
	}
	// A verifier whose set has no signers: unsigned passes only with TERN_ALLOW_UNSIGNED=1, loudly.
	env0 := []string{"TERN_VERIFIER=" + bin, "TERNCHAIN_SIGNERS_PIN=" + filepath.Join(d, "pin0.json")}
	if out, ok := f.run(v, env0...); ok || !strings.Contains(out, "UNSIGNED") {
		t.Fatalf("unsigned, a threshold-0 verifier: %v\n%s", ok, out)
	}
	if out, ok := f.run(v, append(env0, "TERN_ALLOW_UNSIGNED=1")...); !ok || !strings.Contains(out, "!!!") {
		t.Fatalf("unsigned, a threshold-0 verifier, TERN_ALLOW_UNSIGNED=1: %v\n%s", ok, out)
	}
	relSign(t, oscar, f.sums)
	if out, ok := f.run(v, env...); ok || !strings.Contains(out, "needs 2") {
		t.Fatalf("1 of 2, by the verifier: %v\n%s", ok, out)
	}
	relSign(t, nina, f.sums)
	if out, ok := f.run(v, env...); !ok || strings.Contains(out, "UNSIGNED") {
		t.Fatalf("2 of 2, by the verifier: %v\n%s", ok, out)
	}
	os.WriteFile(f.binary, []byte("#!/bin/sh\necho evil\n"), 0o755)
	if out, ok := f.run(v, env...); ok || !strings.Contains(out, "do not run") {
		t.Fatalf("tampered, by the verifier: %v\n%s", ok, out)
	}
}

// No public release goes out unsigned by accident: scripts/publish.sh (and
// release.yml through it) refuses a signer set with threshold 0, or 1,
// unless an explicit override.
func TestPublishSignerGate(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	d := t.TempDir()
	a, b, c := relKeygen(t, d, "oscar"), relKeygen(t, d, "nina"), relKeygen(t, d, "ada")
	gate := func(set *signerSet, env ...string) (string, bool) {
		t.Helper()
		p := filepath.Join(t.TempDir(), "signers.json")
		if set == nil {
			p = repo("releases/signers.json")
		} else {
			writeSignerSet(p, set, 0o644)
		}
		cmd := exec.Command("bash", repo("scripts/publish.sh"), "--signers-only", p)
		for _, e := range os.Environ() {
			if !strings.HasPrefix(e, "RELEASE_ALLOW_") {
				cmd.Env = append(cmd.Env, e)
			}
		}
		cmd.Env = append(cmd.Env, env...)
		out, err := cmd.CombinedOutput()
		return string(out), err == nil
	}
	if out, ok := gate(&signerSet{Version: 1}); ok || !strings.Contains(out, "UNSIGNED") {
		t.Fatalf("threshold 0: %v %s", ok, out)
	}
	if out, ok := gate(&signerSet{Version: 1}, "RELEASE_ALLOW_UNSIGNED=1"); !ok || !strings.Contains(out, "warning") {
		t.Fatalf("threshold 0, RELEASE_ALLOW_UNSIGNED=1: %v %s", ok, out)
	}
	if out, ok := gate(relSet(2, 1, nil, a, b)); ok || !strings.Contains(out, "threshold 1") {
		t.Fatalf("threshold 1: %v %s", ok, out)
	}
	if out, ok := gate(relSet(2, 1, nil, a, b), "RELEASE_ALLOW_UNSIGNED=1"); ok {
		t.Fatalf("threshold 1 isn't allowed by RELEASE_ALLOW_UNSIGNED: %s", out)
	}
	if out, ok := gate(relSet(2, 1, nil, a, b), "RELEASE_ALLOW_ONE_SIGNER=1"); !ok {
		t.Fatalf("threshold 1, RELEASE_ALLOW_ONE_SIGNER=1: %s", out)
	}
	if out, ok := gate(relSet(2, 2, nil, a, b, c)); !ok || !strings.Contains(out, "threshold 2") {
		t.Fatalf("2 of 3: %v %s", ok, out)
	}
	shipped, _ := parseSignerSet(embeddedSigners)
	if out, ok := gate(nil); shipped != nil && ok != (shipped.Threshold >= 2) {
		t.Fatalf("the shipped set (threshold %d): passes=%v %s", shipped.Threshold, ok, out)
	}
}
