// A message bridge between two networks, as client code: a relayer that is
// a name on both reads a public post on the origin, posts it into a group on
// the destination, and attests there where it came from (network, tx, batch
// root, envelope). Anyone on the destination checks the citation against the
// origin's RPC: the tx, the batch in it, the envelope in the batch, and the
// text, opened with the origin channel's published keys. The relayer is
// trusted for liveness only: a false citation fails the check.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const bridgeKind = "bridge.origin"

// Citation: where a relayed post came from.
type Citation struct {
	Network  string `json:"network"`
	ChainID  int64  `json:"chain_id"`
	Inbox    string `json:"inbox"`
	Tx       string `json:"tx"`
	Block    uint64 `json:"block"`
	Root     string `json:"root"` // keccak256 of the batch
	Envelope string `json:"envelope"`
	Group    string `json:"group"`
	Author   string `json:"author"`
	Text     string `json:"text"`
}

// findEnvelope: the Inbox event (posted in full, or committed by hash) that carried envelope id at block.
func findEnvelope(ch *Chain, id common.Hash, block uint64) (tx, root common.Hash, err error) {
	posted, err := ch.eventsFrom(block, "Inbox", "Posted")
	if err != nil {
		return tx, root, err
	}
	for _, ev := range posted {
		if ev.Block != block {
			continue
		}
		b := ev.Args["batch"].([]byte)
		es, _ := decodeBatch(b)
		for _, e := range es {
			if e.hash() == id {
				return ev.Tx, crypto.Keccak256Hash(b), nil
			}
		}
	}
	return tx, root, fmt.Errorf("envelope %s not in a batch posted in full at block %d", id.Hex(), block)
}

// bridgeCite: a citation for a post we have read in a group on this network.
func (c *Client) bridgeCite(postID, network string) (*Citation, error) {
	it := c.find1(postID)
	if it == nil || !strings.HasPrefix(it.To, "group:") || it.Mail == nil {
		return nil, fmt.Errorf("no group post %q in your items (group read first)", postID)
	}
	if it.Status != "delivered" {
		return nil, fmt.Errorf("post %s is %s here: not relayed", postID, it.Status)
	}
	tx, root, err := findEnvelope(c.ch, common.HexToHash(it.ID), it.Block)
	if err != nil {
		return nil, err
	}
	return &Citation{Network: network, ChainID: c.ch.chainID.Int64(), Inbox: c.ch.dep.Contracts["Inbox"], Tx: tx.Hex(), Block: it.Block,
		Root: root.Hex(), Envelope: it.ID, Group: strings.TrimPrefix(it.To, "group:"), Author: it.From, Text: it.Mail.Text}, nil
}

// bridgePost relays a citation into a group here and attests its origin.
func (c *Client) bridgePost(group string, ct *Citation) (string, common.Hash, error) {
	b, _ := json.Marshal(ct)
	text := fmt.Sprintf("[relayed from %s/%s] %s: %s", ct.Network, ct.Group, ct.Author, ct.Text)
	id, err := c.postGroup(group, text, b)
	if err != nil {
		return "", common.Hash{}, err
	}
	uid, err := c.easAttest(common.HexToHash(id), bridgeKind, b, 0, 0, common.Hash{})
	return id, uid, err
}

// publishedChannelKeys: a public group's channel keys, as its admin published them.
func publishedChannelKeys(c *Client, name string) (*GroupKeys, error) {
	evs, err := c.ch.events("Sets", "Published", []common.Hash{setID(name)})
	if err != nil {
		return nil, err
	}
	for i := len(evs) - 1; i >= 0; i-- {
		by, _ := c.res.nameOf(common.Hash(evs[i].Args["by"].([32]byte)))
		if evs[i].Args["key"].(string) != channelKeysSlot || !c.hasRole(name, "admin", by) {
			continue
		}
		var g GroupKeys
		if err := json.Unmarshal(evs[i].Args["value"].([]byte), &g); err != nil {
			return nil, err
		}
		g.IsPublic = true
		return &g, nil
	}
	return nil, fmt.Errorf("%s has no published channel keys on the origin: its posts can't be checked", name)
}

