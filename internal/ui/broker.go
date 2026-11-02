package ui

// The UI broker: a surface only proposes. A button press arrives as an
// event (surface, component, event name, the item it belongs to); the
// broker looks the name up in the surface's action table, maps it to a
// typed action of a closed registry, resolves its parameters from the
// data model the agent wrote (never from labels or from the event), and
// hands it to the executor, which runs it through the agent's guard.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

// The closed registry of action kinds.
const (
	KindLocal       = "local"        // open, expand, filter, sort, ask_agent: stays in the shell
	KindReact       = "react"        // a reaction in a community the agent is in (batched, no money)
	KindPay         = "pay"          // a payment (a tip): the guard's free_pay, else the owner's approval
	KindAnswer      = "answer"       // writing to the card's sender (a new correspondent needs approval)
	KindVote        = "vote"         // a vote: irreversible
	KindReveal      = "reveal"       // showing who you are to someone
	KindLockOpen    = "lock.open"    // escrow, bond, channel
	KindLockRelease = "lock.release" //
	KindLockDispute = "lock.dispute" //
	KindContact     = "contact.add"  //
	KindJoin        = "group.join"   //
	KindPost        = "post"         //
	KindBuy         = "buy"          //
	KindInstall     = "install"      // a UI package or a ranking module
)

// Consequential: kinds that act in the world (money, people, irreversible).
var Consequential = map[string]bool{KindPay: true, KindAnswer: true, KindVote: true, KindReveal: true, KindLockOpen: true,
	KindLockRelease: true, KindLockDispute: true, KindContact: true, KindJoin: true, KindPost: true, KindBuy: true, KindInstall: true}

// Known: every kind a surface may declare.
func Known(kind string) bool { return kind == KindLocal || kind == KindReact || Consequential[kind] }

// Event: what the renderer sends when a button is pressed.
type Event struct {
	SurfaceID string         `json:"surfaceId"`
	Component string         `json:"sourceComponentId"`
	Name      string         `json:"name"`
	Scope     string         `json:"scope,omitempty"` // the data path of the repeated item the button is in ("/items/3")
	Context   map[string]any `json:"context,omitempty"`
	Gesture   bool           `json:"gesture"` // a real user activation
}

// Action: a typed action, as the guard and the approver see it.
type Action struct {
	Kind    string            `json:"kind"`
	Params  map[string]string `json:"params"`
	Name    string            `json:"name"`    // the event name
	Surface string            `json:"surface"` // surface id
	Source  string            `json:"source"`  // who made the surface
	Tainted bool              `json:"tainted"` // the surface shows outside data
	Hash    string            `json:"action_hash"`
	Scope   string            `json:"scope,omitempty"`
}

// ActionHash: keccak(kind, params): what an owner's approval is bound to.
func ActionHash(kind string, params map[string]string) string {
	ks := keys(params)
	var b strings.Builder
	b.WriteString("tern.action/1\x00" + kind)
	for _, k := range ks {
		b.WriteString("\x00" + k + "=" + params[k])
	}
	return "0x" + fmt.Sprintf("%x", crypto.Keccak256([]byte(b.String())))
}

// Result: what came of an event.
type Result struct {
	Status  string         `json:"status"` // done | needs_approval | refused | dropped | local
	Message string         `json:"message"`
	Action  *Action        `json:"action,omitempty"`
	Receipt map[string]any `json:"receipt,omitempty"`
	Ask     *Ask           `json:"ask,omitempty"`
}

// Ask: an action waiting for the owner. The trusted sheet draws it from
// these fields, not from anything the surface drew.
type Ask struct {
	ID           string   `json:"id"`
	Action       Action   `json:"action"`
	What         string   `json:"what"`         // in plain words, from the structured action
	Counterparty string   `json:"counterparty"` // who gets the money or the message
	Rating       string   `json:"rating,omitempty"`
	Amount       string   `json:"amount,omitempty"`
	Why          []string `json:"why"`     // the chain of facts that led here
	Command      string   `json:"command"` // the owner's CLI line for exactly this
	Created      int64    `json:"created"`
}

// Executor runs typed actions (through the agent's guard).
type Executor interface {
	Execute(a Action) Result
}

