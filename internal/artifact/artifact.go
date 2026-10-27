// Package artifact is the file format of Tern's artifacts, with no chain in
// it: sealing (age STREAM), the chunk hashes and their Merkle root, the
// on-chain uri, a BitTorrent v2 (BEP 52) .torrent, and opening a sealed file
// or folder, all streamed: nothing is held in memory but a few buffers.
//
// Encryption is age (filippo.io/age): its STREAM format seals 64 KiB chunks,
// each authenticated, so a reader never needs the whole file. The key is 32
// random bytes (what is sold; keccak256 of it is the on-chain commitment);
// the age passphrase is its hex, wrapped with scrypt at a low work factor (a
// random 256-bit key needs no stretching), so the stock age tool opens it
// too: age -d -o OUT FILE.enc, passphrase = the key in hex.
//
// On chain the artifact keeps sha256(.enc), the size and a uri (≤ 512
// bytes): the distribution URIs separated by spaces, then
// "tern:merkle-sha256:CHUNK:ROOT" (the chunk size and Merkle root).
// The Merkle tree: leaf = sha256(0x00 ‖ chunk), node = sha256(0x01 ‖ l ‖ r),
// an odd last node is carried up unchanged.
package artifact

import (
	"archive/tar"
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"filippo.io/age"
	"github.com/ethereum/go-ethereum/common"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	ChunkSize   = 4 << 20 // Merkle chunk of the .enc
	AgeMagic    = "age-encryption.org/v1\n"
	scryptLogN  = 10
	merkleTag   = "tern:merkle-sha256:"
	chunksMagic = "tern-chunks v1"
)

type Hasher struct {
	all, cur   hash.Hash
	Size, curN uint64
	chunk      uint64
	leaves     []common.Hash
}

func NewHasher(chunk uint64) *Hasher {
	h := &Hasher{all: sha256.New(), chunk: chunk}
	h.startLeaf()
	return h
}

func (h *Hasher) startLeaf() {
	h.cur, h.curN = sha256.New(), 0
	h.cur.Write([]byte{0})
}

func (h *Hasher) Write(p []byte) (int, error) {
	n := len(p)
	h.all.Write(p)
	h.Size += uint64(n)
	for len(p) > 0 {
		k := min(uint64(len(p)), h.chunk-h.curN)
		h.cur.Write(p[:k])
		h.curN += k
		p = p[k:]
		if h.curN == h.chunk {
			h.leaves = append(h.leaves, common.Hash(h.cur.Sum(nil)))
			h.startLeaf()
		}
	}
	return n, nil
}

// Finish: the sha256 of everything and the chunk hashes (an empty stream is one empty chunk).
func (h *Hasher) Finish() (common.Hash, []common.Hash) {
	if h.curN > 0 || len(h.leaves) == 0 {
		h.leaves = append(h.leaves, common.Hash(h.cur.Sum(nil)))
		h.startLeaf()
	}
	return common.Hash(h.all.Sum(nil)), h.leaves
}

func MerkleRoot(leaves []common.Hash) common.Hash {
	if len(leaves) == 0 {
		return common.Hash{}
	}
	lv := append([]common.Hash(nil), leaves...)
	for len(lv) > 1 {
		var next []common.Hash
		for i := 0; i < len(lv); i += 2 {
			if i+1 == len(lv) {
				next = append(next, lv[i])
				continue
			}
			h := sha256.New()
			h.Write([]byte{1})
			h.Write(lv[i][:])
			h.Write(lv[i+1][:])
			next = append(next, common.Hash(h.Sum(nil)))
		}
		lv = next
	}
	return lv[0]
}

// Manifest: what a sealed file is, as recorded on chain and beside the file.
type Manifest struct {
	SHA256 common.Hash
	Size   uint64
	Chunk  uint64
	Root   common.Hash
	Leaves []common.Hash
}

// WriteChunks writes the chunk list beside the encrypted file: a header line, then one hash per line.
func WriteChunks(path string, m *Manifest) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	fmt.Fprintf(w, "%s sha256 chunk=%d size=%d root=%s\n", chunksMagic, m.Chunk, m.Size, m.Root.Hex())
	for _, l := range m.Leaves {
		fmt.Fprintln(w, l.Hex())
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func ReadChunks(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	if !s.Scan() || !strings.HasPrefix(s.Text(), chunksMagic) {
		return nil, fmt.Errorf("%s: not a chunk list", path)
	}
	m := &Manifest{}
	for _, kv := range strings.Fields(s.Text()) {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "chunk":
			m.Chunk, _ = strconv.ParseUint(v, 10, 64)
		case "size":
			m.Size, _ = strconv.ParseUint(v, 10, 64)
		case "root":
			m.Root = common.HexToHash(v)
		}
	}
	for s.Scan() {
		if t := strings.TrimSpace(s.Text()); t != "" {
			m.Leaves = append(m.Leaves, common.HexToHash(t))
		}
	}
	if m.Chunk == 0 || MerkleRoot(m.Leaves) != m.Root {
		return nil, fmt.Errorf("%s: damaged (its hashes don't make its root)", path)
	}
	return m, s.Err()
}

