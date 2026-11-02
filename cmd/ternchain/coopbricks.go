// Bricks over the network's life: which co-op bricks this client speaks,
// which versions the network lists, and which it lists that this client
// doesn't know. A brick lives in clients, but it is versioned through the
// same path as a contract: a Directory key brick/NAME, published by a vote
// of both houses with its specification and review (gov propose upgrade).
// The Directory lists contracts, so a brick version is a marker: a 41-byte
// contract that answers any call with keccak256 of the version's schema id
// (tern.offer/1), deployed by anyone (bricks mark). A client knows its own
// version by asking the markers. So a new version arrives as gov updates
// and the new-version event, like any primitive's, and a brick nobody here
// implements is still listed with its spec. Inside a brick, kinds,
// extensions (offer/NAME), pool rules and comparators are open registries
// that need no vote (docs/ADDING-A-BRICK.md).
//
//	bricks [list]                         what this client speaks, what people published, what the Directory lists
//	bricks publish NAME SCHEMA --uri SPEC  publish a brick under your own name (SchemaNames brick/NAME): no vote, others opt in
//	bricks mark SCHEMA                    deploy a version's marker (then: gov propose upgrade brick/NAME MARKER --spec URI --audit URI)
//
// Publishing comes first and needs nobody's leave: a brick is a name under
// its publisher (as card kinds are), and readers choose whose to see
// (types.json publishers). The Directory starts with no bricks; listing one
// there is an optional endorsement by both houses, never a gate.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// coopBrick: one brick this client implements.
type coopBrick struct {
	Name    string   `json:"name"`
	Schemas []string `json:"schemas"` // the record versions it speaks (the first names the brick's version)
	What    string   `json:"what"`
}

var coopBricks = []coopBrick{
	{"offer", []string{tOffer}, "what a member offers: any kind, typed amounts, parts of a whole, terms, when, where"},
	{"presence", []string{tPresence}, "who is here now with what, sealed to the group, minutes long, never on chain"},
	{"receipt", []string{tReceipt}, "signed counters in any unit, countersigned by the consumer"},
	{"credits", []string{tCreditsPolicy}, "a group's ledger without money, computed by each reader"},
	{"pool", []string{tPool}, "who is served first: a rule module the group picks"},
	{"verify", []string{tWork, tCheck}, "work redone and compared by a comparator module; a panel bond"},
}

const brickPrefix = "brick/"

// brickMarker: the init code of a version's marker: its runtime is
// PUSH32 keccak256(schema) PUSH1 0 MSTORE PUSH1 32 PUSH1 0 RETURN.
func brickMarker(schema string) []byte {
	h := crypto.Keccak256([]byte(schema))
	runtime := append(append([]byte{0x7f}, h...), 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3)
	init := []byte{0x60, byte(len(runtime)), 0x80, 0x60, 0x0b, 0x60, 0x00, 0x39, 0x60, 0x00, 0xf3}
	return append(init, runtime...)
}

// brickMark deploys the marker of a brick version from this identity's key.
func (c *Client) brickMark(schema string) (common.Address, error) {
	r, err := c.ch.sendRaw(c.key, nil, nil, brickMarker(schema))
	if err != nil {
		return common.Address{}, err
	}
	return r.ContractAddress, nil
}

// markerSays: is the contract at a the marker of schema?
func (c *Client) markerSays(a common.Address, schema string) bool {
	b, err := c.ch.rawCall(common.Address{}, a, nil)
	return err == nil && len(b) == 32 && common.BytesToHash(b) == crypto.Keccak256Hash([]byte(schema))
}

// brickUsing: for a brick key, the listed version this client speaks.
func (c *Client) brickUsing(key string) (string, bool) {
	name, ok := strings.CutPrefix(key, brickPrefix)
	if !ok {
		return "", false
	}
	for _, b := range coopBricks {
		if b.Name != name {
			continue
		}
		vs, err := c.dirVersions(key)
		if err != nil {
			return "", false
		}
		for _, v := range vs {
			if c.markerSays(v.Value, b.Schemas[0]) {
				return v.Value.Hex(), true
			}
		}
	}
	return "", false
}

// usingVersion: the version of a Directory key this client uses: a
// contract's address, or the brick version it speaks.
func (c *Client) usingVersion(key string) (string, bool) {
	if a, ok := c.ch.dep.Contracts[key]; ok {
		return a, true
	}
	return c.brickUsing(key)
}

