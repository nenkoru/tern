package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const mailRefreshMax = 3

// mergeRefreshAttempts prevents stale sender processes from restoring retry
// ciphertext after confirmation or forgetting a reserved refresh attempt.
func (s *State) mergeRefreshAttempts(d *State) {
	for id, disk := range d.Sent {
		if !disk.Refreshable {
			continue
		}
		if disk.MailCreated <= s.MailReceiptFloor {
			if local := s.Sent[id]; local != nil {
				local.Env, local.SenderCopy = "", ""
			}
			continue
		}
		if s.Sent == nil {
			s.Sent = map[string]*SentRec{}
		}
		local := s.Sent[id]
		if local == nil {
			s.Sent[id] = disk
			continue
		}
		local.Refreshes = max(local.Refreshes, disk.Refreshes)
		if local.RefreshRoot == "" {
			local.RefreshRoot = disk.RefreshRoot
		}
		if local.Replacement == "" {
			local.Replacement = disk.Replacement
		}
		if disk.Block != 0 && local.Block == 0 {
			local.Block, local.Batch, local.Node = disk.Block, disk.Batch, disk.Node
		}
		if local.Block != 0 || local.Replacement != "" {
			local.Env, local.SenderCopy = "", ""
		}
	}
}

func (c *Client) refreshSource(prefix string) (string, *SentRec, error) {
	var id string
	for key := range c.st.Sent {
		if key == prefix || len(prefix) >= 10 && strings.HasPrefix(key, prefix) {
			if id != "" {
				return "", nil, errors.New("ambiguous message id; use the full id")
			}
			id = key
		}
	}
	for hop := 0; id != "" && hop <= mailRefreshMax; hop++ {
		r := c.st.Sent[id]
		if r == nil {
			break
		}
		if r.Replacement == "" {
			return id, r, nil
		}
		id = r.Replacement
	}
	return "", nil, errors.New("no retained outgoing message with that id")
}

// resend refreshes ordinary unpaid mail without extending the original
// signed expiry or retaining plaintext or keys for longer.
func (c *Client) resend(prefix string) (map[string]any, error) {
	if err := relayFirst(c.relay); err != nil {
		return nil, err
	}
	c.syncGuard()
	id, r, err := c.refreshSource(prefix)
	if err != nil {
		return nil, err
	}
	if !r.Refreshable || r.Block != 0 || r.Env == "" {
		return nil, errors.New("only retained, unconfirmed, unpaid direct mail can be refreshed; paid mail, replies, approvals and group/control messages need their original operation")
	}
	h, err := c.ch.b.HeaderByNumber(context.Background(), nil)
	if err != nil {
		return nil, fmt.Errorf("cannot check message expiry: %w", err)
	}
	now := int64(h.Time)
	if r.MailCreated <= now-mailFreshness || r.MailCreated > now+300 {
		return nil, errors.New("the original message has expired; refreshing cannot extend its lifetime")
	}
	epoch, err := c.ch.epoch()
	if err != nil {
		return nil, fmt.Errorf("cannot check sender key retention: %w", err)
	}
	if _, err := c.destroyOld(epoch); err != nil {
		return nil, fmt.Errorf("cannot enforce sender key retention: %w", err)
	}
	b, err := hex.DecodeString(r.Env)
	es, decodeErr := decodeBatch(b)
	if err != nil || decodeErr != nil || len(es) != 1 || es[0].hash().Hex() != id {
		return nil, errors.New("retained outgoing ciphertext is invalid")
	}
	e := es[0]
	if r.SenderCopy != "" {
		b, err = hex.DecodeString(r.SenderCopy)
		es, decodeErr = decodeBatch(b)
		if err != nil || decodeErr != nil || len(es) != 1 {
			return nil, errors.New("retained sender copy is invalid")
		}
		e = es[0]
	}
	plain, err := e.open(c.identities())
	if err != nil {
		return nil, errors.New("the original sender key is no longer retained; cannot refresh this mail")
	}
	d, signer, _, err := openSigned(plain)
	if err != nil || signer != c.addr() || d.From != c.f.Name || d.To != r.To || d.Type != tMessage || strings.HasPrefix(d.To, "group:") || d.DeliveryID == "" || d.DeliveryID != r.DeliveryID || d.Created != r.MailCreated {
		return nil, errors.New("retained message is not authenticated refreshable mail from this identity")
	}
	var mail Mail
	if err := json.Unmarshal(d.Body, &mail); err != nil || mail.Ask != nil || mail.InReplyTo != "" || mail.Intent == "approval" {
		return nil, errors.New("replies, approvals and owner requests cannot be refreshed")
	}
	if d.Created > now+300 || d.Created <= now-mailFreshness {
		return nil, errors.New("the original message has expired; refreshing cannot extend its lifetime")
	}
	c.res.forget()
	if err := c.res.verifySigner(d.From, signer, h.Number.Uint64(), now); err != nil {
		return nil, fmt.Errorf("sender is no longer authorized: %w", err)
	}
	if err := c.guardSend(d.To, mail.Intent); err != nil {
		return nil, err
	}
	if err := c.noSecrets(mail.Subject+"\n"+mail.Text, mail.Data); err != nil {
		return nil, err
	}
	root := r.RefreshRoot
	if root == "" {
		root = id
	}
	var refusal error
	if err := c.locked(func() {
		original := c.st.Sent[root]
		if original == nil || original.Refreshes >= mailRefreshMax {
			refusal = fmt.Errorf("at most %d refresh attempts per original message", mailRefreshMax)
			return
		}
		if current := c.st.Sent[id]; current == nil || current.Block != 0 || current.Replacement != "" || current.Env == "" {
			refusal = errors.New("delivery changed while refreshing; read its status before retrying")
			return
		}
		original.Refreshes++
	}); err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal
	}
	newID, err := c.sendDatagramDelivery(d.Type, d.To, d.Body, false, d.DeliveryID, d.Created, root)
	if err != nil {
		return nil, err
	}
	if err := c.locked(func() {
		if old := c.st.Sent[id]; old != nil {
			old.Replacement, old.Env, old.SenderCopy, old.Lost = newID, "", "", false
		}
		c.retractAlerts(sentAlertPrefix(id))
	}); err != nil {
		return nil, err
	}
	return map[string]any{"sent": newID, "replaces": id, "delivery_id": d.DeliveryID, "to": d.To, "created": d.Created,
		"note": "refreshed to current keys; original expiry unchanged; delivery still needs confirmation"}, nil
}
