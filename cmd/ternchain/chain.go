// The chain: a connection, the five contracts, transactions and event
// queries. Works against any EVM JSON-RPC endpoint, and against an
// in-process simulated chain for tests and the local devnet.
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern"
	"github.com/nenkoru/tern/internal/zk"
)

var artifacts = tern.Build

// extArtifacts: contracts for networks on a host chain (contracts/ext:
// Base Sepolia's paymaster, ERC-8183 jobs, test USDC), kept out of the core
// set so a running network's contracts hash stays what it was (version.go).
var extArtifacts = tern.BuildExt

// basketSpec is the reference basket fees are priced in (its hash goes on chain).
var basketSpec = tern.Basket

const basketURI = "BASKET.md v1: 1 kWh electricity + 1 GB-month storage + 1 vCPU-hour"

// envelopeKind is the FeeOracle kind the Inbox charges per envelope.
var envelopeKind = crypto.Keccak256Hash([]byte("envelope"))

// The network's contracts. Standards where they exist: EAS for attestations,
// ERC-5564/6538 for stealth payments, OpenZeppelin Governor and Timelock for
// governance. Directory is the root: clients need only its address.
var contractNames = []string{"Registry", "Inbox", "Sets", "Artifacts", "Commitments", "SchemaRegistry", "EAS", "Bank",
	"AttestationCondition", "VoucherCondition", "RateCondition", "RevealCondition", "Random", "Clock", "Items", "PanelCondition", "HashlockCondition", "PledgeCondition", "OptimisticCondition", "RulingCondition", "SwapCondition", "TestUSD", "EntryPoint", "AccountFactory", "TernPaymaster", "ERC5564Announcer", "ERC6538Registry", "StealthPay",
	"WrappedTern", "TernTimelock", "TernGovernor", "Directory", "FeeOracle", "Commons", "Citizens", "Parliament", "Treasury", "Validators",
	"Poseidon2Hasher", "VoteVerifier", "CitizenGroup", "PoolVerifier", "AssociationSets", "PrivacyPool", "GroupTrees", "SchemaNames", "CitizenJoinRule"}

// claimSchema is the one EAS schema Tern clients use for claims about anything.
const claimSchema = "bytes32 subject,string kind,bytes data,int64 score"

// moderationSchema: the moderation-claim standard (contracts/Community.sol
// MODERATION_SCHEMA): hide, ban, unban, unhide in a community of any ICommunity contract.
const moderationSchema = "address community,bytes32 id,uint8 action,bytes32 target,bytes32 by,string reason"

func schemaUID(schema string) common.Hash {
	return crypto.Keccak256Hash([]byte(schema), common.Address{}.Bytes(), []byte{1}) // resolver 0, revocable
}

type contractBuild struct {
	abi abi.ABI
	bin []byte
}

func loadArtifact(name string) (contractBuild, error) {
	fsys, dir := artifacts, "build/"
	if _, err := artifacts.ReadFile(dir + name + "_sol_" + name + ".abi"); err != nil {
		fsys, dir = extArtifacts, "build/ext/"
	}
	a, err := fsys.ReadFile(dir + name + "_sol_" + name + ".abi")
	if err != nil {
		return contractBuild{}, err
	}
	b, err := fsys.ReadFile(dir + name + "_sol_" + name + ".bin")
	if err != nil {
		return contractBuild{}, err
	}
	parsed, err := abi.JSON(strings.NewReader(string(a)))
	if err != nil {
		return contractBuild{}, err
	}
	return contractBuild{parsed, common.FromHex(strings.TrimSpace(string(b)))}, nil
}

// Backend is what we need from a chain connection.
type Backend interface {
	bind.ContractBackend
	bind.DeployBackend
	ethereum.ChainIDReader
	ethereum.BlockNumberReader
	ethereum.ChainStateReader
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	// the transaction behind a log and its block (keypin.go: who sent it)
	TransactionByHash(ctx context.Context, hash common.Hash) (*types.Transaction, bool, error)
	BlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error)
}

// Deployment is where the network's contracts live (written by `ternchain deploy`).
type Deployment struct {
	RPC       string            `json:"rpc"`
	ChainID   int64             `json:"chain_id"`
	Contracts map[string]string `json:"contracts"`
	Block     uint64            `json:"block"`  // first block to scan
	Schema    string            `json:"schema"` // the EAS claim schema uid
	// BucketBits: tag buckets (bucket.go): envelopes carry the top BucketBits
	// bits of their recipient's bucket; readers fetch their buckets from home
	// data nodes instead of every batch. 0: none (every reader reads everything).
	BucketBits int `json:"bucket_bits,omitempty"`
	// Ceremony: the zk keys' setup, the hash of a ternchain ceremony's final
	// transcript, which the verifiers carry as CEREMONY; empty: the devnet keys
	// (ceremony.go). ZKKeys: its keys directory, relative to net.json (default zk-keys).
	Ceremony string `json:"ceremony,omitempty"`
	ZKKeys   string `json:"zk_keys,omitempty"`
}

type Chain struct {
	b        Backend
	chainID  *big.Int
	c        map[string]*bind.BoundContract
	abis     map[string]abi.ABI
	dep      Deployment
	mine     func()     // simulated chains: produce a block now
	mu       sync.Mutex // the bundler's user operations
	senders  sync.Map   // signer address => *sync.Mutex: one key's transactions in nonce order, other keys in parallel
	ccOnce   sync.Once
	cc       *claimCache // claims read so far (eas.go)
	times    sync.Map    // block number => its time (headers don't change)
	aiOnce   sync.Once
	ai       *addrIndex   // address => names held (resolve.go)
	staked   atomic.Value // *stakedAt: data nodes' URLs, read lately (da.go)
	head     atomic.Value // *headAt: the head lately (headNumber)
	nowT     atomic.Value // *headAt: the head's time lately (chainNow)
	fixed    sync.Map     // contract address + method => immutable value (callFixed)
	evc      sync.Map     // "Contract.Event" => *evCache (events)
	leaves   sync.Map     // batch root => its leaf root on chain (bucket.go; never changes once set)
	reversed sync.Map     // Commons action id => revAt (reports.go)
	// cacheDir: where a one-shot process keeps what never changes (claims) for its next run ("": nowhere)
	cacheDir string
	routes   map[common.Address]*AccountRoute // signing key => its smart account (sends become user operations)
	bundler  *ecdsa.PrivateKey                // submits user operations (any node runner can)
	// bundlerURL: the network's ERC-4337 bundler (net.json "bundler"), used
	// when this process has no bundler key of its own.
	bundlerURL string
	hosted     bool          // the contracts live on a chain someone else runs (net.json "host"): its fees, not ours
	settle     *Settlement   // the token payments are in by default (net.json "settle"); nil: the coin
	gasUsed    atomic.Uint64 // gas of the transactions this process waited for (deploy reports it)
	gas        *GasConfig    // who pays for gas (net.json "gas"; gas.go)
	caps       *ValueCaps    // what one send may put in (net.json "caps"; mainnet.go)
	decimals   sync.Map      // token => its decimals (units.go)
	domains    sync.Map      // token => its EIP-712 name and version (units.go tokenDomain)
	zkOK       sync.Map      // circuit/keys => its deployed verifier is from the keys' setup (ceremony.go)
	// witness: other RPCs of the network that confirm a first key pin or a pin
	// change (keypin.go), dialed from witnessURLs when first needed (tests set it)
	witness      []Backend
	witnessURLs  []string
	witnessOnce  sync.Once
	verifyBlocks bool // net.json "verify_blocks" (TERNCHAIN_VERIFY_BLOCKS=1): pinned logs' blocks checked too (keypin.go)
	// unsafePaymaster: --unsafe-paymaster, deposits into TernPaymaster
	// allowed (a local devnet only; accounts.go paymasterFundOK)
	unsafePaymaster bool
}

