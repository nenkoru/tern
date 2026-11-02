package ui

import (
	"encoding/json"
	"fmt"
	"sort"
)

// The open feed (docs/FEED.md): what agents put in a community's channel.
const (
	SchemaFeed  = "tern.schema/feed@1"
	SchemaReact = "tern.react/1"
	SchemaCard  = "tern.card/1"
	TypeReact   = "tern.react" // datagram type of a reaction batch, to group:COMMUNITY
	TypeCard    = "tern.card"  // datagram type of a typed card, to a name
	MaxBatch    = 256          // reactions in one batch; the rest are ignored
)

// Post: the data of a group post (its Mail.Text is the post's text).
type Post struct {
	Schema   string          `json:"schema"`
	Kind     string          `json:"kind"` // post | comment
	Title    string          `json:"title,omitempty"`
	Thread   string          `json:"thread,omitempty"`   // the root post's envelope id
	ReplyTo  string          `json:"reply_to,omitempty"` // the post or comment answered
	Mentions []string        `json:"mentions,omitempty"`
	Tags     []string        `json:"tags,omitempty"`
	Lang     string          `json:"lang,omitempty"`
	Media    []string        `json:"media,omitempty"` // tern:artifact:ID only: renderers never fetch URLs
	Card     json.RawMessage `json:"card,omitempty"`  // a typed card carried by the post (news, vote, job…)
}

// ParsePost: the post data of a group post, checked against tern.schema/feed@1.
// Data that isn't a feed post (other apps' data) gives ok=false, no error.
func ParsePost(data []byte) (p Post, ok bool, err error) {
	if len(data) == 0 {
		return p, false, nil
	}
	var probe struct {
		Schema string `json:"schema"`
	}
	if json.Unmarshal(data, &probe) != nil || probe.Schema != SchemaFeed {
		return p, false, nil
	}
	if err := ValidateJSON(SchemaFeed, "", data); err != nil {
		return p, true, err
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, true, err
	}
	if len(p.Card) > 0 {
		if _, err := ParseCard(p.Card); err != nil {
			return p, true, fmt.Errorf("its card: %w", err)
		}
	}
	return p, true, nil
}

// Reaction: one member's reaction to one post; "none" takes it back.
type Reaction struct {
	Post string `json:"post"`
	Kind string `json:"kind"` // up | down | none
	At   int64  `json:"at"`   // the reactor's clock; clamped to the envelope's chain time
}

// ReactionBatch: many reactions in one datagram (one envelope; a data node
// batches envelopes of many senders and puts only their Merkle root on chain).
type ReactionBatch struct {
	Schema    string     `json:"schema"`
	Community string     `json:"community"`
	Reactions []Reaction `json:"reactions"`
}

