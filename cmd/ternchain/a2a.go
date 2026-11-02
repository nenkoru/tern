// A2A (github.com/a2aproject/A2A, a2a-protocol.org): a ternchain agent as a
// client of any A2A agent. No server: tern stays the wallet, escrow and guard
// layer, and A2A agents are reached where they are.
//
// a2a card URL reads and checks an Agent Card (/.well-known/agent-card.json).
// a2a send URL|ERC8004-ID TEXT sends one message over the JSON-RPC binding:
// SendMessage (A2A 1.0, the current version) or message/send (0.3, which
// most deployed agents still speak), picked from the card. An ERC-8004 id is
// resolved to the A2A service its registration file names.
//
// The owner's rules apply as to mail: writing to a new A2A agent after
// outside text was read needs the owner's approval for "a2a:HOST" (owner
// approve AGENT a2a:HOST), scopes.to must list it when the grant has a list,
// and the fan-out limit counts it. The reply is outside text: fenced and
// tainting. If the agent's endpoint answers HTTP 402, the payment goes
// through x402 pay: the same guard (x402:HOST), free_pay, approvals and the
// per-epoch limit. Fetches go through extHTTP: no private addresses on a
// public network, bounded sizes and timeouts.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
)

// a2aVersion: the A2A version this client speaks first (a2a-protocol.org/latest/specification: 1.0.0).
const a2aVersion = "1.0"

// a2aInterface: where an agent is reached. 1.0: supportedInterfaces
// {url, protocolBinding, protocolVersion}; 0.3: additionalInterfaces {url, transport}.
type a2aInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding,omitempty"`
	ProtocolVersion string `json:"protocolVersion,omitempty"`
	Transport       string `json:"transport,omitempty"`
	Tenant          string `json:"tenant,omitempty"`
}

type a2aSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

// a2aCard: an Agent Card, 1.0 and 0.3 fields both.
type a2aCard struct {
	Name                 string         `json:"name"`
	Description          string         `json:"description"`
	Version              string         `json:"version"`
	SupportedInterfaces  []a2aInterface `json:"supportedInterfaces"`  // 1.0
	ProtocolVersion      string         `json:"protocolVersion"`      // 0.3
	URL                  string         `json:"url"`                  // 0.3
	PreferredTransport   string         `json:"preferredTransport"`   // 0.3
	AdditionalInterfaces []a2aInterface `json:"additionalInterfaces"` // 0.3
	Provider             *struct {
		Organization string `json:"organization"`
		URL          string `json:"url"`
	} `json:"provider"`
	Capabilities struct {
		Streaming         bool `json:"streaming"`
		PushNotifications bool `json:"pushNotifications"`
		Extensions        []struct {
			URI      string `json:"uri"`
			Required bool   `json:"required"`
		} `json:"extensions"`
	} `json:"capabilities"`
	DefaultInputModes    []string                   `json:"defaultInputModes"`
	DefaultOutputModes   []string                   `json:"defaultOutputModes"`
	Skills               []a2aSkill                 `json:"skills"`
	SecuritySchemes      map[string]json.RawMessage `json:"securitySchemes"`
	Security             json.RawMessage            `json:"security"`
	SecurityRequirements json.RawMessage            `json:"securityRequirements"`
}

// a2aEndpoint: the JSON-RPC endpoint to call and its A2A version ("1.0" or "0.3").
type a2aEndpoint struct{ URL, Version, Tenant string }