// abiOnly: contracts the client talks to at many addresses (accounts).
var abiOnly = []string{"OwnerAccount", "AgentAccount"}

func newChain(b Backend, dep Deployment, mine func()) (*Chain, error) {
	id, err := b.ChainID(context.Background())
	if err != nil {
		return nil, err
	}
	ch := &Chain{b: b, chainID: id, c: map[string]*bind.BoundContract{}, abis: map[string]abi.ABI{}, dep: dep, mine: mine,
		routes: map[common.Address]*AccountRoute{}}
	for _, n := range abiOnly {
		a, err := loadArtifact(n)
		if err != nil {
			return nil, err
		}
		ch.abis[n] = a.abi
	}
	for _, n := range append(append([]string{}, contractNames...), extContractNames()...) {
		a, err := loadArtifact(n)
		if err != nil {
			return nil, err
		}
		ch.abis[n] = a.abi
		if addr, ok := dep.Contracts[n]; ok {
			ch.c[n] = bind.NewBoundContract(common.HexToAddress(addr), a.abi, b, b, b)
		}
	}
	return ch, nil
}

// extContractNames: the contracts/ext set (build/ext), bound like the core
// ones when net.json names them (a host chain's paymaster, test USDC).
func extContractNames() []string {
	var out []string
	es, _ := extArtifacts.ReadDir("build/ext")
	for _, e := range es {
		if n, ok := strings.CutSuffix(e.Name(), ".abi"); ok {
			out = append(out, n[:strings.Index(n, "_sol_")])
		}
	}
	return out
}

// Params are the network's constants, fixed at deployment.
type Params struct {
	EpochLength   uint64   // seconds
	RecoveryDelay uint64   // seconds
	Fee           *big.Int // wei per envelope at launch (the oracle's basket price is set from it)
	MinFee        *big.Int // the Inbox's floor per envelope (default Fee/10)
	EnvelopePPM   uint64   // an envelope's target, in millionths of a basket
	PriceRound    uint64   // seconds per price round (commit, then reveal)
	PriceMinBond  *big.Int // smallest bond for a price vote
	PriceQuorum   *big.Int // revealed bond needed to move the price
	PriceStepBps  uint16   // the most a round moves the price
	LoadPerBlock  uint64   // envelopes a block should carry before fees rise
	MaxMultiplier *big.Int // congestion cap, 1e18 = 1×
	Treasury      common.Address
	VotingDelay   uint64   // seconds between a proposal and its coin vote
	VotingPeriod  uint64   // seconds both houses vote
	ExitWindow    uint64   // seconds between a passed vote and its effect (timelock)
	Threshold     *big.Int // wTERN a coin holder needs to propose (citizens always may)
	QuorumPercent uint64   // coin house quorum, % of all wTERN
	Gov           GovParams
	DAStake       *big.Int // a data node's stake
	DABond        *big.Int // a challenger's bond
	DAChallenge   uint64   // seconds to challenge a hash
	DAResolve     uint64   // seconds to put a challenged batch on chain
	// Validators: a QBFT chain's validator set (in its genesis); deploy hands
	// it to the timelock and lists it in the Directory. Zero: none (the
	// devnet, or a header-voting QBFT chain: validator genesis --selection header).
	Validators common.Address
	// KeepValidators: deploy lists Validators but doesn't hand it over (the
	// deployer stays its bootstrap). Required with fewer than 4 founders: a
	// Parliament without citizens can never pass anything, and the set would
	// be frozen at genesis for good (audit 6 VB-1).
	KeepValidators bool
	// Devnet: test-only contracts (TestUSD, anyone mints) are deployed. They
	// always are on the devnet chain ids (zk.DevnetChainIDs: 1337, 31337), never otherwise
	// unless this is set.
	Devnet bool
	// TestNetwork: an operator-run test network (net.json "test_network"):
	// zk keys from a one-party ceremony are allowed (ceremony solo); never on
	// the mainnet chain id.
	TestNetwork bool
	// SoloZK: the public set on a host chain with real money, its secret-ballot
	// keys from the deployer alone (deploy --solo-zk; net.json zk_solo and the
	// notice say so). The public set has no shielded pool, and with no
	// citizens nobody can propose in Parliament, so forged ballots can't
	// move the treasury or the contracts: only groups' own secret ballots.
	SoloZK bool
	// EntryPoint: an ERC-4337 EntryPoint v0.8 already on the chain (the
	// canonical one on a host chain such as Base Sepolia); zero: deploy the
	// vendored one.
	EntryPoint common.Address
	// Resume: what an interrupted deploy already did (deploy --resume): the
	// contracts it put on chain (reused when their code is there) and the
	// steps it finished. Progress gets the state after every step.
	Resume   *DeployProgress
	Progress func(DeployProgress)
	// Sponsor: a SponsorPaymaster (contracts/ext) pays smart accounts' gas
	// within daily caps, so testers need no ETH (a host chain's network);
	// the deployer is its operator and funds it with Deposit.
	Sponsor *SponsorParams
	// ERC8004: "official" names the registries already on the chain (Base
	// Sepolia's); "deploy" deploys look-alikes (contracts/ext; devnets do).
	ERC8004 string
	// Profile "public": the network a public launch ships (docs/base-sepolia.md):
	// no shielded pool or stealth payments, no coin house (NoCoinHouse: the
	// citizens' house decides alone), mail on chain through InboxForwarder at
	// the fee's floor, and a permissionless ModuleRegistry for what anyone
	// adds later. "": every contract (devnets, tern-testnet-1).
	Profile string
	// ForwarderSeed: wei InboxForwarder starts with (the public profile; it pays 1 wei an envelope).
	ForwarderSeed *big.Int
	// GasPaymaster: users pay their own gas in a token through this
	// ERC-20 paymaster: "circle" (Circle Paymaster, Base Sepolia), "test" (a
	// devnet's look-alike, deployed here), or an address; "": not set up.
	GasPaymaster string
	// JobsToken: ERC-8183 jobs (TernJobs, contracts/ext) in this token (the
	// network's settlement token; a devnet's TestUSDC); zero: none.
	JobsToken common.Address
}

// SponsorParams: the sponsoring paymaster's caps (wei of gas a day) and first deposit.
type SponsorParams struct {
	PerName, PerDay, Deposit *big.Int
}

// DeployProgress: an unfinished deploy's state (NET.deploy.json beside net.json).
type DeployProgress struct {
	Contracts map[string]string `json:"contracts"`
	Block     uint64            `json:"block"`
	Done      []string          `json:"done"`
}

// GovParams: the two houses. Launch values by default; Short (devnets) has
// minutes where launch has days, so a vote can be tried in one sitting.
type GovParams struct {
	Genesis          []GenesisCitizen // founding citizens, each claimed by its own key
	FirstTerm        uint64           // seconds before founders must be re-confirmed by K others
	Vouches          uint8            // K
	MaxVouches       uint8            // vouches a citizen may give per VouchTerm
	VouchTerm        uint64
	MinAge           uint64   // N: seconds a name must exist before citizenship
	MinDeals         uint16   // M (needs a deal counter, named later by vote; off at launch)
	Penalty          uint64   // seconds a revoked name, or a sybil's voucher, stays out
	RevokeWindow     uint64   // seconds a revocation vote lasts
	RevokeQuorumBps  uint16   // turnout for a revocation
	RevokeBps        uint16   // yes share to revoke
	CitizenQuorumBps uint16   // turnout in the citizens' house for proposals
	QueueWindow      uint64   // a passed proposal must be queued within this
	QuorumFloor      *big.Int // the coin house's quorum never goes below this (wTERN)
	Short            bool
	// Fixed for the life of the contracts they go into (PROOFREAD-6 #28, #30):
	AdmitBps       uint16 // Citizens: new citizens per term, a share of the citizens (2000)
	AdmitFloor     uint32 // Citizens: ... but at least this many (3)
	CriticalDelayX uint32 // Parliament: a critical proposal waits this many exit windows (3)
	MinTurnout     uint32 // Parliament: the citizens' turnout floor, min(this, half) (5)
	SuperNum       uint8  // Parliament: a critical proposal's supermajority SuperNum/SuperDen (2/3)
	SuperDen       uint8
}

