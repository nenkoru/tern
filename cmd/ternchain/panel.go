// A case before a panel: a hearing, sealed evidence, rulings, settlement.
//
//	panel open     the payer locks the money; the panel, k, and the hearing's end are in the lock
//	panel evidence a party (or panelist) seals a file as an artifact: the hash and time go on
//	               chain as a claim about the case, the key goes sealed to the panel and the other side
//	panel argue    the same for a written argument
//	panel case     everything about a case: parties, panel, time left, evidence (opened and checked)
//	panel rule     a panelist's ruling, accepted only after the hearing (the contract ignores earlier ones)
//	panel settle   applies the panel's rulings: k of them, lower median share
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/nenkoru/tern/internal/artifact"
)

const tEvidence = "tern.evidence"

// panelAfterEvidence: the hearing lasts at least this long after its first
// evidence, so a hearing set at the lock's opening can't close before the
// delivery is in. Clients enforce it (the contract only knows hearingUntil,
// which is never later than this).
const panelAfterEvidence = 180

type EvidenceDelivery struct {
	Lock     uint64 `json:"lock"`
	Artifact uint64 `json:"artifact"`
	Key      []byte `json:"key"`
	URI      string `json:"uri"`
	Note     string `json:"note"`
}

type PanelCase struct {
	Lock         uint64
	Payer, Payee string
	Panel        []string
	K            uint8
	Subject      common.Hash
	HearingUntil uint64
	Total        *big.Int
}

func (c *Client) panelCase(lock uint64) (*PanelCase, error) {
	l, err := c.ch.call("Bank", "locks", u(lock))
	if err != nil {
		return nil, err
	}
	if c.conditionName(l[7].(common.Address)) != "PanelCondition" {
		return nil, fmt.Errorf("lock %d is not a panel case", lock)
	}
	v, err := panelDemandType().Unpack(l[8].([]byte))
	if err != nil || len(v) != 1 {
		return nil, errors.New("bad panel demand")
	}
	d := *abiConvert[panelDemand](v[0])
	pc := &PanelCase{Lock: lock, Payer: c.nameOr(common.Hash(l[1].([32]byte))), Payee: c.nameOr(common.Hash(l[2].([32]byte))),
		K: d.K, Subject: common.Hash(d.Subject), HearingUntil: d.HearingUntil, Total: l[3].(*big.Int)}
	for _, p := range d.Panel {
		pc.Panel = append(pc.Panel, c.nameOr(common.Hash(p)))
	}
	return pc, nil
}

// involved: a party, a panelist, or the owner or an agent of one.
func (pc *PanelCase) role(name string) string {
	for _, p := range pc.Panel {
		if name == p || ownerName(name) == p {
			return "panelist"
		}
	}
	for _, p := range []string{pc.Payer, pc.Payee} {
		if name == p || ownerName(name) == ownerName(p) {
			return "party"
		}
	}
	return ""
}

// party: is this name on the payer's or the payee's side (same owner)? The
// contract ignores such panelists' rulings: nobody judges their own case.
func isParty(name, payer, payee string) bool {
	return ownerName(name) == ownerName(payer) || ownerName(name) == ownerName(payee)
}

// hearing: when the case's hearing ends, and when its first evidence came (0: none).
// It ends at hearingUntil, or panelAfterEvidence after the first evidence if later.
func (c *Client) hearing(pc *PanelCase) (end, first uint64, err error) {
	claims, err := c.easClaims(&pc.Subject)
	if err != nil {
		return 0, 0, err
	}
	for _, cl := range claims {
		if cl.Kind == "evidence" && !cl.Revoked && c.inCase(pc, cl.Issuer) && (first == 0 || cl.Time < first) {
			first = cl.Time
		}
	}
	return hearingEnd(pc.HearingUntil, first), first, nil
}

func hearingEnd(until, first uint64) uint64 {
	if first != 0 && first+panelAfterEvidence > until {
		return first + panelAfterEvidence
	}
	return until
}

