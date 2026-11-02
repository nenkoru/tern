package main

import (
	"context"
	"math/big"
	"testing"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// blockLogs: a chain of head blocks with one log in every block, counting
// the log queries it answers.
type blockLogs struct {
	head  uint64
	calls int
}

func (c *blockLogs) BlockNumber(context.Context) (uint64, error) { return c.head, nil }

func (c *blockLogs) FilterLogs(_ context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	c.calls++
	var out []types.Log
	for n := q.FromBlock.Uint64(); n <= q.ToBlock.Uint64(); n++ {
		out = append(out, types.Log{Address: common.HexToAddress("0x01"), Topics: []common.Hash{{1}}, Data: []byte{byte(n)}, BlockNumber: n, TxHash: common.Hash{byte(n)}})
	}
	return out, nil
}

// A second scan asks the RPC only for the windows near the head, returns the
// same logs, and a new run (empty memory) reads the kept windows from disk.
func TestLogCacheKeepsFinalWindows(t *testing.T) {
	old := logCacheDir
	logCacheDir = t.TempDir()
	defer func() { logCacheDir = old }()
	reset := func() {
		logCache.Lock()
		logCache.m = map[string]map[string][]types.Log{}
		logCache.Unlock()
	}
	reset()
	b := &blockLogs{head: 5000}
	q := ethereum.FilterQuery{FromBlock: big.NewInt(1), Addresses: []common.Address{common.HexToAddress("0x01")}}
	first, err := filterLogsRanged(context.Background(), b, 100, q)
	if err != nil || len(first) != 5000 || b.calls != 50 {
		t.Fatalf("first scan: %d logs, %d calls, %v", len(first), b.calls, err)
	}
	for _, mem := range []bool{true, false} {
		if !mem {
			reset() // a new run of the CLI
		}
		b.calls = 0
		again, err := filterLogsRanged(context.Background(), b, 100, q)
		if err != nil || len(again) != len(first) {
			t.Fatalf("again: %d logs, %v", len(again), err)
		}
		for i := range first {
			if again[i].BlockNumber != first[i].BlockNumber || again[i].TxHash != first[i].TxHash {
				t.Fatalf("log %d differs", i)
			}
		}
		// windows ending at or above head-1000 (block 4000) are asked again: 4001..5000, 10 windows
		if b.calls != 10 {
			t.Fatalf("memory=%v: %d calls, want 10", mem, b.calls)
		}
	}
	// another query (other topics) shares nothing
	q.Topics = [][]common.Hash{{{1}}}
	b.calls = 0
	if _, err := filterLogsRanged(context.Background(), b, 100, q); err != nil || b.calls != 50 {
		t.Fatalf("other query: %d calls, %v", b.calls, err)
	}
	// no folder: no cache
	logCacheDir = ""
	reset()
	b.calls = 0
	if _, err := filterLogsRanged(context.Background(), b, 100, q); err != nil || b.calls != 50 {
		t.Fatalf("no cache: %d calls, %v", b.calls, err)
	}
}