// endpoint: a JSON-RPC interface, 1.0 first (the first listed is the agent's preferred).
func (k *a2aCard) endpoint() (a2aEndpoint, error) {
	for _, i := range k.SupportedInterfaces {
		if strings.EqualFold(i.ProtocolBinding, "JSONRPC") && strings.HasPrefix(i.ProtocolVersion, "1.") {
			return a2aEndpoint{i.URL, a2aVersion, i.Tenant}, nil
		}
	}
	for _, i := range k.SupportedInterfaces { // a 0.x interface listed the 1.0 way
		if strings.EqualFold(i.ProtocolBinding, "JSONRPC") && strings.HasPrefix(i.ProtocolVersion, "0.") {
			return a2aEndpoint{i.URL, "0.3", ""}, nil
		}
	}
	if k.URL != "" && (k.PreferredTransport == "" || strings.EqualFold(k.PreferredTransport, "JSONRPC")) {
		return a2aEndpoint{k.URL, "0.3", ""}, nil
	}
	for _, i := range k.AdditionalInterfaces {
		if strings.EqualFold(i.Transport, "JSONRPC") {
			return a2aEndpoint{i.URL, "0.3", ""}, nil
		}
	}
	return a2aEndpoint{}, errors.New("the Agent Card names no JSON-RPC interface (this client speaks A2A's JSON-RPC binding only)")
}

// check: the card's problems against the spec (required fields; a usable,
// http(s) endpoint): warnings, or what makes it unusable.
func (k *a2aCard) check() ([]string, error) {
	var warn []string
	if strings.TrimSpace(k.Name) == "" {
		return nil, errors.New("not an Agent Card: no name")
	}
	e, err := k.endpoint()
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(e.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("the Agent Card's JSON-RPC endpoint is not an http(s) URL: %.100q", e.URL)
	}
	if u.Scheme == "http" && !allowPrivate {
		warn = append(warn, "the endpoint is plain http: anyone on the way reads and changes the conversation")
	}
	if k.Description == "" {
		warn = append(warn, "no description (required)")
	}
	if k.Version == "" {
		warn = append(warn, "no version (required)")
	}
	if len(k.Skills) == 0 {
		warn = append(warn, "no skills (required)")
	}
	if len(k.DefaultInputModes) == 0 || len(k.DefaultOutputModes) == 0 {
		warn = append(warn, "no defaultInputModes/defaultOutputModes (required)")
	}
	if len(k.SupportedInterfaces) == 0 {
		warn = append(warn, "an A2A 0.3 card (url, protocolVersion): spoken with message/send")
	}
	if len(k.SecuritySchemes) > 0 {
		warn = append(warn, "the agent asks for authentication (securitySchemes): this client sends none, so calls may be refused")
	}
	for _, x := range k.Capabilities.Extensions {
		if x.Required {
			warn = append(warn, "the agent requires an extension this client doesn't speak: "+clip(x.URI, 120))
		}
	}
	return warn, nil
}

// a2aCardURL: where the card of an agent at raw is: raw itself if it names
// a .json file, else ORIGIN/.well-known/agent-card.json.
func a2aCardURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("an A2A agent is an http(s) URL, not %q", raw)
	}
	if strings.HasSuffix(u.Path, ".json") {
		return raw, nil
	}
	return u.Scheme + "://" + u.Host + "/.well-known/agent-card.json", nil
}

// a2aFetchCard: the card at raw (or, for a 0.2 agent, /.well-known/agent.json).
func a2aFetchCard(raw string) (*a2aCard, string, []string, error) {
	cu, err := a2aCardURL(raw)
	if err != nil {
		return nil, "", nil, err
	}
	b, err := fetchOutside(cu, erc8004MaxFile)
	var se outsideStatus
	if errors.As(err, &se) && strings.HasSuffix(cu, "/.well-known/agent-card.json") {
		old := strings.TrimSuffix(cu, "agent-card.json") + "agent.json"
		if b2, err2 := fetchOutside(old, erc8004MaxFile); err2 == nil {
			b, cu, err = b2, old, nil
		}
	}
	if err != nil {
		return nil, cu, nil, fmt.Errorf("Agent Card %s: %w", cu, err)
	}
	var k a2aCard
	if err := json.Unmarshal(b, &k); err != nil {
		return nil, cu, nil, fmt.Errorf("Agent Card %s is not JSON: %v", cu, err)
	}
	warn, bad := k.check()
	if bad != nil {
		return nil, cu, nil, fmt.Errorf("Agent Card %s: %w", cu, bad)
	}
	return &k, cu, warn, nil
}

