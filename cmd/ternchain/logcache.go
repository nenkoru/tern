package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
)

// logCacheDir: where log windows that can no longer change are kept between
// runs (the identity's cache/logs; "": nowhere, as on a local chain that may
// be reset). Every command scans from the deployment block, and on a public
// RPC that is hundreds of windows; with the cache a run after the first asks
// only for the windows near the head. Deleting the folder is always safe.
var logCacheDir string

// logFinality: only windows ending this many blocks below the head are kept,
// deeper than any reorganisation of the host chain.
const logFinality = 1000

var logCache = struct {
	sync.Mutex
	m map[string]map[string][]types.Log // query → "from-to" → its logs
}{m: map[string]map[string][]types.Log{}}

// logQueryKey: the query without its block range.
func logQueryKey(q ethereum.FilterQuery) string {
	var b strings.Builder
	for _, a := range q.Addresses {
		b.WriteString(a.Hex())
	}
	for _, ts := range q.Topics {
		b.WriteString("|")
		for _, t := range ts {
			b.WriteString(t.Hex())
		}
	}
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:12])
}

func logCacheFile(key string) string { return filepath.Join(logCacheDir, "logs-"+key+".json") }

// cachedWindows: the kept windows of a query (loaded from disk once a run).
// Called with logCache held.
func cachedWindows(key string) map[string][]types.Log {
	w, ok := logCache.m[key]
	if !ok {
		w = map[string][]types.Log{}
		if b, err := os.ReadFile(logCacheFile(key)); err == nil {
			if json.Unmarshal(b, &w) != nil {
				w = map[string][]types.Log{} // unreadable: read the chain again
			}
		}
		logCache.m[key] = w
	}
	return w
}

func logWindow(lo, hi uint64) string { return fmt.Sprintf("%d-%d", lo, hi) }

// saveWindows writes a query's windows (temp file and rename: a concurrent
// run sees the old file or the new one; the last writer wins, and what it
// lacks is read from the chain again). Called with logCache held.
func saveWindows(key string, w map[string][]types.Log) {
	b, err := json.Marshal(w)
	if err != nil || os.MkdirAll(logCacheDir, 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(logCacheDir, "logs-*.tmp")
	if err != nil {
		return
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil && cerr == nil {
		if os.Rename(tmp.Name(), logCacheFile(key)) == nil {
			return
		}
	}
	os.Remove(tmp.Name())
}