// parliamentRules is Parliament.Rules on chain.
type parliamentRules struct {
	CriticalDelayX uint32
	MinTurnout     uint32
	SuperNum       uint8
	SuperDen       uint8
}

// GenesisCitizen: a founding name and the key that must claim it (a squatter
// who registers the name first gets the name, not the citizenship).
type GenesisCitizen struct {
	Name string
	Key  common.Address
}

// citizenParams is Citizens.Params on chain.
type citizenParams struct {
	Vouches    uint8
	MaxVouches uint8
	QuorumBps  uint16
	RevokeBps  uint16
	MinDeals   uint16
	MinAge     uint64
	VoteWindow uint64
	Penalty    uint64
	Term       uint64
	Deals      common.Address
}

func (g *GovParams) defaults() {
	day := uint64(86400)
	set := func(v *uint64, launch, short uint64) {
		if *v == 0 {
			*v = launch
			if g.Short {
				*v = short
			}
		}
	}
	set(&g.FirstTerm, 90*day, 7*day)
	set(&g.VouchTerm, 90*day, day)
	set(&g.MinAge, 30*day, 600)
	set(&g.Penalty, 180*day, 3600)
	set(&g.RevokeWindow, 7*day, 600)
	set(&g.QueueWindow, 14*day, day)
	if g.Vouches == 0 {
		g.Vouches = 3
	}
	if g.MaxVouches == 0 {
		g.MaxVouches = 5
	}
	if g.AdmitBps == 0 {
		g.AdmitBps = 2000
	}
	if g.AdmitFloor == 0 {
		g.AdmitFloor = 3
	}
	if g.CriticalDelayX == 0 {
		g.CriticalDelayX = 3
	}
	if g.MinTurnout == 0 {
		g.MinTurnout = 5
	}
	if g.SuperNum == 0 || g.SuperDen == 0 {
		g.SuperNum, g.SuperDen = 2, 3
	}
	if g.RevokeQuorumBps == 0 {
		g.RevokeQuorumBps = 2000
	}
	if g.RevokeBps == 0 {
		g.RevokeBps = 6667
	}
	if g.CitizenQuorumBps == 0 {
		g.CitizenQuorumBps = 2000
	}
	if g.QuorumFloor == nil { // the coin house's least quorum: at launch a few wrapped coins must not pass a change (audit 4 GV-6)
		g.QuorumFloor = new(big.Int).Mul(coinUnit, big.NewInt(10_000))
		if g.Short {
			g.QuorumFloor = new(big.Int).Mul(coinUnit, big.NewInt(10))
		}
	}
}

func (p *Params) defaults() {
	if p.Gov.Short {
		if p.VotingPeriod == 0 {
			p.VotingPeriod = 600
		}
		if p.ExitWindow == 0 {
			p.ExitWindow = 600
		}
	}
	p.Gov.defaults()
	if p.Fee == nil || p.Fee.Sign() == 0 {
		p.Fee = big.NewInt(1e12)
	}
	if p.MinFee == nil {
		p.MinFee = new(big.Int).Div(p.Fee, big.NewInt(10))
		if p.MinFee.Sign() == 0 {
			p.MinFee = big.NewInt(1)
		}
	}
	if p.EnvelopePPM == 0 {
		p.EnvelopePPM = 10
	}
	// Prices move slowly and cost a pusher (FO-1): a day per round, at most 10%
	// a round and a day (the oracle also caps the day), and a quorum of revealed
	// bond worth bringing honest voters out for.
	if p.PriceRound == 0 {
		p.PriceRound = 86400
	}
	if p.PriceMinBond == nil {
		p.PriceMinBond = new(big.Int).Div(coinUnit, big.NewInt(100))
	}
	if p.PriceQuorum == nil {
		p.PriceQuorum = new(big.Int).Mul(coinUnit, big.NewInt(100))
	}
	if p.PriceStepBps == 0 {
		p.PriceStepBps = 1000
	}
	if p.LoadPerBlock == 0 {
		p.LoadPerBlock = 64
	}
	if p.MaxMultiplier == nil {
		p.MaxMultiplier = new(big.Int).Mul(coinUnit, big.NewInt(100))
	}
	if p.DAStake == nil {
		p.DAStake = new(big.Int).Set(coinUnit)
	}
	if p.DABond == nil {
		p.DABond = new(big.Int).Div(coinUnit, big.NewInt(10))
	}
	if p.DAChallenge == 0 {
		p.DAChallenge = 3600
	}
	if p.DAResolve == 0 {
		p.DAResolve = 1800
	}
	if p.VotingDelay == 0 { // launch: two days to see a proposal coming and wrap coins to vote (audit 4 GV-6)
		p.VotingDelay = 2 * 86400
		if p.Gov.Short {
			p.VotingDelay = 1
		}
	}
	if p.VotingPeriod == 0 {
		p.VotingPeriod = 7 * 86400
	}
	if p.ExitWindow == 0 {
		p.ExitWindow = 7 * 86400
	}
	if p.Threshold == nil {
		p.Threshold = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	}
	if p.QuorumPercent == 0 {
		p.QuorumPercent = 4
	}
}

