// Card kinds, intents and reactions from the network's schema registry
// (BRICKS D2; internal/ui/kinds.go). The client reads the SchemaNames
// registry (contracts/Schemas.sol: "card/NAME", "intent/NAME",
// "reaction/NAME" → EAS schema uid, versions, a uri) when the deployment has
// one; on a network without it, the built-in seven card kinds, seven intents
// and up/down/none are all there is. schemaRegistry is a hook: another source
// (a test, a different registry) replaces it.
//
// A card kind's newest version names its declaration in its uri:
// data:application/json,{"body": {JSON Schema}, "responses": {"going": "answer", …}}
// (or ;base64; never a file:// path: audit 6 CM-2): the body's schema and each response's
// action from the CLOSED registry. An owner may trust only some publishers:
// <dir>/types.json {"publishers": ["anya", …]} (absent: any).
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nenkoru/tern/internal/ui"
)

// schemaRegistry: the source of declared card kinds, intents and reactions
// for this client's network (nil source: none).
var schemaRegistry = func(c *Client) ui.KindSource {
	if c.ch == nil || !c.ch.has("SchemaNames") {
		return nil
	}
	return &schemaNamesSource{c: c}
}

var kindsOnce sync.Once

// loadKinds: the declared kinds, intents and reactions, read once per
// process. A declaration that would redefine a built-in or reach outside the
// closed action registry is skipped (and logged); the rest are used.
func (c *Client) loadKinds() {
	if schemaRegistry == nil {
		return
	}
	kindsOnce.Do(func() {
		src := schemaRegistry(c)
		if src == nil {
			return
		}
		for _, err := range ui.LoadKinds(src) {
			log.Printf("schema registry: %v", err)
		}
	})
}

// knownIntent: a built-in mail intent, or one the registry declared.
func (c *Client) knownIntent(i string) bool {
	if intents[i] {
		return true
	}
	c.loadKinds()
	return ui.Intent(i)
}

// schemaNamesSource: ui.KindSource over SchemaNames.
type schemaNamesSource struct {
	c     *Client
	once  sync.Once
	byCat map[string][]typeName // card | intent | reaction => names
	err   error
}

type typeName struct {
	name, publisher, uri string
}

func (s *schemaNamesSource) read() {
	s.once.Do(func() {
		s.byCat = map[string][]typeName{}
		c := s.c
		var trust struct {
			Publishers []string `json:"publishers"`
		}
		if c.dir != "" {
			readJSON(filepath.Join(c.dir, "types.json"), &trust)
		}
		trusted := func(p string) bool {
			if len(trust.Publishers) == 0 {
				return true
			}
			for _, t := range trust.Publishers {
				if t == p {
					return true
				}
			}
			return false
		}
		// Names are per publisher (bricks.go schemaNameList, audit 6 CM-3):
		// the pinned publishers' names, or every publisher's (first per name).
		list, err := c.schemaNameList(trust.Publishers)
		if err != nil {
			s.err = err
			return
		}
		for _, e := range list {
			cat, short, ok := strings.Cut(e.name, "/")
			if !ok || cat != "card" && cat != "intent" && cat != "reaction" || !trusted(e.publisher) {
				continue
			}
			s.byCat[cat] = append(s.byCat[cat], typeName{short, e.publisher, e.uri})
		}
	})
}

// versionURI: the uri field of a SchemaNames.Version as the ABI decoder returns it.
func versionURI(v any) string {
	b, _ := json.Marshal(v)
	var x struct {
		URI string `json:"uri"`
		Uri string `json:"Uri"`
	}
	json.Unmarshal(b, &x)
	return x.URI + x.Uri
}

// maxDeclURI: the longest declaration uri read from chain (a 64 KiB
// declaration, base64 or percent-encoded, fits).
const maxDeclURI = 96 << 10

// declOf: a card kind's declaration from its uri, as written on chain by
// any publisher: only an inline data: uri. Never a file:// or other local
// path: a publisher must not make every reader's client open a file of its
// choosing (audit 6 CM-2). Sizes are checked before anything is decoded.
func declOf(uri string) (ui.KindDecl, error) {
	var d ui.KindDecl
	if len(uri) > maxDeclURI {
		return d, fmt.Errorf("declaration uri of %d bytes: over %d KiB", len(uri), maxDeclURI>>10)
	}
	var raw []byte
	switch {
	case strings.HasPrefix(uri, "data:"):
		meta, data, ok := strings.Cut(strings.TrimPrefix(uri, "data:"), ",")
		if !ok {
			return d, errors.New("a data: uri without data")
		}
		if strings.HasSuffix(meta, ";base64") {
			b, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				return d, err
			}
			raw = b
		} else if s, err := url.PathUnescape(data); err == nil {
			raw = []byte(s)
		} else {
			raw = []byte(data)
		}
	default:
		return d, fmt.Errorf("uri %.60q: this client reads only inline data: declarations from the chain (never a local path or a URL)", uri)
	}
	if len(raw) > 64<<10 {
		return d, errors.New("declaration over 64 KiB")
	}
	return d, json.Unmarshal(raw, &d)
}

func (s *schemaNamesSource) CardKinds() ([]ui.KindDecl, error) {
	s.read()
	var out []ui.KindDecl
	var errs []string
	for _, t := range s.byCat["card"] {
		d, err := declOf(t.uri)
		if err != nil {
			errs = append(errs, "card/"+t.name+": "+err.Error())
			continue
		}
		d.Kind, d.Source = t.name, "SchemaNames card/"+t.name+" by "+t.publisher
		out = append(out, d)
	}
	if len(errs) > 0 {
		return out, errors.New(strings.Join(errs, "; "))
	}
	return out, s.err
}

func (s *schemaNamesSource) names(cat string) ([]string, error) {
	s.read()
	var out []string
	for _, t := range s.byCat[cat] {
		out = append(out, t.name)
	}
	return out, s.err
}

func (s *schemaNamesSource) Intents() ([]string, error)   { return s.names("intent") }
func (s *schemaNamesSource) Reactions() ([]string, error) { return s.names("reaction") }
