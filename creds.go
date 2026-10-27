// Credentials as SD-JWT (RFC 9901, Selective Disclosure for JWTs), signed
// with ES256K (secp256k1, RFC 8812), the keys Tern names already have.
//
//	issuance:      <issuer-signed JWT>~<disclosure>~<disclosure>~
//	presentation:  <issuer-signed JWT>~<chosen disclosures>~<KB-JWT>
//
// The issuer (an ID service, a university, an employer — any name) anchors a
// hash of its JWT on chain as an EAS claim of kind "credential": issuance
// time and revocation. The holder shows only the claims a verifier needs,
// to that verifier only (the KB-JWT names it), in sealed mail. Each
// verifier decides which issuers it trusts.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	tCredential   = "tern.credential"
	tPresentation = "tern.presentation"
)

var b64u = base64.RawURLEncoding

func ctxb() context.Context { return context.Background() }

// HeldCred is what the holder keeps: the issued SD-JWT with every disclosure.
type HeldCred struct {
	SDJWT string `json:"sd_jwt"`
}

type sdPayload struct {
	Iss   string            `json:"iss"`
	Sub   string            `json:"sub"`
	Iat   int64             `json:"iat"`
	Exp   int64             `json:"exp"`
	Vct   string            `json:"vct"`
	SD    []string          `json:"_sd"`
	SDAlg string            `json:"_sd_alg"`
	Cnf   map[string]string `json:"cnf"`
}

type kbPayload struct {
	Iat    int64  `json:"iat"`
	Aud    string `json:"aud"`
	Nonce  string `json:"nonce"`
	SDHash string `json:"sd_hash"`
}

// es256kSign signs SHA-256(signing input) and returns the compact JWS (r||s signature).
func es256kSign(c *Client, header, payload any) (string, error) {
	h, _ := json.Marshal(header)
	p, _ := json.Marshal(payload)
	input := b64u.EncodeToString(h) + "." + b64u.EncodeToString(p)
	d := sha256.Sum256([]byte(input))
	sig, err := crypto.Sign(d[:], c.key)
	if err != nil {
		return "", err
	}
	return input + "." + b64u.EncodeToString(sig[:64]), nil
}

// es256kSigners returns the addresses that may have made a JWS (both recovery ids) and its payload.
func es256kSigners(jws string) ([]common.Address, []byte, error) {
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		return nil, nil, errors.New("not a JWS")
	}
	sig, err := b64u.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, nil, errors.New("bad ES256K signature")
	}
	d := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	var out []common.Address
	for v := byte(0); v < 2; v++ {
		if pub, err := crypto.SigToPub(d[:], append(append([]byte{}, sig...), v)); err == nil {
			out = append(out, crypto.PubkeyToAddress(*pub))
		}
	}
	payload, err := b64u.DecodeString(parts[1])
	return out, payload, err
}

func disclosureDigest(d string) string {
	h := sha256.Sum256([]byte(d))
	return b64u.EncodeToString(h[:])
}

func jwtHash(jwt string) common.Hash { return crypto.Keccak256Hash([]byte(jwt)) }

func (c *Client) chainNow() int64 {
	if perfFixes && c.ch.mine == nil { // asked many times per action: a reading under 250 ms old will do (blocks come every second; a simulated chain jumps)
		if v, ok := c.ch.nowT.Load().(*headAt); ok && time.Since(v.at) < 250*time.Millisecond {
			return int64(v.n)
		}
	}
	if h, err := c.ch.b.HeaderByNumber(ctxb(), nil); err == nil {
		if perfFixes {
			c.ch.nowT.Store(&headAt{h.Time, time.Now()})
		}
		return int64(h.Time)
	}
	return time.Now().Unix()
}

// issue: called by the issuer. It anchors the credential and sends it sealed to the subject.
func (c *Client) issue(subject string, claims map[string]string, days int) (common.Hash, error) {
	var disclosures, digests []string
	names := make([]string, 0, len(claims))
	for n := range claims {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		arr, _ := json.Marshal([]string{b64u.EncodeToString(randBytes(16)), n, claims[n]})
		d := b64u.EncodeToString(arr)
		disclosures = append(disclosures, d)
		digests = append(digests, disclosureDigest(d))
	}
	sort.Strings(digests)
	now := c.chainNow()
	jwt, err := es256kSign(c, map[string]string{"alg": "ES256K", "typ": "dc+sd-jwt", "kid": "tern:" + c.f.Name},
		sdPayload{Iss: c.f.Name, Sub: subject, Iat: now, Exp: now + int64(days)*86400, Vct: "tern-credential", SD: digests,
			SDAlg: "sha-256", Cnf: map[string]string{"kid": "tern:" + subject}})
	if err != nil {
		return common.Hash{}, err
	}
	h := jwtHash(jwt)
	if _, err := c.easAttest(h, "credential", nil, 0, 0, common.Hash{}); err != nil {
		return common.Hash{}, err
	}
	if _, err := c.sendDatagram(tCredential, subject, HeldCred{SDJWT: jwt + "~" + strings.Join(disclosures, "~") + "~"}); err != nil {
		return common.Hash{}, err
	}
	return h, nil
}

