// Epoch and channel keys come in two kinds, chosen by whoever will read with them:
//
//	light (default)  X25519: ~100 bytes per reader in each envelope
//	post-quantum     ML-KEM-768 + X25519 hybrid: ~1.1 kB per reader; protects
//	                 today's mail against a future quantum computer
//
// A sender seals to whatever keys its readers published, so the two mix freely.
package main

import (
	"bytes"
	"compress/flate"
	"io"
	"strings"

	"filippo.io/age"
)

func newEpochIdentity(pq bool) (identity, recipient string, err error) {
	if pq {
		id, err := age.GenerateHybridIdentity()
		if err != nil {
			return "", "", err
		}
		return id.String(), id.Recipient().String(), nil
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", err
	}
	return id.String(), id.Recipient().String(), nil
}

func parseIdentity(s string) (age.Identity, string, error) {
	s = strings.TrimSpace(s)
	if id, err := age.ParseX25519Identity(s); err == nil {
		return id, id.Recipient().String(), nil
	}
	id, err := age.ParseHybridIdentity(s)
	if err != nil {
		return nil, "", err
	}
	return id, id.Recipient().String(), nil
}

func parseRecipient(s string) (age.Recipient, error) {
	if r, err := age.ParseX25519Recipient(s); err == nil {
		return r, nil
	}
	return age.ParseHybridRecipient(s)
}

// Plaintext is compressed before sealing when that makes it smaller
// (ciphertext can't be compressed). Marked by a leading 0x01; a datagram
// (JSON) starts with '{', so old envelopes read as before.
const compressedMark = 0x01

func compressPlain(p []byte) []byte {
	var b bytes.Buffer
	b.WriteByte(compressedMark)
	w, _ := flate.NewWriter(&b, flate.BestCompression)
	w.Write(p)
	w.Close()
	if b.Len() < len(p) {
		return b.Bytes()
	}
	return p
}

func decompressPlain(p []byte) ([]byte, error) {
	if len(p) == 0 || p[0] != compressedMark {
		return p, nil
	}
	return io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(p[1:])), 256<<10))
}