// a2aResolve: the Agent Card URL for a target: a URL, or an ERC-8004 id
// whose registration names an A2A service.
func (c *Client) a2aResolve(target string) (cardURL string, via map[string]any, err error) {
	if !isERC8004ID(target) {
		return target, nil, nil
	}
	id, err := c.parseERC8004ID(target)
	if err != nil {
		return "", nil, err
	}
	_, _, reg, regErr, err := c.erc8004Read(id)
	if err != nil {
		return "", nil, err
	}
	if reg == nil {
		return "", nil, fmt.Errorf("ERC-8004 agent %s: registration file: %v", id, regErr)
	}
	s := reg.service("A2A")
	if s == nil || s.Endpoint == "" {
		return "", nil, fmt.Errorf("ERC-8004 agent %s names no A2A service in its registration file", id)
	}
	return s.Endpoint, map[string]any{"agent_id": id.String(), "a2a_service": clip(s.Endpoint, 300)}, nil
}

// a2aCardView: a card as the agent gets to see it (its words fenced).
func (c *Client) a2aCardView(target string) (map[string]any, error) {
	cu, via, err := c.a2aResolve(target)
	if err != nil {
		return nil, err
	}
	k, cu, warn, err := a2aFetchCard(cu)
	if err != nil {
		return nil, err
	}
	e, _ := k.endpoint()
	label := "a2a:" + hostOf(e.URL)
	r := map[string]any{"card": cu, "endpoint": e.URL, "a2a_version": e.Version, "name": c.outsideLabel(label, clip(k.Name, 200)),
		"streaming": k.Capabilities.Streaming, "input_modes": k.DefaultInputModes, "output_modes": k.DefaultOutputModes}
	if k.Description != "" {
		r["description"] = c.outsideLabel(label, clip(k.Description, 2000))
	}
	if k.Version != "" {
		r["version"] = clip(k.Version, 40)
	}
	if k.Provider != nil && k.Provider.Organization != "" {
		r["provider"] = c.outsideLabel(label, clip(k.Provider.Organization, 200))
	}
	var sk []string
	for _, s := range k.Skills[:min(len(k.Skills), 20)] {
		sk = append(sk, clip(s.Name+": "+s.Description, 300))
	}
	if len(sk) > 0 {
		r["skills"] = c.outsideLabel(label, strings.Join(sk, "\n"))
	}
	if len(warn) > 0 {
		r["warnings"] = warn
	}
	for k, v := range via {
		r[k] = v
	}
	if hostOf(cu) != hostOf(e.URL) {
		r["note"] = "the card names an endpoint on another host than its own"
	}
	return r, nil
}

// ---- message/send and SendMessage

// a2aPart: a part of a message or artifact (1.0: one of text, raw, url,
// data; 0.3: kind + text, file, data).
type a2aPart struct {
	Kind      string          `json:"kind,omitempty"`
	Text      *string         `json:"text,omitempty"`
	Raw       string          `json:"raw,omitempty"`
	URL       string          `json:"url,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	MediaType string          `json:"mediaType,omitempty"`
	Filename  string          `json:"filename,omitempty"`
	File      *struct {
		Name     string `json:"name"`
		MimeType string `json:"mimeType"`
		URI      string `json:"uri"`
		Bytes    string `json:"bytes"`
	} `json:"file,omitempty"`
}

type a2aMessage struct {
	Kind      string         `json:"kind,omitempty"` // 0.3: "message"
	Role      string         `json:"role"`
	Parts     []a2aPart      `json:"parts"`
	MessageID string         `json:"messageId"`
	ContextID string         `json:"contextId,omitempty"`
	TaskID    string         `json:"taskId,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type a2aTask struct {
	Kind      string `json:"kind,omitempty"`
	ID        string `json:"id"`
	ContextID string `json:"contextId"`
	Status    struct {
		State   string      `json:"state"`
		Message *a2aMessage `json:"message"`
	} `json:"status"`
	Artifacts []struct {
		Name  string    `json:"name"`
		Parts []a2aPart `json:"parts"`
	} `json:"artifacts"`
	History  []a2aMessage   `json:"history"`
	Metadata map[string]any `json:"metadata"`
}

