// Big artifacts (a game build, a dataset, model weights: hundreds of GB),
// streamed: nothing is held in memory but a few buffers.
//
//	artifact add FILE|DIR --uri U[,U…]   age-encrypt to FILE.enc chunk by chunk (a directory goes as a tar
//	                                     stream), sha256 of the .enc, a Merkle root over its 4 MiB chunks,
//	                                     FILE.enc.chunks (the chunk hashes, to publish beside the file)
//	artifact verify ID FILE.enc          stream it: size, sha256, and which chunks differ
//	artifact decrypt ID FILE.enc --key K --out DIR   stream it open (a tar is unpacked into DIR)
//	artifact torrent FILE.enc            a BitTorrent v2 (BEP 52) .torrent and its btmh magnet
//
// The file format (age STREAM, the chunk Merkle root, the on-chain uri,
// BEP 52 torrents) is internal/artifact; this file is its chain side.
package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern/internal/artifact"
)

// ---- hashing: sha256 of the whole stream and of each chunk

// ---- age, streaming

// ---- the uri on chain: where to get it, and the chunk root

// ArtifactInfo is the artifact as the chain keeps it.
type ArtifactInfo struct {
	Owner   common.Hash
	SHA256  common.Hash
	Size    uint64
	URI     string
	License string
	Price   *big.Int
	Commit  common.Hash
	Active  bool
}

func (c *Client) artifactInfo(id uint64) (*ArtifactInfo, error) {
	o, err := c.ch.call("Artifacts", "all", u(id))
	if err != nil {
		return nil, fmt.Errorf("no artifact %d: %w", id, err)
	}
	return &ArtifactInfo{Owner: common.Hash(o[0].([32]byte)), SHA256: common.Hash(o[1].([32]byte)), Size: o[2].(uint64), URI: o[3].(string),
		License: o[4].(string), Price: o[5].(*big.Int), Commit: common.Hash(o[6].([32]byte)), Active: o[7].(bool)}, nil
}

// addArtifact seals path (file or directory) to path.enc and registers it.
// In uris, the word "torrent" makes a v2 .torrent of the .enc and puts its magnet there.
func (c *Client) addArtifact(path, uris, license string, price *big.Int) (map[string]any, error) {
	var rest []string
	makeTorrent := false
	for _, x := range strings.FieldsFunc(uris, func(r rune) bool { return r == ',' || r == ' ' }) {
		if x == "torrent" {
			makeTorrent = true
		} else {
			rest = append(rest, x)
		}
	}
	dist, err := artifact.DistURIs(strings.Join(rest, " "))
	if err != nil {
		return nil, err
	}
	enc := strings.TrimSuffix(filepath.Clean(path), string(filepath.Separator)) + ".enc"
	m, key, err := artifact.SealPath(path, enc)
	if err != nil {
		return nil, err
	}
	torrent := ""
	if makeTorrent {
		torrent = enc + ".torrent"
		_, mag, err := artifact.TorrentV2(enc, torrent, artifact.ChunkSize, nil)
		if err != nil {
			return nil, err
		}
		dist = append([]string{mag}, dist...)
	}
	if len(dist) == 0 {
		abs, _ := filepath.Abs(enc)
		dist = []string{"file://" + abs}
	}
	keyFile := strings.TrimSuffix(enc, ".enc") + ".key"
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		return nil, err
	}
	uri, err := artifact.OnChainURI(dist, m)
	if err != nil {
		return nil, err
	}
	id, err := c.registerArtifact([32]byte(m.SHA256), m.Size, uri, license, price, key)
	if err != nil {
		return nil, err
	}
	o := map[string]any{"artifact": id, "encrypted_file": enc, "chunks_file": enc + ".chunks (publish it beside the file: verify uses it to name a bad chunk)",
		"key_file": keyFile + " (keep it; it is what you sell)", "sha256": m.SHA256.Hex(), "size": m.Size, "chunk_size": m.Chunk,
		"merkle_root": m.Root.Hex(), "chunks": len(m.Leaves), "uri": uri, "price": fmtCoins(price)}
	if torrent != "" {
		o["torrent"] = torrent + " (seed it with any BitTorrent v2 client)"
	}
	return o, nil
}

