// Co-ops: people pooling anything they have (a GPU, disk, an API quota, a
// car, a seat, a room, a tool, their time) inside a group, without anyone
// in the middle. Not a product and not a contract: bricks that clients put
// together, each a versioned record with open extensions.
//
//	offer      what a member offers (tern.offer/1): kind, how much, which parts of a whole, terms, when, where
//	presence   who is online now with what, sealed to the group, held by data nodes for minutes (never on chain)
//	usage      signed counters of work done for someone (tern.receipt/1), settled by channel voucher or in credits
//	credits    a group's ledger without money, computed by each reader from receipts posted in the group
//	bricks     which bricks this client speaks, which the network lists (coopbricks.go)
//	pool       who is served first: a rule module the group picks (credits, turns, quota, first come, or anyone's)
//	spotcheck  signed work statements compared by majority; reports, scores, and a bond a panel decides
//
// The engine that does the work (a model split across machines, a seedbox,
// a game server, a car) is someone else's; the bricks carry who is in, what
// they offer and the record. An optional module (modules enable coop).
//
// Records carry their schema and version (tern.offer/1). What a version
// doesn't name goes in ext, under a published name (offer/gpu): a client
// checks the extensions it can resolve, carries the others, and a signature
// covers them all; a new top-level field is a new version, which older
// clients report as unknown instead of misreading (docs/ADDING-A-BRICK.md).
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern/internal/ui"
)

// coopWords: the module's commands (each takes a subcommand).
var coopWords = map[string]bool{"offer": true, "presence": true, "receipt": true, "credits": true, "pool": true, "verify": true, "bricks": true}

func init() {
	for w := range coopWords {
		twoWord[w] = true
	}
}

// coopFlags: the module's own flags (registered beside the CLI's).
type coopFlags struct {
	public bool
	state  string // presence: online, busy, leaving
	ttl    int
	reach  string
	load   float64
	memo   string
	every  int
	// presence list: parts held by fewer are "thin"
	minCopies int
	// usage
	unit      string
	session   string
	resource  string
	group     string
	unitPrice string
	work      string
	observed  uint64
	quotaDay  float64 // pool rule
	// spotcheck
	canon   string
	engine  string
	rate    float64
	seed    string
	vec     string        // verify statement: numbers to compare within a tolerance
	compare string        // verify compare: the comparator's line
	fs      *flag.FlagSet // the CLI's flags (--panel, --k, --days are the core's)
}

// global: a flag the core registers (empty when there is none).
func (f coopFlags) global(name string) string {
	if f.fs == nil {
		return ""
	}
	if fl := f.fs.Lookup(name); fl != nil {
		return fl.Value.String()
	}
	return ""
}

func registerCoopFlags(fs *flag.FlagSet) *coopFlags {
	f := &coopFlags{fs: fs}
	fs.BoolVar(&f.public, "public", false, "")
	fs.StringVar(&f.state, "state", "online", "")
	fs.IntVar(&f.ttl, "ttl", 120, "")
	fs.StringVar(&f.reach, "reach", "", "")
	fs.Float64Var(&f.load, "load", 0, "")
	fs.StringVar(&f.memo, "memo", "", "")
	if fs.Lookup("every") == nil { // the core registers --every for group rotate: share it
		fs.IntVar(&f.every, "every", 0, "")
	}
	fs.IntVar(&f.minCopies, "min-copies", 2, "")
	fs.StringVar(&f.unit, "unit", "token", "")
	fs.StringVar(&f.session, "session", "", "")
	fs.StringVar(&f.resource, "resource", "", "")
	fs.StringVar(&f.group, "group", "", "")
	fs.StringVar(&f.unitPrice, "unit-price", "", "")
	fs.StringVar(&f.work, "work", "", "")
	fs.Uint64Var(&f.observed, "observed", 0, "")
	fs.Float64Var(&f.quotaDay, "quota-day", 0, "")
	fs.StringVar(&f.canon, "canon", "", "")
	fs.StringVar(&f.engine, "engine", "", "")
	fs.Float64Var(&f.rate, "rate", 0, "")
	fs.StringVar(&f.seed, "seed", "", "")
	fs.StringVar(&f.vec, "vec", "", "")
	fs.StringVar(&f.compare, "compare", "", "")
	return f
}

func runCoop(c *Client, cmd string, pos []string, f coopFlags) error {
	word, _, _ := strings.Cut(cmd, " ")
	switch word {
	case "offer":
		return runOffer(c, cmd, pos, f)
	case "presence":
		return runPresence(c, cmd, pos, f)
	case "receipt":
		return runReceipt(c, cmd, pos, f)
	case "credits":
		return runCredits(c, cmd, pos, f)
	case "pool":
		return runPool(c, cmd, pos, f)
	case "verify":
		return runVerify(c, cmd, pos, f)
	case "bricks":
		return runBricks(c, cmd, pos, f)
	}
	return unknownCommand(cmd)
}

// coopInput: a JSON argument given inline, as @FILE, or as - (stdin).
func coopInput(arg string) ([]byte, error) {
	switch {
	case arg == "-":
		return io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	case strings.HasPrefix(arg, "@"):
		return os.ReadFile(arg[1:])
	}
	return []byte(arg), nil
}

