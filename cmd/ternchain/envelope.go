// The network's one transport primitive: a sealed, signed datagram.
//
//	envelope = "TC2" | epoch u64 | eph [32] | tag [8] | tag2 [8] | len u32 | ct
//
// ct is an age file (X25519, or ML-KEM-768 + X25519 for post-quantum readers;
// one key per epoch) of the plaintext, deflated when that is smaller,
// whose plaintext is a signed datagram of any type. Mail is one type among
// many: the network does not care what agents say, only that it is sealed,
// signed and addressed by tag. Outsiders see size, time and two 8-byte tags:
// one for the recipient, one for the sender's family (sent copies for the owner).
package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const envMagic = "TC2"

type Envelope struct {
	Epoch uint64
	Eph   [32]byte
	Tag   [8]byte
	Tag2  [8]byte
	CT    []byte
	hv    common.Hash // its hash, set when decoded from a batch (perfFixes)
}

func (e *Envelope) encode() []byte {
	var b bytes.Buffer
	b.WriteString(envMagic)
	binary.Write(&b, binary.BigEndian, e.Epoch)
	b.Write(e.Eph[:])
	b.Write(e.Tag[:])
	b.Write(e.Tag2[:])
	binary.Write(&b, binary.BigEndian, uint32(len(e.CT)))
	b.Write(e.CT)
	return b.Bytes()
}

func (e *Envelope) hash() common.Hash {
	if e.hv != (common.Hash{}) {
		return e.hv
	}
	return crypto.Keccak256Hash(e.encode())
}

// decodeBatch splits a posted batch into envelopes.
func decodeBatch(b []byte) ([]Envelope, error) {
	var out []Envelope
	r := bytes.NewReader(b)
	for r.Len() > 0 {
		magic := make([]byte, 3)
		if _, err := io.ReadFull(r, magic); err != nil || string(magic) != envMagic {
			return out, errors.New("bad envelope")
		}
		var e Envelope
		var n uint32
		if binary.Read(r, binary.BigEndian, &e.Epoch) != nil {
			return out, errors.New("short envelope")
		}
		if _, err := io.ReadFull(r, e.Eph[:]); err != nil {
			return out, err
		}
		if _, err := io.ReadFull(r, e.Tag[:]); err != nil {
			return out, err
		}
		if _, err := io.ReadFull(r, e.Tag2[:]); err != nil {
			return out, err
		}
		if binary.Read(r, binary.BigEndian, &n) != nil || int(n) > r.Len() || n > 256<<10 {
			return out, errors.New("bad envelope length")
		}
		e.CT = make([]byte, n)
		io.ReadFull(r, e.CT)
		if perfFixes { // read many times per scan (lookups, receipts, checks): hashed once
			e.hv = crypto.Keccak256Hash(e.encode())
		}
		out = append(out, e)
	}
	return out, nil
}

func encodeBatch(es []Envelope) []byte {
	var b bytes.Buffer
	for i := range es {
		b.Write(es[i].encode())
	}
	return b.Bytes()
}

// tag binds an envelope to a view key without revealing which.
func computeTag(epoch uint64, shared, eph []byte) [8]byte {
	h := sha256.New()
	h.Write([]byte("tern-tag"))
	binary.Write(h, binary.BigEndian, epoch)
	h.Write(shared)
	h.Write(eph)
	var t [8]byte
	copy(t[:], h.Sum(nil))
	return t
}

// seal makes an envelope tagged for view (the recipient) and senderView (the
// sender's family), readable by recipients. With bits > 0 (a network with
// tag buckets, bucket.go) each tag's first two bytes are a bucket hint: the
// top bits name the bucket of that view key in this epoch, the rest are random.
func seal(plain []byte, epoch uint64, view, senderView *ecdh.PublicKey, recipients []string, bits int) (Envelope, error) {
	var rs []age.Recipient
	seen := map[string]bool{}
	for _, s := range recipients {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		r, err := parseRecipient(s)
		if err != nil {
			return Envelope{}, fmt.Errorf("bad recipient key: %w", err)
		}
		rs = append(rs, r)
	}
	if len(rs) == 0 {
		return Envelope{}, errors.New("no recipients")
	}
	var ct bytes.Buffer
	w, err := age.Encrypt(&ct, rs...)
	if err != nil {
		return Envelope{}, err
	}
	w.Write(compressPlain(plain))
	if err := w.Close(); err != nil {
		return Envelope{}, err
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Envelope{}, err
	}
	shared, err := eph.ECDH(view)
	if err != nil {
		return Envelope{}, err
	}
	e := Envelope{Epoch: epoch, CT: ct.Bytes()}
	copy(e.Eph[:], eph.PublicKey().Bytes())
	e.Tag = computeTag(epoch, shared, e.Eph[:])
	if bits > 0 {
		h := bucketHint(epoch, view.Bytes(), bits)
		copy(e.Tag[:2], h[:])
	}
	if senderView != nil {
		s2, err := eph.ECDH(senderView)
		if err != nil {
			return Envelope{}, err
		}
		e.Tag2 = computeTag(epoch, s2, e.Eph[:])
		if bits > 0 {
			h := bucketHint(epoch, senderView.Bytes(), bits)
			copy(e.Tag2[:2], h[:])
		}
	} else {
		randRead(e.Tag2[:])
	}
	return e, nil
}