// panelEvidence seals a file for the case, timestamps it on chain, and sends
// the key to everyone else in the case.
func (c *Client) panelEvidence(lock uint64, path, note string) (uint64, error) {
	pc, err := c.panelCase(lock)
	if err != nil {
		return 0, err
	}
	if !c.inCase(pc, c.f.Name) {
		return 0, errors.New("you are not in this case: only its parties send evidence (panel case CASE shows them)")
	}
	end, first, err := c.hearing(pc)
	if err != nil {
		return 0, err
	}
	// The first evidence opens the hearing whenever it comes (until the case is settled).
	if now := uint64(c.chainNow()); now >= end && first != 0 {
		return 0, errors.New("the hearing is closed: evidence is no longer taken")
	}
	if rem, err := c.ch.call("Bank", "remaining", u(lock)); err == nil && rem[0].(*big.Int).Sign() == 0 {
		return 0, errors.New("the case is settled: evidence is no longer taken")
	}
	plain, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	ct, hash, key, err := artifact.SealFile(plain)
	if err != nil {
		return 0, err
	}
	enc, _ := filepath.Abs(path + ".enc")
	if err := os.WriteFile(enc, ct, 0o644); err != nil {
		return 0, err
	}
	art, err := c.registerArtifact(hash, uint64(len(ct)), enc, "evidence", new(big.Int), key)
	if err != nil {
		return 0, err
	}
	ph := sha256.Sum256(plain)
	rec, _ := json.Marshal(map[string]any{"artifact": art, "note": note, "plain_sha256": hex.EncodeToString(ph[:])})
	if _, err := c.easAttest(pc.Subject, "evidence", rec, 0, 0, common.Hash{}); err != nil {
		return art, err
	}
	var failed []string
	for _, to := range c.caseRecipients(pc) {
		if to == c.f.Name || ownerName(to) == ownerName(c.f.Name) {
			continue
		}
		// One recipient offline must not keep the key from the others.
		if _, err := c.sendDatagram(tEvidence, to, EvidenceDelivery{Lock: lock, Artifact: art, Key: key, URI: enc, Note: note}); err != nil {
			failed = append(failed, to+" ("+err.Error()+")")
		}
	}
	if len(failed) > 0 {
		return art, fmt.Errorf("evidence %d is on chain and its key went to the others, not to: %s; send again later with 'panel evidence'", art, strings.Join(failed, "; "))
	}
	return art, nil
}

// caseRecipients: who gets a case's evidence keys: the parties and the panel.
// A community has no mailbox: its admins get the key for it.
func (c *Client) caseRecipients(pc *PanelCase) []string {
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, p := range append([]string{pc.Payer, pc.Payee}, pc.Panel...) {
		if c.ch.has("Commons") && c.isCommunity(p) {
			if cm, err := c.community(p); err == nil {
				for _, h := range c.roleHolders(cm.Id) {
					if h["role"] == "admin" {
						add(fmt.Sprint(h["name"]))
					}
				}
			}
			continue
		}
		add(p)
	}
	return out
}

func (c *Client) panelArgue(lock uint64, text string) (uint64, error) {
	dir := filepath.Join(c.dir, "evidence")
	os.MkdirAll(dir, 0o700)
	f := filepath.Join(dir, fmt.Sprintf("argument-%d-%d.txt", lock, c.chainNow()))
	if err := os.WriteFile(f, []byte(text), 0o600); err != nil {
		return 0, err
	}
	return c.panelEvidence(lock, f, "argument")
}

