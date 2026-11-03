package scan

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func fast(t *testing.T) {
	b, rb, hb := backoff, rateBackoff, httpBackoff
	backoff, rateBackoff, httpBackoff = time.Millisecond, time.Millisecond, time.Millisecond
	t.Cleanup(func() { backoff, rateBackoff, httpBackoff = b, rb, hb })
}

// A call that fails now and then is tried again; a refused range comes back
// at once (Update asks for a smaller one); a rate limit is waited out longer
// and never makes the range smaller; a revert is not asked again; one that
// keeps failing gives up.
func TestRetry(t *testing.T) {
	fast(t)
	ctx := context.Background()
	calls := func(msg string) (int, error) {
		n := 0
		_, err := retry(ctx, func() (int, error) { n++; return 0, errors.New(msg) })
		return n, err
	}

	n := 0
	v, err := retry(ctx, func() (int, error) {
		if n++; n < 3 {
			return 0, errors.New(`Post "https://rpc": net/http: TLS handshake timeout`)
		}
		return 7, nil
	})
	if v != 7 || err != nil || n != 3 {
		t.Fatalf("flaky: %d %v after %d calls", v, err, n)
	}

	for _, m := range []string{"query exceeds max block range 10000", "query returned more than 10000 results", "block range is too wide"} {
		n, err := calls(m)
		if _, ok := narrower(err, 10_000); n != 1 || !ok {
			t.Fatalf("too wide %q: %d calls, narrower %v", m, n, ok)
		}
	}
	if next, _ := narrower(errors.New("eth_getLogs is limited to a 200 range"), 10_000); next != 199 {
		t.Fatalf("the RPC's own limit: %d", next)
	}
	for _, m := range []string{"429 Too Many Requests", "rate limit exceeded", "exceeded its compute units per second capacity", "daily request limit reached"} {
		n, err := calls(m)
		if _, ok := narrower(err, 10_000); n != rateTries || ok {
			t.Fatalf("rate limit %q: %d calls, narrower %v", m, n, ok)
		}
	}
	if n, _ := calls("execution reverted"); n != 1 {
		t.Fatalf("a revert was asked %d times", n)
	}
	if n, _ := calls("502 bad gateway"); n != tries {
		t.Fatalf("down: %d calls", n)
	}
}

// Dial's transport waits as long as Retry-After says, then asks again.
func TestRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":"0x2a"}`)
	}))
	defer srv.Close()
	c, err := Dial(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	n, err := c.BlockNumber(context.Background())
	if err != nil || n != 42 || hits.Load() != 2 || time.Since(start) < time.Second {
		t.Fatalf("block %d, %v, after %d requests and %s", n, err, hits.Load(), time.Since(start))
	}
}

// An RPC's key (in its path, query or user) never shows in an error.
func TestRedact(t *testing.T) {
	err := fmt.Errorf("logs 1-2: %w", fmt.Errorf(`Post "https://user:pw@base-sepolia.g.alchemy.com/v2/SECRETKEY?k=SECRET2": %w`, context.DeadlineExceeded))
	r := Redact(err)
	if s := r.Error(); strings.Contains(s, "SECRET") || strings.Contains(s, "pw") || !strings.Contains(s, "https://base-sepolia.g.alchemy.com") {
		t.Fatalf("redacted: %s", s)
	}
	if !errors.Is(r, context.DeadlineExceeded) || errors.Unwrap(r) != nil {
		t.Fatal("the redacted error should still say what it is, and not hand back the original")
	}
	if Origin("wss://x.example:8546/KEY") != "wss://x.example:8546" || Origin("not a url") != "(an rpc)" {
		t.Fatal("origin")
	}
}

func TestPrintable(t *testing.T) {
	if got := Printable("ok\x1b]52;c;ZXZpbA==\x07 ‮gnp.exe\u009b2J\nend"); got != "ok ]52;c;ZXZpbA==  gnp.exe 2J end" {
		t.Fatalf("%q", got)
	}
}

// ---- a chain of logs and view calls, to drive Update

type fakeChain struct {
	head      uint64
	logs      []types.Log
	call      func(to common.Address, data []byte) ([]byte, error)
	failLogs  func(n int) error // the n-th FilterLogs (from 1) fails with this
	nLogs     int
	queries   [][2]uint64
	callCount int
}

func (f *fakeChain) FilterLogs(_ context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	f.nLogs++
	if f.failLogs != nil {
		if err := f.failLogs(f.nLogs); err != nil {
			return nil, err
		}
	}
	a, b := q.FromBlock.Uint64(), q.ToBlock.Uint64()
	f.queries = append(f.queries, [2]uint64{a, b})
	var out []types.Log
	for _, l := range f.logs {
		if l.BlockNumber >= a && l.BlockNumber <= b {
			out = append(out, l)
		}
	}
	return out, nil
}

