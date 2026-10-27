// Small files inside sealed mail: up to 64 kB, sealed like the text. Larger
// files go as artifacts (sealed, on any storage) with their key in the mail.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const attachMax = 64 << 10

type Attachment struct {
	Name   string `json:"name"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
	B64    string `json:"b64"`
}

func attachFile(path string) (json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) > attachMax {
		return nil, fmt.Errorf("%s is %d bytes: attach up to 64 kB; send larger files as an artifact (artifact add, then the key in mail)", path, len(b))
	}
	h := sha256.Sum256(b)
	return json.Marshal(map[string]Attachment{"attachment": {Name: filepath.Base(path), Size: len(b), SHA256: hex.EncodeToString(h[:]), B64: base64.StdEncoding.EncodeToString(b)}})
}

// saveAttachment writes a message's attachment to path, after checking its hash.
func saveAttachment(data json.RawMessage, path string) (*Attachment, error) {
	var d struct {
		Attachment *Attachment `json:"attachment"`
	}
	if json.Unmarshal(data, &d) != nil || d.Attachment == nil {
		return nil, errors.New("this message has no attachment")
	}
	b, err := base64.StdEncoding.DecodeString(d.Attachment.B64)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != d.Attachment.SHA256 {
		return nil, errors.New("attachment damaged: hash mismatch")
	}
	return d.Attachment, os.WriteFile(path, b, 0o644)
}
