package artifact

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The v2 torrent: the piece layer hashes up to the pieces root, and the
// infohash is sha256 of the info dict.
func TestTorrentV2(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.enc")
	b := make([]byte, 9<<20+5000) // 3 pieces of 4 MiB, the last partial
	rand.Read(b)
	os.WriteFile(p, b, 0o644)
	ih, mag, err := TorrentV2(p, p+".torrent", 4<<20, []string{"udp://tracker.example:1337"})
	if err != nil || !strings.Contains(mag, "btmh:1220"+ih) {
		t.Fatal(err, mag)
	}
	tb, _ := os.ReadFile(p + ".torrent")
	// Recompute by hand: 16 KiB leaves, pieces of 256 leaves, padded to 4 pieces.
	zero := make([]byte, 32)
	var layer [][]byte
	for off := 0; off < len(b); off += 4 << 20 {
		var leaves [][]byte
		for o := off; o < min(off+4<<20, len(b)); o += btBlock {
			s := sha256.Sum256(b[o:min(o+btBlock, len(b))])
			leaves = append(leaves, s[:])
		}
		layer = append(layer, btTree(leaves, 256, zero))
	}
	root := btTree(layer, 4, btTree(nil, 256, zero))
	if !bytes.Contains(tb, append([]byte("11:pieces root32:"), root...)) || !bytes.Contains(tb, bytes.Join(layer, nil)) {
		t.Fatal("pieces root / piece layer")
	}
	i := bytes.Index(tb, []byte("4:infod"))
	var info bytes.Buffer
	bencode(&info, map[string]any{"meta version": 2, "name": "x.enc", "piece length": 4 << 20,
		"file tree": map[string]any{"x.enc": map[string]any{"": map[string]any{"length": int64(len(b)), "pieces root": root}}}})
	if i < 0 || !bytes.HasPrefix(tb[i+6:], info.Bytes()) {
		t.Fatal("info dict")
	}
	if s := sha256.Sum256(info.Bytes()); hex.EncodeToString(s[:]) != ih {
		t.Fatal("infohash")
	}
}