// verifyArtifact streams an encrypted file against the chain: size, sha256,
// the Merkle root, and with the chunk list (FILE.chunks) which chunks differ.
func (c *Client) verifyArtifact(id uint64, path string) (map[string]any, error) {
	a, err := c.artifactInfo(id)
	if err != nil {
		return nil, err
	}
	_, chunk, root := artifact.ParseURI(a.URI)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if chunk == 0 {
		chunk = artifact.ChunkSize
	}
	h := artifact.NewHasher(chunk)
	if _, err := io.CopyBuffer(h, f, make([]byte, 1<<20)); err != nil {
		return nil, err
	}
	sum, leaves := h.Finish()
	o := map[string]any{"artifact": id, "file": path, "size": h.Size, "size_on_chain": a.Size, "size_ok": h.Size == a.Size,
		"sha256_ok": sum == a.SHA256, "chunk_size": chunk, "chunks": len(leaves)}
	if root != (common.Hash{}) {
		o["merkle_ok"] = artifact.MerkleRoot(leaves) == root
	}
	ok := h.Size == a.Size && sum == a.SHA256
	if !ok {
		var ref *artifact.Manifest
		for _, p := range []string{path + ".chunks", strings.TrimSuffix(path, ".enc") + ".enc.chunks"} {
			if m, err := artifact.ReadChunks(p); err == nil && (root == (common.Hash{}) || m.Root == root) {
				ref = m
				break
			}
		}
		if ref == nil {
			o["bad_chunks"] = "unknown: put the seller's chunk list (FILE.enc.chunks, whose root is on chain) beside the file to see which"
		} else {
			var bad []int
			for i := 0; i < max(len(leaves), len(ref.Leaves)); i++ {
				if i >= len(leaves) || i >= len(ref.Leaves) || leaves[i] != ref.Leaves[i] {
					bad = append(bad, i)
				}
			}
			o["bad_chunks"] = bad
			if len(bad) > 0 {
				o["first_bad_byte_range"] = fmt.Sprintf("%d-%d", uint64(bad[0])*chunk, min(uint64(bad[0]+1)*chunk, max(h.Size, ref.Size))-1)
			}
		}
	}
	o["ok"] = ok
	return o, nil
}

// readKeyArg: a key file (32 raw bytes or hex) or the key in hex.
func readKeyArg(s string) ([]byte, error) {
	if b, err := os.ReadFile(s); err == nil {
		if len(b) == 32 {
			return b, nil
		}
		s = strings.TrimSpace(string(b))
	}
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(b) != 32 {
		return nil, errors.New("--key: a key file (as artifact add writes) or the 32-byte key in hex")
	}
	return b, nil
}

// decryptArtifact opens an encrypted artifact into outDir, streaming. The key
// must match the on-chain commitment and the file its hash and size; on any
// mismatch nothing is left in outDir.
func (c *Client) decryptArtifact(id uint64, encPath string, key []byte, outDir string) (map[string]any, error) {
	a, err := c.artifactInfo(id)
	if err != nil {
		return nil, err
	}
	if crypto.Keccak256Hash(key) != a.Commit {
		return nil, errors.New("the key does not match the seller's commitment on chain")
	}
	return artifact.DecryptTo(encPath, key, outDir, a.SHA256, a.Size)
}

// ---- BitTorrent v2 (BEP 52): a single-file .torrent, SHA-256 merkle trees of 16 KiB blocks

// ---- small sealed files (evidence, tests) use the same format, in memory

// ---- the CLI