// bridgeVerify checks a bridge attestation here against the origin network.
func (c *Client) bridgeVerify(uid common.Hash, originNet string) (map[string]any, error) {
	cl, err := c.claimByUID(uid.Hex())
	if err != nil {
		return nil, err
	}
	if cl.Kind != bridgeKind {
		return nil, fmt.Errorf("claim %s is a %q, not a bridge citation", uid.Hex(), cl.Kind)
	}
	var ct Citation
	if err := json.Unmarshal([]byte(cl.Data), &ct); err != nil {
		return nil, fmt.Errorf("bad citation: %w", err)
	}
	m := map[string]any{"relayed_by": cl.Issuer, "post_here": cl.Subject.Hex(), "origin": ct.Network, "origin_tx": ct.Tx, "origin_block": ct.Block}
	fail := func(why string) (map[string]any, error) {
		m["verified"], m["why"] = false, why
		return m, nil
	}
	chA, df, err := connect(originNet)
	if err != nil {
		return nil, err
	}
	if df.Network != ct.Network || chA.chainID.Int64() != ct.ChainID || !strings.EqualFold(chA.dep.Contracts["Inbox"], ct.Inbox) {
		return fail(fmt.Sprintf("the origin file is %s (chain %d, inbox %s), the citation names %s (chain %d, inbox %s)",
			df.Network, chA.chainID, chA.dep.Contracts["Inbox"], ct.Network, ct.ChainID, ct.Inbox))
	}
	r, err := chA.b.TransactionReceipt(context.Background(), common.HexToHash(ct.Tx))
	if err != nil || r.Status != 1 || r.BlockNumber.Uint64() != ct.Block {
		return fail("no successful tx " + ct.Tx + " at that block on the origin")
	}
	posted := chA.abis["Inbox"].Events["Posted"]
	var env *Envelope
	for _, l := range r.Logs {
		if l.Address != common.HexToAddress(ct.Inbox) || len(l.Topics) == 0 || l.Topics[0] != posted.ID {
			continue
		}
		v, err := posted.Inputs.NonIndexed().Unpack(l.Data)
		if err != nil {
			continue
		}
		b := v[1].([]byte)
		if crypto.Keccak256Hash(b).Hex() != ct.Root {
			continue
		}
		es, _ := decodeBatch(b)
		for i := range es {
			if es[i].hash().Hex() == ct.Envelope {
				env = &es[i]
			}
		}
	}
	if env == nil {
		return fail("the tx carries no batch with that root holding that envelope")
	}
	// Open it as any reader of the public channel would, on the origin.
	tmp, err := os.MkdirTemp("", "tern-bridge-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	idc := *c.Ident
	idc.dir = tmp
	ca := &Client{Ident: &idc, ch: chA, res: newResolver(chA), st: &State{Contacts: map[string]bool{}, Groups: map[string]*GroupKeys{}}}
	g, err := publishedChannelKeys(ca, ct.Group)
	if err != nil {
		return fail(err.Error())
	}
	bt, _ := chA.blockTime(ct.Block)
	it := ca.openGroup(*env, ct.Block, bt, g, nil)
	if it == nil || it.Mail == nil || !strings.HasPrefix(it.Verified, "ok") {
		return fail("the envelope doesn't open as a signed post of " + ct.Group)
	}
	if it.From != ct.Author || it.Mail.Text != ct.Text {
		return fail(fmt.Sprintf("on the origin the post is by %s: %q; the citation says %s: %q", it.From, it.Mail.Text, ct.Author, ct.Text))
	}
	m["verified"], m["author"], m["text"], m["origin_group"] = true, it.From, c.outside(it.From, it.Mail.Text), ct.Group
	m["checked"] = "tx succeeded at that block; its Inbox batch hashes to the root and holds the envelope; the envelope opens with " +
		ct.Group + "'s published keys and is signed by the author's key as registered on the origin"
	if here := c.item(cl.Subject.Hex()); here != nil && here.Mail != nil && !strings.Contains(here.Mail.Text, ct.Text) {
		m["verified"], m["why"] = false, "the relayed post here doesn't carry the origin's text"
	}
	return m, nil
}

func runBridge(c *Client, cmd string, pos []string, network, outFile, origin string) error {
	switch cmd {
	case "bridge cite":
		if err := need(pos, 1, "bridge cite POST-ID [--out FILE]   (on the origin network)"); err != nil {
			return err
		}
		if network == "" {
			return errors.New("this network file has no network name: the citation must name its origin")
		}
		ct, err := c.bridgeCite(pos[0], network)
		if err != nil {
			return err
		}
		if outFile != "" {
			b, _ := json.MarshalIndent(ct, "", "  ")
			if err := os.WriteFile(outFile, b, 0o644); err != nil {
				return err
			}
		}
		out(ct)
	case "bridge post":
		if err := need(pos, 2, "bridge post GROUP CITATION-FILE   (on the destination network)"); err != nil {
			return err
		}
		b, err := os.ReadFile(pos[1])
		if err != nil {
			return err
		}
		var ct Citation
		if err := json.Unmarshal(b, &ct); err != nil {
			return err
		}
		id, uid, err := c.bridgePost(pos[0], &ct)
		if err != nil {
			return err
		}
		out(map[string]any{"post": id, "citation": uid.Hex(), "origin": ct.Network + " tx " + ct.Tx,
			"readers_check": "bridge verify " + uid.Hex() + " --origin ORIGIN-NET.json"})
	case "bridge verify":
		if err := need(pos, 1, "bridge verify CITATION-UID --origin ORIGIN-NET.json"); err != nil {
			return err
		}
		if origin == "" {
			return errors.New("--origin: the origin network's net.json (its RPC and contracts)")
		}
		v, err := c.bridgeVerify(common.HexToHash(pos[0]), origin)
		if err != nil {
			return err
		}
		out(v)
	default:
		return unknownCommand(cmd)
	}
	return nil
}
