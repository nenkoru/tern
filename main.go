// ternchain: the Tern idea natively on a chain. Prototype.
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern/internal/netlimit"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Print(coreUsage)
		os.Exit(2)
	}
	if err := run(os.Args[1:]); err != nil && err != errForwarded {
		out(map[string]any{"error": err.Error() + gasHint(err.Error())})
		os.Exit(1)
	}
}

// outW: where commands print their result; nil is os.Stdout (MCP captures
// it: its stdout is the protocol).
var outW io.Writer

func out(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	w := outW
	if w == nil {
		w = os.Stdout
	}
	fmt.Fprintln(w, string(b))
}

// coinsRE: an amount of coins is a plain decimal; no 0x, exponent, sign or
// fraction, so what a person or an autopilot reads is what is paid (audit 6 HP-1).
var coinsRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// parseArgs: flags may come between positionals; a "--" ends the flags for
// good: everything after it is a positional, even if it starts with "-" (a
// script passes data after "--": audit 6 HP-9).
func parseArgs(fs *flag.FlagSet, rest []string) ([]string, error) {
	flagArgs, tail := rest, []string(nil)
	for i, a := range rest {
		if a == "--" {
			flagArgs, tail = rest[:i], rest[i+1:]
			break
		}
	}
	var pos []string
	for in := flagArgs; ; {
		if err := fs.Parse(in); err != nil {
			return nil, err
		}
		in = fs.Args()
		if len(in) == 0 {
			break
		}
		pos = append(pos, in[0])
		in = in[1:]
	}
	return append(pos, tail...), nil
}