type LogEntry struct {
	Time    int64  `json:"time"`
	Surface string `json:"surface"`
	Name    string `json:"name"`
	Kind    string `json:"kind,omitempty"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type Broker struct {
	mu           sync.Mutex
	surfaces     map[string]*Surface
	exec         Executor
	recent       map[string][]time.Time
	frozen       map[string]bool
	log          []LogEntry
	MaxPerMinute int // consequential actions a surface may emit in a minute before it is frozen (default 6)
	Now          func() time.Time
}

func NewBroker(exec Executor) *Broker {
	return &Broker{surfaces: map[string]*Surface{}, exec: exec, recent: map[string][]time.Time{}, frozen: map[string]bool{}, MaxPerMinute: 6, Now: time.Now}
}

// Register: a surface the shell shows now (its actions become dispatchable).
func (b *Broker) Register(s *Surface) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.surfaces[s.ID] = s
}

func (b *Broker) Surface(id string) *Surface {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.surfaces[id]
}

func (b *Broker) Log() []LogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]LogEntry(nil), b.log...)
}

var scopeRe = regexp.MustCompile(`^(/[a-z_]+/[0-9]{1,4})?$`)

// Dispatch: an event from the renderer → a typed action → the executor.
func (b *Broker) Dispatch(ev Event) Result {
	r, kind := b.resolve(ev)
	b.mu.Lock()
	msg := r.Message
	b.log = append(b.log, LogEntry{Time: b.Now().Unix(), Surface: ev.SurfaceID, Name: ev.Name, Kind: kind, Status: r.Status, Message: msg})
	if len(b.log) > 500 {
		b.log = b.log[len(b.log)-500:]
	}
	b.mu.Unlock()
	return r
}

func (b *Broker) resolve(ev Event) (Result, string) {
	drop := func(f string, a ...any) (Result, string) {
		return Result{Status: "dropped", Message: fmt.Sprintf(f, a...)}, ""
	}
	b.mu.Lock()
	s := b.surfaces[ev.SurfaceID]
	frozen := b.frozen[ev.SurfaceID]
	b.mu.Unlock()
	if s == nil {
		return drop("no surface %q is shown", ev.SurfaceID)
	}
	if frozen {
		return Result{Status: "refused", Message: "this surface is frozen: it sent too many consequential actions"}, ""
	}
	decl, ok := s.Actions[ev.Name]
	if !ok || !Known(decl.Kind) {
		return drop("undeclared action %q from %s: dropped and logged", ev.Name, s.Source)
	}
	if !emits(s, ev.Component, ev.Name) {
		return drop("no button %q on %s emits %q", ev.Component, ev.SurfaceID, ev.Name)
	}
	if !scopeRe.MatchString(ev.Scope) {
		return drop("bad scope %q", ev.Scope)
	}
	if ev.Scope != "" {
		if _, ok := Lookup(s.Data, "", ev.Scope); !ok {
			return drop("no item %s on %s", ev.Scope, ev.SurfaceID)
		}
	}
	params := map[string]string{}
	for k, p := range decl.Params {
		if lit, ok := strings.CutPrefix(p, "="); ok {
			params[k] = lit
			continue
		}
		v, ok := Lookup(s.Data, ev.Scope, p)
		if !ok {
			return Result{Status: "refused", Message: fmt.Sprintf("%s: the data model has no %s", ev.Name, p)}, decl.Kind
		}
		if IsFenced(v) {
			return Result{Status: "refused", Message: fmt.Sprintf("%s: parameter %s would come from someone else's text: refused", ev.Name, k)}, decl.Kind
		}
		switch x := v.(type) {
		case string:
			params[k] = x
		case float64, bool:
			params[k] = fmt.Sprint(x)
		default:
			return Result{Status: "refused", Message: fmt.Sprintf("%s: parameter %s is not a value", ev.Name, k)}, decl.Kind
		}
	}
	a := Action{Kind: decl.Kind, Params: params, Name: ev.Name, Surface: s.ID, Source: s.Source, Tainted: s.Tainted, Scope: ev.Scope}
	a.Hash = ActionHash(a.Kind, a.Params)
	if a.Kind == KindLocal {
		return Result{Status: "local", Message: ev.Name, Action: &a}, a.Kind
	}
	if Consequential[a.Kind] || a.Kind == KindReact {
		if Consequential[a.Kind] && !ev.Gesture {
			return Result{Status: "refused", Message: "a consequential action needs a real click", Action: &a}, a.Kind
		}
		b.mu.Lock()
		now := b.Now()
		var keep []time.Time
		for _, t := range b.recent[s.ID] {
			if now.Sub(t) < time.Minute {
				keep = append(keep, t)
			}
		}
		limit := b.MaxPerMinute
		if a.Kind == KindReact {
			limit *= 5 // reactions are cheap, but a surface clicking for you is still a surface clicking for you
		}
		if len(keep) >= limit {
			b.frozen[s.ID] = true
			b.mu.Unlock()
			return Result{Status: "refused", Message: fmt.Sprintf("surface %s sent more than %d actions a minute: frozen and reported", s.ID, limit), Action: &a}, a.Kind
		}
		b.recent[s.ID] = append(keep, now)
		b.mu.Unlock()
	}
	r := b.exec.Execute(a)
	if r.Action == nil {
		r.Action = &a
	}
	return r, a.Kind
}

// emits: the surface has that button and it emits that event.
func emits(s *Surface, comp, name string) bool {
	for _, c := range s.Components {
		if c["id"] != comp {
			continue
		}
		ev, _ := c["action"].(map[string]any)["event"].(map[string]any)
		return ev["name"] == name
	}
	return false
}

// DeclaredKinds: what a surface (or a package) can ask for, for the install screen.
func DeclaredKinds(actions map[string]ActionDecl) []string {
	seen := map[string]bool{}
	for _, d := range actions {
		seen[d.Kind] = true
	}
	out := keys(seen)
	sort.Strings(out)
	return out
}
