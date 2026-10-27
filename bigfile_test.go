package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nenkoru/tern/internal/artifact"
)

func sha256File(t *testing.T, p string) [32]byte {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return [32]byte(h.Sum(nil))
}

// A 300 MB artifact streams through add, verify and decrypt with a small,
// flat heap; verify names the chunk with a flipped byte; decrypt refuses it.
func TestBigArtifactStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("300 MB on disk")
	}
	const size = 300 << 20
	dir := t.TempDir()
	src := filepath.Join(dir, "game.bin")
	f, _ := os.Create(src)
	f.Truncate(size) // sparse, with random blocks every 32 MiB
	blk := make([]byte, 1<<20)
	for off := int64(0); off < size; off += 32 << 20 {
		rand.Read(blk)
		f.WriteAt(blk, off)
	}
	f.Close()
	want := sha256File(t, src)

	w := newWorld(t)
	seller, _ := w.owner("studio")

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	go func() {
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				runtime.ReadMemStats(&m)
				if m.HeapInuse > peak.Load() {
					peak.Store(m.HeapInuse)
				}
			}
		}
	}()
	o, err := seller.addArtifact(src, "magnet:?xt=urn:btmh:1220"+strings.Repeat("ab", 32)+"&dn=game,https://cdn.example/game.enc", "eula", coins(3))
	if err != nil {
		t.Fatal(err)
	}
	id := o["artifact"].(uint64)
	enc := src + ".enc"
	key, _ := os.ReadFile(src + ".key")
	if v, err := seller.verifyArtifact(id, enc); err != nil || v["ok"] != true || v["merkle_ok"] != true {
		t.Fatalf("verify: %v %v", v, err)
	}
	out := filepath.Join(dir, "out")
	if r, err := seller.decryptArtifact(id, enc, key, out); err != nil {
		t.Fatalf("decrypt: %v %v", r, err)
	}
	close(stop)
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	grew := peak.Load() - min(peak.Load(), base.HeapInuse)
	t.Logf("300 MB: heap in use %d MiB before, peak %d MiB (+%d MiB), max RSS %d MiB (whole test process, chain included)",
		base.HeapInuse>>20, peak.Load()>>20, grew>>20, ru.Maxrss>>20)
	if grew > 48<<20 {
		t.Fatalf("heap grew by %d MiB for a 300 MB file: not streaming", grew>>20)
	}
	if sha256File(t, filepath.Join(out, "game.bin")) != want {
		t.Fatal("decrypted file differs")
	}
	a, _ := seller.artifactInfo(id)
	dist, chunk, root := artifact.ParseURI(a.URI)
	if len(dist) != 2 || chunk != artifact.ChunkSize || root.Hex() != o["merkle_root"] || a.Size != o["size"].(uint64) {
		t.Fatalf("uri on chain: %q", a.URI)
	}

	// Flip one byte in chunk 37: verify says which, decrypt writes nothing.
	ef, _ := os.OpenFile(enc, os.O_RDWR, 0)
	off := int64(37*artifact.ChunkSize + 12345)
	b := []byte{0}
	ef.ReadAt(b, off)
	b[0] ^= 0x40
	ef.WriteAt(b, off)
	ef.Close()
	v, err := seller.verifyArtifact(id, enc)
	if err != nil || v["ok"] != false {
		t.Fatalf("verify missed a flipped byte: %v %v", v, err)
	}
	if bad, _ := v["bad_chunks"].([]int); len(bad) != 1 || bad[0] != 37 {
		t.Fatalf("bad chunks: %v", v["bad_chunks"])
	}
	out2 := filepath.Join(dir, "out2")
	if _, err := seller.decryptArtifact(id, enc, key, out2); err == nil {
		t.Fatal("decrypted a damaged file")
	}
	if ents, _ := os.ReadDir(out2); len(ents) != 0 {
		t.Fatalf("left files behind: %v", ents)
	}
	// A wrong key is refused before reading anything.
	if _, err := seller.decryptArtifact(id, enc, make([]byte, 32), out2); err == nil || !strings.Contains(err.Error(), "commitment") {
		t.Fatalf("wrong key: %v", err)
	}
}

// A folder (a game build) goes as a tar and comes back as the folder; a v1
// magnet is refused; the stock age format opens with the hex passphrase.
func TestArtifactFolderAndURIs(t *testing.T) {
	dir := t.TempDir()
	build := filepath.Join(dir, "build")
	os.MkdirAll(filepath.Join(build, "data", "levels"), 0o755)
	os.WriteFile(filepath.Join(build, "game.exe"), []byte("MZ pretend"), 0o755)
	os.WriteFile(filepath.Join(build, "data", "levels", "1.pak"), bytes.Repeat([]byte{7}, 100000), 0o644)
	w := newWorld(t)
	seller, _ := w.owner("studio")
	if _, err := seller.addArtifact(build, "magnet:?xt=urn:btih:"+strings.Repeat("ab", 20), "", new(big.Int)); err == nil {
		t.Fatal("took a v1 magnet")
	}
	o, err := seller.addArtifact(build, "bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi,torrent", "", coins(1))
	if err != nil {
		t.Fatal(err)
	}
	if u := o["uri"].(string); !strings.HasPrefix(u, "magnet:?xt=urn:btmh:1220") || !strings.Contains(u, " ipfs://bafy") {
		t.Fatalf("uri: %s", u)
	}
	key, _ := os.ReadFile(filepath.Join(dir, "build.key"))
	out := filepath.Join(dir, "got")
	r, err := seller.decryptArtifact(o["artifact"].(uint64), build+".enc", key, out)
	if err != nil || r["tar"] != true {
		t.Fatalf("%v %v", r, err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "build", "data", "levels", "1.pak")); len(b) != 100000 {
		t.Fatal("folder didn't come back")
	}
	// sealFile/openFile (evidence, small sales) use the same age format.
	ct, _, k, _ := artifact.SealFile([]byte("hello"))
	if !bytes.HasPrefix(ct, []byte(artifact.AgeMagic)) {
		t.Fatal("not age")
	}
	if p, err := artifact.OpenFile(ct, k); err != nil || string(p) != "hello" {
		t.Fatal(err)
	}
}