func deploy(b Backend, key *ecdsa.PrivateKey, p Params, rpcURL string, mine func()) (*Chain, error) {
	ch, err := newChain(b, Deployment{RPC: rpcURL, Contracts: map[string]string{}}, mine)
	if err != nil {
		return nil, err
	}
	ch.dep.ChainID = ch.chainID.Int64()
	if p.Validators != (common.Address{}) && !p.KeepValidators && len(p.Gov.Genesis) < minHandoverFounders {
		return nil, fmt.Errorf("the chain's validator set would go to a Parliament with %d founders: with fewer than %d nobody can ever become a citizen, "+
			"so the set (and every governed parameter) would be frozen at genesis for good (audit 6 VB-1). Run a test network in header-voting mode "+
			"(validator genesis --selection header), or deploy with --keep-validators (the deployer stays the Validators bootstrap), or name >= %d founders",
			len(p.Gov.Genesis), minHandoverFounders, minHandoverFounders)
	}
	if err := zkDeployGate(ch, p.TestNetwork || p.SoloZK); err != nil { // the devnet zk keys: devnet chain ids only (audit 5 ZK-4)
		return nil, err
	}
	if n, err := b.BlockNumber(context.Background()); err == nil {
		ch.dep.Block = n
	}
	prog := DeployProgress{Contracts: ch.dep.Contracts}
	if p.Resume != nil { // the network starts where the interrupted run began
		ch.dep.Block = p.Resume.Block
		prog.Done = append([]string(nil), p.Resume.Done...)
	}
	prog.Block = ch.dep.Block
	saved := func() {
		if p.Progress != nil {
			p.Progress(prog)
		}
	}
	put := func(name string, args ...any) (common.Address, error) {
		a, err := zkArtifact(name)
		if err != nil {
			return common.Address{}, err
		}
		if p.Resume != nil && p.Resume.Contracts[name] != "" { // put there by the interrupted run
			at := common.HexToAddress(p.Resume.Contracts[name])
			if code, err := b.CodeAt(context.Background(), at, nil); err == nil && len(code) > 0 {
				ch.c[name] = bind.NewBoundContract(at, a.abi, b, b, b)
				ch.dep.Contracts[name] = at.Hex()
				return at, nil
			}
		}
		opts, err := ch.opts(key, nil)
		if err != nil {
			return common.Address{}, err
		}
		addr, tx, bc, err := bind.DeployContract(opts, a.abi, a.bin, b, args...)
		if err != nil {
			return common.Address{}, fmt.Errorf("deploy %s: %w", name, err)
		}
		if _, err := ch.wait(tx); err != nil {
			return common.Address{}, fmt.Errorf("deploy %s: %w", name, err)
		}
		ch.c[name] = bc
		ch.dep.Contracts[name] = addr.Hex()
		saved()
		return addr, nil
	}
	// once: a step that is a transaction, not a contract: skipped when the
	// interrupted run finished it.
	once := func(id string, f func() error) error {
		if slices.Contains(prog.Done, id) {
			return nil
		}
		if err := f(); err != nil {
			return err
		}
		prog.Done = append(prog.Done, id)
		saved()
		return nil
	}
	public := p.Profile == "public"
	if public { // the fee's floor, always: senders pay their gas, not the network's coin (InboxForwarder pays the 1 wei)
		p.Fee, p.MinFee = big.NewInt(1), big.NewInt(1)
	}
	p.defaults()
	A := map[string]common.Address{}
	step := func(name string, args ...any) {
		if err == nil {
			A[name], err = put(name, args...)
		}
	}
	deployer := crypto.PubkeyToAddress(key.PublicKey)
	step("Registry", p.EpochLength, p.RecoveryDelay)
	// The exit window and the Directory come first: the treasury hangs off both.
	step("TernTimelock", new(big.Int).SetUint64(p.ExitWindow), []common.Address{}, []common.Address{{}}, deployer)
	step("Directory", deployer)
	step("Treasury", A["Registry"], A["Directory"], A["TernTimelock"], "tern-treasury")
	if p.Treasury == (common.Address{}) { // fees go to the network's treasury unless told otherwise
		p.Treasury = A["Treasury"]
	}
	// The Inbox prices envelopes with the Directory's current FeeOracle: a vote replaces the mechanism.
	step("Inbox", p.MinFee, p.Treasury, p.DAStake, p.DABond, p.DAChallenge, p.DAResolve, A["Directory"])
	for _, n := range []string{"Sets", "Artifacts", "Commitments"} {
		step(n, A["Registry"])
	}
	step("SchemaRegistry")
	step("EAS", A["SchemaRegistry"])
	step("Bank", A["Registry"], A["Inbox"])
	if err == nil {
		err = once("Inbox.setBank", func() error {
			if _, e := ch.send(key, nil, "Inbox", "setBank", A["Bank"]); e != nil {
				return fmt.Errorf("Inbox.setBank: %w", e)
			}
			return nil
		})
	}
	step("CommonsVotes", A["Registry"], A["Sets"], A["Bank"], A["EAS"]) // elections, recalls, moderation claims: Commons runs this code on its storage
	step("Commons", A["Registry"], A["Sets"], A["Bank"], A["CommonsVotes"])
	step("AttestationCondition", A["EAS"], A["Registry"])
	step("VoucherCondition")
	step("RateCondition", A["Registry"])
	step("RevealCondition", A["Commitments"])
	step("Random", A["Commitments"])
	step("Clock", A["Registry"])
	step("Items", A["Registry"]) // plain ERC-1155: sales are Bank item locks under SwapCondition
	step("PanelCondition", A["EAS"], A["Registry"], A["Bank"])
	step("HashlockCondition", A["Bank"])                            // HTLC: atomic swaps across networks
	step("PledgeCondition", A["Bank"])                              // all or nothing: crowdfunding
	step("OptimisticCondition", A["EAS"], A["Registry"], A["Bank"]) // pay unless disputed
	step("RulingCondition", A["EAS"], A["Registry"], A["Bank"])     // one arbiter or a panel, any aggregation, optional dispute window and bond
	step("SwapCondition", A["Bank"])                                // two locks swapped atomically: items for coins or tokens
	if testTokens(p, ch.chainID.Uint64()) {                         // test money: devnets only (BRICKS #26)
		step("TestUSD")
		step("TestUSDC") // shaped like Circle's USDC (6 decimals, EIP-3009): x402 and USDC settlement on a devnet
		if p.ERC8004 == "" {
			p.ERC8004 = "deploy"
		}
	}
	if public || testTokens(p, ch.chainID.Uint64()) {
		step("ModuleRegistry", A["Registry"]) // modules anyone publishes in their own namespace (no gate)
	}
	if public { // mail on chain without the network's coin: the fee's floor paid from here
		seed := p.ForwarderSeed
		if seed == nil {
			seed = big.NewInt(1e12) // a trillion envelopes at 1 wei
		}
		step("InboxForwarder", A["Inbox"])
		if err == nil && seed.Sign() > 0 {
			err = once("InboxForwarder.seed", func() error { // its receive() logs the gift: more than a plain transfer's gas
				fwd := A["InboxForwarder"]
				_, err := ch.sendRaw(key, seed, &fwd, nil)
				return err
			})
		}
	}
	if p.JobsToken == (common.Address{}) && A["TestUSDC"] != (common.Address{}) {
		p.JobsToken = A["TestUSDC"]
	}
	if p.JobsToken != (common.Address{}) { // ERC-8183 jobs over Bank locks (jobs.go)
		step("TernJobs", p.JobsToken, A["Bank"], A["Registry"])
	}
	switch p.ERC8004 { // ERC-8004 agent identities and reputation (erc8004.go)
	case "deploy":
		step("ERC8004IdentityRegistry")
		step("ERC8004ReputationRegistry", A["ERC8004IdentityRegistry"])
	case "official":
		hp, known := hostByChain(ch.chainID.Int64())
		if !known && err == nil {
			err = fmt.Errorf("--erc8004 official: no known ERC-8004 registries on chain %s (Base and Base Sepolia have them)", ch.chainID)
		}
		for n, a := range map[string]string{"ERC8004IdentityRegistry": hp.ERC8004Identity, "ERC8004ReputationRegistry": hp.ERC8004Reputation} {
			if err != nil {
				break
			}
			if code, e := b.CodeAt(context.Background(), common.HexToAddress(a), nil); e != nil || len(code) == 0 {
				err = fmt.Errorf("--erc8004 official: no registry at %s on this chain", a)
				break
			}
			ea, _ := loadArtifact(n)
			ch.c[n] = bind.NewBoundContract(common.HexToAddress(a), ea.abi, b, b, b)
			ch.dep.Contracts[n] = common.HexToAddress(a).Hex()
			A[n] = common.HexToAddress(a)
		}
	}
	if p.ERC8004 != "" { // smart accounts register through it (the registry mints with _safeMint)
		step("ERC8004Registrar", A["ERC8004IdentityRegistry"])
	}
	step("SchemaNames", A["SchemaRegistry"], A["Registry"]) // names and versions for types (card kinds, intents, reactions)
	step("AnonGroups")                                      // anonymous group membership: trees of identity commitments, no names (contracts/ext, anon_groups.go)
	step("PrivateReviews", A["Bank"], A["Registry"])        // reviews as hidden leaves: reputation without the social graph (reputation.go)
	if p.EntryPoint != (common.Address{}) {                 // the chain's own (canonical) EntryPoint: not ours to deploy
		if err == nil {
			if err = checkEntryPoint(b, p.EntryPoint); err == nil {
				ea, _ := loadArtifact("EntryPoint")
				ch.c["EntryPoint"] = bind.NewBoundContract(p.EntryPoint, ea.abi, b, b, b)
				ch.dep.Contracts["EntryPoint"] = p.EntryPoint.Hex()
				A["EntryPoint"] = p.EntryPoint
			}
		}
	} else {
		step("EntryPoint")
	}
	step("AccountFactory", A["EntryPoint"])
	step("TernPaymaster", A["EntryPoint"], A["Registry"])
	switch p.GasPaymaster {
	case "test":
		step("TestTokenPaymaster", A["EntryPoint"], A["TestUSDC"])
		if err == nil {
			err = once("TestTokenPaymaster.deposit", func() error {
				_, err := ch.send(key, new(big.Int).Mul(coinUnit, big.NewInt(10)), "TestTokenPaymaster", "deposit")
				return err
			})
		}
	case "", "circle":
	default:
		if !common.IsHexAddress(p.GasPaymaster) && err == nil {
			err = fmt.Errorf("--gas-paymaster %q: circle, or an address", p.GasPaymaster)
		}
	}
	if p.Sponsor != nil {
		step("SponsorPaymaster", A["EntryPoint"], A["Registry"], A["AccountFactory"], deployer, p.Sponsor.PerName, p.Sponsor.PerDay)
		if err == nil && p.Sponsor.Deposit != nil && p.Sponsor.Deposit.Sign() > 0 {
			err = once("SponsorPaymaster.deposit", func() error {
				_, err := ch.send(key, p.Sponsor.Deposit, "SponsorPaymaster", "deposit")
				return err
			})
		}
	}
	if !public { // stealth payments (ERC-5564/6538): not in the public profile
		step("ERC5564Announcer")
		step("ERC6538Registry")
		step("StealthPay", A["ERC5564Announcer"])
	}
	// Two houses: coins (wTERN, counted by the Governor) and citizens (one
	// vote each); Parliament needs both, then the timelock's exit window.
	// The public profile has no coin: its coin house abstains (NoCoinHouse).
	coinHouse, coinVotes := "TernGovernor", "WrappedTern"
	if public {
		step("NoCoinHouse", new(big.Int).SetUint64(p.VotingPeriod))
		coinHouse, coinVotes = "NoCoinHouse", "NoCoinHouse"
	} else {
		step("WrappedTern")
		step("TernGovernor", A["WrappedTern"], A["TernTimelock"], new(big.Int).SetUint64(p.VotingDelay), uint32(p.VotingPeriod),
			new(big.Int).SetUint64(p.QuorumPercent), p.Gov.QuorumFloor)
	}
	g := p.Gov
	gids, gkeys := [][32]byte{}, []common.Address{}
	for _, c := range g.Genesis {
		gids, gkeys = append(gids, idOf(c.Name)), append(gkeys, c.Key)
	}
	// Secret votes: Citizens keeps the citizens' tree (CitizenGroup, made by
	// its constructor); proofs are checked by gnark's exported verifier.
	step("Poseidon2Hasher")
	step("VoteVerifier")
	step("GroupTrees", A["Registry"], A["VoteVerifier"], A["Poseidon2Hasher"]) // secret ballots for any group
	step("Citizens", A["Registry"], A["TernTimelock"], gids, gkeys, g.FirstTerm, citizenParams{Vouches: g.Vouches, MaxVouches: g.MaxVouches,
		QuorumBps: g.RevokeQuorumBps, RevokeBps: g.RevokeBps, MinDeals: g.MinDeals, MinAge: g.MinAge, VoteWindow: g.RevokeWindow,
		Penalty: g.Penalty, Term: g.VouchTerm}, A["VoteVerifier"], A["Poseidon2Hasher"], g.AdmitBps, g.AdmitFloor)
	if err == nil {
		if out, e := ch.call("Citizens", "group"); e != nil {
			err = e
		} else {
			A["CitizenGroup"] = out[0].(common.Address)
			ga, _ := loadArtifact("CitizenGroup")
			ch.c["CitizenGroup"] = bind.NewBoundContract(A["CitizenGroup"], ga.abi, b, b, b)
			ch.dep.Contracts["CitizenGroup"] = A["CitizenGroup"].Hex()
		}
	}
	step("CitizenJoinRule", A["Citizens"], A["Registry"]) // a set join rule: citizens only
	// The shielded pool (Privacy Pools) and the public list of association
	// sets: not in the public profile (an optional module anyone may deploy).
	circuits := []string{"vote"}
	if !public {
		step("PoolVerifier")
		step("AssociationSets")
		step("PrivacyPool", A["PoolVerifier"], A["AssociationSets"], A["Bank"], A["ERC5564Announcer"], A["Poseidon2Hasher"])
		circuits = []string{"withdraw", "vote"}
	}
	for _, c := range circuits { // the verifiers on chain are the keys' (CEREMONY)
		if err == nil {
			err = ch.zkCheck(c)
		}
	}
	step("Parliament", A["Registry"], A["Citizens"], A[coinHouse], A["TernTimelock"], A[coinVotes], A["Directory"], A["Treasury"],
		p.Threshold, g.CitizenQuorumBps, g.QueueWindow, parliamentRules{g.CriticalDelayX, g.MinTurnout, g.SuperNum, g.SuperDen})
	// Fees: the basket priced so an envelope costs p.Fee at launch; the launch
	// targets, basket and meter go in the constructor, later changes are the timelock's.
	basket := new(big.Int).Div(new(big.Int).Mul(p.Fee, big.NewInt(1e6)), new(big.Int).SetUint64(p.EnvelopePPM))
	step("FeeOracle", basket, p.PriceRound, p.PriceMinBond, p.PriceStepBps, p.PriceQuorum, A["TernTimelock"], p.Treasury,
		new(big.Int).SetUint64(p.LoadPerBlock), p.MaxMultiplier, [][32]byte{envelopeKind}, []*big.Int{new(big.Int).SetUint64(p.EnvelopePPM)},
		crypto.Keccak256Hash(basketSpec), basketURI, []common.Address{A["Inbox"]})
	if err != nil {
		return nil, err
	}
	for i, sc := range []string{claimSchema, moderationSchema} {
		if err := once(fmt.Sprintf("schema.%d", i), func() error {
			_, err := ch.send(key, nil, "SchemaRegistry", "register", sc, common.Address{}, true)
			return err
		}); err != nil {
			return nil, err
		}
	}
	ch.dep.Schema = schemaUID(claimSchema).Hex()
	// The timelock takes proposals only from Parliament (both houses passed
	// them); anyone may execute after the delay; nobody can cancel.
	r, err := ch.call("TernTimelock", "PROPOSER_ROLE")
	if err != nil {
		return nil, err
	}
	if err := once("TernTimelock.grantRole", func() error {
		_, err := ch.send(key, nil, "TernTimelock", "grantRole", r[0].([32]byte), A["Parliament"])
		return err
	}); err != nil {
		return nil, err
	}
	if p.Validators != (common.Address{}) { // the chain's own validator set: listed now, handed over last
		a, _ := loadArtifact("Validators")
		ch.c["Validators"] = bind.NewBoundContract(p.Validators, a.abi, b, b, b)
		ch.dep.Contracts["Validators"] = p.Validators.Hex()
		A["Validators"] = p.Validators
	}
	for _, n := range contractNames {
		if n == "Directory" || A[n] == (common.Address{}) {
			continue
		}
		if err := once("Directory.set."+n, func() error {
			_, err := ch.send(key, nil, "Directory", "set", n, A[n], [32]byte{}, "")
			return err
		}); err != nil {
			return nil, err
		}
	}
	// Hand the Directory to the timelock and give up the deployer's powers.
	if err := once("Directory.transferOwnership", func() error {
		_, err := ch.send(key, nil, "Directory", "transferOwnership", A["TernTimelock"])
		return err
	}); err != nil {
		return nil, err
	}
	admin, err := ch.call("TernTimelock", "DEFAULT_ADMIN_ROLE")
	if err != nil {
		return nil, err
	}
	if err := once("TernTimelock.renounceRole", func() error {
		_, err := ch.send(key, nil, "TernTimelock", "renounceRole", admin[0].([32]byte), deployer)
		return err
	}); err != nil {
		return nil, err
	}
	// The validator set last (audit 6 VB-4): a deploy that fails before this
	// leaves the bootstrap able to retry, not a half-governed network. From
	// now on only both houses change the set.
	if p.Validators != (common.Address{}) && !p.KeepValidators {
		if _, err := ch.send(key, nil, "Validators", "setGovernance", A["TernTimelock"], A["Registry"], A["Citizens"]); err != nil {
			return nil, fmt.Errorf("Validators.setGovernance (the deployer must be the genesis bootstrap): %w", err)
		}
	}
	return ch, nil
}