// mine reports whether the envelope carries the tag of view private key v.
// The first two bytes of a tag may be a bucket hint (bucket.go): the last six
// are compared (a false match is 1 in 2^48 per envelope and key).
func (e *Envelope) mine(v *ecdh.PrivateKey) bool {
	pub, err := ecdh.X25519().NewPublicKey(e.Eph[:])
	if err != nil {
		return false
	}
	shared, err := v.ECDH(pub)
	if err != nil {
		return false
	}
	t := computeTag(e.Epoch, shared, e.Eph[:])
	return [6]byte(t[2:]) == [6]byte(e.Tag[2:]) || [6]byte(t[2:]) == [6]byte(e.Tag2[2:])
}

func (e *Envelope) open(ids []age.Identity) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(e.CT), ids...)
	if err != nil {
		return nil, err
	}
	p, err := io.ReadAll(io.LimitReader(r, 256<<10))
	if err != nil {
		return nil, err
	}
	return decompressPlain(p)
}

// ---- signed datagrams

// Datagram is what an envelope carries: a typed, signed body. Mail is
// type "tern.message"; group keys are "tern.group-key"; applications use
// reverse-DNS types of their own.
type Datagram struct {
	DeliveryID string          `json:"delivery_id,omitempty"`
	Type       string          `json:"type"`
	From       string          `json:"from"`    // a name: "oscar" or "ada.oscar"
	To         string          `json:"to"`      // a name, or "group:<name>"
	Created    int64           `json:"created"` // unix seconds
	Readers    []string        `json:"readers"` // fingerprints of the epoch keys sealed to
	Body       json.RawMessage `json:"body"`
}

type Signed struct {
	Payload []byte   `json:"payload"`        // the exact datagram bytes
	Sig     string   `json:"sig,omitempty"`  // hex secp256k1 signature, or
	Ring    *RingSig `json:"ring,omitempty"` // a ring signature: "one of these members"
	Anon    *AnonSig `json:"anon,omitempty"` // a membership proof: "a leaf of this group's tree" (anon_groups.go)
}

func signDigest(payload []byte) []byte {
	return crypto.Keccak256([]byte("tern-chain-v1:datagram\n"), payload)
}

func signDatagram(d Datagram, key interface{ sign([]byte) ([]byte, error) }) ([]byte, error) {
	p, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	sig, err := key.sign(signDigest(p))
	if err != nil {
		return nil, err
	}
	return json.Marshal(Signed{Payload: p, Sig: hex.EncodeToString(sig)})
}

// openSigned returns the datagram and the address that signed it.
func openSigned(plain []byte) (*Datagram, common.Address, []byte, error) {
	var s Signed
	if err := json.Unmarshal(plain, &s); err != nil {
		return nil, common.Address{}, nil, err
	}
	sig, err := hex.DecodeString(s.Sig)
	if err != nil || len(sig) != 65 {
		return nil, common.Address{}, nil, errors.New("bad signature encoding")
	}
	pub, err := crypto.SigToPub(signDigest(s.Payload), sig)
	if err != nil {
		return nil, common.Address{}, nil, err
	}
	var d Datagram
	if err := json.Unmarshal(s.Payload, &d); err != nil {
		return nil, common.Address{}, nil, err
	}
	if d.Type == "" || d.From == "" || d.To == "" {
		return nil, common.Address{}, nil, errors.New("datagram lacks type, from or to")
	}
	return &d, crypto.PubkeyToAddress(*pub), s.Payload, nil
}

// fingerprint of an age recipient string, for the readers list.
func fingerprint(recipient string) string {
	h := sha256.Sum256([]byte(recipient))
	return hex.EncodeToString(h[:8])
}

func splitKeys(b []byte) []string {
	var out []string
	for _, s := range strings.Split(string(b), "\n") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