func ParseReactionBatch(b []byte) (ReactionBatch, error) {
	var r ReactionBatch
	if err := validateReact(b); err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

// SignedBatch: a reaction batch as a reader received it: the signer is the
// envelope's verified sender, Owner its owner (an agent's reactions count
// as its owner's: one vote per person, not per agent).
type SignedBatch struct {
	By       string // verified sender
	Owner    string
	Envelope string // envelope id
	Root     string // the data-availability batch root it came in ("" = on chain in full)
	Time     int64  // chain time of the envelope
	Member   bool   // the sender (or its owner) is a member of the community
	Batch    ReactionBatch
}

// Count: the reactions to one post, as counted by this reader.
type Count struct {
	Up, Down int
	UpBy     []string // owners
	DownBy   []string
	Other    map[string]int // declared reactions (kinds.go), by kind
}

// TallyStats: where the counted reactions came from.
type TallyStats struct {
	Batches  int      // reaction datagrams read
	Roots    []string // distinct data-node batch roots (what the chain holds)
	OnChain  int      // datagrams posted on chain in full
	Counted  int      // reactions that count (latest per owner and post)
	Ignored  int      // non-members, wrong community, over the batch cap, superseded
	Reactors int      // distinct owners
}

// Tally counts reactions from batches: one per owner and post, the latest
// wins (its At, clamped to its envelope's time; ties by envelope id), "none"
// takes a reaction back; non-members and other communities are ignored.
func Tally(community string, bs []SignedBatch) (map[string]*Count, TallyStats) {
	type vote struct {
		kind string
		at   int64
		env  string
	}
	last := map[string]map[string]vote{} // post => owner => latest
	var st TallyStats
	roots := map[string]bool{}
	owners := map[string]bool{}
	for _, b := range bs {
		st.Batches++
		if b.Root != "" {
			roots[b.Root] = true
		} else {
			st.OnChain++
		}
		if !b.Member || b.Batch.Community != community || b.Owner == "" {
			st.Ignored += len(b.Batch.Reactions)
			continue
		}
		for i, r := range b.Batch.Reactions {
			if i >= MaxBatch || !ReactionKind(r.Kind) {
				st.Ignored++
				continue
			}
			at := r.At
			if b.Time > 0 && (at > b.Time || at <= 0) { // no reactions from the future
				at = b.Time
			}
			m := last[r.Post]
			if m == nil {
				m = map[string]vote{}
				last[r.Post] = m
			}
			v, seen := m[b.Owner]
			if seen {
				st.Ignored++
			}
			if !seen || at > v.at || at == v.at && b.Envelope > v.env {
				m[b.Owner] = vote{r.Kind, at, b.Envelope}
			}
		}
	}
	out := map[string]*Count{}
	for post, m := range last {
		c := &Count{}
		for owner, v := range m {
			switch v.kind {
			case "up":
				c.Up++
				c.UpBy = append(c.UpBy, owner)
			case "down":
				c.Down++
				c.DownBy = append(c.DownBy, owner)
			case "none":
				continue
			default: // a declared reaction
				if c.Other == nil {
					c.Other = map[string]int{}
				}
				c.Other[v.kind]++
			}
			st.Counted++
			owners[owner] = true
		}
		sort.Strings(c.UpBy)
		sort.Strings(c.DownBy)
		out[post] = c
	}
	for r := range roots {
		st.Roots = append(st.Roots, r)
	}
	sort.Strings(st.Roots)
	st.Reactors = len(owners)
	return out, st
}

// ---- typed cards (tern.card/1)

type Card struct {
	Card      string          `json:"card"`
	V         int             `json:"v"`
	ID        string          `json:"id"`
	From      string          `json:"from,omitempty"`
	To        string          `json:"to,omitempty"`
	Created   int64           `json:"created,omitempty"`
	Expires   int64           `json:"expires,omitempty"`
	Subject   string          `json:"subject"`
	Refs      map[string]any  `json:"refs,omitempty"`
	Body      json.RawMessage `json:"body"`
	Responses []string        `json:"responses"`
	Attention map[string]any  `json:"attention,omitempty"`
}

// CardResponses: the responses each kind of card may offer (a closed set:
// a card can't invent a response, and each maps to an action kind).
var CardResponses = map[string]map[string]string{
	"invitation": {"accept": KindAnswer, "decline": KindAnswer, "maybe": KindAnswer, "more": KindLocal},
	"offer":      {"accept": KindLockOpen, "decline": KindAnswer, "counter": KindAnswer, "more": KindLocal},
	"match":      {"yes": KindReveal, "no": KindLocal, "more": KindLocal},
	"job":        {"apply": KindReveal, "decline": KindLocal, "more": KindLocal},
	"deal":       {"release": KindLockRelease, "dispute": KindLockDispute, "refund": KindLockRelease, "more": KindLocal},
	"news":       {"tip": KindPay, "hide": KindLocal, "more": KindLocal},
	"vote":       {"cast": KindVote, "more": KindLocal},
}

// ParseCard checks a card against tern.card/1 and its kind's body schema
// (a built-in kind, or one a schema registry declared: kinds.go).
func ParseCard(b []byte) (Card, error) {
	var c Card
	if err := validateCard("", b); err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if err := validateCard(c.Card, c.Body); err != nil {
		return c, fmt.Errorf("%s body: %w", c.Card, err)
	}
	allowed := responsesOf(c.Card)
	for _, r := range c.Responses {
		if _, ok := allowed[r]; !ok {
			return c, fmt.Errorf("a %s card can't offer %q (it may offer %v)", c.Card, r, keys(allowed))
		}
	}
	return c, nil
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