// minHandoverFounders: Citizens needs more founders than K (3) to ever
// admit anyone; with fewer the validator set must not go to Parliament.
const minHandoverFounders = 4

// testTokens: a deployment gets test-only contracts (TestUSD, anyone mints)
// on the devnet chain ids, or when told it is a devnet; never otherwise.
func testTokens(p Params, chainID uint64) bool { return p.Devnet || zk.IsDevnetChain(chainID) }

// Primitive is one entry of the governance table.
type Primitive struct {
	Key     string `json:"key"`
	Address string `json:"address"`
	Spec    string `json:"spec,omitempty"`
	SpecURI string `json:"spec_uri,omitempty"`
	Known   bool   `json:"known"`             // this client has the ABI for it
	Using   string `json:"you_use,omitempty"` // another version than the current one
}

// refresh reads which contracts are current from the Directory, so new
// primitives reach every client. It is advisory for what a client already
// uses: a new version of one is reported (Using), never switched to, since
// the client's funds and locks live in the version it uses ('gov use' moves
// it). Keys this client has no ABI for are listed, not used.
func (ch *Chain) refresh() ([]Primitive, error) {
	if _, ok := ch.c["Directory"]; !ok {
		return nil, nil
	}
	raw, ok := ch.directoryKept()
	if !ok {
		var err error
		if raw, err = ch.readDirectory(); err != nil {
			return nil, err
		}
	}
	var ps []Primitive
	for _, p := range raw {
		addr := common.HexToAddress(p.Address)
		if parsed, ok := ch.abis[p.Key]; ok {
			p.Known = true
			if using, had := ch.dep.Contracts[p.Key]; !had {
				ch.c[p.Key] = bind.NewBoundContract(addr, parsed, ch.b, ch.b, ch.b)
				ch.dep.Contracts[p.Key] = addr.Hex()
			} else if !strings.EqualFold(using, addr.Hex()) {
				p.Using = using
			}
		}
		ps = append(ps, p)
	}
	if ch.dep.Schema == "" {
		ch.dep.Schema = schemaUID(claimSchema).Hex()
	}
	return ps, nil
}