// bricksList: each brick this client speaks with the Directory's word on
// it, and the bricks the Directory lists that this client doesn't know.
func (c *Client) bricksList() (map[string]any, error) {
	keys := map[string]bool{}
	if c.ch.has("Directory") {
		if ks, err := c.ch.call("Directory", "keys"); err == nil {
			for _, k := range ks[0].([]string) {
				if strings.HasPrefix(k, brickPrefix) {
					keys[k] = true
				}
			}
		}
	}
	var speaks []map[string]any
	for _, b := range coopBricks {
		k := brickPrefix + b.Name
		m := map[string]any{"brick": b.Name, "schemas": b.Schemas, "what": b.What, "key": k}
		if !keys[k] {
			m["directory"] = "not listed (listing is optional: bricks mark " + b.Schemas[0] + ", then gov propose upgrade " + k + " MARKER --spec URI --audit URI)"
		} else {
			cur, err := c.dirCurrent(k)
			switch {
			case err != nil:
				m["directory"] = err.Error()
			case c.markerSays(cur, b.Schemas[0]):
				m["directory"] = "current"
			default:
				m["directory"] = "the network lists another version (" + cur.Hex() + "): read 'gov versions " + k + "'; a client that speaks it is an update of ternchain"
			}
			delete(keys, k)
		}
		speaks = append(speaks, m)
	}
	var unknown []map[string]any
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		m := map[string]any{"key": k, "note": "listed by the network, unknown to this client: read its spec; a client that implements it speaks it"}
		if vs, err := c.dirVersions(k); err == nil && len(vs) > 0 {
			v := vs[len(vs)-1]
			m["version"], m["spec_uri"] = len(vs), v.SpecURI
		}
		unknown = append(unknown, m)
	}
	published, err := c.publishedBricks()
	if err != nil {
		return nil, err
	}
	res := map[string]any{"speaks": speaks, "published": published, "open_registries": map[string]string{
		"offer kinds":           "any word (gpu, storage, seat, car, api-quota…); no registry needed",
		"extensions":            "offer/NAME, presence/NAME, receipt/NAME, work/NAME: JSON Schemas anyone publishes in SchemaNames (offer ext)",
		"pool rules":            "policy modules of kind pool (built in: pool-default@1, pool-round-robin@1, pool-quota@1, pool-fifo@1)",
		"comparators":           "policy modules of kind compare (built in: compare-default@1, compare-cosine@1, compare-topk@1, compare-abs@1)",
		"units, canons, wholes": "any word; agreed inside a group",
	}}
	if len(unknown) > 0 {
		res["unknown"] = unknown
	}
	return res, nil
}

// brickDecl: what a published brick name points at.
type brickDecl struct {
	Schema string `json:"schema"` // the record's id and version, in its publisher's namespace (oscar.rota/1)
	Spec   string `json:"spec"`   // where its specification is (a URI)
	What   string `json:"what,omitempty"`
}

// publishedBricks: brick/NAME entries in SchemaNames, by the pinned
// publishers (types.json) or, without pins, everyone's: what people
// published for others to opt into. Data, not instructions.
func (c *Client) publishedBricks() ([]map[string]any, error) {
	if !c.ch.has("SchemaNames") {
		return nil, nil
	}
	var trust struct {
		Publishers []string `json:"publishers"`
	}
	if c.dir != "" {
		readJSON(filepath.Join(c.dir, "types.json"), &trust)
	}
	list, err := c.schemaNameList(trust.Publishers)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, e := range list {
		name, ok := strings.CutPrefix(e.name, brickPrefix)
		if !ok {
			continue
		}
		m := map[string]any{"brick": name, "publisher": e.publisher}
		var d brickDecl
		if raw, err := dataURI(e.uri); err == nil && json.Unmarshal(raw, &d) == nil && d.Schema != "" {
			m["schema"], m["spec"] = d.Schema, d.Spec
			if d.What != "" {
				m["what"] = fence(e.publisher, d.What)
			}
		} else {
			m["note"] = "no readable declaration"
		}
		out = append(out, m)
	}
	return out, nil
}

// brickPublish: NAME under this identity's name, pointing at a record
// schema and its spec. Anyone may; nobody is asked.
func (c *Client) brickPublish(name, schema, spec, what string) (map[string]any, error) {
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`).MatchString(name) {
		return nil, errors.New("NAME: a lower-case word, like rota")
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,80}/[0-9]{1,4}$`).MatchString(schema) {
		return nil, errors.New("SCHEMA: a record id with its version, in your namespace, like " + c.f.Name + ".rota/1")
	}
	if spec == "" {
		return nil, errors.New("--uri SPEC: where the specification is (ipfs://…, https://…)")
	}
	v, uid, err := c.publishType(brickPrefix+name, "string brick", declURI(brickDecl{Schema: schema, Spec: spec, What: firstLine(what, 200)}))
	if err != nil {
		return nil, err
	}
	return map[string]any{"published": brickPrefix + name, "publisher": c.f.Name, "version": v, "schema": uid.Hex(),
		"note": "others see it in bricks list (if they pin you as a publisher, or pin nobody) and opt in by running a client or agent that speaks it; no vote"}, nil
}

func runBricks(c *Client, cmd string, pos []string, f coopFlags) error {
	switch cmd {
	case "bricks", "bricks list":
		res, err := c.bricksList()
		if err != nil {
			return err
		}
		out(res)
		return nil
	case "bricks publish":
		if len(pos) < 2 {
			return errors.New("usage: bricks publish NAME SCHEMA --uri SPEC [--memo WHAT]")
		}
		res, err := c.brickPublish(pos[0], pos[1], f.global("uri"), f.memo)
		if err != nil {
			return err
		}
		out(res)
		return nil
	case "bricks mark":
		if len(pos) < 1 {
			return errors.New("usage: bricks mark SCHEMA (a brick version's id, like tern.offer/2)")
		}
		a, err := c.brickMark(pos[0])
		if err != nil {
			return err
		}
		out(map[string]any{"schema": pos[0], "marker": a.Hex(),
			"next": fmt.Sprintf("gov propose upgrade %sNAME %s --spec URI --audit URI [--notes TEXT]", brickPrefix, a.Hex())})
		return nil
	}
	return unknownCommand(cmd)
}