// a2aRequest: the JSON-RPC request for one text message.
func a2aRequest(e a2aEndpoint, text, contextID, taskID string) ([]byte, string) {
	var n [16]byte
	rand.Read(n[:])
	mid := hex.EncodeToString(n[:])
	t := text
	m := a2aMessage{Role: "ROLE_USER", Parts: []a2aPart{{Text: &t}}, MessageID: mid, ContextID: contextID, TaskID: taskID}
	type config struct {
		AcceptedOutputModes []string `json:"acceptedOutputModes"`
		Blocking            bool     `json:"blocking,omitempty"` // 0.3; 1.0 waits for the answer unless returnImmediately
	}
	method, cfg := "SendMessage", config{AcceptedOutputModes: []string{"text/plain", "application/json"}}
	if e.Version != a2aVersion {
		m.Kind, m.Role, m.Parts[0].Kind = "message", "user", "text"
		method, cfg.Blocking = "message/send", true
	}
	params := map[string]any{"message": m, "configuration": cfg}
	if e.Tenant != "" {
		params["tenant"] = e.Tenant
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": mid, "method": method, "params": params})
	return b, mid
}

// a2aResult: a JSON-RPC answer's result as a task or a message (1.0: {"task":…}
// or {"message":…}; 0.3: the object itself, with kind).
func a2aResult(b []byte) (*a2aTask, *a2aMessage, error) {
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, nil, fmt.Errorf("not a JSON-RPC answer: %v", err)
	}
	if env.Error != nil {
		return nil, nil, a2aError{env.Error.Code, clip(env.Error.Message, 300)}
	}
	var w struct {
		Task    *a2aTask    `json:"task"`
		Message *a2aMessage `json:"message"`
		Kind    string      `json:"kind"`
	}
	if err := json.Unmarshal(env.Result, &w); err != nil {
		return nil, nil, fmt.Errorf("A2A result: %v", err)
	}
	switch {
	case w.Task != nil:
		return w.Task, nil, nil
	case w.Message != nil:
		return nil, w.Message, nil
	case w.Kind == "task":
		var t a2aTask
		return &t, nil, json.Unmarshal(env.Result, &t)
	case w.Kind == "message":
		var m a2aMessage
		return nil, &m, json.Unmarshal(env.Result, &m)
	}
	return nil, nil, errors.New("A2A result is neither a task nor a message")
}

type a2aError struct {
	Code int
	Msg  string
}

func (e a2aError) Error() string { return fmt.Sprintf("A2A error %d: %s", e.Code, e.Msg) }

// partsText: the parts as plain text (files and data named, not fetched).
func partsText(ps []a2aPart) string {
	var out []string
	for _, p := range ps {
		switch {
		case p.Text != nil:
			out = append(out, *p.Text)
		case p.URL != "":
			out = append(out, fmt.Sprintf("[file %s %s: %s]", p.Filename, p.MediaType, p.URL))
		case p.Raw != "":
			out = append(out, fmt.Sprintf("[file %s %s: %d bytes inline, not shown]", p.Filename, p.MediaType, len(p.Raw)*3/4))
		case p.File != nil:
			out = append(out, fmt.Sprintf("[file %s %s: %s]", p.File.Name, p.File.MimeType, firstNonEmpty(p.File.URI, fmt.Sprintf("%d bytes inline, not shown", len(p.File.Bytes)*3/4))))
		case len(p.Data) > 0:
			out = append(out, "[data] "+clip(string(p.Data), 4000))
		}
	}
	return strings.Join(out, "\n")
}