// panelView: the whole case, with evidence opened where we hold the key.
func (c *Client) panelView(lock uint64) (map[string]any, error) {
	pc, err := c.panelCase(lock)
	if err != nil {
		return nil, err
	}
	if _, err := c.scan(); err != nil && err != errNotJoined {
		return nil, err
	}
	keys := map[uint64]EvidenceDelivery{}
	for _, it := range c.st.Items {
		var e EvidenceDelivery
		if it.Type == tEvidence && it.Status != "refused" && json.Unmarshal(it.Body, &e) == nil && e.Lock == lock {
			keys[e.Artifact] = e
		}
	}
	now := uint64(c.chainNow())
	end, first, err := c.hearing(pc)
	if err != nil {
		return nil, err
	}
	hearing := "closed: panelists may rule now"
	switch {
	case first == 0 && now >= end:
		hearing = "waiting for the first evidence: it opens the hearing for " + fmt.Sprint(panelAfterEvidence) + " s; panelists may rule now, without evidence"
	case now < end:
		hearing = fmt.Sprintf("open for %d more seconds: evidence and arguments now; rulings before the end don't count", end-now)
	}
	claims, err := c.easClaims(&pc.Subject)
	if err != nil {
		return nil, err
	}
	var evidence, rulings []map[string]any
	for _, cl := range claims {
		if cl.Revoked {
			continue
		}
		switch cl.Kind {
		case "evidence":
			var rec struct {
				Artifact uint64 `json:"artifact"`
				Note     string `json:"note"`
				Plain    string `json:"plain_sha256"`
			}
			json.Unmarshal([]byte(cl.Data), &rec)
			e := map[string]any{"by": cl.Issuer, "time": cl.Time, "artifact": rec.Artifact, "note": c.outside(cl.Issuer, rec.Note)}
			if cl.Time >= end {
				e["late"] = "submitted after the hearing closed"
			}
			if d, ok := keys[rec.Artifact]; ok {
				if text, path, err := c.openEvidence(d, rec.Plain); err != nil {
					e["open"] = "FAILED: " + err.Error()
				} else {
					e["file"] = path
					if text != "" {
						e["text"] = c.outside(cl.Issuer, text)
					}
				}
			} else if c.inCase(pc, cl.Issuer) && ownerName(cl.Issuer) == ownerName(c.f.Name) {
				e["file"] = "(yours)"
			} else {
				if c.inCase(pc, c.f.Name) {
					e["open"] = "key not received yet (it travels as sealed mail): read the case again in a moment"
				} else {
					e["open"] = "no key here (sent only to the panel and the parties)"
				}
			}
			evidence = append(evidence, e)
		case "ruling":
			if pc.role(cl.Issuer) != "panelist" {
				continue
			}
			r := map[string]any{"by": cl.Issuer, "share_bps": cl.Score, "reason": cl.Data, "uid": cl.UID.Hex()}
			if isParty(cl.Issuer, pc.Payer, pc.Payee) {
				r["counts"] = false
				r["why"] = "party: a panelist on the payer's or the payee's side doesn't count"
			} else if c.ruledFor(cl.UID) != c.rulingTag(lock) {
				r["counts"] = false
				r["why"] = "made for another lock about the same subject"
			} else if cl.Time < end {
				r["counts"] = false
				r["why"] = "made during the hearing"
			} else {
				r["counts"] = true
			}
			rulings = append(rulings, r)
		}
	}
	rem, _ := c.ch.call("Bank", "remaining", u(lock))
	m := map[string]any{"case": lock, "payer": pc.Payer, "payee": pc.Payee, "panel": pc.Panel, "k": pc.K, "at_stake": fmtCoins(pc.Total),
		"hearing": hearing, "hearing_until": end, "evidence": evidence, "rulings": rulings, "your_role": pc.role(c.f.Name)}
	if len(rem) > 0 {
		m["remaining"] = fmtCoins(rem[0].(*big.Int))
		switch {
		case rem[0].(*big.Int).Sign() == 0:
			m["status"] = "settled"
		case now < end:
			m["status"] = "hearing"
		default:
			m["status"] = "awaiting rulings"
		}
	}
	return m, nil
}