func runArtifactBig(c *Client, cmd string, pos []string, f primFlags, key, trackers string, pieceKiB int) error {
	switch cmd {
	case "artifact add":
		if err := need(pos, 1, "artifact add FILE|DIR --price P --license L --uri MAGNET|ipfs://CID|https://…[,…]"); err != nil {
			return err
		}
		price, err := parseCoins(f.price)
		if err != nil {
			return err
		}
		o, err := c.addArtifact(pos[0], f.uri, f.license, price)
		if err != nil {
			return err
		}
		out(o)
	case "artifact show":
		if err := need(pos, 1, "artifact show ID [FILE.enc]"); err != nil {
			return err
		}
		a, err := c.artifactInfo(atoi(pos[0]))
		if err != nil {
			return err
		}
		who, _ := c.res.nameOf(a.Owner)
		c.taint(who) // someone else's artifact: what we read about it is theirs
		dist, chunk, root := artifact.ParseURI(a.URI)
		for i := range dist {
			dist[i] = c.outside(who, dist[i])
		}
		o := map[string]any{"artifact": pos[0], "owner": who, "price": fmtCoins(a.Price), "active": a.Active, "sha256": a.SHA256.Hex(),
			"size": a.Size, "encrypted": a.Commit != (common.Hash{}), "uris": dist, "license": c.outside(who, a.License)}
		if chunk > 0 {
			o["chunk_size"], o["merkle_root"] = chunk, root.Hex()
		}
		if len(pos) > 1 {
			p := pos[1]
			if a.Commit != (common.Hash{}) && !strings.HasSuffix(p, ".enc") {
				if _, err := os.Stat(p + ".enc"); err == nil {
					p += ".enc"
					o["note"] = "checked " + p + ": the published hash is of the encrypted file"
				}
			}
			v, err := c.verifyArtifact(atoi(pos[0]), p)
			if err != nil {
				return err
			}
			o["file"], o["matches"] = p, v["ok"]
			if v["ok"] != true && a.Commit != (common.Hash{}) && o["note"] == nil {
				o["note"] = "this hashes the encrypted file as published; pass FILE.enc (the plaintext never matches)"
			}
		}
		out(o)
	case "artifact verify":
		if err := need(pos, 2, "artifact verify ID FILE.enc"); err != nil {
			return err
		}
		o, err := c.verifyArtifact(atoi(pos[0]), pos[1])
		if err != nil {
			return err
		}
		out(o)
		if o["ok"] != true {
			return errors.New("the file does not match the artifact on chain")
		}
	case "artifact decrypt":
		if err := need(pos, 2, "artifact decrypt ID FILE.enc --key K --out DIR"); err != nil {
			return err
		}
		if key == "" {
			return errors.New("--key K: the key file (or hex) you were sold; artifact get LOCK FILE.enc --out DIR finds it in your mail")
		}
		k, err := readKeyArg(key)
		if err != nil {
			return err
		}
		o, err := c.decryptArtifact(atoi(pos[0]), pos[1], k, f.out)
		if err != nil {
			return err
		}
		o["artifact"] = pos[0]
		out(o)
	case "artifact get":
		if err := need(pos, 2, "artifact get LOCK ENCRYPTED-FILE --out DIR"); err != nil {
			return err
		}
		kd, err := c.receiveKeyFor(atoi(pos[0]))
		if err != nil {
			return fmt.Errorf("%w (ask the arbiter)", err)
		}
		dir := f.out
		if dir == "" {
			dir = "."
		}
		o, err := c.decryptArtifact(kd.Artifact, pos[1], kd.Key, dir)
		if err != nil {
			return fmt.Errorf("%w (ask the arbiter)", err)
		}
		o["lock"], o["artifact"] = pos[0], kd.Artifact
		out(o)
	case "artifact torrent":
		if err := need(pos, 1, "artifact torrent FILE.enc [--tracker URL,…] [--piece-kib 4096] [--out F.torrent]"); err != nil {
			return err
		}
		dest := f.out
		if dest == "" {
			dest = pos[0] + ".torrent"
		}
		var tr []string
		for _, t := range strings.Split(trackers, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tr = append(tr, t)
			}
		}
		ih, mag, err := artifact.TorrentV2(pos[0], dest, pieceKiB<<10, tr)
		if err != nil {
			return err
		}
		out(map[string]any{"torrent": dest, "infohash_v2": ih, "magnet": mag, "next": "seed it; artifact add takes the magnet in --uri (drop &tr= to save uri space)"})
	default:
		return unknownCommand(cmd)
	}
	return nil
}
