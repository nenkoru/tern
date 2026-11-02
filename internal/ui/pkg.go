package ui

// Signed artifacts: UI packages (tern.ui-package/1), ranking modules
// (tern.ranker/1) and client policy modules (tern.policy/1). A bundle is a folder: manifest.json (it lists every other
// file's sha256), the files, and SIGNATURE.json: the author's key signs the
// manifest's hash. Installed copies are pinned by hash; nothing updates
// itself: a newer version is shown, and the owner moves (Use).

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	KindPackage = "tern.ui-package/1"
	KindRanker  = "tern.ranker/1"
	sigFile     = "SIGNATURE.json"
	manFile     = "manifest.json"
)

// Header: the fields every artifact manifest has.
type Header struct {
	Schema  string            `json:"schema"`
	Name    string            `json:"name"`
	Version string            `json:"version"`
	Line    string            `json:"line"`
	Author  string            `json:"author"`
	Payee   string            `json:"payee"`
	Parent  *ParentRef        `json:"parent,omitempty"`
	Royalty *Royalty          `json:"royalty,omitempty"`
	Files   map[string]string `json:"files"`
}

type ParentRef struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Hash    string `json:"hash"`
	Payee   string `json:"payee"`
	Author  string `json:"author"`
}

type Royalty struct {
	ParentBps int `json:"parent_bps"`
}

// Signature: SIGNATURE.json.
type Signature struct {
	Schema   string `json:"schema"`   // tern.sig/1
	Manifest string `json:"manifest"` // 0x sha256 of the canonical manifest
	Signer   string `json:"signer"`   // the address that signed
	Name     string `json:"name"`     // the name it signs for (checked on chain by the caller)
	Sig      string `json:"sig"`
}

// Bundle: an opened, checked artifact.
type Bundle struct {
	Dir    string
	Raw    []byte // the canonical manifest
	Header Header
	Hash   string // 0x sha256 of Raw
	Sig    Signature
}

func canonical(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v) // object keys sorted: one byte string per manifest
}

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// digest: what the key signs (domain-separated from mail and transactions).
func digest(manifestHash string) []byte {
	return crypto.Keccak256([]byte("\x19tern.artifact:\n" + manifestHash))
}

// fileHashes: every file under dir but the manifest and the signature.
func fileHashes(dir string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == manFile || rel == sigFile || strings.HasPrefix(filepath.Base(rel), ".") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = sha(b)
		return nil
	})
	return out, err
}

// Seal fills the manifest's author, payee (unless set) and file hashes,
// validates it, writes it canonically and signs its hash with key.
func Seal(dir, author string, key *ecdsa.PrivateKey) (*Bundle, error) {
	raw, err := os.ReadFile(filepath.Join(dir, manFile))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	files, err := fileHashes(dir)
	if err != nil {
		return nil, err
	}
	fm := map[string]any{}
	for k, v := range files {
		fm[k] = v
	}
	m["files"], m["author"] = fm, author
	if p, _ := m["payee"].(string); p == "" {
		m["payee"] = author
	}
	b, _ := json.Marshal(m)
	b, _ = canonical(b)
	if err := validateManifest(b); err != nil {
		return nil, err
	}
	pretty, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, manFile), append(pretty, '\n'), 0o644); err != nil {
		return nil, err
	}
	h := "0x" + sha(b)
	sig, err := crypto.Sign(digest(h), key)
	if err != nil {
		return nil, err
	}
	s := Signature{Schema: "tern.sig/1", Manifest: h, Signer: crypto.PubkeyToAddress(key.PublicKey).Hex(), Name: author, Sig: "0x" + hex.EncodeToString(sig)}
	sb, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, sigFile), append(sb, '\n'), 0o644); err != nil {
		return nil, err
	}
	return Open(dir)
}

func validateManifest(canon []byte) error {
	var h Header
	if err := json.Unmarshal(canon, &h); err != nil {
		return err
	}
	switch h.Schema {
	case KindPackage:
		if err := ValidateJSON(KindPackage, "", canon); err != nil {
			return err
		}
	case KindRanker:
		if _, err := CompileRanker(canon); err != nil {
			return err
		}
	case KindPolicy:
		if _, err := CompilePolicy(canon); err != nil {
			return err
		}
	default:
		return fmt.Errorf("manifest schema %q: %s, %s or %s", h.Schema, KindPackage, KindRanker, KindPolicy)
	}
	if want := h.Name + "@" + major(h.Version); h.Line != want {
		return fmt.Errorf("line %q: a %s %s belongs to line %s (a new major version is a new line)", h.Line, h.Name, h.Version, want)
	}
	if h.Royalty != nil && h.Parent == nil {
		return errors.New("a royalty without a parent")
	}
	return nil
}

func major(v string) string { m, _, _ := strings.Cut(v, "."); return m }

