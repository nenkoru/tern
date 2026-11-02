// The standalone faucet service for networks that are not a devnet (the L1
// testnet, chain 83760): ternchain-ops faucet serve --key FILE --listen ADDR.
// It serves the same POST /faucet {"address": "0x…"} as the devnet's
// built-in faucet (devnet.go faucetHandler), with the same caps: one grant
// per address a day, a few per IPv4 address or IPv6 /64 a day (/56 and /48
// get more), and a ceiling per hour for everyone together. A grant is the
// coins plus a little gas, from the key in FILE (the genesis faucet key).
// Put it behind the TLS proxy that serves the RPC (--trusted-proxy names
// that proxy, so the caps see the caller, not the proxy) and set net.json's
// faucet_url to https://HOST/faucet.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/nenkoru/tern/internal/netlimit"
)

type faucetServeConfig struct {
	keyFile, listen, logFile, trustedProxy string
	coins, gas                             *big.Int
	perHour, perIPDay                      int
}

// faucetServer: a running faucet service (Close stops it).
type faucetServer struct {
	srv *http.Server
	ln  net.Listener
	fh  *faucetHandler
	URL string // http://ADDR/faucet
}

func (s *faucetServer) Close() error { return s.srv.Close() }

// startFaucetServer: the faucet on cfg.listen, paying from cfg.keyFile's key.
func startFaucetServer(ch *Chain, df *DevnetFile, cfg faucetServeConfig) (*faucetServer, error) {
	if cfg.keyFile == "" {
		return nil, errors.New("faucet serve needs --key FILE (the faucet's key, hex, mode 0600)")
	}
	if fi, err := os.Stat(cfg.keyFile); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others (mode %o): chmod 600 it first", cfg.keyFile, fi.Mode().Perm())
	}
	key, err := readKeyFile(cfg.keyFile)
	if err != nil {
		return nil, err
	}
	if cfg.coins == nil || cfg.coins.Sign() <= 0 {
		return nil, errors.New("faucet serve: --faucet-coins must be above 0")
	}
	if cfg.gas == nil {
		cfg.gas = new(big.Int)
	}
	if err := netlimit.SetTrustedProxies(cfg.trustedProxy); err != nil {
		return nil, err
	}
	fh := &faucetHandler{ch: ch, key: key, coins: cfg.coins, gas: cfg.gas, byAddr: map[common.Address]int64{}, byIP: map[string][]int64{},
		notice: noticeOf(df), perHour: cfg.perHour, perIPDay: cfg.perIPDay}
	fh.path = cfg.logFile
	if fh.path == "" {
		fh.path = cfg.keyFile + ".faucet.json"
	}
	fh.load()
	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/faucet", fh)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { // what this is, for a person who opens the URL
		perHour, perIPDay := fh.limits()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"faucet": "POST /faucet {\"address\": \"0x…\"}", "network": networkName(df),
			"coins": fmtCoins(fh.coins), "gas": fmtCoins(fh.gas), "per_address": "once a day",
			"per_ip_day": perIPDay, "per_hour": perHour, "notice": fh.notice})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 120 * time.Second}
	go srv.Serve(ln)
	return &faucetServer{srv: srv, ln: ln, fh: fh, URL: "http://" + ln.Addr().String() + "/faucet"}, nil
}

func networkName(df *DevnetFile) string {
	if df == nil {
		return ""
	}
	return df.Network
}

// runFaucetServe: ternchain-ops faucet serve, until interrupted.
func runFaucetServe(ch *Chain, df *DevnetFile, cfg faucetServeConfig) error {
	s, err := startFaucetServer(ch, df, cfg)
	if err != nil {
		return err
	}
	from := crypto.PubkeyToAddress(s.fh.key.PublicKey)
	bal, _ := ch.balance(from)
	out(map[string]any{"faucet": s.URL, "from": from.Hex(), "balance": fmtCoins(orZero(bal)), "grant": fmtCoins(new(big.Int).Add(cfg.coins, cfg.gas)),
		"log": s.fh.path, "net_json": "set faucet_url to the public https URL of this /faucet (behind your TLS proxy; --trusted-proxy names it)"})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Printf("ternchain faucet: stopping")
	return s.Close()
}

// faucetChain: the chain the faucet pays on: --rpc URL alone (it only sends
// coins: no contracts), or the net.json's RPCs; the net.json, when there is
// one, gives the notice and checks the chain id.
func faucetChain(netPath, rpc string) (*Chain, *DevnetFile, error) {
	var df *DevnetFile
	var f DevnetFile
	if readJSON(netPath, &f) == nil {
		df = &f
	}
	if rpc == "" {
		if df == nil {
			return nil, nil, fmt.Errorf("faucet serve needs --rpc URL or a net.json (--net %s not found)", netPath)
		}
		ch, err := dialNet(df, netPath)
		return ch, df, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cl, err := ethclient.DialContext(ctx, rpc)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", rpc, err)
	}
	var dep Deployment
	if df != nil {
		dep = df.Deployment
	}
	dep.RPC = rpc
	ch, err := newChain(cl, dep, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("can't reach %s: %w", rpc, err)
	}
	if df != nil && df.ChainID != 0 && df.ChainID != ch.chainID.Int64() {
		return nil, nil, fmt.Errorf("%s serves chain %d, %s is chain %d", rpc, ch.chainID.Int64(), netPath, df.ChainID)
	}
	return ch, df, nil
}
