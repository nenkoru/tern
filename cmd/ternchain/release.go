// Release signatures by several independent signers (docs/SUPPLY-CHAIN.md).
//
// A release is a SHA256SUMS file (scripts/release.sh writes it; the build is
// reproducible, so anyone can rebuild it and get the same bytes) and
// SHA256SUMS.sigs beside it: ed25519 signatures over the SHA256SUMS bytes and
// the version, one per signer who rebuilt the release and got the same sums.
// A release counts as signed when at least the threshold of distinct signers
// of the signer set signed it.
//
// The signer set (releases/signers.json, embedded in the binary) is a rotation
// chain: each new set names the previous one's hash (prev) and carries the
// signatures of at least the previous set's threshold of its signers over the
// new set's canonical bytes. The client pins the last set it verified (in the
// user's config folder) and accepts a new set only when it is such a rotation;
// a set that replaces the pinned one without the old signers' approval is
// refused loudly (unless --accept-signer-change): that is what a coerced or
// compromised single maintainer would have to do.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/nenkoru/tern"
)

var embeddedSigners = tern.Signers

const (
	signerSetDomain  = "ternchain-signers/v1\n"
	releaseSigDomain = "ternchain-release/v1\n"
	signerKeyKind    = "ternchain-release-signer/1"
	signerChangeLoud = "THE RELEASE SIGNER SET CHANGED WITHOUT THE OLD SIGNERS' APPROVAL: possible coercion or compromise; do not install"
	unsignedStatus   = "UNSIGNED: the signer set has no signers yet (threshold 0); only the hashes were checked, and a forged release passes that too. Rebuild it yourself (scripts/repro.sh) or compare sha256sum through an independent channel"
)

// releaseSigner: one signer of a set: a name for people, an ed25519 key
// ("ed25519:BASE64", or 64 hex digits).
type releaseSigner struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// releaseSig: one signature; key says whose, signer is only a label.
type releaseSig struct {
	Signer string `json:"signer,omitempty"`
	Key    string `json:"key"`
	Sig    string `json:"sig"`
}

// signerSet: releases/signers.json.
type signerSet struct {
	Version   int             `json:"version"`
	Threshold int             `json:"threshold"`
	Signers   []releaseSigner `json:"signers"`
	Prev      string          `json:"prev"`
	Sigs      []releaseSig    `json:"sigs"`
	builtIn   bool            // the set embedded in this binary (not read from a file)
}

// releaseSigs: a release's SHA256SUMS.sigs.
type releaseSigs struct {
	Version string       `json:"version"`
	Sums    string       `json:"sums_sha256"`
	Sigs    []releaseSig `json:"sigs"`
}

// signerKeyFile: what release keygen writes (mode 0600).
type signerKeyFile struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
	Seed      string `json:"seed"`
}

func parseSignerKey(s string) (ed25519.PublicKey, error) {
	t := strings.TrimPrefix(strings.TrimSpace(s), "ed25519:")
	var raw []byte
	var err error
	if len(t) == 2*ed25519.PublicKeySize {
		raw, err = hex.DecodeString(t)
	} else {
		raw, err = base64.StdEncoding.DecodeString(t)
	}
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bad signer key %q: ed25519:BASE64 or 64 hex digits", s)
	}
	return ed25519.PublicKey(raw), nil
}

func fmtSignerKey(k ed25519.PublicKey) string {
	return "ed25519:" + base64.StdEncoding.EncodeToString(k)
}

func parseSignerSet(b []byte) (*signerSet, error) {
	var s signerSet
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("signer set: %v", err)
	}
	if err := s.check(); err != nil {
		return nil, err
	}
	return &s, nil
}