// Open reads a bundle and checks it: every file's hash against the
// manifest, no file the manifest doesn't list, the manifest against its
// schema, and the signature over the manifest's hash. Whether the signer
// holds the author's name is the chain's to say (the caller checks).
func Open(dir string) (*Bundle, error) {
	raw, err := os.ReadFile(filepath.Join(dir, manFile))
	if err != nil {
		return nil, err
	}
	canon, err := canonical(raw)
	if err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	b := &Bundle{Dir: dir, Raw: canon, Hash: "0x" + sha(canon)}
	if err := json.Unmarshal(canon, &b.Header); err != nil {
		return nil, err
	}
	if err := validateManifest(canon); err != nil {
		return nil, err
	}
	have, err := fileHashes(dir)
	if err != nil {
		return nil, err
	}
	for f, h := range b.Header.Files {
		if have[f] != h {
			return nil, fmt.Errorf("%s: %s doesn't match its manifest hash (changed or missing)", b.Header.Name, f)
		}
	}
	for f := range have {
		if _, ok := b.Header.Files[f]; !ok {
			return nil, fmt.Errorf("%s: %s is not in the manifest", b.Header.Name, f)
		}
	}
	sb, err := os.ReadFile(filepath.Join(dir, sigFile))
	if err != nil {
		return nil, fmt.Errorf("%s: unsigned (%v)", b.Header.Name, err)
	}
	if err := json.Unmarshal(sb, &b.Sig); err != nil {
		return nil, err
	}
	if b.Sig.Manifest != b.Hash {
		return nil, fmt.Errorf("%s: the signature is for manifest %s, this one is %s", b.Header.Name, short(b.Sig.Manifest), short(b.Hash))
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(b.Sig.Sig, "0x"))
	if err != nil || len(sig) != 65 {
		return nil, fmt.Errorf("%s: bad signature", b.Header.Name)
	}
	pub, err := crypto.SigToPub(digest(b.Hash), sig)
	if err != nil {
		return nil, fmt.Errorf("%s: bad signature: %v", b.Header.Name, err)
	}
	if got := crypto.PubkeyToAddress(*pub); got != common.HexToAddress(b.Sig.Signer) {
		return nil, fmt.Errorf("%s: signed by %s, not by %s as it says", b.Header.Name, got.Hex(), b.Sig.Signer)
	}
	if b.Sig.Name != b.Header.Author {
		return nil, fmt.Errorf("%s: signed for %s, but its author is %s", b.Header.Name, b.Sig.Name, b.Header.Author)
	}
	return b, nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// File: a file of the bundle (its hash checked again: tamper-evident on every read).
func (b *Bundle) File(rel string) ([]byte, error) {
	want, ok := b.Header.Files[rel]
	if !ok {
		return nil, fmt.Errorf("%s has no file %s", b.Header.Name, rel)
	}
	data, err := os.ReadFile(filepath.Join(b.Dir, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	if sha(data) != want {
		return nil, fmt.Errorf("%s: %s changed since it was signed", b.Header.Name, rel)
	}
	return data, nil
}

// ---- the local store: installed, pinned, never auto-updated

type Pin struct {
	Kind        string `json:"kind"`
	Line        string `json:"line"`
	Version     string `json:"version"`
	Hash        string `json:"hash"`
	Author      string `json:"author"`
	Signer      string `json:"signer"`
	Dir         string `json:"dir"` // relative to the store
	InstalledAt int64  `json:"installed_at"`
	Note        string `json:"note,omitempty"` // e.g. "honest fork of reddit-classic 1.0.0 (10% to anya)"
}

type Store struct{ Dir string }

type storeFile struct {
	Pins map[string]Pin `json:"pins"` // line => the version in use
}

func (s Store) path() string { return filepath.Join(s.Dir, "installed.json") }

func (s Store) read() storeFile {
	var f storeFile
	if b, err := os.ReadFile(s.path()); err == nil {
		json.Unmarshal(b, &f)
	}
	if f.Pins == nil {
		f.Pins = map[string]Pin{}
	}
	return f
}

func (s Store) write(f storeFile) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

// Install copies a checked bundle into the store and pins its line to this
// version (the owner's explicit choice). It never touches other lines.
func (s Store) Install(b *Bundle, note string) (Pin, error) {
	rel := filepath.Join("bundles", b.Header.Name, b.Header.Version+"-"+b.Hash[2:10])
	dst := filepath.Join(s.Dir, rel)
	if err := copyDir(b.Dir, dst); err != nil {
		return Pin{}, err
	}
	nb, err := Open(dst)
	if err != nil {
		return Pin{}, fmt.Errorf("the installed copy: %w", err)
	}
	if nb.Hash != b.Hash {
		return Pin{}, errors.New("the installed copy differs from what was checked")
	}
	p := Pin{Kind: b.Header.Schema, Line: b.Header.Line, Version: b.Header.Version, Hash: b.Hash, Author: b.Header.Author,
		Signer: b.Sig.Signer, Dir: rel, InstalledAt: time.Now().Unix(), Note: note}
	f := s.read()
	f.Pins[p.Line] = p
	return p, s.write(f)
}

func (s Store) Pins() []Pin {
	f := s.read()
	var out []Pin
	for _, p := range f.Pins {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

func (s Store) Pin(line string) (Pin, bool) {
	p, ok := s.read().Pins[line]
	return p, ok
}

// Load opens the pinned version of a line, re-checking its hash.
func (s Store) Load(line string) (*Bundle, error) {
	p, ok := s.Pin(line)
	if !ok {
		return nil, fmt.Errorf("%s is not installed", line)
	}
	b, err := Open(filepath.Join(s.Dir, p.Dir))
	if err != nil {
		return nil, err
	}
	if b.Hash != p.Hash {
		return nil, fmt.Errorf("%s: the installed files are not the ones pinned (%s)", line, short(p.Hash))
	}
	return b, nil
}

// Ref: a published version somewhere (an on-chain claim, a shared folder).
type Ref struct {
	Kind, Name, Version, Line, Hash, Author, URI string
}

// Update: a newer version of an installed line; shown, never applied.
type Update struct {
	Line      string `json:"line"`
	Installed string `json:"installed"`
	Available string `json:"available"`
	Hash      string `json:"hash"`
	Author    string `json:"author"`
	Note      string `json:"note"`
}

// Updates: newer versions on the lines the owner installed (by the same
// author only: a stranger can't publish "the next version" of your package).
func (s Store) Updates(avail []Ref) []Update {
	var out []Update
	pins := s.read().Pins
	best := map[string]Ref{}
	for _, r := range avail {
		p, ok := pins[r.Line]
		if !ok || r.Author != p.Author || !newer(r.Version, p.Version) {
			continue
		}
		if b, ok := best[r.Line]; !ok || newer(r.Version, b.Version) {
			best[r.Line] = r
		}
	}
	for line, r := range best {
		out = append(out, Update{Line: line, Installed: pins[line].Version, Available: r.Version, Hash: r.Hash, Author: r.Author,
			Note: "not installed: nothing updates itself; 'ui install " + r.Name + "@" + r.Version + "' moves to it"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

func newer(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3 && i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x > y
		}
	}
	return false
}

func copyDir(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(t, 0o755)
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(t, b, 0o644)
	})
}

// ---- forks: a fork names its parent and pays it a share through a split

// Fork copies a bundle to out as a new line name@1 (version 1.0.0) whose
// manifest names the parent and the royalty; Seal it after editing. The
// payee is set at publish: a split giving parentBps to the parent's payee.
func Fork(parent *Bundle, out, name string, parentBps int) error {
	if err := copyDir(parent.Dir, out); err != nil {
		return err
	}
	os.Remove(filepath.Join(out, sigFile))
	var m map[string]any
	json.Unmarshal(parent.Raw, &m)
	m["name"], m["version"], m["line"] = name, "1.0.0", name+"@1"
	m["parent"] = map[string]any{"name": parent.Header.Name, "version": parent.Header.Version, "hash": parent.Hash,
		"payee": parent.Header.Payee, "author": parent.Header.Author}
	m["royalty"] = map[string]any{"parent_bps": parentBps}
	delete(m, "payee")
	delete(m, "author")
	if d, ok := m["description"].(string); ok {
		m["description"] = clip("fork of "+parent.Header.Name+": "+d, 290)
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(filepath.Join(out, manFile), append(b, '\n'), 0o644)
}

// ForkCheck: what the client shows about a fork's economics. shares are the
// payee split's shares by payee id (nil: the payee is not a split); idOf maps
// a name to its id the way the chain does.
func ForkCheck(h Header, shares map[string]int, idOf func(string) string) (honest bool, note string) {
	if h.Parent == nil {
		return true, "original"
	}
	want := 0
	if h.Royalty != nil {
		want = h.Royalty.ParentBps
	}
	of := fmt.Sprintf("fork of %s %s by %s", h.Parent.Name, h.Parent.Version, h.Parent.Author)
	if want == 0 {
		return false, of + " without royalty (it pays its parent nothing)"
	}
	if shares == nil {
		return false, fmt.Sprintf("%s without royalty: it promises %d bps to the parent, but its payee %s is not a split", of, want, h.Payee)
	}
	got := shares[idOf(h.Parent.Payee)]
	if got < want {
		return false, fmt.Sprintf("%s without royalty: its payee split gives the parent %d bps, not the %d it promises", of, got, want)
	}
	return true, fmt.Sprintf("honest %s (%.0f%% of what it earns goes to %s, checked on chain)", of, float64(got)/100, h.Parent.Payee)
}