// readDirectory: the Directory's table from the chain (1 + 3 calls a key),
// kept for the next run with the block it was read at (directoryKept).
func (ch *Chain) readDirectory() ([]Primitive, error) {
	at, _ := ch.b.BlockNumber(context.Background())
	out, err := ch.call("Directory", "keys")
	if err != nil {
		return nil, err
	}
	var ps []Primitive
	for _, k := range out[0].([]string) {
		a, err := ch.call("Directory", "current", k)
		if err != nil {
			return nil, err
		}
		sp, _ := ch.call("Directory", "specOf", k)
		su, _ := ch.call("Directory", "specURIOf", k)
		p := Primitive{Key: k, Address: a[0].(common.Address).Hex()}
		if len(sp) > 0 && sp[0].([32]byte) != [32]byte{} {
			p.Spec = common.Hash(sp[0].([32]byte)).Hex()
		}
		if len(su) > 0 {
			p.SpecURI = su[0].(string)
		}
		ps = append(ps, p)
	}
	if d := mirrorDir(); at > 0 && cacheReady(d) {
		writeJSON(filepath.Join(d, "directory.json"), directoryFile{ch.c["Directory"].Address().Hex(), at, ps})
	}
	return ps, nil
}

// directoryFile: the Directory's table as read at a block. Every change to
// it is a Set event; none since that block: still the table (the event comes
// from the kept history, so this costs no call of its own).
type directoryFile struct {
	Directory string      `json:"directory"`
	Block     uint64      `json:"block"`
	Keys      []Primitive `json:"keys"`
}

func (ch *Chain) directoryKept() ([]Primitive, bool) {
	d := mirrorDir()
	if !cacheReady(d) {
		return nil, false
	}
	var f directoryFile
	dir := ch.c["Directory"].Address()
	if readJSON(filepath.Join(d, "directory.json"), &f) != nil || f.Directory != dir.Hex() || len(f.Keys) == 0 {
		return nil, false
	}
	ev := ch.abis["Directory"].Events["Set"]
	logs, err := ch.b.FilterLogs(context.Background(), ethereum.FilterQuery{FromBlock: new(big.Int).SetUint64(f.Block), Addresses: []common.Address{dir},
		Topics: [][]common.Hash{{ev.ID}}})
	if err != nil || len(logs) > 0 {
		return nil, false
	}
	return f.Keys, true
}

func (ch *Chain) opts(key *ecdsa.PrivateKey, value *big.Int) (*bind.TransactOpts, error) {
	o, err := bind.NewKeyedTransactorWithChainID(key, ch.chainID)
	if err != nil {
		return nil, err
	}
	o.Context = context.Background()
	o.Value = value
	return o, nil
}