func parseCoins(s string) (*big.Int, error) {
	if !coinsRE.MatchString(s) {
		return nil, fmt.Errorf("bad amount %q: a number of coins, like 0.5", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return nil, fmt.Errorf("bad amount %q: a number of coins, like 0.5", s)
	}
	r.Mul(r, new(big.Rat).SetInt(coinUnit))
	if !r.IsInt() {
		return nil, fmt.Errorf("amount %q has too many decimals", s)
	}
	return r.Num(), nil
}

func fmtCoins(w *big.Int) string {
	return new(big.Rat).SetFrac(w, coinUnit).FloatString(6)
}

// testChain, when set by a test, replaces the network the CLI connects to.
var testChain *Chain

// Commands with a subcommand ("pot open", "group join").
var twoWord = map[string]bool{"policy": true, "modules": true, "asp": true, "owner": true, "agent": true, "group": true, "bank": true, "escrow": true, "recovery": true, "cond": true, "bond": true, "artifact": true, "sub": true, "gov": true, "lock": true, "channel": true, "private": true, "cred": true, "relay": true, "pot": true, "bounty": true, "revealbond": true, "token": true, "split": true, "item": true, "random": true, "clock": true, "panel": true, "gas": true, "account": true, "da": true, "price": true, "commons": true, "blocklist": true, "htlc": true, "fund": true, "bridge": true, "arbiter": true, "ui": true, "contract": true}

func run(args []string) error {
	cmd := args[0]
	rest := args[1:]
	for _, a := range rest { // COMMAND --help: its lines of the usage, for every command
		if a == "--" {
			break
		}
		if a == "--help" || a == "-help" || a == "-h" {
			if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") && hasSubs(cmd) {
				cmd += " " + rest[0]
			}
			fmt.Print(helpFor(cmd))
			return nil
		}
	}
	if isOpsCommand(args) && !opsBuild { // an operator command: ternchain-ops (ops.go)
		return forwardOps(args)
	}
	switch cmd {
	case "validator", "deploy":
		return runValidatorCmd(args) // a people's L1 (Besu QBFT): validator.go
	case "fork-from-state", "checkpoint":
		return runForkCmd(args) // the social recovery of a people's L1: validator_fork.go
	case "simulate":
		return runSimulate(rest) // a network at scale, driven in this process: scale_sim.go (sim/scale/)
	case "ceremony":
		return runCeremony(rest) // the zk keys' multi-party trusted setup: ceremony.go
	case "pay", "balance": // the core's names for bank pay and bank balance
		cmd, args = "bank", append([]string{"bank", cmd}, rest...)
		rest = args[1:]
	}
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") && twoWord[cmd] {
		cmd += " " + rest[0]
		rest = rest[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	netPath := fs.String("net", envOr("TERNCHAIN_NET", "net.json"), "network file")
	dir := fs.String("dir", os.Getenv("TERNCHAIN_DIR"), "identity directory")
	name := fs.String("name", "", "")
	owner := fs.String("owner", "", "")
	gdir := fs.String("guardian-dir", "", "")
	listen := fs.String("listen", "127.0.0.1:8545", "")
	adminL := fs.String("admin", "127.0.0.1:8546", "")
	epoch := fs.Uint64("epoch", 86400, "")
	recovery := fs.Uint64("recovery", 72*3600, "")
	feeS := fs.String("fee", "0.000001", "")
	days := fs.Int("days", 30, "")
	spendS := fs.String("spend", "3", "") // per epoch (a UTC day): one deal, a pledge and the data node's channel fit
	scopes := fs.String("scopes", "{}", "")
	gasS := fs.String("gas", "0.5", "")
	outF := fs.String("out", "", "")
	to := fs.String("to", "", "")
	intent := fs.String("intent", "", "")
	subject := fs.String("subject", "", "")
	all := fs.Bool("all", false, "")
	off := fs.Bool("off", false, "")
	private := fs.Bool("private", false, "")
	compromised := fs.Bool("compromised", false, "")
	period := fs.Uint64("period", 0, "")
	minutes := fs.Int("minutes", 0, "")
	once := fs.Bool("once", false, "")
	timeout := fs.Int("timeout", 0, "")
	freshF := fs.Bool("fresh", false, "")
	types := fs.String("types", "", "")
	waitGroup := fs.String("wait-group", "", "")
	tokenS := fs.String("token", "", "")
	maxN := fs.Uint64("max", 0, "")
	soulbound := fs.Bool("soulbound", false, "")
	buyer := fs.String("buyer", "", "")
	payTo := fs.String("pay-to", "", "")
	perMove := fs.Uint64("per-move", 60, "")
	referee := fs.String("referee", "", "")
	hearing := fs.Int("hearing-minutes", 20, "")
	passkey := fs.Bool("passkey", false, "")
	dealF := fs.String("deal", "", "")
	attach := fs.String("attach", "", "")
	outFile := fs.String("save", "", "")
	accountF := fs.Bool("account", false, "")
	pqF := fs.Bool("pq", false, "")
	daF := fs.Bool("da", false, "")
	publicURL := fs.String("public-url", "", "")
	storeDir := fs.String("store", "", "")
	mirrorF := fs.Bool("mirror", false, "")
	trust := fs.String("trust", "", "")
	execCmd := fs.String("exec", "", "")
	scoreS := fs.String("score", "", "")
	postage := fs.Bool("postage", false, "")
	relayURL := fs.String("relay", os.Getenv("TERNCHAIN_RELAY"), "")
	anon := fs.Bool("anon", false, "")
	thread := fs.String("thread", "", "")
	trust2 := fs.String("issuers", "", "")
	priceS := fs.String("price", "0", "")
	license := fs.String("license", "", "")
	uri := fs.String("uri", "", "")
	again := fs.Bool("again", false, "")
	textFile := fs.String("text-file", "", "")
	until := fs.String("until", "", "")
	bondS := fs.String("bond", "", "")
	termF := fs.Uint64("term", 30*86400, "")
	voteWin := fs.Uint64("vote-window", 3*86400, "")
	vetoWin := fs.Uint64("veto-window", 2*86400, "")
	vetoBps := fs.Uint("veto-bps", 3000, "")
	petitionBps := fs.Uint("petition-bps", 2000, "")
	quorumBps := fs.Uint("quorum-bps", 3000, "")
	budgetS := fs.String("budget", "0", "")
	agentsVote := fs.Bool("agents-vote", false, "")
	rulesS := fs.String("rules", "", "")
	dataDir := fs.String("datadir", "", "")
	faucetF := fs.String("faucet", "", "")
	faucetCoins := fs.String("faucet-coins", "20", "")
	faucetCoinsAlias := fs.String("coins", "", "")     // faucet serve: the same as --faucet-coins
	cursorF := fs.String("cursor", "", "")             // watch --cursor NAME: its own cursor and state under DIR/cursors/NAME
	stateDirF := fs.String("state-dir", "", "")        // the same with any folder
	rpcF := fs.String("rpc", "", "")                   // faucet serve: sign through this RPC (no net.json needed)
	faucetHourF := fs.Int("faucet-per-hour", 0, "")    // devnet: grants an hour, all addresses (0: 100)
	faucetIPDayF := fs.Int("faucet-per-ip-day", 0, "") // devnet: grants a day per IPv4 address or IPv6 /64 (0: 5)
	relayDefault := fs.String("relay-url", "", "")
	netName := fs.String("network", "", "")
	hours := fs.Int("hours", 0, "")
	secretFile := fs.String("secret-file", "", "")
	algoS := fs.String("algo", "sha256", "")
	expectS := fs.String("expect", "", "")
	againstS := fs.String("against", "", "")
	panelS := fs.String("panel", "", "")
	kF := fs.Uint64("k", 0, "")
	arbiterS := fs.String("arbiter", "", "")
	originS := fs.String("origin", "", "")
	gf := govFlags{}
	fs.StringVar(&gf.spec, "spec", "", "")
	fs.StringVar(&gf.specHash, "spec-hash", "", "")
	fs.StringVar(&gf.audit, "audit", "", "")
	fs.StringVar(&gf.auditHash, "audit-hash", "", "")
	fs.StringVar(&gf.notes, "notes", "", "")
	fs.StringVar(&gf.house, "house", "", "")
	fs.StringVar(&gf.reviewers, "reviewers", "", "")
	fs.StringVar(&gf.why, "why", "", "")
	fs.BoolVar(&gf.sybil, "sybil", false, "")
	genesisF := fs.String("genesis", "", "")
	keyS := fs.String("key", "", "")
	trackerS := fs.String("tracker", "", "")
	pieceKiB := fs.Int("piece-kib", 4096, "")
	lockS := fs.String("lock", "", "")
	chainIDF := fs.Int64("chain-id", 0, "")
	optimisticF := fs.Int("optimistic", 0, "")
	forceF := fs.Bool("force", false, "")
	sharedF := fs.Bool("shared", false, "")
	logIndexF := fs.Bool("log-index", true, "")
	bucketBitsF := fs.Int("bucket-bits", -1, "")
	operatorF := fs.String("operator", "", "")
	contactF := fs.String("contact", "", "")
	askF := fs.String("ask", "", "")
	aspF := fs.String("asp", "", "")
	openF := fs.Bool("open", false, "")
	ruleS := fs.String("rule", "", "")
	quotaF := fs.Uint("quota", 0, "")       // lock open ruling --rule threshold: how many of the k must give the payee >= 50%
	anyoneF := fs.Bool("anyone", false, "") // lock open ruling: anyone may dispute (with the bond)
	labelsS := fs.String("labels", "", "")
	directF := fs.Bool("direct", false, "")
	proxyF := fs.String("proxy", os.Getenv("TERNCHAIN_PROXY"), "")
	poolFeeS := fs.String("pool-fee", "", "")
	citizensOnlyF := fs.Bool("citizens-only", false, "")
	announceF := fs.Bool("announce", false, "")      // commons found --announce: an announce-only community (news.go)
	noAnnounceF := fs.Bool("no-announce", false, "") // agent join / owner init: don't join the network's news community
	ownersF := fs.Bool("owners", false, "")
	categoryF := fs.String("category", "", "")
	keepPostageF := fs.Bool("keep-postage", false, "")
	communityF := fs.String("community", "", "")
	trustDepthF := fs.Int("trust-depth", 0, "")
	rankingF := fs.String("ranking", "trust", "")
	trustedProxyF := fs.String("trusted-proxy", "", "")
	plainF := fs.Bool("plain", false, "")
	alertsF := fs.Bool("alerts", false, "") // done --alerts: clear every current alert
	replyF := fs.String("reply", "", "")
	actionF := fs.String("action", "", "")       // owner approve: bind the approval to one typed UI action (its hash)
	ownerDirF := fs.String("owner-dir", "", "")  // ui serve: the owner's folder, for the trusted approval sheet
	shareF := fs.String("share", "", "")         // ui publish: the folder bundles are copied to (file:// distribution)
	llmCmdF := fs.String("llm-cmd", "", "")      // ui serve: a command that reads a prompt on stdin and writes OpenUI Lang
	parentBpsF := fs.Int("parent-bps", 1000, "") // ui fork: the parent's share of what the fork earns
	titleF := fs.String("title", "", "")         // ui post
	cardF := fs.String("card", "", "")           // ui post: a typed card (JSON) carried by the post
	var cf contractFlags                         // contract …: bring your own contract (contract.go, BUILD.md)
	fs.StringVar(&cf.artifact, "artifact", "", "")
	fs.StringVar(&cf.abi, "abi", "", "")
	fs.StringVar(&cf.bin, "bin", "", "")
	fs.StringVar(&cf.args, "args", "", "")
	fs.StringVar(&cf.value, "value", "", "")
	fs.StringVar(&cf.source, "source", "", "")
	fs.StringVar(&cf.note, "note", "", "")
	pos, err := parseArgs(fs, rest)
	if err != nil {
		return err
	}
	if *proxyF != "" { // before anything dials out
		if err := useProxy(*proxyF); err != nil {
			return err
		}
	}
	// Flags may also come before the subcommand: "pot --dir D open …".
	if twoWord[cmd] && len(pos) > 0 {
		cmd += " " + pos[0]
		pos = pos[1:]
	}
	if cmd != "help" && !knownCommands()[cmd] { // before connecting: what failed and what to type instead
		return unknownCommand(cmd)
	}
	if err := checkModule(cmd, *dir); err != nil { // an optional module that is off (modules.go)
		return err
	}
	if p, _, _ := strings.Cut(cmd, " "); p == "modules" { // no network needed
		return runModules(cmd, pos, *dir)
	}
	deprecationNote(cmd) // an old recipe name (escrow open, pot award…): one line on stderr, then as before
	if *textFile != "" { // the text from a file, or stdin with -: no shell quoting to get wrong
		if err := textFromFile(cmd, &pos, *textFile); err != nil {
			return err
		}
	}
	arg := func(i int, what string) (string, error) {
		if len(pos) <= i {
			return "", fmt.Errorf("%s: missing %s (ternchain %s --help)", cmd, what, cmd)
		}
		return pos[i], nil
	}
	listenSet := false
	fs.Visit(func(f *flag.Flag) { listenSet = listenSet || f.Name == "listen" })
	if cmd == "devnet" {
		fee, err := parseCoins(*feeS)
		if err != nil {
			return err
		}
		fc, err := parseCoins(*faucetCoins)
		if err != nil {
			return err
		}
		gen, err := parseGenesisStrict(*genesisF)
		if err != nil {
			return err
		}
		return runDevnet(devnetConfig{net: *netPath, listen: *listen, admin: *adminL, dataDir: *dataDir, publicURL: *publicURL,
			relay: *relayDefault, name: *netName, faucetCoins: fc, faucetPerHour: *faucetHourF, faucetPerIPDay: *faucetIPDayF, trustedProxy: *trustedProxyF, chainID: *chainIDF, shared: *sharedF, operator: *operatorF, contact: *contactF, noLogIndex: !*logIndexF, bucketBits: *bucketBitsF,
			p: Params{EpochLength: *epoch, RecoveryDelay: *recovery, Fee: fee, Gov: GovParams{Genesis: gen, Short: true}}})
	}
	if cmd == "help" {
		if *all || firstOr(pos, 0, "") == "all" || firstOr(pos, 0, "") == "--all" {
			fmt.Print(fullUsage())
		} else if firstOr(pos, 0, "") == "modules" || firstOr(pos, 0, "") == "ops" {
			fmt.Print(sectionUsage(pos[0]))
		} else if len(pos) > 0 {
			fmt.Print(helpFor(strings.Join(pos, " ")))
		} else {
			fmt.Print(coreUsage)
		}
		return nil
	}
	if cmd == "version" {
		v := versionInfo()
		var f DevnetFile // the network this machine is set up for, from its file only (no connection)
		if readJSON(*netPath, &f) == nil {
			v["net_file"], v["network"], v["chain_id"], v["mode"] = *netPath, f.Network, f.ChainID, f.Mode
			if f.ContractsHash != "" {
				v["network_contracts_hash"] = f.ContractsHash
				v["matches_network"] = f.ContractsHash == v["contracts_hash"]
			}
		}
		out(v)
		return nil
	}
	if cmd == "owner keygen" { // offline: a key whose address a founder hands the operator before the network exists
		addr, err := keygen(*outF)
		if err != nil {
			return err
		}
		r := map[string]any{"address": addr.Hex(), "key_file": *outF + " (mode 0600: your owner key; keep it like id/)",
			"next": "after the network starts: ternchain owner init --key " + *outF + " --dir id/NAME --name NAME --guardian-dir G"}
		if *name != "" {
			r["genesis"] = *name + "=" + addr.Hex()
			r["founder"] = "send the operator this genesis line; once the network runs: owner init --key, then 'gov claim'"
		}
		out(r)
		return nil
	}
	if cmd == "artifact torrent" { // a file only: no network needed
		return runArtifactBig(nil, cmd, pos, primFlags{out: *outF}, "", *trackerS, *pieceKiB)
	}
	if cmd == "faucet" && firstOr(pos, 0, "") == "serve" { // the standalone faucet service (faucet.go); no contracts needed
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		coinsS, gasV := "5", "0.1"
		if set["faucet-coins"] {
			coinsS = *faucetCoins
		}
		if set["coins"] {
			coinsS = *faucetCoinsAlias
		}
		if set["gas"] {
			gasV = *gasS
		}
		fc, err := parseCoins(coinsS)
		if err != nil {
			return err
		}
		gc, err := parseCoins(gasV)
		if err != nil {
			return err
		}
		if !listenSet {
			*listen = "127.0.0.1:8660"
		}
		logF := ""
		if *dataDir != "" {
			logF = filepath.Join(*dataDir, "faucet.json")
		}
		ch, df, err := faucetChain(*netPath, *rpcF)
		if err != nil {
			return err
		}
		return runFaucetServe(ch, df, faucetServeConfig{keyFile: *keyS, listen: *listen, logFile: logF, trustedProxy: *trustedProxyF,
			coins: fc, gas: gc, perHour: *faucetHourF, perIPDay: *faucetIPDayF})
	}
	ch, df, err := connect(*netPath)
	if testChain != nil { // tests run the CLI against a simulated chain
		ch, err = testChain, nil
	}
	if err != nil {
		return err
	}
	if *relayURL == "" && df != nil && df.Relay != "" { // the network's default data node
		*relayURL = df.Relay
	}
	if df != nil && df.Mode == "shared" { // what clients of a shared test network refuse
		sharedNetwork = true // and its MCP tools (runTool)
		sharedRefusals = df.Refuse
		if err := sharedRefusal(cmd, pos, *passkey); err != nil {
			return err
		}
	}
	client := func() (*Client, error) {
		if *dir == "" {
			return nil, errors.New("--dir is required (the identity folder, e.g. --dir id/nina; or set TERNCHAIN_DIR)")
		}
		if ch.bundler == nil && faucetKey(df) != "" { // devnet: the faucet bundles user operations
			if k, err := crypto.HexToECDSA(df.Faucet); err == nil {
				ch.bundler = k
			}
		}
		if !longLived && ch.cacheDir == "" { // the next run of the CLI starts from what this one read
			ch.cacheDir = filepath.Join(*dir, "cache")
		}
		sd := *stateDirF
		if *cursorF != "" { // a second reader of this identity (a bridge's watch): its own cursor and state
			if !nameRE.MatchString(*cursorF) {
				return nil, fmt.Errorf("--cursor %q: lower-case letters, digits and hyphens", *cursorF)
			}
			sd = filepath.Join(*dir, "cursors", *cursorF)
		}
		c, err := openClientState(*dir, sd, ch)
		if c != nil {
			c.relay, c.force = *relayURL, *forceF
			if df != nil {
				c.announce = df.Announce // the network's news community (news.go)
			}
			if *tokenS != "" { // --token usd: payments and locks in that token
				a, terr := c.tokenAddr(*tokenS)
				if terr != nil {
					return nil, terr
				}
				c.asset = a
			}
		}
		return c, err
	}

	if cmd == "relay serve" {
		longLived = true
		if !listenSet { // not the devnet's RPC port
			*listen = "127.0.0.1:8650"
		}
		if err := netlimit.SetTrustedProxies(*trustedProxyF); err != nil {
			return err
		}
		// The node's key: an identity's (then senders pay that name), or the devnet faucet.
		var k *ecdsa.PrivateKey
		name := ""
		if *dir != "" {
			i, err := loadIdent(*dir)
			if err != nil {
				return err
			}
			k, name = i.key, i.f.Name
		} else if fk, err := crypto.HexToECDSA(faucetKey(df)); err == nil {
			k = fk
		} else {
			return errors.New("relay serve needs --dir (a funded identity) on networks without a faucet")
		}
		r := &Relay{ch: ch, key: k}
		if *poolFeeS != "" {
			f, err := parseCoins(*poolFeeS)
			if err != nil {
				return err
			}
			r.poolFee = f
		}
		if *aspF != "" { // keep an association set by a rule, republished every 10 minutes
			go r.publishASP(*aspF, *ruleS)
		}
		if *daF { // a data node: hashes on chain, bytes kept here (on disk with --store)
			url := *publicURL
			if url == "" {
				url = "http://" + *listen
			}
			price, err := parseCoins(*priceS)
			if err != nil {
				return err
			}
			if localURL(url) && !localURL(ch.dep.RPC) {
				log.Printf("relay: the staked URL %s is local: readers elsewhere can't fetch batches; pass --public-url https://…", url)
			}
			if r, err = newDataNode(ch, k, *storeDir, name, price, *mirrorF); err != nil {
				return err
			}
			if err := r.ensureStake(url); err != nil {
				return fmt.Errorf("staking as a data node: %w", err)
			}
			r.publicURL = url
		}
		return r.Serve(*listen)
	}
	if p, _, _ := strings.Cut(cmd, " "); p == "token" || p == "split" || p == "item" || p == "random" || p == "clock" || p == "panel" {
		c, err := client()
		if err != nil {
			return err
		}
		return runWorld(c, cmd, pos, worldFlags{max: *maxN, soulbound: *soulbound, buyer: *buyer, payTo: *payTo, perMove: *perMove,
			referee: *referee, minutes: *minutes, days: *days, hearing: *hearing})
	}
	if p, _, _ := strings.Cut(cmd, " "); p == "ui" { // your own Reddit with a chat client: ui.go, internal/ui, FEED.md
		if cmd == "ui serve" {
			longLived = true
		}
		c, err := client()
		if err != nil {
			return err
		}
		return runUI(c, cmd, pos, uiFlags{listen: *listen, listenSet: listenSet, ownerDir: *ownerDirF, community: *communityF, share: *shareF,
			llm: *llmCmdF, name: *name, out: *outF, parentBps: *parentBpsF, trust: *trust, title: *titleF, card: *cardF, thread: *thread, reply: *replyF})
	}
	if p, _, _ := strings.Cut(cmd, " "); p == "price" || p == "commons" || p == "blocklist" {
		c, err := client()
		if err != nil {
			return err
		}
		switch p {
		case "price":
			return runPrice(c, cmd, pos, *bondS)
		case "blocklist":
			return runBlocklist(c, cmd, pos, *ownersF)
		}
		return runCommons(c, cmd, pos, commonsFlags{term: *termF, voteWindow: *voteWin, vetoWindow: *vetoWin, vetoBps: *vetoBps,
			petitionBps: *petitionBps, quorumBps: *quorumBps, budget: *budgetS, rules: *rulesS, agentsVote: *agentsVote, off: *off,
			citizensOnly: *citizensOnlyF, owners: *ownersF, announce: *announceF,
			panel: *panelS, k: *kF, arbiter: *arbiterS, hearing: uint64(max(*hearing, 0)), days: uint64(max(*days, 1)), plain: *plainF})
	}
	if p, _, _ := strings.Cut(cmd, " "); p == "htlc" || p == "fund" || p == "bridge" || p == "arbiter" {
		c, err := client()
		if err != nil {
			return err
		}
		switch p {
		case "arbiter":
			return runArbiter(c, cmd, pos)
		case "htlc":
			return runHTLC(c, cmd, pos, htlcFlags{hours: *hours, minutes: *minutes, secretFile: *secretFile, algo: *algoS,
				expect: *expectS, against: *againstS})
		case "fund":
			return runFund(c, cmd, pos, *days, *minutes, *to, *uri)
		}
		network := ""
		if df != nil {
			network = df.Network
		}
		return runBridge(c, cmd, pos, network, *outF, *originS)
	}
	if p, _, _ := strings.Cut(cmd, " "); p == "contract" {
		c, err := client()
		if err != nil {
			return err
		}
		return runContract(c, cmd, pos, cf)
	}
	if p, _, _ := strings.Cut(cmd, " "); p == "policy" { // client policy modules: policy.go
		c, err := client()
		if err != nil {
			return err
		}
		return runPolicy(c, cmd, pos, policyFlags{name: *name, share: *shareF})
	}
	if p, _, _ := strings.Cut(cmd, " "); p == "private" || p == "cred" {
		c, err := client()
		if err != nil {
			return err
		}
		return runPrivate(c, cmd, pos, *trust2, *days, poolSendOpts{asp: *aspF, direct: *directF})
	}
	if p, _, _ := strings.Cut(cmd, " "); p == "asp" {
		c, err := client()
		if err != nil {
			return err
		}
		return runASP(c, cmd, pos, *name, *ruleS, *labelsS, *uri)
	}
	switch cmd {
	case "artifact add", "artifact show", "artifact verify", "artifact decrypt", "artifact get":
		c, err := client()
		if err != nil {
			return err
		}
		return runArtifactBig(c, cmd, pos, primFlags{price: *priceS, license: *license, uri: *uri, out: *outF}, *keyS, *trackerS, *pieceKiB)
	case "escrow milestones":
		if len(pos) < 3 {
			return errors.New("usage: escrow milestones WORKER ARBITER AMOUNT:TERMS… [--days N] (milestone i is due i×N days from now)")
		}
		ms, err := parseMilestones(pos[2:])
		if err != nil {
			return err
		}
		c, err := client()
		if err != nil {
			return err
		}
		every := uint64(max(*days, 1)) * 86400
		if *minutes > 0 {
			every = uint64(*minutes) * 60
		}
		locks, err := c.openMilestones(pos[0], pos[1], ms, every)
		out(map[string]any{"milestones": locks, "next": "per milestone: the worker delivers an artifact and attests SUBJECT delivered " +
			`'{"lock":L,"artifact":A}'; the payer runs artifact verify, then lock release L; a dispute goes to the arbiter (escrow rule SUBJECT BPS)`})
		return err
	}
	if p, _, _ := strings.Cut(cmd, " "); p == "gov" {
		c, err := client()
		if err != nil {
			return err
		}
		gf.k, gf.days, gf.all, gf.open = *kF, uint64(*days), *all, *openF
		if gf.reviewers == "" { // gov propose grant … --panel a,b,c: the same flag as commissions and panels
			gf.reviewers = *panelS
		}
		return runGov(c, cmd, pos, gf)
	}
	pf := primFlags{days: *days, price: *priceS, license: *license, uri: *uri, out: *outF, period: *period, minutes: *minutes,
		optimistic: *optimisticF, arbiter: *arbiterS, panel: *panelS, k: *kF, hours: *hours}
	if cmd == "lock open" { // one grammar for every recipe: cli_lock.go
		c, err := client()
		if err != nil {
			return err
		}
		return runLockOpen(c, pos, lockFlags{prim: pf, hearing: *hearing, rule: *ruleS, quota: *quotaF, bond: *bondS, anyone: *anyoneF, htlc: htlcFlags{hours: *hours, minutes: *minutes, secretFile: *secretFile,
			algo: *algoS, expect: *expectS, against: *againstS}})
	}
	if p, _, _ := strings.Cut(cmd, " "); p == "cond" || p == "bond" || p == "artifact" || p == "sub" || p == "escrow" || p == "lock" || p == "channel" || p == "pot" || p == "bounty" || p == "revealbond" || cmd == "approve" {
		c, err := client()
		if err != nil {
			return err
		}
		return runPrimitive(c, cmd, pos, pf)
	}
	switch cmd {
	case "faucet":
		a, err := arg(0, "ADDRESS")
		if err != nil {
			return err
		}
		amt, err := parseCoins(firstOr(pos, 1, "10"))
		if err != nil {
			return err
		}
		k, err := crypto.HexToECDSA(faucetKey(df))
		if err != nil {
			return errors.New("no faucet key here: it is in the operator's file (" + operatorPath(*netPath) + "); others get coins from the faucet URL (owner init does it)")
		}
		if err := ch.transfer(k, common.HexToAddress(a), amt); err != nil {
			return err
		}
		out(map[string]any{"funded": a, "coins": fmtCoins(amt)})
		return nil
	case "advance":
		s, err := arg(0, "SECONDS")
		if err != nil {
			return err
		}
		if df == nil || df.Admin == "" {
			return errors.New("no admin URL here: it is in the operator's file (" + operatorPath(*netPath) + ")")
		}
		resp, err := http.Get(df.Admin + "?seconds=" + s)
		if err != nil {
			return err
		}
		resp.Body.Close()
		out(map[string]any{"advanced_seconds": s})
		return nil

	case "owner init":
		if *dir == "" || *name == "" || *gdir == "" {
			return errors.New("owner init needs --dir, --name and --guardian-dir (e.g. --dir id/nina --name nina --guardian-dir usb/nina)")
		}
		var key *ecdsa.PrivateKey // --key FILE: a key made earlier with owner keygen (a founder's)
		if *keyS != "" {
			k, err := readKeyFile(*keyS)
			if err != nil {
				return err
			}
			key = k
		}
		// A re-run after a failed funding (no coins yet) reuses the folders it
		// made: the same key, the same guardian, the same address to fund.
		var g *Ident
		resumed := false
		if i, err := loadIdent(*dir); err == nil {
			if i.f.Kind != "owner" || i.f.Name != *name {
				return fmt.Errorf("%s holds %s %q, not owner %q: use another folder", *dir, i.f.Kind, i.f.Name, *name)
			}
			if key != nil && crypto.PubkeyToAddress(key.PublicKey) != i.addr() {
				return fmt.Errorf("%s holds another key than --key %s: use another folder", *dir, *keyS)
			}
			resumed = true
		}
		if gi, err := loadIdent(*gdir); err == nil {
			if gi.f.Kind != "guardian" || gi.f.Name != *name {
				return fmt.Errorf("%s holds %s %q, not the guardian of %q: use another --guardian-dir", *gdir, gi.f.Kind, gi.f.Name, *name)
			}
			g = gi
		} else if g, err = newIdentDir(*gdir, IdentityFile{Name: *name, Kind: "guardian"}); err != nil {
			return err
		}
		if !resumed {
			if _, err := newIdentDirKey(*dir, IdentityFile{Name: *name, Kind: "owner", Owner: *name, PQ: *pqF}, key); err != nil {
				return err
			}
		}
		c, err := client()
		if err != nil {
			return err
		}
		registered := false // by this key already (a run that stopped after register)
		if o, err := ch.call("Registry", "owners", idOf(*name)); err == nil {
			if a := o[0].(common.Address); a != (common.Address{}) {
				if a != c.me() {
					return fmt.Errorf("the name %q is taken (by %s): pick another --name (with new folders)", *name, a.Hex())
				}
				registered = true
			}
		}
		noCoins := func(why string) error {
			return fmt.Errorf("no test coins yet for %s (%s). Your key and guardian folders are kept: send this address to the network's operator "+
				"(or try the faucet again later), then run the same owner init command again — it reuses them: %s", c.me().Hex(), why, c.me().Hex())
		}
		if err := mixedScriptErr(*name, *forceF); err != nil {
			return err
		}
		if k := c.registeredLookalike(*name); k != "" && !*forceF {
			return fmt.Errorf("the name %q looks like the existing name %q (people would mistake one for the other); pick another, or add --force", *name, k)
		}
		if *passkey { // the name is held by a smart account guarded by a passkey
			if err := c.enablePasskeyAccount(); err != nil {
				return err
			}
		}
		bal, _ := ch.balance(c.me())
		funded := bal != nil && bal.Cmp(mustCoins("0.01")) >= 0                // coins arrived already (the operator sent some): no faucet
		if k, err := crypto.HexToECDSA(faucetKey(df)); err == nil && !funded { // the operator's machine
			ch.transfer(k, c.me(), mustCoins("100"))
			ch.transfer(k, g.addr(), mustCoins("1"))
		} else if u := *faucetF; !funded && (u != "" || df != nil && df.FaucetURL != "") { // anyone else: the network's faucet
			if u == "" {
				u = df.FaucetURL
			}
			if err := askFaucet(u, c.me()); err != nil {
				return noCoins("the faucet " + u + " said: " + err.Error())
			}
			ch.transfer(c.key, g.addr(), mustCoins("0.05")) // gas for the guardian, should it ever recover the name
		} else if !funded && !registered {
			return noCoins("this network's net.json names no faucet")
		}
		switch {
		case registered && c.view == nil:
			if err := c.finishRegister(); err != nil {
				return friendlyGas(err, c.me())
			}
		case !registered:
			if err := c.registerOwner(g.addr()); err != nil {
				if strings.Contains(err.Error(), "insufficient funds") {
					return noCoins("not enough for the gas of registering")
				}
				return err
			}
		}
		// Private receiving by default: stealth keys (ERC-6538), so payers who
		// support it pay a new address each time, and the ring key.
		privRecv := "on: payers who support it pay you at one-time addresses (private notes)"
		if err := c.enableStealth(); err != nil {
			privRecv = "off: " + err.Error() + " (private enable turns it on)"
		} else if err := c.enableRing(); err != nil {
			privRecv = "stealth on, ring key not published: " + err.Error()
		}
		o := map[string]any{"owner": *name, "address": c.me().Hex(), "guardian": g.addr().Hex(), "private_receiving": privRecv,
			"note":   "keep the guardian directory apart from this device: it can recover the name after a 72-hour delay",
			"notice": noticeOf(df)}
		if df != nil && df.Announce != "" { // the network's news community, next to its notice
			o["announce"] = df.Announce
			if !*noAnnounceF {
				o["network_news"] = c.joinNews()
			}
		}
		if df != nil && df.Mode == "shared" {
			o["terms"] = "TERMS.md, next to net.json where you got it"
		}
		if founder, err := c.ch.call("Citizens", "genesisKey", c.id()); err == nil && founder[0].(common.Address) == c.me() {
			o["founder"] = "a founding citizen of this network: run 'ternchain gov claim --dir " + *dir + "' now (before the first term ends)"
		}
		if b, err := ch.balance(c.me()); err == nil {
			o["coins"] = fmtCoins(b) + " (test coins, for gas; bank deposit AMOUNT moves some to your bank for your agents)"
		}
		if *passkey {
			if k, err := crypto.HexToECDSA(faucetKey(df)); err == nil { // devnet: some gas for the owner's operations
				ch.send(k, mustCoins("5"), "TernPaymaster", "fund", idOf(*name))
			}
			o["account"] = "smart account (ERC-4337), passkey-guarded; " + c.addr().Hex() + " is now only the mail key"
		}
		out(o)
		return nil
	case "agent init":
		if *dir == "" || *name == "" || *owner == "" {
			return errors.New("agent init needs --dir, --name and --owner (e.g. --dir agent/ada.oscar --name ada --owner oscar)")
		}
		i, err := newIdentDir(*dir, IdentityFile{Name: *name + "." + *owner, Kind: "agent", Owner: *owner})
		if err != nil {
			return err
		}
		req := AgentRequest{Name: *name, Owner: *owner, Address: i.addr().Hex()}
		if *accountF { // the agent's key becomes a session key of its own account; no ETH needed
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.enableAgentAccount(); err != nil {
				return err
			}
			req.Address, req.Account = c.me().Hex(), true
		}
		p := filepath.Join(*dir, "request.json")
		if err := writeJSON(p, req); err != nil {
			return err
		}
		out(map[string]any{"agent": *name + "." + *owner, "request": p, "next": "the owner runs: ternchain owner grant --dir id/" + *owner + " " + p + " --spend 3 --scopes '{\"free_pay\":\"0.3\"}' --gas 0.05"})
		return nil
	case "owner grant":
		c, err := client()
		if err != nil {
			return err
		}
		p, err := arg(0, "REQUEST.json")
		if err != nil {
			return err
		}
		var req AgentRequest
		if err := readJSON(p, &req); err != nil {
			return err
		}
		spend, err := parseCoins(*spendS)
		if err != nil {
			return err
		}
		gas, err := parseCoins(*gasS)
		if err != nil {
			return err
		}
		b, err := c.grant(req, *days, spend, *scopes, gas)
		if err != nil {
			return err
		}
		if *outF == "" {
			*outF = filepath.Join(filepath.Dir(p), "bundle.json")
		}
		if err := writeJSON(*outF, b); err != nil {
			return err
		}
		r := map[string]any{"granted": b.Agent, "spend_per_epoch": *spendS, "days": *days, "bundle": *outF, "epoch": c.epochWords()}
		if b.Gas != "" {
			r["gas"] = b.Gas
		}
		out(r)
		return nil
	case "agent join":
		c, err := client()
		if err != nil {
			return err
		}
		p, err := arg(0, "BUNDLE.json")
		if err != nil {
			return err
		}
		var b GrantBundle
		if err := readJSON(p, &b); err != nil {
			return err
		}
		if err := c.join(b); err != nil {
			return err
		}
		gone := os.Remove(p) == nil // the bundle holds the family view key
		r := map[string]any{"joined": c.f.Name}
		if c.announce != "" && !*noAnnounceF { // the network's news community: a failure here never fails the join
			r["network_news"] = c.joinNews()
		}
		if gone {
			r["bundle"] = p + " removed: it held your family's view key, now kept in " + *dir
		}
		out(r)
		return nil
	case "owner approve":
		c, err := client()
		if err != nil {
			return err
		}
		if *askF != "" { // exactly what an ask-owner asked for
			r, err := c.approveAsk(*askF)
			if err != nil {
				return err
			}
			out(r)
			return nil
		}
		if len(pos) < 2 {
			return errors.New("usage: owner approve AGENT TO [AMOUNT]   (one payment of at most AMOUNT to TO, or without AMOUNT: writing to TO; valid a day;\n" +
				"       TO = cred:VERIFIER lets the agent present your credentials to VERIFIER)\n" +
				"       owner approve --ask ID   (exactly what the agent's ask-owner ID asks for)")
		}
		var amt *big.Int
		if len(pos) > 2 {
			if amt, err = parseCoins(pos[2]); err != nil {
				return err
			}
		}
		if *actionF != "" { // exactly one typed UI action (ui.go): the approval is bound to its hash
			if err := c.ownerSignsAction(pos[0], pos[1], amt, false, *actionF); err != nil {
				return err
			}
			out(map[string]any{"approved": pos[0], "to": pos[1], "amount": firstOr(pos, 2, ""), "action": *actionF, "valid": "only for that action, 24 hours"})
			return nil
		}
		if *off && amt != nil { // withdraw a payment approval not spent yet (escrow.sh after a failed lock)
			if err := c.ownerSigns(pos[0], pos[1], amt, true); err != nil {
				return err
			}
			out(map[string]any{"withdrawn": pos[0], "to": pos[1], "amount": pos[2]})
			return nil
		}
		if err := c.approveAction(pos[0], pos[1], amt); err != nil {
			return err
		}
		if amt == nil && strings.HasPrefix(pos[1], "thread:") {
			out(map[string]any{"approved": pos[0], "to": pos[1], "allows": "that thread's limit, once (thread_per_day more today, or the hop count reset); nothing else"})
			return nil
		}
		if amt == nil {
			out(map[string]any{"approved": pos[0], "to": pos[1], "contact": pos[1] + " is now a lasting correspondent of " + pos[0] +
				" (owner contact " + pos[0] + " " + pos[1] + " --off withdraws it); payments still need their own approval above free_pay"})
			return nil
		}
		out(map[string]any{"approved": pos[0], "to": pos[1], "amount": pos[2], "valid": "one payment of at most that, 24 hours"})
		return nil
	case "owner contact":
		c, err := client()
		if err != nil {
			return err
		}
		if len(pos) < 2 {
			return errors.New("usage: owner contact AGENT NAME [--off]   (NAME becomes a lasting correspondent of AGENT: it may write to NAME after reading outside mail)")
		}
		if err := c.contactFor(pos[0], pos[1], *off); err != nil {
			return err
		}
		out(map[string]any{"agent": pos[0], "contact": pos[1], "on": !*off, "note": "signed by you; arrives as mail to the agent within seconds"})
		return nil
	case "owner revoke":
		c, err := client()
		if err != nil {
			return err
		}
		a, err := arg(0, "AGENT")
		if err != nil {
			return err
		}
		if err := c.revokeAgent(a, *compromised); err != nil {
			return err
		}
		out(map[string]any{"revoked": a, "compromised": *compromised})
		return nil
	case "owner cancel-recovery":
		c, err := client()
		if err != nil {
			return err
		}
		if err := c.cancelRecovery(); err != nil {
			return err
		}
		out(map[string]any{"recovery": "cancelled"})
		return nil
	case "recovery start", "recovery finish":
		c, err := client()
		if err != nil {
			return err
		}
		o, err := arg(0, "OWNER")
		if err != nil {
			return err
		}
		if cmd == "recovery start" {
			a, err := arg(1, "NEW-ADDRESS")
			if err != nil {
				return err
			}
			err = c.startRecovery(o, common.HexToAddress(a))
			if err != nil {
				return err
			}
			out(map[string]any{"recovery": "started", "owner": o, "note": "takes effect after the delay unless the owner cancels"})
			return nil
		}
		if err := c.finishRecovery(o); err != nil {
			return err
		}
		out(map[string]any{"recovery": "finished", "owner": o})
		return nil

	case "whoami":
		c, err := client()
		if err != nil {
			return err
		}
		out(c.whoami())
		return nil
	case "keys":
		c, err := client()
		if err != nil {
			return err
		}
		gone, err := c.rotateEpochs()
		if err != nil {
			return err
		}
		e, _ := ch.epoch()
		out(map[string]any{"epoch": e, "published_through": e + keysAhead - 1, "destroyed_epochs": gone})
		return nil
	case "inbox":
		c, err := client()
		if err != nil {
			return err
		}
		if _, err := c.scan(); err != nil {
			return err
		}
		out(c.inboxView(*all))
		return nil
	case "read", "done":
		c, err := client()
		if err != nil {
			return err
		}
		if cmd == "done" && *alertsF { // done --alerts: the alerts inbox shows, cleared
			n := c.clearAlerts()
			if err := c.save(); err != nil {
				return err
			}
			out(map[string]any{"alerts_cleared": n})
			return nil
		}
		id, err := arg(0, "ID")
		if err != nil {
			return err
		}
		it := c.find1(id)
		if it == nil { // not fetched yet (a group post, new mail): fetch, then look again
			if _, err := c.scan(); err == nil {
				it = c.find1(id)
			}
		}
		if it == nil {
			if sid, r := c.sentRec(id); r != nil { // one we sent: where it landed
				if _, err := c.scan(); err == nil {
					sid, r = c.sentRec(id)
				}
				out(sentView(sid, r))
				return nil
			}
			return fmt.Errorf("no item %s (run inbox)", id)
		}
		if it.Batch == "" { // read before batch roots were kept: look it up
			if it.Batch, it.Node = c.findBatch(it.ID, it.Block); it.Batch != "" {
				c.save()
			}
		}
		if cmd == "done" {
			it.Done = true
			c.save()
		}
		if *outFile == "" && cmd == "read" { // read ID --out FILE: the same as --save, as elsewhere
			*outFile = *outF
		}
		if *outFile != "" && it.Mail != nil { // read ID --save FILE: keep the attachment
			at, err := saveAttachment(it.Mail.Data, *outFile)
			if err != nil {
				return err
			}
			out(map[string]any{"saved": *outFile, "name": at.Name, "size": at.Size, "sha256": at.SHA256})
			return nil
		}
		out(c.readViewOf(it))
		return nil
	case "send", "reply":
		c, err := client()
		if err != nil {
			return err
		}
		if cmd == "send" && *replyF != "" { // send --reply ID: as reply ID
			cmd, pos = "reply", append([]string{*replyF}, pos...)
		}
		o := SendOpts{To: *to, Intent: intentAlias(*intent), Subject: *subject, Postage: *postage, Category: *categoryF, KeepPostage: *keepPostageF}
		if *attach != "" { // a small file travels inside the sealed message
			d, err := attachFile(*attach)
			if err != nil {
				return err
			}
			o.Data = d
		}
		if cmd == "reply" {
			id, err := arg(0, "ID")
			if err != nil {
				return err
			}
			orig := c.findFetch(id)
			if orig == nil {
				return fmt.Errorf("no item %s (checked new mail too: is the id right? inbox lists them)", id)
			}
			o.To, o.ReplyTo = orig.From, orig.ID
			if o.Intent == "" {
				o.Intent = "answer"
			}
			o.Text = strings.Join(pos[1:], " ")
		} else {
			o.Text = strings.Join(pos, " ")
		}
		id, err := c.send(o)
		if err != nil {
			return err
		}
		r := map[string]any{"sent": id, "to": o.To}
		if c.lastRefund != nil {
			r["postage"] = c.lastRefund
		}
		if w := c.lookalikeWarning(o.To); w != "" {
			r["warning"] = w
		}
		if c.relay != "" {
			r["via"] = c.relay
			r["note"] = "queued at the relay; read " + id[:18] + " shows when it is on chain (and its batch root); inbox alerts if it never arrives"
		}
		out(r)
		return nil
	case "ask-owner":
		c, err := client()
		if err != nil {
			return err
		}
		if c.isOwner() {
			return errors.New("ask-owner is for agents, and you are an owner: your agents ask you (inbox shows their asks first)")
		}
		r, err := c.askOwner(*subject, strings.Join(pos, " "))
		if err != nil {
			return err
		}
		out(r)
		return nil
	case "contact":
		c, err := client()
		if err != nil {
			return err
		}
		n, err := arg(0, "NAME")
		if err != nil {
			return err
		}
		if !c.isOwner() { // an agent's correspondents come from its owner, signed: its own tools can't widen them
			return fmt.Errorf("an agent can't add its own contacts (a message could talk it into that); ask your owner, who runs: "+
				"ternchain owner contact --dir %s %s %s", c.ownerDirWords(), c.f.Name, n)
		}
		if err := c.setContact(n, !*off); err != nil {
			return err
		}
		out(map[string]any{"contacts": c.st.Contacts})
		return nil

	case "group create", "group join", "group post", "group read", "group admit", "group mod", "group vote", "group tally", "group members":
		c, err := client()
		if err != nil {
			return err
		}
		if cmd == "group admit" {
			done, err := c.admitPending()
			if err != nil {
				return err
			}
			r := map[string]any{"admitted": done, "members": c.adminGroupMembers()}
			if len(done) == 0 {
				r["note"] = "nobody waiting (public groups let people in when they join); list a group with group members NAME"
			}
			out(r)
			return nil
		}
		g, err := arg(0, "NAME")
		if err != nil {
			return err
		}
		switch cmd {
		case "group create":
			if err := mixedScriptErr(g, *forceF); err != nil {
				return err
			}
			if err := c.createGroup(g, !*private); err != nil {
				return err
			}
			r := map[string]any{"group": g, "public": !*private}
			if *private {
				r["note"] = "members only: people ask with group join, you let them in with group admit (it sends them the keys)"
			} else {
				r["note"] = "public: anyone can join and read it. For a members-only group create another name with --private (names can't be reused)"
			}
			out(r)
		case "group members":
			ms, err := c.setMembers(g)
			if err != nil {
				return err
			}
			if ms == nil {
				ms = []string{}
			}
			out(map[string]any{"group": g, "members": ms})
		case "group join":
			msg, err := c.joinGroup(g)
			if err != nil {
				return err
			}
			out(map[string]any{"group": g, "status": msg})
		case "group post":
			if strings.TrimSpace(strings.Join(pos[1:], " ")) == "" {
				return errors.New("usage: group post NAME TEXT (or --text-file FILE, - for stdin): the post is empty")
			}
			var data json.RawMessage
			if *attach != "" { // a small file inside the post, sealed like it; readers keep it with read ID --save FILE
				if data, err = attachFile(*attach); err != nil {
					return err
				}
			}
			if *anon {
				id, err := c.postAnon(g, *thread, strings.Join(pos[1:], " "), data)
				if err != nil {
					return err
				}
				out(map[string]any{"posted": id, "group": g, "as": "an anonymous member"})
				return nil
			}
			id, err := c.postGroup(g, strings.Join(pos[1:], " "), data)
			if err != nil {
				return err
			}
			v := map[string]any{"posted": id, "group": g}
			if data != nil {
				v["attachment"] = filepath.Base(*attach) + ": members keep it with 'read " + id[:18] + " --save FILE'"
			}
			out(v)
		case "group vote":
			opt, err := arg(1, "OPTION")
			if err != nil {
				return err
			}
			id, err := c.postAnon(g, *thread, "vote", json.RawMessage(`{"vote":`+strconv.Quote(opt)+`}`))
			if err != nil {
				return err
			}
			out(map[string]any{"voted": opt, "group": g, "thread": *thread, "envelope": id})
		case "group tally":
			if _, err := c.scan(); err != nil {
				return err
			}
			out(map[string]any{"group": g, "thread": *thread, "votes": c.tally(g, *thread)})
		case "group read":
			if _, err := c.scan(); err != nil {
				return err
			}
			c.refreshCommunity(g)
			var lines []map[string]any
			for _, it := range c.groupItems(g) {
				if v, ok := c.blockedView(it, *all); ok {
					lines = append(lines, v)
				}
			}
			out(map[string]any{"group": g, "posts": lines})
		case "group mod":
			action, err := arg(1, "hide|ban|unban|mod|unmod")
			if err != nil {
				return err
			}
			target, err := arg(2, "TARGET")
			if err != nil {
				return err
			}
			if err := c.moderate(g, action, target, strings.Join(pos[min(3, len(pos)):], " ")); err != nil {
				return err
			}
			out(map[string]any{"group": g, action: target})
		}
		return nil

	case "bank deposit", "bank balance", "bank pay":
		c, err := client()
		if err != nil {
			return err
		}
		switch cmd {
		case "bank deposit":
			a, err := parseCoins(firstOr(pos, 0, ""))
			if err != nil {
				return err
			}
			if err := c.deposit(a); err != nil {
				return err
			}
		case "bank pay":
			t, err := arg(0, "TO")
			if err != nil {
				return err
			}
			a, err := parseCoins(firstOr(pos, 1, ""))
			if err != nil {
				return err
			}
			r, err := c.payReceipt(t, a, strings.Join(pos[min(2, len(pos)):], " "), *again)
			if err != nil {
				return err
			}
			out(r)
			return nil
		}
		who := firstOr(pos, 0, c.f.Owner)
		if cmd != "bank balance" {
			who = c.f.Owner
		}
		b, err := c.bankBalance(who)
		if err != nil {
			return err
		}
		out(map[string]any{"owner": who, "bank_coins": fmtCoins(b)})
		return nil
	case "attest":
		c, err := client()
		if err != nil {
			return err
		}
		r, err := runAttest(c, pos, *lockS, *scoreS)
		if err != nil {
			return err
		}
		out(r)
		return nil
	case "feed":
		c, err := client()
		if err != nil {
			return err
		}
		v, err := c.feed(feedOpts{community: *communityF, depth: *trustDepthF, ranking: *rankingF, limit: int(*maxN)})
		if err != nil {
			return err
		}
		out(v)
		return nil
	case "rating":
		c, err := client()
		if err != nil {
			return err
		}
		s, err := arg(0, "NAME")
		if err != nil {
			return err
		}
		window := 0
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "days" {
				window = *days
			}
		})
		tb := 20 * time.Second // time-boxed: a cold one-shot CLI used to read the network's history for minutes
		if *timeout > 0 {
			tb = time.Duration(*timeout) * time.Second
		}
		r, err := c.ratingTimed(s, ratingOpts{trust: parseTrust(*trust), days: window}, tb, *freshF)
		if err != nil {
			return err
		}
		out(r)
		return nil
	case "attestations":
		c, err := client()
		if err != nil {
			return err
		}
		s, err := arg(0, "SUBJECT")
		if err != nil {
			return err
		}
		var tr map[string]bool
		if *trust != "" {
			tr = map[string]bool{}
			for _, x := range strings.Split(*trust, ",") {
				tr[strings.TrimSpace(x)] = true
			}
		}
		as, err := c.attestationsAbout(s, tr)
		if err != nil {
			return err
		}
		out(as)
		return nil
	case "fiat":
		c, err := client()
		if err != nil {
			return err
		}
		sub, _ := arg(0, "paid|request|confirm|deny|list")
		switch sub {
		case "request", "claim", "invoice": // the payee's side: "you owe me"
			if len(pos) < 4 {
				return errors.New("usage (payee): fiat request PAYER AMOUNT CURRENCY [METHOD] [REF] [MEMO…] [--deal SUBJECT]")
			}
			p := FiatPayment{To: pos[1], Amount: pos[2], Currency: pos[3], Method: firstOr(pos, 4, ""), Ref: firstOr(pos, 5, ""), Deal: *dealF}
			if len(pos) > 6 {
				p.Memo = strings.Join(pos[6:], " ")
			}
			uid, err := c.fiatRequest(p)
			if err != nil {
				return err
			}
			out(map[string]any{"request": uid.Hex(), "payer": p.To, "next": fmt.Sprintf("%s pays outside the network, then 'fiat paid %s %s %s METHOD %s'; you then 'fiat confirm CLAIM-UID' (fiat list shows it)",
				p.To, c.f.Name, p.Amount, p.Currency, p.Ref)})
		case "paid":
			if len(pos) < 5 {
				return errors.New("usage: fiat paid TO AMOUNT CURRENCY METHOD [REF] [MEMO…]")
			}
			p := FiatPayment{To: pos[1], Amount: pos[2], Currency: pos[3], Method: pos[4]}
			if len(pos) > 5 {
				p.Ref = pos[5]
			}
			if len(pos) > 6 {
				p.Memo = strings.Join(pos[6:], " ")
			}
			p.Deal = *dealF
			uid, err := c.fiatPaid(p)
			if err != nil {
				return err
			}
			out(map[string]any{"claim": uid.Hex(), "next": p.To + " confirms with: fiat confirm " + uid.Hex()})
		case "confirm", "deny":
			if len(pos) < 2 {
				return errors.New("usage: fiat confirm|deny CLAIM-UID [NOTE…]")
			}
			uid, err := c.fiatAnswer(common.HexToHash(pos[1]), sub == "confirm", strings.Join(pos[2:], " "))
			if err != nil {
				return err
			}
			out(map[string]any{"answer": uid.Hex(), sub: pos[1]})
		case "list":
			l, err := c.fiatList(firstOr(pos, 1, ""))
			if err != nil {
				return err
			}
			out(l)
		default:
			return errors.New("usage: fiat request (payee: you owe me) | fiat paid (payer: I paid you) | fiat confirm|deny UID (payee: it arrived, or not) | fiat list [NAME]")
		}
		return nil
	case "like", "mutual":
		c, err := client()
		if err != nil {
			return err
		}
		if len(pos) < 2 {
			return errors.New("usage: like|mutual OTHER TOPIC")
		}
		if cmd == "like" {
			if err := c.like(pos[0], pos[1]); err != nil {
				return err
			}
			out(map[string]any{"liked": "marker published; nobody can tell for whom"})
			return nil
		}
		m, err := c.mutual(pos[0], pos[1])
		if err != nil {
			return err
		}
		out(map[string]any{"mutual": m, "other": pos[0], "topic": pos[1]})
		return nil
	case "reveals":
		c, err := client()
		if err != nil {
			return err
		}
		if len(pos) < 1 {
			return errors.New("usage: reveals TOPIC")
		}
		list, err := c.commitments(pos[0])
		if err != nil {
			return err
		}
		out(map[string]any{"topic": pos[0], "commitments": list})
		return nil
	case "commit", "reveal":
		c, err := client()
		if err != nil {
			return err
		}
		if len(pos) < 1 || (cmd == "commit" && len(pos) < 2) {
			return errors.New("usage: commit TOPIC VALUE [--days N | --minutes N] | reveal ID [PREIMAGE-HEX]")
		}
		if cmd == "commit" {
			h, err := ch.b.HeaderByNumber(context.Background(), nil)
			if err != nil {
				return err
			}
			// The preimage stays with the identity, so "reveal ID" works later.
			id, _, err := c.commitValueKept(pos[0], strings.Join(pos[1:], " "), commitDeadline(h.Time, *days, *minutes))
			if err != nil {
				return err
			}
			out(map[string]any{"commitment": id, "preimage": "kept in your identity dir (never printed): reveal with 'reveal " + fmt.Sprint(id) + "'"})
			return nil
		}
		ph := firstOr(pos, 1, c.st.Secrets[pos[0]])
		if ph == "" {
			return errors.New("no preimage for that commitment here: pass it as the second argument")
		}
		pre, err := hex.DecodeString(ph)
		if err != nil {
			return err
		}
		var id uint64
		fmt.Sscan(pos[0], &id)
		if err := c.revealValue(id, pre); err != nil {
			return err
		}
		out(map[string]any{"revealed": id})
		return nil
	case "card":
		c, err := client()
		if err != nil {
			return err
		}
		if err := c.setCard(strings.Join(pos, " ")); err != nil {
			return err
		}
		out(map[string]any{"card": "set"})
		return nil
	case "request":
		c, err := client()
		if err != nil {
			return err
		}
		svc, err := arg(0, "SERVICE")
		if err != nil {
			return err
		}
		if len(pos) < 2 {
			return errors.New("usage: ternchain request --dir D SERVICE TEXT [--budget COINS]  (what you need, in words)")
		}
		budget := *budgetS
		if budget == "0" {
			budget = ""
		}
		id, info, err := c.request(svc, WorkRequest{What: strings.Join(pos[1:], " "), Budget: budget})
		if err != nil {
			return err
		}
		info["sent"], info["to"] = id, svc
		info["next"] = "its answer (an offer) comes to your inbox as a reply: inbox, then read ID"
		out(info)
		return nil
	case "find":
		c, err := client()
		if err != nil {
			return err
		}
		f, err := c.find(strings.Join(pos, " "))
		if err != nil {
			return err
		}
		out(f)
		return nil
	case "mcp":
		longLived = true
		c, err := client()
		if err != nil {
			return err
		}
		return runMCP(c, os.Stdin, os.Stdout)
	case "gas fund", "gas budget", "gas show", "account show":
		c, err := client()
		if err != nil {
			return err
		}
		switch cmd {
		case "gas fund":
			n, err := arg(0, "OWNER")
			if err != nil {
				return err
			}
			amt, err := parseCoins(firstOr(pos, 1, ""))
			if err != nil {
				return err
			}
			if err := c.fundGas(n, amt); err != nil {
				return err
			}
			out(map[string]any{"funded": n, "coins": pos[1], "for": "gas of the owner's accounts and its agents' (within their budgets)"})
		case "gas budget":
			n, err := arg(0, "AGENT")
			if err != nil {
				return err
			}
			amt, err := parseCoins(firstOr(pos, 1, ""))
			if err != nil {
				return err
			}
			if err := c.setGasBudget(n, amt); err != nil {
				return err
			}
			out(map[string]any{"agent": n, "gas_per_epoch": pos[1]})
		case "gas show":
			n := firstOr(pos, 0, c.f.Name)
			o := map[string]any{"name": n}
			if d, err := ch.call("TernPaymaster", "deposit", idOf(ownerName(n))); err == nil {
				o["owner_deposit"] = fmtCoins(d[0].(*big.Int))
			}
			if n != ownerName(n) {
				e, _ := ch.epoch()
				b, err1 := ch.call("TernPaymaster", "gasBudget", idOf(n))
				sp, err2 := ch.call("TernPaymaster", "gasSpent", idOf(n), e)
				if err1 == nil && err2 == nil && b[0].(*big.Int).Sign() == 0 {
					o["budget_per_epoch"] = "none: no paymaster budget (an agent without --account pays its own gas)"
				} else if err1 == nil && err2 == nil {
					budget, spent := b[0].(*big.Int), sp[0].(*big.Int)
					left := new(big.Int).Sub(budget, spent)
					if left.Sign() < 0 {
						left.SetInt64(0)
					}
					fee := ch.maxFeeNow()
					reserve := opReserve(sessionVerify, typicalCallGas, fee) // a typical operation's maximum cost
					used := new(big.Int).Mul(big.NewInt(typicalUsedGas), new(big.Int).SetUint64(fee))
					ops := 0
					if left.Cmp(reserve) >= 0 { // the last one needs its whole reserve free; the others cost what they use
						ops = 1 + int(new(big.Int).Div(new(big.Int).Sub(left, reserve), used).Int64())
					}
					o["budget_per_epoch"], o["spent_this_epoch"], o["left_this_epoch"] = fmtCoins(budget), fmtCoins(spent), fmtCoins(left)
					o["gas_price_now"] = fmt.Sprintf("%.3f gwei (offered: tip + 2 x base fee)", float64(fee)/1e9)
					o["reserved_per_operation"] = fmtCoins(reserve)
					o["operations_left_about"] = ops
					o["note"] = "each operation first reserves its maximum cost from the budget and gets back what it did not use, so an operation " +
						"needs about " + fmtCoins(reserve) + " free even though it costs about " + fmtCoins(used) + "; when less is left, the agent waits for the next epoch"
				}
			}
			out(o)
		case "account show":
			o := map[string]any{"name": c.f.Name, "key": c.addr().Hex()}
			if c.f.Account == "" {
				o["account"] = "none: the key itself holds the name (pays its own gas)"
			} else {
				code, _ := ch.b.CodeAt(ctxb(), c.me(), nil)
				o["account"], o["deployed"] = c.f.Account, len(code) > 0
				if c.isOwner() {
					o["guard"], o["key_role"] = "passkey (WebAuthn P-256)", "mail key: signs messages only"
				} else {
					o["guard"], o["key_role"] = "session key", "this key, through the account; gas from the owner's paymaster budget"
				}
				if b, err := ch.balance(c.me()); err == nil {
					o["account_coins"] = fmtCoins(b)
				}
			}
			out(o)
		}
		return nil
	case "da prove-missed", "da slash-missed":
		c, err := client()
		if err != nil {
			return err
		}
		id, err := arg(0, "MSGID (an envelope you sent through a data node)")
		if err != nil {
			return err
		}
		var r map[string]any
		if cmd == "da prove-missed" {
			r, err = c.proveMissed(id)
		} else {
			r, err = c.slashMissed(id)
		}
		if err != nil {
			return err
		}
		out(r)
		return nil
	case "da status", "da challenge", "da expire":
		c, err := client()
		if err != nil {
			return err
		}
		root := common.HexToHash(firstOr(pos, 0, ""))
		switch cmd {
		case "da challenge":
			bond, err := ch.call("Inbox", "challengeBond")
			if err != nil {
				return err
			}
			if _, err := ch.send(c.key, bond[0].(*big.Int), "Inbox", "challenge", root); err != nil {
				return err
			}
		case "da expire":
			if _, err := ch.send(c.key, nil, "Inbox", "expire", root); err != nil {
				return err
			}
		}
		st, err := ch.call("Inbox", "commitment", root)
		if err != nil {
			return err
		}
		state := map[uint8]string{0: "unknown", 1: "committed", 2: "challenged: the poster must put it on chain", 3: "resolved: on chain", 4: "lost: the poster's stake went to the challenger"}[st[2].(uint8)]
		out(map[string]any{"root": root.Hex(), "poster": st[0].(common.Address).Hex(), "committed_at": st[1].(uint64), "state": state})
		return nil
	case "timeline":
		c, err := client()
		if err != nil {
			return err
		}
		var from, to int64
		fmt.Sscan(firstOr(pos, 0, "0"), &from)
		fmt.Sscan(firstOr(pos, 1, "99999999999999"), &to)
		evs, err := c.timeline(from, to)
		if err != nil {
			return err
		}
		out(evs)
		return nil
	case "watch":
		longLived = true
		c, err := client()
		if err != nil {
			return err
		}
		ts, o := *types, *once
		if *until != "" { // --until mail|lock|any: wait for one such event, then return
			m := map[string]string{"mail": "mail,group-post,ask,network-news", "lock": "lock-opened,lock-moved,decision,paid,refund-available,disputed,claimable", "any": ""}
			t, ok := m[*until]
			if !ok {
				return fmt.Errorf("--until %q: mail, lock or any", *until)
			}
			ts, o = t, true
		}
		return c.watch(watchOpts{exec: *execCmd, once: o, timeout: *timeout, types: ts, groups: *waitGroup, allGroups: *all})
	}
	return unknownCommand(cmd)
}