// a2aState: a task state the same in both versions (completed, input-required…).
func a2aState(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, "TASK_STATE_")), "_", "-")
}

// a2aSend: one text message to an A2A agent (URL or ERC-8004 id); the
// reply fenced. contextID and taskID continue a conversation or a task.
func (c *Client) a2aSend(target, text, contextID, taskID string, max *big.Int) (map[string]any, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("nothing to send")
	}
	cu, via, err := c.a2aResolve(target)
	if err != nil {
		return nil, err
	}
	k, cu, warn, err := a2aFetchCard(cu)
	if err != nil {
		return nil, err
	}
	e, _ := k.endpoint()
	label := "a2a:" + hostOf(e.URL)
	if err := c.guardSend(label, ""); err != nil { // scopes.to, owner approvals, the fan-out limit: as mail to someone new
		return nil, err
	}
	wasClean := len(c.st.Taint) == 0
	body, mid := a2aRequest(e, text, contextID, taskID)
	hdr := map[string]string{"Content-Type": "application/json", "Accept": "application/json", "A2A-Version": e.Version}
	resp, b, paid, err := c.x402PayWith(extHTTP, e.URL, http.MethodPost, body, hdr, max) // a 402 is paid within the owner's rules
	if err != nil {
		return nil, err
	}
	c.taintLabel(label) // the answer is outside text, whatever it is
	if !c.isOwner() {
		if wasClean { // written before any outside text was read: a correspondent from now on, as with mail
			if c.st.Wrote == nil {
				c.st.Wrote = map[string]bool{}
			}
			c.st.Wrote[label] = true
		}
		c.save() // and the fan-out count
	}
	r := map[string]any{"to": label, "endpoint": e.URL, "a2a_version": e.Version, "message_id": mid, "card": cu}
	for k, v := range via {
		r[k] = v
	}
	if paid != nil {
		r["x402"] = paid
	}
	if len(warn) > 0 {
		r["warnings"] = warn
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("A2A %s: %s %s", e.URL, resp.Status, c.outsideLabel(label, clip(strings.TrimSpace(string(b)), 300)))
	}
	task, msg, err := a2aResult(b)
	if err != nil {
		var ae a2aError
		if errors.As(err, &ae) {
			return nil, fmt.Errorf("%s: A2A error %d: %s", label, ae.Code, c.outsideLabel(label, ae.Msg))
		}
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	var reply []string
	if msg != nil {
		r["context_id"], r["kind"] = msg.ContextID, "message"
		reply = append(reply, partsText(msg.Parts))
	}
	if task != nil {
		r["kind"], r["task_id"], r["context_id"], r["state"] = "task", task.ID, task.ContextID, a2aState(task.Status.State)
		if task.Status.Message != nil {
			reply = append(reply, partsText(task.Status.Message.Parts))
			if st, _ := task.Status.Message.Metadata["x402.payment.status"].(string); st == "payment-required" {
				r["payment_asked"] = "in-band, by the a2a-x402 extension: not paid (this client pays x402 over HTTP 402 only); ask your owner"
			}
		}
		for _, a := range task.Artifacts {
			if t := partsText(a.Parts); t != "" {
				reply = append(reply, t)
			}
		}
		switch r["state"] {
		case "submitted", "working":
			r["next"] = "the task is still running: this client doesn't poll it (no tasks/get yet); send again with --task " + task.ID
		case "input-required", "auth-required":
			r["next"] = "the agent asks for more: answer with a2a send … --task " + task.ID + " --context " + task.ContextID
		}
	}
	text = strings.TrimSpace(strings.Join(reply, "\n"))
	r["reply"] = c.outsideLabel(label, clip(text, 16000))
	return r, nil
}