// claimsOf lists the issuer and the claim names in a held credential.
func (h HeldCred) claimsOf() (issuer string, names []string) {
	parts := strings.Split(strings.TrimSuffix(h.SDJWT, "~"), "~")
	if _, p, err := es256kSigners(parts[0]); err == nil {
		var pl sdPayload
		json.Unmarshal(p, &pl)
		issuer = pl.Iss
	}
	for _, d := range parts[1:] {
		raw, err := b64u.DecodeString(d)
		var arr []string
		if err == nil && json.Unmarshal(raw, &arr) == nil && len(arr) == 3 {
			names = append(names, arr[1])
		}
	}
	return
}

// presentTo discloses some of a held credential's claims to a verifier. For
// an agent it goes through the guard (audit 4 AS-2): scopes.to and taint as
// for any mail, and the owner approves every new verifier once
// (owner approve AGENT cred:VERIFIER), since the claims are the owner's.
func (c *Client) presentTo(h HeldCred, names []string, verifier string) error {
	if err := c.guardSend(verifier, ""); err != nil {
		return fmt.Errorf("cred present: %w", err)
	}
	if !c.isOwner() && !c.family(verifier) {
		if err := c.useApproval("cred:"+verifier, nil, "presenting credential claims "+strings.Join(names, ",")+" to "+verifier); err != nil {
			return err
		}
	}
	p, err := c.present(h, names, verifier)
	if err != nil {
		return err
	}
	_, err = c.sendDatagram(tPresentation, verifier, p)
	return err
}

// present builds an SD-JWT presentation of some claims, for one verifier.
func (c *Client) present(h HeldCred, names []string, verifier string) (string, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	parts := strings.Split(strings.TrimSuffix(h.SDJWT, "~"), "~")
	chosen := []string{parts[0]}
	found := 0
	for _, d := range parts[1:] {
		raw, err := b64u.DecodeString(d)
		var arr []string
		if err == nil && json.Unmarshal(raw, &arr) == nil && len(arr) == 3 && want[arr[1]] {
			chosen = append(chosen, d)
			found++
		}
	}
	if found != len(want) {
		return "", errors.New("the credential lacks some of those claims")
	}
	sdPart := strings.Join(chosen, "~") + "~"
	sh := sha256.Sum256([]byte(sdPart))
	kb, err := es256kSign(c, map[string]string{"alg": "ES256K", "typ": "kb+jwt"},
		kbPayload{Iat: c.chainNow(), Aud: verifier, Nonce: b64u.EncodeToString(randBytes(12)), SDHash: b64u.EncodeToString(sh[:])})
	if err != nil {
		return "", err
	}
	return sdPart + kb, nil
}