// openEvidence decrypts a delivered piece of evidence and checks both hashes.
func (c *Client) openEvidence(d EvidenceDelivery, plainHash string) (string, string, error) {
	_, _, _, hash, commit, err := c.artifact(d.Artifact)
	if err != nil {
		return "", "", err
	}
	if commitOf(d.Key) != commit {
		return "", "", errors.New("the key does not match the artifact")
	}
	ct, err := os.ReadFile(d.URI)
	if err != nil {
		return "", "", fmt.Errorf("can't fetch %s: %w", d.URI, err)
	}
	if sha256.Sum256(ct) != [32]byte(hash) {
		return "", "", errors.New("the file is not the one registered on chain")
	}
	plain, err := artifact.OpenFile(ct, d.Key)
	if err != nil {
		return "", "", err
	}
	if ph := sha256.Sum256(plain); hex.EncodeToString(ph[:]) != plainHash {
		return "", "", errors.New("the content does not match the hash timestamped on chain")
	}
	dir := filepath.Join(c.dir, "evidence")
	os.MkdirAll(dir, 0o700)
	out := filepath.Join(dir, fmt.Sprintf("case%d-artifact%d", d.Lock, d.Artifact))
	if err := os.WriteFile(out, plain, 0o600); err != nil {
		return "", "", err
	}
	text := ""
	if s := string(plain); len(s) < 4000 && !strings.ContainsRune(s, 0) {
		text = s
	}
	return text, out, nil
}

// panelRule: a panelist's ruling on the case, once the hearing is over.
func (c *Client) panelRule(lock uint64, shareBps int64, reason string) (common.Hash, error) {
	pc, err := c.panelCase(lock)
	if err != nil {
		return common.Hash{}, err
	}
	if pc.role(c.f.Name) != "panelist" {
		return common.Hash{}, errors.New("you are not on this panel: only its panelists rule (panel case CASE shows them)")
	}
	if isParty(c.f.Name, pc.Payer, pc.Payee) {
		return common.Hash{}, errors.New("you are a party to this case: your ruling would not count")
	}
	end, _, err := c.hearing(pc)
	if err != nil {
		return common.Hash{}, err
	}
	if now := uint64(c.chainNow()); now < end {
		return common.Hash{}, fmt.Errorf("the hearing is open for %d more seconds: read the evidence ('panel case %d'), rule after", end-now, lock)
	}
	if shareBps < 0 || shareBps > 10000 {
		return common.Hash{}, errors.New("share is 0..10000 basis points (of the money at stake, to the payee)")
	}
	return c.decideOn(lock, pc.Subject, "ruling", []byte(reason), shareBps, pc.Payee, false) // made for this case's lock
}

// panelSettleAuto applies the rulings that count, collected from the chain.
func (c *Client) panelSettleAuto(lock uint64) error {
	pc, err := c.panelCase(lock)
	if err != nil {
		return err
	}
	claims, err := c.easClaims(&pc.Subject)
	if err != nil {
		return err
	}
	var uids []common.Hash
	end, _, _ := c.hearing(pc)
	for _, cl := range claims {
		if cl.Kind == "ruling" && !cl.Revoked && cl.Time >= max(end, pc.HearingUntil) && pc.role(cl.Issuer) == "panelist" && !isParty(cl.Issuer, pc.Payer, pc.Payee) &&
			c.ruledFor(cl.UID) == c.rulingTag(lock) {
			uids = append(uids, cl.UID)
		}
	}
	if len(uids) < int(pc.K) {
		return fmt.Errorf("%d of the %d rulings needed so far (only those made after the hearing count)", len(uids), pc.K)
	}
	return c.settlePanel(lock, uids)
}

func commitOf(key []byte) common.Hash { return crypto.Keccak256Hash(key) }

// abiConvert turns an unpacked ABI tuple into a Go struct.
func abiConvert[T any](v any) *T { return abi.ConvertType(v, new(T)).(*T) }