// extDecls: published extension schemas by name (offer/gpu), resolved once
// per process: the pinned publishers' (types.json) or, without pins, the
// first publisher of each name.
type extDecl struct {
	publisher string
	body      ui.Schema
	err       error
}

var (
	extOnce  sync.Once
	extNames map[string][]typeName
)

func (c *Client) extLookup(name string) extDecl {
	short, pub, pinned := strings.Cut(name, "@")
	extOnce.Do(func() {
		extNames = map[string][]typeName{}
		var trust struct {
			Publishers []string `json:"publishers"`
		}
		if c.dir != "" {
			readJSON(filepath.Join(c.dir, "types.json"), &trust)
		}
		list, _ := c.schemaNameList(trust.Publishers)
		for _, e := range list {
			extNames[e.name] = append(extNames[e.name], e)
		}
	})
	var hit *typeName
	if pinned {
		if uid, n, err := c.typeOf(pub, short); err == nil && uid != (common.Hash{}) {
			if v, err := c.ch.call("SchemaNames", "version", idOf(pub), short, new(big.Int).SetUint64(n)); err == nil {
				hit = &typeName{short, pub, versionURI(v[0])}
			}
		}
	} else if es := extNames[short]; len(es) > 0 {
		hit = &es[0]
	}
	if hit == nil {
		return extDecl{err: errors.New("no published schema")}
	}
	d, err := declOf(hit.uri)
	if err == nil && d.Body == nil {
		err = errors.New("its declaration has no body schema")
	}
	return extDecl{publisher: hit.publisher, body: d.Body, err: err}
}

// extCheck: what each extension of a record is: checked against the schema
// its name's publisher published, invalid, or carried unchecked.
func (c *Client) extCheck(ext map[string]json.RawMessage) map[string]string {
	out := map[string]string{}
	for k, raw := range ext {
		d := c.extLookup(k)
		if d.err != nil {
			out[k] = "unchecked (" + d.err.Error() + "): carried, not interpreted"
			continue
		}
		var v any
		json.Unmarshal(raw, &v)
		if err := ui.ValidateSchema(d.body, v); err != nil {
			out[k] = "invalid against " + d.publisher + "'s schema: " + err.Error()
			continue
		}
		out[k] = "checked against " + d.publisher + "'s schema"
	}
	return out
}

// publishExt: an extension schema (a JSON Schema file) as NAME under this
// identity's name in SchemaNames, the same way card kinds are published.
func (c *Client) publishExt(name, arg string) (map[string]any, error) {
	if !extNameRE.MatchString(name) || strings.Contains(name, "@") {
		return nil, fmt.Errorf("extension name %q: like offer/gpu", name)
	}
	raw, err := coopInput(arg)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("schema: %v", err)
	}
	delete(body, "$id")
	v, uid, err := c.publishType(name, "bytes ext", declURI(map[string]any{"body": body}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"published": name, "publisher": c.f.Name, "version": v, "schema": uid.Hex(),
		"note": "records carrying ext[\"" + name + "\"] are checked against it by readers who pin you (types.json) or find no earlier publisher of the name"}, nil
}

// jsonStrict: decode a record, refusing fields it doesn't have (a field a
// signer didn't sign would be shown as if it had).
func jsonStrict(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// jsonOf: a value as its JSON (for records whose signature covers it).
func jsonOf(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// coopDigest: what a member signs for a co-op record: EIP-191 over
// keccak(type ‖ "@" ‖ chain id ‖ "\n" ‖ the record's JSON without its
// signatures), so a record signed on one network or as one type is never
// valid as another.
func (c *Client) coopDigest(typ string, body []byte) []byte {
	pre := fmt.Sprintf("%s@%s\n", typ, c.ch.chainID)
	return accounts.TextHash(crypto.Keccak256(append([]byte(pre), body...)))
}

func (c *Client) coopSign(typ string, body []byte) (string, error) {
	sig, err := crypto.Sign(c.coopDigest(typ, body), c.key)
	if err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(sig), nil
}

// coopVerify: did name sign body (as typ) with a key that speaks for it now?
func (c *Client) coopVerify(name, typ string, body []byte, sigHex string) error {
	return c.coopVerifyAt(name, typ, body, sigHex, 0)
}

// coopVerifyAt: the same at block b (0: now). A record that reached the
// chain is checked at its block, so it stays valid after the signer's grant
// ends, and a key revoked before it never counts.
func (c *Client) coopVerifyAt(name, typ string, body []byte, sigHex string, b uint64) error {
	sig, err := hex.DecodeString(strings.TrimPrefix(sigHex, "0x"))
	if err != nil || len(sig) != 65 {
		return errors.New("not a signature")
	}
	pub, err := crypto.SigToPub(c.coopDigest(typ, body), sig)
	if err != nil {
		return err
	}
	if b == 0 {
		if b, err = c.ch.b.BlockNumber(ctxb()); err != nil {
			return err
		}
	}
	bt, _ := c.ch.blockTime(b)
	return c.res.verifySigner(name, crypto.PubkeyToAddress(*pub), b, bt)
}

// groupMember: is name (or its owner, for an agent) a member of the group's set?
func (c *Client) groupMember(group, name string) bool {
	for _, n := range []string{name, ownerName(name)} {
		if out, err := c.ch.call("Sets", "members", setID(group), idOf(n)); err == nil && out[0].(bool) {
			return true
		}
	}
	return false
}