// verifyPresentation checks everything and returns the disclosed claims.
// trusted: issuers this verifier accepts (the application chooses).
func (c *Client) verifyPresentation(pres string, trusted map[string]bool) (map[string]string, error) {
	i := strings.LastIndex(pres, "~")
	if i < 0 {
		return nil, errors.New("not an SD-JWT presentation")
	}
	sdPart, kb := pres[:i+1], pres[i+1:]
	parts := strings.Split(strings.TrimSuffix(sdPart, "~"), "~")
	issuerAddrs, payload, err := es256kSigners(parts[0])
	if err != nil {
		return nil, err
	}
	var pl sdPayload
	if err := json.Unmarshal(payload, &pl); err != nil || pl.SDAlg != "sha-256" {
		return nil, errors.New("not a Tern SD-JWT")
	}
	if trusted != nil && !trusted[pl.Iss] { // nil: any issuer (the reader judges it)
		return nil, fmt.Errorf("issuer %s is not one this verifier trusts", pl.Iss)
	}
	// The anchor: when it was issued, and whether it was revoked.
	h := jwtHash(parts[0])
	anchors, err := c.easClaims(&h)
	if err != nil {
		return nil, err
	}
	var anchor *Claim2
	for _, a := range anchors {
		if a.Kind == "credential" && a.Issuer == pl.Iss {
			anchor = a
		}
	}
	if anchor == nil {
		return nil, errors.New("credential is not anchored on chain")
	}
	if anchor.Revoked {
		return nil, errors.New("the issuer revoked this credential")
	}
	bt, _ := c.ch.blockTime(anchor.Block)
	ok := false
	for _, a := range issuerAddrs {
		if c.res.verifySigner(pl.Iss, a, anchor.Block, bt) == nil {
			ok = true
		}
	}
	if !ok {
		return nil, errors.New("not signed by the issuer's key")
	}
	if c.chainNow() > pl.Exp {
		return nil, errors.New("credential expired")
	}
	// Key binding: the holder (the subject, now) made this presentation for us.
	holderAddrs, kbRaw, err := es256kSigners(kb)
	if err != nil {
		return nil, fmt.Errorf("key binding: %w", err)
	}
	var k kbPayload
	if err := json.Unmarshal(kbRaw, &k); err != nil {
		return nil, err
	}
	if k.Aud != c.f.Name && k.Aud != c.f.Owner {
		return nil, errors.New("presentation was made for someone else")
	}
	sh := sha256.Sum256([]byte(sdPart))
	if k.SDHash != b64u.EncodeToString(sh[:]) {
		return nil, errors.New("key binding does not cover these disclosures")
	}
	head, _ := c.ch.b.BlockNumber(ctxb())
	ht, _ := c.ch.blockTime(head)
	bound := false
	for _, a := range holderAddrs {
		if c.res.verifySigner(pl.Sub, a, head, ht) == nil {
			bound = true
		}
	}
	if !bound {
		return nil, errors.New("presented by someone other than the subject")
	}
	digests := map[string]bool{}
	for _, d := range pl.SD {
		digests[d] = true
	}
	out := map[string]string{"_issuer": pl.Iss, "_subject": pl.Sub}
	for _, d := range parts[1:] {
		if !digests[disclosureDigest(d)] {
			return nil, errors.New("a disclosure was not issued")
		}
		raw, _ := b64u.DecodeString(d)
		var arr []string
		if json.Unmarshal(raw, &arr) != nil || len(arr) != 3 {
			return nil, errors.New("bad disclosure")
		}
		out[arr[1]] = arr[2]
	}
	return out, nil
}

// presentationView: a received presentation, checked at once against the
// chain for any issuer; whether to trust the issuer is the reader's call.
func (c *Client) presentationView(it *Item) map[string]any {
	var p string
	json.Unmarshal(it.Body, &p)
	claims, err := c.verifyPresentation(p, nil)
	if err != nil {
		return map[string]any{"presentation": "FAILED: " + err.Error()}
	}
	iss := claims["_issuer"]
	disclosed := map[string]string{}
	for k, v := range claims {
		if !strings.HasPrefix(k, "_") {
			disclosed[c.claimName(iss, k)] = c.outside(iss, v)
		}
	}
	return map[string]any{"presentation": "verified: anchored on chain, not revoked or expired, signed by its issuer, presented to you by its subject",
		"issuer": iss, "subject": claims["_subject"], "claims": disclosed,
		"note": "the claims are the issuer's word: trust " + iss + " or not ('cred check --issuers a,b' accepts only those you name)"}
}

func (c *Client) revokeCredential(h common.Hash) error {
	cls, err := c.easClaims(&h)
	if err != nil {
		return err
	}
	for _, cl := range cls {
		if cl.Kind == "credential" && cl.Attester == c.me() && !cl.Revoked {
			return c.easRevoke(cl.UID)
		}
	}
	return errors.New("no such credential of yours (cred list shows them)")
}

func (c *Client) heldCredentials() []HeldCred {
	var out []HeldCred
	for _, it := range c.st.Items {
		if it.Type == tCredential && it.Status != "refused" {
			var h HeldCred
			if json.Unmarshal(it.Body, &h) == nil && h.SDJWT != "" {
				out = append(out, h)
			}
		}
	}
	return out
}

// claimName: a claim's name is its issuer's words too (audit 4 AS-9): a
// plain identifier is shown as is, anything else fenced (and tainting).
func (c *Client) claimName(issuer, k string) string {
	plain := len(k) > 0 && len(k) <= 40
	for _, r := range k {
		plain = plain && (r == '_' || r == '-' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
	}
	if plain {
		return k
	}
	return c.outside(issuer, k)
}