func agePass(key []byte) string { return hex.EncodeToString(key) }

func SealStream(dst io.Writer, src io.Reader, key []byte) error {
	r, err := age.NewScryptRecipient(agePass(key))
	if err != nil {
		return err
	}
	r.SetWorkFactor(scryptLogN)
	w, err := age.Encrypt(dst, r)
	if err != nil {
		return err
	}
	if _, err := io.CopyBuffer(w, src, make([]byte, 1<<20)); err != nil {
		return err
	}
	return w.Close()
}

func OpenStream(src io.Reader, key []byte) (io.Reader, error) {
	id, err := age.NewScryptIdentity(agePass(key))
	if err != nil {
		return nil, err
	}
	return age.Decrypt(src, id)
}

// tarStream writes a directory as a tar (regular files and folders; links
// are left out) under its base name.
func tarStream(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	base := filepath.Dir(filepath.Clean(dir))
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, p)
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uname, hdr.Gname, hdr.Uid, hdr.Gid = "", "", 0, 0
		hdr.Format = tar.FormatPAX
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.CopyBuffer(tw, f, make([]byte, 1<<20))
		f.Close()
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// SealPath encrypts a file or a directory (as a tar) to encPath, streaming,
// and writes encPath.chunks. It returns the manifest and the key.
func SealPath(src, encPath string) (*Manifest, []byte, error) {
	fi, err := os.Stat(src)
	if err != nil {
		return nil, nil, err
	}
	var in io.Reader
	if fi.IsDir() {
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(tarStream(src, pw)) }()
		defer pr.Close()
		in = pr
	} else {
		f, err := os.Open(src)
		if err != nil {
			return nil, nil, err
		}
		defer f.Close()
		in = f
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, nil, err
	}
	tmp := encPath + ".partial"
	out, err := os.Create(tmp)
	if err != nil {
		return nil, nil, err
	}
	bw := bufio.NewWriterSize(out, 1<<20)
	h := NewHasher(ChunkSize)
	err = SealStream(io.MultiWriter(bw, h), in, key)
	if err == nil {
		err = bw.Flush()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return nil, nil, err
	}
	if err := os.Rename(tmp, encPath); err != nil {
		return nil, nil, err
	}
	sum, leaves := h.Finish()
	m := &Manifest{SHA256: sum, Size: h.Size, Chunk: ChunkSize, Root: MerkleRoot(leaves), Leaves: leaves}
	return m, key, WriteChunks(encPath+".chunks", m)
}

// DistURIs checks distribution URIs: a BitTorrent v2 magnet (btmh, SHA-256),
// an IPFS CID, https (or http, file) URLs. Separated by commas or spaces.
func DistURIs(s string) ([]string, error) {
	var out []string
	for _, u := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		switch {
		case strings.HasPrefix(u, "magnet:"):
			p, err := url.Parse(u)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", u, err)
			}
			ok := false
			for _, xt := range p.Query()["xt"] {
				if h, found := strings.CutPrefix(xt, "urn:btmh:1220"); found && len(h) == 64 {
					if _, err := hex.DecodeString(h); err == nil {
						ok = true
					}
				}
			}
			if !ok {
				return nil, fmt.Errorf("%s: a BitTorrent v2 magnet is needed (xt=urn:btmh:1220 + 64 hex digits: the SHA-256 infohash, BEP 52; artifact torrent FILE.enc makes one)", u)
			}
		case strings.HasPrefix(u, "ipfs://"), strings.HasPrefix(u, "https://"), strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "file://"), strings.HasPrefix(u, "ar://"):
		case strings.HasPrefix(u, "bafy"), strings.HasPrefix(u, "Qm"):
			u = "ipfs://" + u
		default:
			return nil, fmt.Errorf("%s: a magnet:?xt=urn:btmh:…, ipfs://CID (or a bare CID), https:// URL", u)
		}
		out = append(out, u)
	}
	return out, nil
}

func OnChainURI(dist []string, m *Manifest) (string, error) {
	s := strings.TrimSpace(strings.Join(append(dist, fmt.Sprintf("%s%d:%x", merkleTag, m.Chunk, m.Root[:])), " "))
	if len(s) > 512 {
		return "", fmt.Errorf("the uri would be %d bytes; the chain keeps 512: fewer or shorter URIs (a magnet's tr= trackers can go)", len(s))
	}
	return s, nil
}

