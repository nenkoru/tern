// Censorship and liveness watching (docs/abuse/VALIDATORS.md, F4/F5).
//
//	ternchain validator watch --rpc URL [--k 10] [--seconds S] [--report-dir DIR]
//
// A watcher (best a data node no validator runs) records every transaction
// in its pool and the block it first saw it. One still pending K blocks
// later, while blocks are produced with room for it, whose sender's nonce is
// next and whose fee covers the base fee, is censored: an alert line, and a
// report file (the transaction itself and the proposers who skipped it)
// that a citizen puts on chain with 'gov validator report FILE'
// (Validators.CensorshipReported) as evidence for a removal vote, and
// rebroadcasts to every node so any honest proposer includes it. A head
// that stops moving is a halt: an alert too.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

type watchedTx struct {
	Hash      common.Hash     `json:"hash"`
	From      common.Address  `json:"from"`
	Nonce     uint64          `json:"nonce"`
	Gas       uint64          `json:"gas"`
	MaxFee    *big.Int        `json:"max_fee"`
	FirstSeen uint64          `json:"first_seen_block"`
	SeenAt    time.Time       `json:"first_seen_at"`
	Tx        json.RawMessage `json:"tx"`
	alerted   bool
}

// CensorshipReport: what a watcher hands a citizen to put on chain.
type CensorshipReport struct {
	Tx            common.Hash      `json:"tx"`
	From          common.Address   `json:"from"`
	Nonce         uint64           `json:"nonce"`
	FirstSeen     uint64           `json:"first_seen_block"`
	Head          uint64           `json:"head"`
	BlocksPending uint64           `json:"blocks_pending"`
	SpareBlocks   int              `json:"blocks_with_room"` // blocks that had gas left for it
	Proposers     []common.Address `json:"proposers"`        // who made those blocks
	ByProposer    map[string]int   `json:"blocks_by_proposer"`
	Raw           json.RawMessage  `json:"transaction"` // as the pool has it: anyone can rebroadcast it
	Watcher       string           `json:"watcher"`
	At            string           `json:"at"`
}