// faucetKey: the faucet key, if this machine has the operator's file.
func faucetKey(df *DevnetFile) string {
	if df == nil {
		return ""
	}
	return df.Faucet
}

const testNotice = "A test network for invited people. Coins are test coins: they have no value, can't be bought, sold or exchanged, " +
	"and are deleted when the network is reset. Nothing agreed here is a real contract or purchase; never send real money for anything here. " +
	"Mail is encrypted, but the operator sees who sends how much and when; public posts and reviews are visible to everyone; " +
	"what your AI agent reads goes to its AI provider."

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func firstOr(xs []string, i int, d string) string {
	if len(xs) > i {
		return xs[i]
	}
	return d
}

func mustCoins(s string) *big.Int {
	v, err := parseCoins(s)
	if err != nil {
		panic(err)
	}
	return v
}

func keysOf[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// commitDeadline: --minutes wins over --days, for short games.
func commitDeadline(now uint64, days, minutes int) uint64 {
	if minutes > 0 {
		return now + uint64(minutes)*60
	}
	return now + uint64(days)*86400
}

// textArgs: commands that take a text, and how many arguments come before it.
var textArgs = map[string]int{"send": 0, "reply": 1, "ask-owner": 0, "group post": 1, "panel argue": 1, "commons rules": 1}

// textFromFile: --text-file FILE (- for stdin) gives the text, appended as
// the command's last argument.
func textFromFile(cmd string, pos *[]string, path string) error {
	n, ok := textArgs[cmd]
	if !ok {
		return fmt.Errorf("%s takes no text: --text-file is for send, reply, ask-owner, group post, panel argue and commons rules", cmd)
	}
	if len(*pos) != n {
		return fmt.Errorf("with --text-file, %s takes %d argument(s) before the text, and the text only from the file", cmd, n)
	}
	var b []byte
	var err error
	if path == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return err
	}
	*pos = append(*pos, strings.TrimRight(string(b), "\n"))
	return nil
}

// intentAlias accepts the words agents reach for.
func intentAlias(i string) string {
	switch i {
	case "reply":
		return "answer"
	case "propose", "proposal":
		return "offer"
	case "info":
		return "inform"
	}
	return i
}