func (f *fakeChain) SubscribeFilterLogs(context.Context, ethereum.FilterQuery, chan<- types.Log) (ethereum.Subscription, error) {
	return nil, errors.New("no")
}
func (f *fakeChain) BlockNumber(context.Context) (uint64, error) { return f.head, nil }
func (f *fakeChain) HeaderByNumber(_ context.Context, n *big.Int) (*types.Header, error) {
	return &types.Header{Number: n, Time: 1_700_000_000 + 2*n.Uint64()}, nil
}
func (f *fakeChain) CallContract(_ context.Context, m ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	f.callCount++
	return f.call(*m.To, m.Data)
}

var (
	regAddr  = common.HexToAddress("0x1000000000000000000000000000000000000001")
	bankAddr = common.HexToAddress("0x1000000000000000000000000000000000000002")
	easAddr  = common.HexToAddress("0x1000000000000000000000000000000000000003")
	optAddr  = common.HexToAddress("0x1000000000000000000000000000000000000004")
	schema   = common.HexToHash("0x95437816dd9b27e9ec381bd8a14446086a6d775547e80d380b4b1f93ac3ae52f")
)

func fakeNet() Net {
	return Net{Network: "fake", ChainID: 1, Block: 100, Schema: schema.Hex(), LogRange: 1000, Contracts: map[string]string{
		"Registry": regAddr.Hex(), "Bank": bankAddr.Hex(), "EAS": easAddr.Hex(), "OptimisticCondition": optAddr.Hex()}}
}