func validatorWatch(rpcURL string, k, seconds uint64, reportDir string, w io.Writer) error {
	ctx := context.Background()
	rc, err := rpc.DialContext(ctx, rpcURL)
	if err != nil {
		return err
	}
	defer rc.Close()
	cl := ethclient.NewClient(rc)
	emit := func(m map[string]any) {
		m["at"] = time.Now().UTC().Format(time.RFC3339)
		b, _ := json.Marshal(m)
		fmt.Fprintln(w, string(b))
	}
	if reportDir != "" {
		if err := os.MkdirAll(reportDir, 0o755); err != nil {
			return err
		}
	}
	seen := map[common.Hash]*watchedTx{}
	var lastHead uint64
	lastMove := time.Now()
	halted := false
	deadline := time.Time{}
	if seconds > 0 {
		deadline = time.Now().Add(time.Duration(seconds) * time.Second)
	}
	emit(map[string]any{"event": "watching", "rpc": rpcURL, "k": k})
	for deadline.IsZero() || time.Now().Before(deadline) {
		head, err := cl.HeaderByNumber(ctx, nil)
		if err != nil {
			emit(map[string]any{"event": "rpc-error", "error": err.Error()})
			time.Sleep(2 * time.Second)
			continue
		}
		n := head.Number.Uint64()
		if n != lastHead {
			if halted {
				emit(map[string]any{"event": "resumed", "head": n, "stalled_s": int(time.Since(lastMove).Seconds())})
			}
			lastHead, lastMove, halted = n, time.Now(), false
		} else if !halted && time.Since(lastMove) > 15*time.Second {
			halted = true
			emit(map[string]any{"event": "halt", "head": n, "stalled_s": int(time.Since(lastMove).Seconds()),
				"note": "no block for 15 s: more than a third of the validators are down or refuse to seal; if it lasts, the social recovery (fork-from-state)"})
		}
		// The pool now.
		var pool []struct {
			Hash common.Hash `json:"hash"`
		}
		if err := rc.CallContext(ctx, &pool, "txpool_besuTransactions"); err != nil {
			emit(map[string]any{"event": "rpc-error", "error": "txpool_besuTransactions (the TXPOOL api): " + err.Error()})
		}
		inPool := map[common.Hash]bool{}
		for _, p := range pool {
			inPool[p.Hash] = true
			if seen[p.Hash] != nil {
				continue
			}
			var raw json.RawMessage
			if rc.CallContext(ctx, &raw, "eth_getTransactionByHash", p.Hash) != nil || len(raw) == 0 || string(raw) == "null" {
				continue
			}
			var t struct {
				From         common.Address `json:"from"`
				Nonce        hexutil.Uint64 `json:"nonce"`
				Gas          hexutil.Uint64 `json:"gas"`
				GasPrice     *hexutil.Big   `json:"gasPrice"`
				MaxFeePerGas *hexutil.Big   `json:"maxFeePerGas"`
			}
			json.Unmarshal(raw, &t)
			fee := (*big.Int)(t.GasPrice)
			if t.MaxFeePerGas != nil {
				fee = (*big.Int)(t.MaxFeePerGas)
			}
			seen[p.Hash] = &watchedTx{Hash: p.Hash, From: t.From, Nonce: uint64(t.Nonce), Gas: uint64(t.Gas), MaxFee: fee, FirstSeen: n, SeenAt: time.Now(), Tx: raw}
		}
		for h, x := range seen {
			if r, err := cl.TransactionReceipt(ctx, h); err == nil && r != nil {
				emit(map[string]any{"event": "included", "tx": h.Hex(), "from": x.From.Hex(), "blocks_waited": int64(r.BlockNumber.Uint64()) - int64(x.FirstSeen),
					"seconds_waited": int(time.Since(x.SeenAt).Seconds()), "was_alerted": x.alerted})
				delete(seen, h)
				continue
			}
			if !inPool[h] && time.Since(x.SeenAt) > time.Minute {
				delete(seen, h) // dropped or replaced
				continue
			}
			if x.alerted || n < x.FirstSeen+k {
				continue
			}
			nonce, err := cl.NonceAt(ctx, x.From, nil)
			if err != nil || nonce != x.Nonce {
				continue // not next in line: waiting on its sender, not on the validators
			}
			if x.MaxFee != nil && head.BaseFee != nil && x.MaxFee.Cmp(head.BaseFee) < 0 {
				continue // underpriced
			}
			rep := CensorshipReport{Tx: h, From: x.From, Nonce: x.Nonce, FirstSeen: x.FirstSeen, Head: n, BlocksPending: n - x.FirstSeen,
				ByProposer: map[string]int{}, Raw: x.Tx, Watcher: rpcURL, At: time.Now().UTC().Format(time.RFC3339)}
			props := map[common.Address]bool{}
			for b := x.FirstSeen + 1; b <= n; b++ {
				bh, err := cl.HeaderByNumber(ctx, new(big.Int).SetUint64(b))
				if err != nil {
					continue
				}
				if bh.GasUsed+x.Gas > bh.GasLimit && !cheaperInside(ctx, rc, b, x) {
					continue // full of transactions paying at least as much: no room for it, not censorship
				}
				rep.SpareBlocks++
				rep.ByProposer[bh.Coinbase.Hex()]++
				props[bh.Coinbase] = true
			}
			if rep.SpareBlocks < int(k) {
				continue
			}
			for p := range props {
				rep.Proposers = append(rep.Proposers, p)
			}
			sort.Slice(rep.Proposers, func(i, j int) bool { return rep.Proposers[i].Hex() < rep.Proposers[j].Hex() })
			x.alerted = true
			m := map[string]any{"event": "censorship", "tx": h.Hex(), "from": x.From.Hex(), "nonce": x.Nonce, "first_seen_block": x.FirstSeen,
				"blocks_pending": rep.BlocksPending, "blocks_with_room": rep.SpareBlocks, "proposers_skipping": rep.ByProposer, "unverified": true,
				"note": "this watcher's view (its pool, the blocks' coinbase): unverified evidence, not a verdict; pending, next in line, paying the base fee, and left out of blocks with room (or padded with cheaper transactions): evidence for a removal vote ('gov validator report FILE')"}
			if reportDir != "" {
				p := filepath.Join(reportDir, "censorship-"+h.Hex()[2:12]+".json")
				b, _ := json.MarshalIndent(rep, "", "  ")
				if os.WriteFile(p, b, 0o644) == nil {
					m["report"] = p
				}
			}
			emit(m)
		}
		time.Sleep(time.Second)
	}
	var still []string
	for h := range seen {
		still = append(still, h.Hex())
	}
	emit(map[string]any{"event": "done", "head": lastHead, "still_pending": still})
	return nil
}

// cheaperInside (VA-9): a full block that still took a transaction paying a
// lower fee cap than x: padding (or a preference), not a lack of room.
func cheaperInside(ctx context.Context, rc *rpc.Client, n uint64, x *watchedTx) bool {
	if x.MaxFee == nil {
		return false
	}
	var b struct {
		Transactions []struct {
			GasPrice     *hexutil.Big `json:"gasPrice"`
			MaxFeePerGas *hexutil.Big `json:"maxFeePerGas"`
		} `json:"transactions"`
	}
	if rc.CallContext(ctx, &b, "eth_getBlockByNumber", hexutil.EncodeUint64(n), true) != nil {
		return false
	}
	for _, t := range b.Transactions {
		fee := t.MaxFeePerGas
		if fee == nil {
			fee = t.GasPrice
		}
		if fee != nil && (*big.Int)(fee).Cmp(x.MaxFee) < 0 {
			return true
		}
	}
	return false
}