func readSignerSet(path string) (*signerSet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := parseSignerSet(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	return s, nil
}

// check: well formed: a version, distinct names and keys, 0 < threshold <=
// signers (or threshold 0 with no signers: nobody signs yet).
func (s *signerSet) check() error {
	if s.Version < 1 {
		return errors.New("signer set: version must be 1 or more")
	}
	names, keys := map[string]bool{}, map[string]bool{}
	for _, x := range s.Signers {
		k, err := parseSignerKey(x.Key)
		if err != nil {
			return fmt.Errorf("signer set: %s: %v", x.Name, err)
		}
		if x.Name == "" || names[x.Name] || keys[string(k)] {
			return fmt.Errorf("signer set: signer %q: every signer needs its own name and key", x.Name)
		}
		names[x.Name], keys[string(k)] = true, true
	}
	if s.Threshold < 0 || s.Threshold > len(s.Signers) || (s.Threshold == 0) != (len(s.Signers) == 0) {
		return fmt.Errorf("signer set: threshold %d of %d signers (0 only for a set with no signers yet)", s.Threshold, len(s.Signers))
	}
	return nil
}

// canonical: the bytes the previous set's signers sign: the set without its
// signatures, keys normalised, after a domain prefix.
func (s *signerSet) canonical() []byte {
	c := struct {
		Version   int             `json:"version"`
		Threshold int             `json:"threshold"`
		Signers   []releaseSigner `json:"signers"`
		Prev      string          `json:"prev"`
	}{s.Version, s.Threshold, []releaseSigner{}, s.Prev}
	for _, x := range s.Signers {
		k, _ := parseSignerKey(x.Key)
		c.Signers = append(c.Signers, releaseSigner{x.Name, fmtSignerKey(k)})
	}
	b, _ := json.Marshal(c)
	return append([]byte(signerSetDomain), b...)
}

// id: the set's hash, what the next set names as prev.
func (s *signerSet) id() string {
	h := sha256.Sum256(s.canonical())
	return hex.EncodeToString(h[:])
}

func (s *signerSet) names() []string {
	n := []string{}
	for _, x := range s.Signers {
		n = append(n, x.Name)
	}
	return n
}

// countSigs: how many distinct signers of set signed msg (a signer who
// signed twice counts once; a key outside the set doesn't count).
func countSigs(set []releaseSigner, msg []byte, sigs []releaseSig) (int, []string) {
	byKey := map[string]string{}
	for _, x := range set {
		if k, err := parseSignerKey(x.Key); err == nil {
			byKey[string(k)] = x.Name
		}
	}
	counted := map[string]bool{}
	var who []string
	for _, sg := range sigs {
		k, err := parseSignerKey(sg.Key)
		if err != nil {
			continue
		}
		name, ok := byKey[string(k)]
		if !ok || counted[string(k)] {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(sg.Sig)
		if err != nil || !ed25519.Verify(k, msg, raw) {
			continue
		}
		counted[string(k)] = true
		who = append(who, name)
	}
	return len(who), who
}

// checkRotation: next is a proper successor of prev: a later version naming
// prev's hash, signed by at least prev's threshold of prev's signers.
func checkRotation(prev, next *signerSet) error {
	if next.Version <= prev.Version {
		return fmt.Errorf("set v%d does not follow v%d", next.Version, prev.Version)
	}
	if next.Prev != prev.id() {
		return fmt.Errorf("set v%d names prev %q, not the hash of v%d (%s)", next.Version, next.Prev, prev.Version, prev.id())
	}
	if n, _ := countSigs(prev.Signers, next.canonical(), next.Sigs); n < prev.Threshold {
		return fmt.Errorf("set v%d is signed by %d of v%d's signers, its threshold is %d", next.Version, n, prev.Version, prev.Threshold)
	}
	return nil
}

// signerChange: what a rotation added and removed, for the notice.
func signerChange(from, to *signerSet) string {
	old, now := map[string]string{}, map[string]string{}
	for _, x := range from.Signers {
		k, _ := parseSignerKey(x.Key)
		old[string(k)] = x.Name
	}
	for _, x := range to.Signers {
		k, _ := parseSignerKey(x.Key)
		now[string(k)] = x.Name
	}
	var added, removed []string
	for k, n := range now {
		if _, ok := old[k]; !ok {
			added = append(added, n)
		}
	}
	for k, n := range old {
		if _, ok := now[k]; !ok {
			removed = append(removed, n)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	s := fmt.Sprintf("the release signer set changed from v%d (%d of %v) to v%d (%d of %v), approved by the old signers", from.Version, from.Threshold,
		from.names(), to.Version, to.Threshold, to.names())
	if from.Threshold == 0 {
		s = fmt.Sprintf("the release signer set is now v%d (%d of %v); the pinned set had no signers, so nobody approved it: trust on first use", to.Version, to.Threshold, to.names())
	}
	return fmt.Sprintf("%s; added: %v; removed: %v", s, added, removed)
}

// signerResolution: the set a release is checked against, and what to tell.
type signerResolution struct {
	set     *signerSet
	notice  string // a proper rotation happened
	warning string // a set changed without approval (accepted only with the override)
	changed bool   // the pin should be updated
}

// resolveSigners: from the pinned set and the sets a release offers (its
// signers.json, the embedded one of a newer binary), the set to check against.
// Offered sets with a version at or below the pinned one are ignored when
// they are the pinned set or older; one with the pinned version but another
// hash is a fork. Every later one must chain from the pinned set.
func resolveSigners(pinned *signerSet, offered []*signerSet, override bool) (signerResolution, error) {
	pid := pinned.id()
	var later []*signerSet
	seen := map[string]bool{pid: true}
	for _, s := range offered {
		id := s.id()
		if seen[id] {
			continue
		}
		seen[id] = true
		switch {
		case s.Version > pinned.Version:
			later = append(later, s)
		case s.Version == pinned.Version:
			return refuseSigners(pinned, s, fmt.Sprintf("another set with the pinned version v%d (%s, pinned %s)", s.Version, id, pid), override)
		}
	}
	if len(later) == 0 {
		return signerResolution{set: pinned}, nil
	}
	sort.Slice(later, func(i, j int) bool { return later[i].Version < later[j].Version })
	cur := pinned
	for _, s := range later {
		if s.Version == cur.Version {
			return refuseSigners(pinned, later[len(later)-1], fmt.Sprintf("two different sets with version v%d", s.Version), override)
		}
		if cur.Threshold == 0 && !s.builtIn {
			// Nobody can approve the first real set: one read from a release
			// folder or a file is whatever the server put there (an attacker
			// who gets one fake release in front of a user would own the pin).
			// Only the set built into this binary is taken on first use;
			// any other needs the owner's out-of-band check.
			return refuseSigners(pinned, later[len(later)-1], fmt.Sprintf("the pinned set has no signers (threshold 0), so nobody approved set v%d (%s): it came with the release, not with this binary; compare its id with the maintainers' own channels, then --accept-signer-change", s.Version, s.id()), override)
		}
		if err := checkRotation(cur, s); err != nil {
			return refuseSigners(pinned, later[len(later)-1], err.Error(), override)
		}
		cur = s
	}
	return signerResolution{set: cur, notice: signerChange(pinned, cur), changed: true}, nil
}

func refuseSigners(pinned, offered *signerSet, why string, override bool) (signerResolution, error) {
	bar := strings.Repeat("!", 100)
	w := fmt.Sprintf("%s\n%s.\n  pinned set:  v%d %s, %d of %v\n  offered set: v%d %s, %d of %v\n  why: %s\n%s",
		bar, signerChangeLoud, pinned.Version, pinned.id(), pinned.Threshold, pinned.names(),
		offered.Version, offered.id(), offered.Threshold, offered.names(), why, bar)
	if !override {
		return signerResolution{warning: w}, fmt.Errorf("%s (%s); if you know why, out of band, from several of the old signers: --accept-signer-change", signerChangeLoud, why)
	}
	return signerResolution{set: offered, warning: w + "\n--accept-signer-change: checking against the offered set anyway; it is now pinned", changed: true}, nil
}

// signerPinPath: where the last verified set is remembered.
func signerPinPath(flagPath string) string {
	if flagPath != "" {
		return flagPath
	}
	if p := os.Getenv("TERNCHAIN_SIGNERS_PIN"); p != "" {
		return p
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "ternchain", "signers.json")
}

// pinnedSigners: the pinned set on disk, else the embedded one.
func pinnedSigners(pinPath string) (*signerSet, string, error) {
	if pinPath != "" {
		if b, err := os.ReadFile(pinPath); err == nil {
			s, err := parseSignerSet(b)
			if err != nil {
				return nil, "", fmt.Errorf("the pinned signer set %s: %v (remove it to start again from the one built in)", pinPath, err)
			}
			return s, pinPath, nil
		}
	}
	s, err := parseSignerSet(embeddedSigners)
	if err != nil {
		return nil, "", fmt.Errorf("the built-in releases/signers.json: %v", err)
	}
	return s, "built in", nil
}

func writeSignerSet(path string, s *signerSet, mode os.FileMode) error {
	if s.Signers == nil {
		s.Signers = []releaseSigner{}
	}
	if s.Sigs == nil {
		s.Sigs = []releaseSig{}
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, append(b, '\n'), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// releaseMessage: what a signer signs for a release: the version and the
// SHA256SUMS bytes' hash, after a domain prefix.
func releaseMessage(version string, sums []byte) []byte {
	h := sha256.Sum256(sums)
	return []byte(releaseSigDomain + version + "\n" + hex.EncodeToString(h[:]))
}

type sumLine struct{ hash, name string }

func parseSums(b []byte) ([]sumLine, error) {
	var ls []sumLine
	for i, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		f := strings.Fields(l)
		if len(f) != 2 || len(f[0]) != 64 {
			return nil, fmt.Errorf("SHA256SUMS line %d: %q is not HASH  NAME", i+1, l)
		}
		if _, err := hex.DecodeString(f[0]); err != nil {
			return nil, fmt.Errorf("SHA256SUMS line %d: bad hash", i+1)
		}
		name := strings.TrimPrefix(f[1], "*")
		if name != filepath.Base(name) || name == "." || name == ".." {
			return nil, fmt.Errorf("SHA256SUMS line %d: %q is not a plain file name", i+1, name)
		}
		ls = append(ls, sumLine{strings.ToLower(f[0]), name})
	}
	if len(ls) == 0 {
		return nil, errors.New("SHA256SUMS lists no files")
	}
	return ls, nil
}

// sumsVersion: the version the file names carry (ternchain-VERSION-OS-ARCH,
// ternchain-ops-VERSION-OS-ARCH): all must agree.
func sumsVersion(ls []sumLine) (string, error) {
	v := ""
	for _, l := range ls {
		n := strings.TrimSuffix(l.name, ".exe")
		rest, ok := strings.CutPrefix(n, "ternchain-ops-")
		if !ok {
			rest, ok = strings.CutPrefix(n, "ternchain-")
		}
		parts := strings.Split(rest, "-")
		if !ok || len(parts) < 3 {
			return "", fmt.Errorf("%s is not ternchain[-ops]-VERSION-OS-ARCH: give --version", l.name)
		}
		x := strings.Join(parts[:len(parts)-2], "-")
		if v != "" && x != v {
			return "", fmt.Errorf("SHA256SUMS mixes versions %s and %s", v, x)
		}
		v = x
	}
	return v, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sumsPath: DIR or DIR/SHA256SUMS.
func sumsPath(arg string) string {
	if fi, err := os.Stat(arg); err == nil && fi.IsDir() {
		return filepath.Join(arg, "SHA256SUMS")
	}
	return arg
}

// selfSumsPath: for version --verify DIR, the sums that list this binary:
// ternchain-ops is in DIR/SHA256SUMS-ops (release.yml), signed the same way
// (SHA256SUMS-ops.sigs, the same signer set and threshold).
func selfSumsPath(arg string, ops bool) string {
	if fi, err := os.Stat(arg); err == nil && fi.IsDir() && ops {
		if p := filepath.Join(arg, "SHA256SUMS-ops"); fileExists(p) {
			return p
		}
	}
	return arg
}

// releaseReport: what release verify found.
type releaseReport struct {
	Version   string
	Signed    bool
	SignedBy  []string
	Files     map[string]string // name -> ok | mismatch | not here
	Lines     []sumLine
	Threshold int
}

// verifyRelease: SHA256SUMS against its .sigs and set, then each listed file
// that is beside it against its hash. A set with threshold 0 reports the
// release unsigned (the hashes are still checked).
func verifyRelease(path string, set *signerSet, wantVersion string) (*releaseReport, error) {
	sums, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ls, err := parseSums(sums)
	if err != nil {
		return nil, err
	}
	r := &releaseReport{Files: map[string]string{}, Lines: ls, Threshold: set.Threshold}
	var sigs releaseSigs
	sigErr := readJSON(path+".sigs", &sigs)
	if sigErr != nil && !os.IsNotExist(sigErr) {
		return nil, fmt.Errorf("%s.sigs: %v", path, sigErr)
	}
	v := sigs.Version
	if sigErr != nil {
		if v, err = sumsVersion(ls); err != nil {
			if wantVersion == "" {
				return nil, err
			}
			v = wantVersion
		}
	}
	if v == "" {
		return nil, fmt.Errorf("%s.sigs names no version", path)
	}
	if wantVersion != "" && v != wantVersion {
		return nil, fmt.Errorf("the release is %q, not %q", v, wantVersion)
	}
	r.Version = v
	for _, l := range ls {
		if !strings.Contains(l.name, "-"+v+"-") {
			return nil, fmt.Errorf("%s does not carry the signed version %q", l.name, v)
		}
	}
	if set.Threshold > 0 {
		if sigErr != nil {
			return nil, fmt.Errorf("%s.sigs is missing: the release is not signed (the signer set needs %d of %v)", path, set.Threshold, set.names())
		}
		h := sha256.Sum256(sums)
		if !strings.EqualFold(sigs.Sums, hex.EncodeToString(h[:])) {
			return nil, fmt.Errorf("%s.sigs signs another SHA256SUMS (sha256 %s, this one is %x)", path, sigs.Sums, h)
		}
		n, who := countSigs(set.Signers, releaseMessage(v, sums), sigs.Sigs)
		if n < set.Threshold {
			return nil, fmt.Errorf("release %s is signed by %d distinct signers %v of the set (v%d); it needs %d of %v", v, n, who, set.Version, set.Threshold, set.names())
		}
		r.Signed, r.SignedBy = true, who
	}
	var bad []string
	dir := filepath.Dir(path)
	for _, l := range ls {
		got, err := fileSHA256(filepath.Join(dir, l.name))
		switch {
		case os.IsNotExist(err):
			r.Files[l.name] = "not here"
		case err != nil:
			return nil, err
		case got == l.hash:
			r.Files[l.name] = "ok"
		default:
			r.Files[l.name] = "mismatch"
			bad = append(bad, l.name)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return r, fmt.Errorf("do not run: %v differ from the SHA256SUMS of release %s", bad, v)
	}
	return r, nil
}

// releaseOpts: release verify's and version --verify's flags.
type releaseOpts struct {
	signers  string // comma-separated signer set files the release offers
	pin      string
	override bool
	unsigned bool // --allow-unsigned: accept a hash-only check of a release no signer set covers
	version  string
	notes    io.Writer // notices and warnings (stderr)
}

// verifyReleaseCLI: resolve the signer set (pinned, plus what the release
// and this binary offer), verify, and remember the set.
func verifyReleaseCLI(arg string, o releaseOpts) (*releaseReport, map[string]any, error) {
	if o.notes == nil {
		o.notes = os.Stderr
	}
	pinPath := signerPinPath(o.pin)
	pinned, from, err := pinnedSigners(pinPath)
	if err != nil {
		return nil, nil, err
	}
	var offered []*signerSet
	if emb, err := parseSignerSet(embeddedSigners); err == nil {
		emb.builtIn = true
		offered = append(offered, emb)
	}
	path := sumsPath(arg)
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "signers*.json"))
	if o.signers != "" {
		files = append(files, strings.Split(o.signers, ",")...)
	}
	for _, f := range files {
		s, err := readSignerSet(f)
		if err != nil {
			return nil, nil, err
		}
		offered = append(offered, s)
	}
	res, err := resolveSigners(pinned, offered, o.override)
	if res.warning != "" {
		fmt.Fprintln(o.notes, res.warning)
	}
	if err != nil {
		return nil, nil, err
	}
	if res.notice != "" {
		fmt.Fprintln(o.notes, "notice: "+res.notice)
	}
	r, err := verifyRelease(path, res.set, o.version)
	if err != nil {
		return r, nil, err
	}
	m := map[string]any{"version": r.Version, "signed": r.Signed, "files": r.Files,
		"signer_set": map[string]any{"version": res.set.Version, "id": res.set.id(), "threshold": res.set.Threshold, "signers": res.set.names(), "pinned_from": from}}
	if r.Signed {
		m["signed_by"] = r.SignedBy
	} else {
		m["status"] = unsignedStatus
		if !o.unsigned {
			return r, m, errors.New(unsignedStatus + " (--allow-unsigned accepts a hash-only check)")
		}
		fmt.Fprintln(o.notes, "warning: "+unsignedStatus)
	}
	if res.notice != "" {
		m["signer_set_notice"] = res.notice
	}
	if res.warning != "" {
		m["signer_set_warning"] = signerChangeLoud
	}
	if pinPath != "" && (res.changed || from != pinPath) {
		if err := writeSignerSet(pinPath, res.set, 0o600); err != nil {
			m["pin"] = "not saved: " + err.Error()
		} else {
			m["pin"] = pinPath
		}
	}
	return r, m, nil
}

// selfCheck: this binary's sha256 is one of the verified SHA256SUMS' files.
func selfCheck(r *releaseReport, m map[string]any) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	h, err := fileSHA256(exe)
	if err != nil {
		return err
	}
	self := map[string]any{"path": exe, "sha256": h}
	m["self"] = self
	for _, l := range r.Lines {
		if l.hash == h {
			self["listed_as"] = l.name
			if !strings.Contains(l.name, runtime.GOOS+"-"+runtime.GOARCH) {
				self["note"] = "listed for another platform than " + runtime.GOOS + "/" + runtime.GOARCH
			}
			return nil
		}
	}
	return fmt.Errorf("this binary (%s, sha256 %s) is not in release %s's verified SHA256SUMS: it is not that release's build", exe, h, r.Version)
}

func readSignerKey(path string) (ed25519.PrivateKey, string, error) {
	var k signerKeyFile
	if err := readJSON(path, &k); err != nil {
		return nil, "", fmt.Errorf("signer key %s: %v", path, err)
	}
	seed, err := base64.StdEncoding.DecodeString(k.Seed)
	if k.Kind != signerKeyKind || err != nil || len(seed) != ed25519.SeedSize {
		return nil, "", fmt.Errorf("%s is not a release signer key (ternchain release keygen --out FILE)", path)
	}
	return ed25519.NewKeyFromSeed(seed), k.Name, nil
}

func releaseKeygen(path, name string) (string, error) {
	if path == "" {
		return "", errors.New("release keygen --out FILE")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(signerKeyFile{signerKeyKind, name, fmtSignerKey(pub), base64.StdEncoding.EncodeToString(priv.Seed())}, "", "  ")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("%v (release keygen never overwrites a key)", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return "", err
	}
	return fmtSignerKey(pub), f.Close()
}

// releaseSign: adds (or replaces) this key's signature in SHA256SUMS.sigs.
// Files of the release beside SHA256SUMS must match it.
func releaseSign(keyPath, arg, version string) (map[string]any, error) {
	priv, name, err := readSignerKey(keyPath)
	if err != nil {
		return nil, err
	}
	path := sumsPath(arg)
	sums, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ls, err := parseSums(sums)
	if err != nil {
		return nil, err
	}
	if version == "" {
		if version, err = sumsVersion(ls); err != nil {
			return nil, err
		}
	}
	checked := 0
	for _, l := range ls {
		got, err := fileSHA256(filepath.Join(filepath.Dir(path), l.name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || got != l.hash {
			return nil, fmt.Errorf("refusing to sign: %s beside SHA256SUMS does not match it", l.name)
		}
		checked++
	}
	h := sha256.Sum256(sums)
	var rs releaseSigs
	if err := readJSON(path+".sigs", &rs); err == nil {
		if rs.Version != version || !strings.EqualFold(rs.Sums, hex.EncodeToString(h[:])) {
			return nil, fmt.Errorf("%s.sigs holds signatures over another release (%s, sums %s): not mixing them", path, rs.Version, rs.Sums)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	rs.Version, rs.Sums = version, hex.EncodeToString(h[:])
	pub := priv.Public().(ed25519.PublicKey)
	sig := releaseSig{name, fmtSignerKey(pub), base64.StdEncoding.EncodeToString(ed25519.Sign(priv, releaseMessage(version, sums)))}
	rs.Sigs = replaceSig(rs.Sigs, sig)
	b, _ := json.MarshalIndent(rs, "", "  ")
	if err := os.WriteFile(path+".sigs", append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	return map[string]any{"public_key": sig.Key, "signer": name, "version": version, "sigs_file": path + ".sigs", "signatures": len(rs.Sigs),
		"files_checked": checked}, nil
}

func replaceSig(sigs []releaseSig, s releaseSig) []releaseSig {
	k, _ := parseSignerKey(s.Key)
	var out []releaseSig
	for _, x := range sigs {
		if xk, err := parseSignerKey(x.Key); err == nil && bytes.Equal(xk, k) {
			continue
		}
		out = append(out, x)
	}
	return append(out, s)
}

// releaseSignSet: this key approves a new signer set (a signer of the set
// before it, rotating to it).
func releaseSignSet(keyPath, path string) (map[string]any, error) {
	priv, name, err := readSignerKey(keyPath)
	if err != nil {
		return nil, err
	}
	s, err := readSignerSet(path)
	if err != nil {
		return nil, err
	}
	pub := priv.Public().(ed25519.PublicKey)
	sig := releaseSig{name, fmtSignerKey(pub), base64.StdEncoding.EncodeToString(ed25519.Sign(priv, s.canonical()))}
	s.Sigs = replaceSig(s.Sigs, sig)
	if err := writeSignerSet(path, s, 0o644); err != nil {
		return nil, err
	}
	return map[string]any{"public_key": sig.Key, "signer": name, "set_version": s.Version, "set_id": s.id(), "prev": s.Prev, "signatures": len(s.Sigs)}, nil
}

// releaseSignersInfo: a set (FILE, else the pinned one) and, for a FILE,
// whether the pinned set would accept it.
func releaseSignersInfo(file, pin string) (map[string]any, error) {
	pinPath := signerPinPath(pin)
	pinned, from, err := pinnedSigners(pinPath)
	if err != nil {
		return nil, err
	}
	s := pinned
	if file != "" {
		if s, err = readSignerSet(file); err != nil {
			return nil, err
		}
	}
	m := map[string]any{"version": s.Version, "id": s.id(), "threshold": s.Threshold, "signers": s.Signers, "prev": s.Prev,
		"pinned": map[string]any{"version": pinned.Version, "id": pinned.id(), "from": from}}
	if file != "" && s.id() != pinned.id() {
		if err := checkRotation(pinned, s); err != nil {
			m["rotation"] = "not accepted: " + err.Error()
		} else {
			m["rotation"] = "accepted: " + signerChange(pinned, s)
		}
	}
	return m, nil
}

// runRelease: release keygen|sign|sign-set|verify|signers.
func runRelease(args []string) error {
	const usage = "ternchain release keygen --out FILE [--name NAME] | release sign --key FILE SHA256SUMS|DIR [--version V] | " +
		"release sign-set --key FILE SIGNERS.json | release verify SHA256SUMS|DIR [--signers FILE,…] [--accept-signer-change] [--allow-unsigned] | release signers [FILE]"
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: " + usage)
	}
	cmd := "release " + args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	outF := fs.String("out", "", "")
	name := fs.String("name", "", "")
	key := fs.String("key", "", "")
	signers := fs.String("signers", "", "")
	pin := fs.String("pin", "", "")
	ver := fs.String("version", "", "")
	override := fs.Bool("accept-signer-change", false, "")
	allowUnsigned := fs.Bool("allow-unsigned", false, "")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return fmt.Errorf("%v (usage: %s)", err, usage)
	}
	need := func(what string) (string, error) {
		if len(pos) == 0 {
			return "", fmt.Errorf("%s: missing %s (ternchain %s --help)", cmd, what, cmd)
		}
		return pos[0], nil
	}
	switch cmd {
	case "release keygen":
		pub, err := releaseKeygen(*outF, *name)
		if err != nil {
			return err
		}
		out(map[string]any{"public_key": pub, "name": *name, "key_file": *outF + " (mode 0600: your release signing key; keep it offline)",
			"next": "send the maintainers your name and public_key for releases/signers.json; per release: scripts/repro.sh, then ternchain release sign --key " + *outF + " SHA256SUMS"})
		return nil
	case "release sign":
		p, err := need("SHA256SUMS")
		if err != nil {
			return err
		}
		if *key == "" {
			return errors.New("release sign --key FILE SHA256SUMS")
		}
		m, err := releaseSign(*key, p, *ver)
		if err != nil {
			return err
		}
		out(m)
		return nil
	case "release sign-set":
		p, err := need("SIGNERS.json")
		if err != nil {
			return err
		}
		if *key == "" {
			return errors.New("release sign-set --key FILE SIGNERS.json")
		}
		m, err := releaseSignSet(*key, p)
		if err != nil {
			return err
		}
		out(m)
		return nil
	case "release verify":
		p, err := need("SHA256SUMS or DIR")
		if err != nil {
			return err
		}
		_, m, err := verifyReleaseCLI(p, releaseOpts{signers: *signers, pin: *pin, override: *override, unsigned: *allowUnsigned, version: *ver})
		if err != nil {
			return err
		}
		out(m)
		return nil
	case "release signers":
		m, err := releaseSignersInfo(firstOr(pos, 0, ""), *pin)
		if err != nil {
			return err
		}
		out(m)
		return nil
	}
	return unknownCommand(cmd)
}