func mustABI(t *testing.T, name string) abi.ABI {
	a, err := loadABI(name)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// eventLog: an event of a, its indexed arguments as topics and the rest as data.
func eventLog(t *testing.T, a abi.ABI, addr common.Address, block uint64, event string, topics []common.Hash, data ...any) types.Log {
	ev := a.Events[event]
	b, err := ev.Inputs.NonIndexed().Pack(data...)
	if err != nil {
		t.Fatal(err)
	}
	return types.Log{Address: addr, BlockNumber: block, Topics: append([]common.Hash{ev.ID}, topics...), Data: b}
}

func id(name string) common.Hash { return crypto.Keccak256Hash([]byte(name)) }

type easAttestation struct {
	Uid            [32]byte
	Schema         [32]byte
	Time           uint64
	ExpirationTime uint64
	RevocationTime uint64
	RefUID         [32]byte
	Recipient      common.Address
	Attester       common.Address
	Revocable      bool
	Data           []byte
}

// lagging: a chain whose view calls first answer from a node that hasn't
// seen the lock and the claim (zeros), then catch up.
func lagging(t *testing.T) (*fakeChain, *atomic.Bool) {
	reg, bank, eas := mustABI(t, "Registry"), mustABI(t, "Bank"), mustABI(t, "EAS")
	alice, bob := id("alice"), id("bob")
	aliceKey := common.HexToAddress("0xa11ce00000000000000000000000000000000001")
	uid := common.HexToHash("0xc1a1")
	claim, _ := abi.Arguments{{Type: mustType("bytes32")}, {Type: mustType("string")}, {Type: mustType("bytes")}, {Type: mustType("int64")}}.
		Pack([32]byte(bob), "review", []byte("on time"), int64(90))
	caught := new(atomic.Bool)
	f := &fakeChain{head: 5000, logs: []types.Log{
		eventLog(t, reg, regAddr, 200, "OwnerRegistered", []common.Hash{alice}, "alice", aliceKey, common.Address{}),
		eventLog(t, reg, regAddr, 201, "OwnerRegistered", []common.Hash{bob}, "bob", common.Address{}, common.Address{}),
		eventLog(t, bank, bankAddr, 1500, "Locked", []common.Hash{common.BigToHash(big.NewInt(0)), alice, {}},
			big.NewInt(500), uint64(1_800_000_000), optAddr, []byte{}),
		eventLog(t, bank, bankAddr, 2500, "PayeeSet", []common.Hash{common.BigToHash(big.NewInt(0)), bob}),
		eventLog(t, bank, bankAddr, 2500, "Claimed", []common.Hash{common.BigToHash(big.NewInt(0))}, big.NewInt(500), common.Address{}),
		eventLog(t, eas, easAddr, 3500, "Attested", []common.Hash{common.BytesToHash(common.Address{}.Bytes()), common.BytesToHash(aliceKey.Bytes()), schema}, [32]byte(uid)),
	}}
	f.call = func(to common.Address, data []byte) ([]byte, error) {
		m, err := bank.MethodById(data[:4])
		if to == easAddr {
			m, err = eas.MethodById(data[:4])
		}
		if err != nil {
			return nil, err
		}
		ok := caught.Load()
		switch m.Name {
		case "locks":
			total, cond := big.NewInt(500), optAddr
			if !ok {
				total, cond = new(big.Int), common.Address{}
			}
			return m.Outputs.Pack([32]byte(alice), [32]byte(alice), [32]byte{}, total, new(big.Int), new(big.Int), uint64(0), cond, []byte{}, common.Address{})
		case "itemOf":
			return m.Outputs.Pack(new(big.Int))
		case "getAttestation":
			a := easAttestation{}
			if ok {
				a = easAttestation{Uid: uid, Schema: schema, Attester: aliceKey, Data: claim}
			}
			return m.Outputs.Pack(a)
		}
		return nil, fmt.Errorf("execution reverted: %s", m.Name)
	}
	return f, caught
}

// A view call answered from a state without the lock (or the claim) is an
// error: the scan stops before that range, keeps what came before, and reads
// the range again next time instead of keeping a wrong lock or losing a claim.
func TestUpdateRereadsLaggingViews(t *testing.T) {
	fast(t)
	f, caught := lagging(t)
	n := fakeNet()
	s := Load(filepath.Join(t.TempDir(), "x.json"), n)
	err := s.Update(context.Background(), f, n, Options{})
	if err == nil || !strings.Contains(err.Error(), "try again") || s.ToBlock != 1099 || len(s.Locks) != 0 {
		t.Fatalf("a lagging lock: %v, scanned to %d, %d locks", err, s.ToBlock, len(s.Locks))
	}
	if f.callCount != 1 {
		t.Fatalf("the zeros were asked for %d times (not an RPC failure: no retries)", f.callCount)
	}
	caught.Store(true)
	if err := s.Update(context.Background(), f, n, Options{}); err != nil {
		t.Fatal(err)
	}
	if len(s.Locks) != 1 || len(s.Claims) != 1 || s.ToBlock != 5000 {
		t.Fatalf("after: %d locks, %d claims, to %d", len(s.Locks), len(s.Claims), s.ToBlock)
	}
	// PayeeSet: the lock was opened with no payee; the claim named bob.
	if k := s.Locks[0]; k.Payee != "bob" || k.Status != "paid" || k.Kind != "optimistic" || k.Token != Coin {
		t.Fatalf("the lock: %+v", k)
	}
	if c := s.Claims[0]; c.Attester != "alice" || c.Subject != "bob" || c.Kind != "review" || c.Score != 90 {
		t.Fatalf("the claim: %+v", c)
	}

	// A claim the RPC doesn't know yet stops the scan the same way.
	caught.Store(false)
	f.logs = f.logs[len(f.logs)-1:] // only the claim
	s = Load(filepath.Join(t.TempDir(), "y.json"), n)
	if err := s.Update(context.Background(), f, n, Options{}); err == nil || !strings.Contains(err.Error(), "claim") || s.ToBlock != 3099 {
		t.Fatalf("a lagging claim: %v, scanned to %d", err, s.ToBlock)
	}
}

// A rate limit is waited out with the same range; a refused range is halved.
func TestUpdateRateLimitKeepsRange(t *testing.T) {
	fast(t)
	f, caught := lagging(t)
	caught.Store(true)
	f.failLogs = func(n int) error {
		if n <= 3 {
			return errors.New("429 Too Many Requests: rate limit exceeded")
		}
		return nil
	}
	n := fakeNet()
	s := Load(filepath.Join(t.TempDir(), "x.json"), n)
	if err := s.Update(context.Background(), f, n, Options{}); err != nil {
		t.Fatal(err)
	}
	for _, q := range f.queries {
		if q[1]-q[0]+1 != 1000 && q[1] != 5000 {
			t.Fatalf("a range of %d blocks after a rate limit: %v", q[1]-q[0]+1, f.queries)
		}
	}

	f.queries, f.nLogs = nil, 0
	f.failLogs = func(n int) error {
		if n == 1 {
			return errors.New("query exceeds max block range 500")
		}
		return nil
	}
	s = Load(filepath.Join(t.TempDir(), "y.json"), n)
	if err := s.Update(context.Background(), f, n, Options{}); err != nil {
		t.Fatal(err)
	}
	if q := f.queries[0]; q[1]-q[0]+1 != 499 {
		t.Fatalf("after a refused range: %v", f.queries[:2])
	}
}

// Save leaves no temp file behind, and two saves at once both end in a whole cache.
func TestSave(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	s := newState(fakeNet())
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- s.Save(p) }()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	es, _ := os.ReadDir(dir)
	if len(es) != 1 || Load(p, fakeNet()).Fingerprint != s.Fingerprint {
		t.Fatalf("after two saves: %v", es)
	}
}