func (ch *Chain) wait(tx *types.Transaction) (*types.Receipt, error) {
	if ch.mine != nil {
		ch.mine()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := waitMined(ctx, ch.b, tx.Hash())
	if err != nil {
		return nil, err
	}
	// The receipt can be readable a moment before the head (and so the state
	// the next call reads) moves to its block: wait for that too. (Seen as
	// "balance 0" right after a funding transfer, now and then.)
	for i := 0; i < 100; i++ {
		if h, err := ch.b.BlockNumber(ctx); err != nil || r.BlockNumber == nil || h >= r.BlockNumber.Uint64() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ch.gasUsed.Add(r.GasUsed)
	if r.Status != types.ReceiptStatusSuccessful {
		return r, errors.New("transaction reverted: " + ch.revertReason(tx, r))
	}
	return r, nil
}

// waitMined: the receipt, asked for at once and then every 50 ms, slowing to
// a second (bind.WaitMined asks once a second: a transaction would cost a
// whole second even on a chain with faster blocks).
func waitMined(ctx context.Context, b bind.DeployBackend, h common.Hash) (*types.Receipt, error) {
	delay := 25 * time.Millisecond
	for {
		if r, err := b.TransactionReceipt(ctx, h); err == nil && r != nil {
			return r, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 120*time.Millisecond { // blocks come every second: a receipt is looked for about 8 times a second (an approved action waited up to a second more)
			delay = delay * 5 / 4
		}
	}
}

// revertReason replays a failed call to read its reason.
func (ch *Chain) revertReason(tx *types.Transaction, r *types.Receipt) string {
	signer := types.LatestSignerForChainID(ch.chainID)
	from, err := types.Sender(signer, tx)
	if err != nil {
		return "unknown"
	}
	_, err = ch.b.CallContract(context.Background(), ethereum.CallMsg{From: from, To: tx.To(), Data: tx.Data(), Value: tx.Value(), Gas: tx.Gas()}, r.BlockNumber)
	if err != nil {
		return err.Error()
	}
	return "unknown"
}

// lockSender: transactions of one key are made and waited for one at a time
// (nonces stay in order); different keys go in parallel. (Before the scale
// simulation one lock serialised every transaction of a process: a process
// with many names, or a data node posting while it claims, made one
// transaction per block.)
func (ch *Chain) lockSender(a common.Address) func() {
	if ch.mine != nil { // a simulated chain mines on each send: one at a time, as its tests expect
		a = common.Address{}
	}
	m, _ := ch.senders.LoadOrStore(a, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// send makes a transaction and waits for it. Calls with one key are
// serialised so its nonces stay in order.
func (ch *Chain) send(key *ecdsa.PrivateKey, value *big.Int, contract, method string, args ...any) (*types.Receipt, error) {
	if err := ch.capsCheck(contract, method, value, args); err != nil { // a mainnet's caps (net.json; mainnet.go)
		return nil, err
	}
	if contract == "TernPaymaster" && method == "fund" { // its deposits can be drained (audit H-1)
		if err := ch.paymasterFundOK(); err != nil {
			return nil, err
		}
	}
	if r := ch.routes[crypto.PubkeyToAddress(key.PublicKey)]; r != nil {
		return ch.sendViaAccount(r, value, contract, method, args...)
	}
	defer ch.lockSender(crypto.PubkeyToAddress(key.PublicKey))()
	c, ok := ch.c[contract]
	if !ok {
		return nil, fmt.Errorf("contract %s not deployed", contract)
	}
	opts, err := ch.opts(key, value)
	if err != nil {
		return nil, err
	}
	// Gas with headroom: the estimate runs on this block, the transaction on a
	// later one, where a different time can mean one more storage write.
	if data, err := ch.abis[contract].Pack(method, args...); err == nil {
		to := common.HexToAddress(ch.dep.Contracts[contract])
		if est, err := ch.b.EstimateGas(context.Background(), ethereum.CallMsg{From: opts.From, To: &to, Value: value, Data: data}); err == nil {
			opts.GasLimit = est*5/4 + 20_000
		}
	}
	tx, err := c.Transact(opts, method, args...)
	// A node's transaction pool reads balances from the head it last saw: just
	// after the block that funded this key it can still say "insufficient
	// funds". Asked again a moment later, it knows.
	for i := 0; err != nil && strings.Contains(err.Error(), "insufficient funds") && i < 8; i++ {
		time.Sleep(50 * time.Millisecond)
		tx, err = c.Transact(opts, method, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %w", contract, method, cleanErr(err))
	}
	r, err := ch.wait(tx)
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %w", contract, method, err)
	}
	return r, nil
}

func cleanErr(err error) error {
	s := err.Error()
	if i := strings.Index(s, "execution reverted: "); i >= 0 {
		return errors.New(s[i+len("execution reverted: "):])
	}
	return err
}

// callHook, when set (measurements), sees every contract read.
var callHook func(contract, method string)

func (ch *Chain) call(contract, method string, args ...any) ([]any, error) {
	c, ok := ch.c[contract]
	if !ok {
		return nil, fmt.Errorf("contract %s not deployed", contract)
	}
	if callHook != nil {
		callHook(contract, method)
	}
	var out []any
	if err := c.Call(&bind.CallOpts{Context: context.Background()}, &out, method, args...); err != nil {
		return nil, fmt.Errorf("%s.%s: %w", contract, method, cleanErr(err))
	}
	return out, nil
}

// transfer sends coins between plain addresses (funding agents' gas).
func (ch *Chain) transfer(key *ecdsa.PrivateKey, to common.Address, amount *big.Int) error {
	if r := ch.routes[crypto.PubkeyToAddress(key.PublicKey)]; r != nil { // from a smart account
		cd, err := ch.executeCallData(to, amount, nil)
		if err != nil {
			return err
		}
		_, err = ch.userOp(r, cd)
		return err
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	defer ch.lockSender(from)()
	ctx := context.Background()
	nonce, err := ch.b.PendingNonceAt(ctx, from)
	if err != nil {
		return err
	}
	tip, err := ch.b.SuggestGasTipCap(ctx)
	if err != nil {
		return err
	}
	head, err := ch.b.HeaderByNumber(ctx, nil)
	if err != nil {
		return err
	}
	fee := new(big.Int).Add(tip, new(big.Int).Mul(head.BaseFee, big.NewInt(2)))
	tx := types.NewTx(&types.DynamicFeeTx{ChainID: ch.chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: fee, Gas: 21000, To: &to, Value: amount})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(ch.chainID), key)
	if err != nil {
		return err
	}
	err = ch.b.SendTransaction(ctx, signed)
	for i := 0; err != nil && strings.Contains(err.Error(), "insufficient funds") && i < 8; i++ { // the pool's view of the head lags (see send)
		time.Sleep(50 * time.Millisecond)
		err = ch.b.SendTransaction(ctx, signed)
	}
	if err != nil {
		return err
	}
	_, err = ch.wait(signed)
	return err
}

func (ch *Chain) balance(a common.Address) (*big.Int, error) {
	return ch.b.BalanceAt(context.Background(), a, nil)
}

// event is a decoded log.
type event struct {
	Name   string
	Block  uint64
	Index  uint
	Tx     common.Hash
	Args   map[string]any
	topics []common.Hash // the log's topics (cached histories filter on them)
	bhash  common.Hash   // the log's block hash and data (keypin.go checks them against other RPCs and the block)
	data   []byte
}

// events returns all logs of one event, oldest first, decoded. topics filter
// indexed arguments (nil = any).
func (ch *Chain) events(contract, name string, topics ...[]common.Hash) ([]event, error) {
	if perfFixes && len(topics) == 0 {
		return ch.eventsCached(contract, name)
	}
	if perfFixes && longLived { // a process that keeps running answers "the history of X" from its own copy
		all, err := ch.eventsCached(contract, name)
		if err != nil {
			return nil, err
		}
		var out []event
		for _, e := range all {
			ok := true
			for i, want := range topics {
				if len(want) == 0 {
					continue
				}
				if i+1 >= len(e.topics) || !containsHash(want, e.topics[i+1]) {
					ok = false
					break
				}
			}
			if ok {
				out = append(out, e)
			}
		}
		return out, nil
	}
	return ch.eventsFrom(ch.dep.Block, contract, name, topics...)
}

// longLived: this process keeps running (watch, mcp, a relay, a simulation):
// whole event histories are kept here and topic queries over them answered
// locally. A one-shot command asks the node for just the matching logs.
var longLived = false

func containsHash(xs []common.Hash, h common.Hash) bool {
	for _, x := range xs {
		if x == h {
			return true
		}
	}
	return false
}

// evCache: a whole event history, read once per process and then only from
// where it was read (perfFixes). Before, every rating, find, data node list
// and address index read the history from deployment, each time.
type evCache struct {
	mu   sync.RWMutex
	addr common.Address
	next uint64
	evs  []event
}

func (ch *Chain) eventsCached(contract, name string) ([]event, error) {
	c, ok := ch.c[contract]
	if !ok {
		return nil, fmt.Errorf("contract %s not deployed", contract)
	}
	a := ch.abis[contract]
	ev, ok := a.Events[name]
	if !ok {
		return nil, fmt.Errorf("no event %s", name)
	}
	v, _ := ch.evc.LoadOrStore(contract+"."+name, &evCache{addr: c.Address(), next: ch.dep.Block})
	cc := v.(*evCache)
	// The common case: nothing new since the last read.
	cc.mu.RLock()
	next, same := cc.next, cc.addr == c.Address()
	cc.mu.RUnlock()
	head, err := ch.b.BlockNumber(context.Background()) // asked outside the lock: readers don't queue behind it
	if err != nil {
		return nil, err
	}
	if same && head < next {
		cc.mu.RLock()
		out := append([]event(nil), cc.evs...)
		cc.mu.RUnlock()
		return out, nil
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.addr != c.Address() { // a new version of the contract: read its history from the start
		cc.addr, cc.next, cc.evs = c.Address(), ch.dep.Block, nil
	}
	if head >= cc.next {
		q := ethereum.FilterQuery{FromBlock: new(big.Int).SetUint64(cc.next), ToBlock: new(big.Int).SetUint64(head), Addresses: []common.Address{c.Address()},
			Topics: [][]common.Hash{{ev.ID}}}
		logs, err := ch.b.FilterLogs(context.Background(), q)
		if err != nil {
			return nil, err
		}
		for _, l := range logs {
			m := map[string]any{}
			if err := c.UnpackLogIntoMap(m, name, l); err != nil {
				return nil, err
			}
			cc.evs = append(cc.evs, event{Name: name, Block: l.BlockNumber, Index: l.Index, Tx: l.TxHash, Args: m, topics: l.Topics, bhash: l.BlockHash, data: l.Data})
		}
		cc.next = head + 1
	}
	return append([]event(nil), cc.evs...), nil
}

// eventsFrom is events from a given block on (a watcher's cursor).
func (ch *Chain) eventsFrom(from uint64, contract, name string, topics ...[]common.Hash) ([]event, error) {
	c, ok := ch.c[contract]
	if !ok {
		return nil, fmt.Errorf("contract %s not deployed", contract)
	}
	a := ch.abis[contract]
	ev, ok := a.Events[name]
	if !ok {
		return nil, fmt.Errorf("no event %s", name)
	}
	if from < ch.dep.Block {
		from = ch.dep.Block
	}
	if head, err := ch.headNumber(from); err == nil && from > head {
		return nil, nil // nothing new yet
	}
	q := ethereum.FilterQuery{FromBlock: new(big.Int).SetUint64(from), Addresses: []common.Address{c.Address()},
		Topics: append([][]common.Hash{{ev.ID}}, topics...)}
	logs, err := ch.b.FilterLogs(context.Background(), q)
	if err != nil {
		return nil, err
	}
	out := make([]event, 0, len(logs))
	for _, l := range logs {
		m := map[string]any{}
		if err := c.UnpackLogIntoMap(m, name, l); err != nil {
			return nil, err
		}
		out = append(out, event{Name: name, Block: l.BlockNumber, Index: l.Index, Tx: l.TxHash, Args: m, topics: l.Topics, bhash: l.BlockHash, data: l.Data})
	}
	return out, nil
}

// headNumber: the head, from a reading under 250 ms old when that is already
// at or past `need` (perfFixes; otherwise, and when it isn't, asked now).
func (ch *Chain) headNumber(need uint64) (uint64, error) {
	if perfFixes {
		if v, ok := ch.head.Load().(*headAt); ok && v.n >= need && time.Since(v.at) < 250*time.Millisecond {
			return v.n, nil
		}
	}
	n, err := ch.b.BlockNumber(context.Background())
	if err == nil && perfFixes {
		ch.head.Store(&headAt{n, time.Now()})
	}
	return n, err
}

type headAt struct {
	n  uint64
	at time.Time
}

// eventsMulti: several events of one contract from block `from` on, in one
// query, oldest first.
func (ch *Chain) eventsMulti(from uint64, contract string, names ...string) ([]event, error) {
	c, ok := ch.c[contract]
	if !ok {
		return nil, fmt.Errorf("contract %s not deployed", contract)
	}
	a := ch.abis[contract]
	var ids []common.Hash
	byID := map[common.Hash]string{}
	for _, n := range names {
		ev, ok := a.Events[n]
		if !ok {
			return nil, fmt.Errorf("no event %s", n)
		}
		ids = append(ids, ev.ID)
		byID[ev.ID] = n
	}
	if from < ch.dep.Block {
		from = ch.dep.Block
	}
	if head, err := ch.headNumber(from); err == nil && from > head {
		return nil, nil
	}
	q := ethereum.FilterQuery{FromBlock: new(big.Int).SetUint64(from), Addresses: []common.Address{c.Address()}, Topics: [][]common.Hash{ids}}
	logs, err := ch.b.FilterLogs(context.Background(), q)
	if err != nil {
		return nil, err
	}
	out := make([]event, 0, len(logs))
	for _, l := range logs {
		name := byID[l.Topics[0]]
		m := map[string]any{}
		if err := c.UnpackLogIntoMap(m, name, l); err != nil {
			return nil, err
		}
		out = append(out, event{Name: name, Block: l.BlockNumber, Index: l.Index, Tx: l.TxHash, Args: m})
	}
	return out, nil
}

func (ch *Chain) blockTime(n uint64) (int64, error) {
	if perfFixes {
		if t, ok := ch.times.Load(n); ok {
			return t.(int64), nil
		}
		if t, ok := keptTime(n); ok { // an earlier run's (logcache.go)
			ch.times.Store(n, t)
			return t, nil
		}
	}
	h, err := ch.b.HeaderByNumber(context.Background(), new(big.Int).SetUint64(n))
	if err != nil {
		return 0, err
	}
	if perfFixes { // a block's time is fixed once it is made (a reorg could change it: only an item's displayed time)
		ch.times.Store(n, int64(h.Time))
		keepTime(n, int64(h.Time))
	}
	return int64(h.Time), nil
}

func (ch *Chain) epoch() (uint64, error) {
	out, err := ch.call("Registry", "epoch")
	if err != nil {
		return 0, err
	}
	return out[0].(uint64), nil
}

// callFixed: a value that never changes for a deployed contract (an
// immutable), read once per contract address (perfFixes).
func (ch *Chain) callFixed(contract, method string) ([]any, error) {
	if !perfFixes {
		return ch.call(contract, method)
	}
	k := ch.dep.Contracts[contract] + "." + method
	if v, ok := ch.fixed.Load(k); ok {
		return v.([]any), nil
	}
	out, err := ch.call(contract, method)
	if err == nil {
		ch.fixed.Store(k, out)
	}
	return out, err
}

func (ch *Chain) epochLength() (uint64, error) {
	out, err := ch.callFixed("Registry", "epochLength") // immutable
	if err != nil {
		return 0, err
	}
	return out[0].(uint64), nil
}

func (ch *Chain) fee() (*big.Int, error) {
	out, err := ch.call("Inbox", "fee")
	if err != nil {
		return nil, err
	}
	return out[0].(*big.Int), nil
}

// feeFor is what to attach for n envelopes: the current fee plus half again
// as headroom (the price or the load may move before the block); the Inbox
// refunds what it doesn't charge.
func (ch *Chain) feeFor(n int) (*big.Int, error) {
	f, err := ch.fee()
	if err != nil {
		return nil, err
	}
	t := new(big.Int).Mul(f, big.NewInt(int64(n)))
	return t.Add(t, new(big.Int).Div(t, big.NewInt(2))), nil
}

// has: the contract is deployed on this network (older devnets lack new ones).
func (ch *Chain) has(name string) bool {
	_, ok := ch.c[name]
	return ok
}

func idOf(name string) common.Hash { return crypto.Keccak256Hash([]byte(name)) }

func keyHex(k *ecdsa.PrivateKey) string { return hex.EncodeToString(crypto.FromECDSA(k)) }