// ParseURI splits an on-chain uri into distribution URIs and the chunk root (chunk 0 if none).
func ParseURI(s string) (dist []string, chunk uint64, root common.Hash) {
	for _, f := range strings.Fields(s) {
		if v, ok := strings.CutPrefix(f, merkleTag); ok {
			c, r, _ := strings.Cut(v, ":")
			chunk, _ = strconv.ParseUint(c, 10, 64)
			root = common.HexToHash(r)
			continue
		}
		dist = append(dist, f)
	}
	return
}

func DecryptTo(encPath string, key []byte, outDir string, wantSum common.Hash, wantSize uint64) (map[string]any, error) {
	if outDir == "" {
		return nil, errors.New("--out DIR is required")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.Open(encPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := NewHasher(ChunkSize)
	pr, err := OpenStream(io.TeeReader(bufio.NewReaderSize(f, 1<<20), h), key)
	if err != nil {
		return nil, fmt.Errorf("the key does not open the file: %w", err)
	}
	br := bufio.NewReaderSize(pr, 1<<20)
	stage, err := os.MkdirTemp(outDir, ".tern-partial-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	head, _ := br.Peek(512)
	isTar := len(head) == 512 && string(head[257:262]) == "ustar"
	var written []string
	var n int64
	if isTar {
		written, n, err = untar(br, stage)
	} else {
		name := strings.TrimSuffix(filepath.Base(encPath), ".enc")
		if name == filepath.Base(encPath) {
			name += ".plain"
		}
		var w *os.File
		if w, err = os.OpenFile(filepath.Join(stage, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
			n, err = io.CopyBuffer(w, br, make([]byte, 1<<20))
			if cerr := w.Close(); err == nil {
				err = cerr
			}
			written = []string{name}
		}
	}
	if err == nil { // the rest (tar padding), so age checks the last chunk and the hash covers all
		_, err = io.Copy(io.Discard, br)
	}
	if err != nil {
		return nil, fmt.Errorf("decrypting: %w (nothing written)", err)
	}
	io.Copy(h, f) // anything age didn't need (there should be nothing)
	sum, _ := h.Finish()
	if sum != wantSum || h.Size != wantSize {
		return nil, fmt.Errorf("the file does not match the registered hash/size (got %d bytes, sha256 %s): nothing written; artifact verify names the bad chunk", h.Size, sum.Hex())
	}
	tops := map[string]bool{}
	for _, w := range written {
		tops[strings.SplitN(filepath.ToSlash(w), "/", 2)[0]] = true
	}
	var moved []string
	for t := range tops {
		if _, err := os.Lstat(filepath.Join(outDir, t)); err == nil {
			return nil, fmt.Errorf("%s already exists in %s: move it away first (nothing written)", t, outDir)
		}
	}
	for t := range tops {
		if err := os.Rename(filepath.Join(stage, t), filepath.Join(outDir, t)); err != nil {
			return nil, err
		}
		moved = append(moved, filepath.Join(outDir, t))
	}
	sort.Strings(moved)
	return map[string]any{"verified": "key matches the commitment; file matches the hash and size", "plain_bytes": n,
		"tar": isTar, "files": len(written), "written": moved}, nil
}

// untar unpacks regular files and folders under dir, refusing paths that leave it.
func untar(r io.Reader, dir string) ([]string, int64, error) {
	tr := tar.NewReader(r)
	var names []string
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names, total, nil
		}
		if err != nil {
			return nil, 0, err
		}
		name := filepath.FromSlash(strings.TrimSuffix(hdr.Name, "/"))
		if name == "" || filepath.IsAbs(name) || !filepath.IsLocal(name) {
			return nil, 0, fmt.Errorf("tar entry %q leaves the folder: refused", hdr.Name)
		}
		p := filepath.Join(dir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return nil, 0, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return nil, 0, err
			}
			w, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, hdr.FileInfo().Mode().Perm()|0o600)
			if err != nil {
				return nil, 0, err
			}
			n, err := io.CopyBuffer(w, tr, make([]byte, 1<<20))
			w.Close()
			if err != nil {
				return nil, 0, err
			}
			total += n
		default:
			continue // links and devices are not unpacked
		}
		names = append(names, name)
	}
}

const btBlock = 16 << 10

func bencode(w *bytes.Buffer, v any) {
	switch x := v.(type) {
	case int64:
		fmt.Fprintf(w, "i%de", x)
	case int:
		fmt.Fprintf(w, "i%de", x)
	case string:
		fmt.Fprintf(w, "%d:%s", len(x), x)
	case []byte:
		fmt.Fprintf(w, "%d:", len(x))
		w.Write(x)
	case []any:
		w.WriteByte('l')
		for _, e := range x {
			bencode(w, e)
		}
		w.WriteByte('e')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // raw byte order, as BEP 3 requires
		w.WriteByte('d')
		for _, k := range keys {
			bencode(w, k)
			bencode(w, x[k])
		}
		w.WriteByte('e')
	default:
		panic(fmt.Sprintf("bencode: %T", v))
	}
}

func sha2(a, b []byte) []byte {
	h := sha256.New()
	h.Write(a)
	h.Write(b)
	return h.Sum(nil)
}

// btTree: the root of a tree over leaves padded with zero hashes to n (a power of two).
func btTree(leaves [][]byte, n int, pad []byte) []byte {
	lv := make([][]byte, n)
	for i := range lv {
		if i < len(leaves) {
			lv[i] = leaves[i]
		} else {
			lv[i] = pad
		}
	}
	for len(lv) > 1 {
		next := make([][]byte, len(lv)/2)
		for i := range next {
			next[i] = sha2(lv[2*i], lv[2*i+1])
		}
		lv = next
	}
	return lv[0]
}

func pow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// TorrentV2 writes a v2 .torrent for one file (streaming it once) and
// returns the infohash (sha256 of the info dict) and a magnet link.
func TorrentV2(path, out string, pieceLen int, trackers []string) (string, string, error) {
	if pieceLen < btBlock || pieceLen&(pieceLen-1) != 0 {
		return "", "", errors.New("piece length: a power of two, at least 16 KiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	perPiece := pieceLen / btBlock
	zero := make([]byte, 32)
	var layer [][]byte // one hash per piece
	var blocks [][]byte
	var size int64
	buf := make([]byte, btBlock)
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		n, err := io.ReadFull(br, buf)
		if n > 0 {
			s := sha256.Sum256(buf[:n])
			blocks = append(blocks, s[:])
			size += int64(n)
		}
		if len(blocks) == perPiece || (err != nil && len(blocks) > 0 && size > int64(pieceLen)) {
			layer = append(layer, btTree(blocks, perPiece, zero))
			blocks = nil
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return "", "", err
		}
	}
	if size == 0 {
		return "", "", errors.New("an empty file has no torrent")
	}
	var root []byte
	info := map[string]any{"meta version": 2, "name": filepath.Base(path), "piece length": pieceLen}
	top := map[string]any{"created by": "ternchain"}
	if size <= int64(pieceLen) { // one piece: the tree over its blocks, no piece layer
		if len(layer) == 1 {
			root = layer[0]
		} else {
			root = btTree(blocks, pow2(len(blocks)), zero)
		}
		top["piece layers"] = map[string]any{}
	} else {
		padPiece := btTree(nil, perPiece, zero) // a piece of zero leaves
		root = btTree(layer, pow2(len(layer)), padPiece)
		top["piece layers"] = map[string]any{string(root): bytes.Join(layer, nil)}
	}
	info["file tree"] = map[string]any{filepath.Base(path): map[string]any{"": map[string]any{"length": size, "pieces root": root}}}
	top["info"] = info
	if len(trackers) > 0 {
		top["announce"] = trackers[0]
		if len(trackers) > 1 {
			var tiers []any
			for _, t := range trackers {
				tiers = append(tiers, []any{t})
			}
			top["announce-list"] = tiers
		}
	}
	var ib bytes.Buffer
	bencode(&ib, info)
	ih := sha256.Sum256(ib.Bytes())
	var tb bytes.Buffer
	bencode(&tb, top)
	if err := os.WriteFile(out, tb.Bytes(), 0o644); err != nil {
		return "", "", err
	}
	mag := "magnet:?xt=urn:btmh:1220" + hex.EncodeToString(ih[:]) + "&dn=" + url.QueryEscape(filepath.Base(path))
	for _, t := range trackers {
		mag += "&tr=" + url.QueryEscape(t)
	}
	return hex.EncodeToString(ih[:]), mag, nil
}

func SealFile(plain []byte) (ct []byte, hash [32]byte, key []byte, err error) {
	key = make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return
	}
	var b bytes.Buffer
	if err = SealStream(&b, bytes.NewReader(plain), key); err != nil {
		return
	}
	ct = b.Bytes()
	hash = sha256.Sum256(ct)
	return
}

// OpenFile opens an age file, or one sealed by earlier versions (XChaCha20-Poly1305, nonce first).
func OpenFile(ct, key []byte) ([]byte, error) {
	if bytes.HasPrefix(ct, []byte(AgeMagic)) {
		r, err := OpenStream(bytes.NewReader(ct), key)
		if err != nil {
			return nil, err
		}
		return io.ReadAll(r)
	}
	return openLegacy(ct, key)
}

// openLegacy opens files sealed before artifacts moved to age (bigfile.go).
func openLegacy(ct, key []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if len(ct) < aead.NonceSize() {
		return nil, errors.New("short file")
	}
	return aead.Open(nil, ct[:aead.NonceSize()], ct[aead.NonceSize():], nil)
}
