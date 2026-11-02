package main

import (
	"encoding/json"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"
)

const mailFreshness int64 = 7 * 86400

// Receipts outlive purged plaintext only while a signed message remains fresh.
// The floor prevents an expired receipt from being restored by a stale writer.
type MailReceipt struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Purged  bool   `json:"purged,omitempty"`
}

func directReceiptKey(d *Datagram, payload []byte) string {
	if d.DeliveryID == "" {
		return "payload:" + crypto.Keccak256Hash(payload).Hex()
	}
	b, _ := json.Marshal([]string{d.From, d.DeliveryID})
	return "delivery:" + crypto.Keccak256Hash(b).Hex()
}

func directArchive(it *Item) bool {
	return it.Type == tMessage && it.Status != "refused" && !strings.HasPrefix(it.To, "group:")
}

func (s *State) seedMailReceipts() {
	if s.MailReceipts == nil {
		s.MailReceipts = map[string]MailReceipt{}
	}
	for _, it := range s.Items {
		if !directArchive(it) {
			continue
		}
		if it.DirectReceipt == "" {
			// Prior archives have no signed payload left; their envelope ID is
			// still sufficient to recognize an exact replay after purge.
			it.DirectReceipt, it.Created = "envelope:"+it.ID, it.Time
		}
		if it.Created > s.MailReceiptFloor {
			if _, ok := s.MailReceipts[it.DirectReceipt]; !ok {
				s.MailReceipts[it.DirectReceipt] = MailReceipt{ID: it.ID, Created: it.Created}
			}
		}
	}
}

func (s *State) purgeDirectMail(it *Item) {
	if it.DirectReceipt != "" {
		s.MailReceipts[it.DirectReceipt] = MailReceipt{ID: it.ID, Created: it.Created, Purged: true}
	}
}

func (s *State) mergeMailReceipts(d *State) {
	s.seedMailReceipts()
	d.seedMailReceipts()
	s.MailReceiptFloor = max(s.MailReceiptFloor, d.MailReceiptFloor)
	for key, r := range d.MailReceipts {
		if own, ok := s.MailReceipts[key]; !ok || r.Purged && !own.Purged {
			s.MailReceipts[key] = r
		}
	}
	local := map[string]*Item{}
	for _, it := range s.Items {
		local[it.ID] = it
	}
	disk := map[string]bool{}
	for _, it := range d.Items {
		disk[it.ID] = true
	}
	var keep []*Item
	for _, it := range s.Items {
		if it.DirectReceipt != "" {
			r := s.MailReceipts[it.DirectReceipt]
			// New receipt-backed items enter only inside the locked acceptance
			// below. Missing disk plaintext therefore means a completed purge.
			if r.Purged || !disk[it.ID] {
				continue
			}
		}
		keep = append(keep, it)
	}
	for _, it := range d.Items {
		if it.DirectReceipt != "" && local[it.ID] == nil && !s.MailReceipts[it.DirectReceipt].Purged {
			keep = append(keep, it)
		}
	}
	s.Items = keep
	for key, r := range s.MailReceipts {
		if r.Created <= s.MailReceiptFloor {
			delete(s.MailReceipts, key)
		}
	}
}

// Claim and archive are one state-file transaction. Duplicate envelopes never
// reach policy, and a crash cannot leave a claim without its archived message.
func (c *Client) acceptIncoming(it *Item) (bool, error) {
	if it.directMail == nil {
		c.st.Items = append(c.st.Items, it)
		return true, nil
	}
	accepted := false
	err := c.locked(func() {
		c.st.seedMailReceipts()
		if _, seen := c.st.MailReceipts[it.DirectReceipt]; seen {
			return
		}
		if _, seen := c.st.MailReceipts["envelope:"+it.ID]; seen {
			return
		}
		if it.Created <= c.st.MailReceiptFloor {
			return
		}
		c.fileBody(it, it.directMail)
		c.policy(it, it.directMail)
		c.st.Items = append(c.st.Items, it)
		c.st.MailReceipts[it.DirectReceipt] = MailReceipt{ID: it.ID, Created: it.Created}
		accepted = true
	})
	if err != nil && accepted {
		delete(c.st.MailReceipts, it.DirectReceipt)
		for i, stored := range c.st.Items {
			if stored == it {
				c.st.Items = append(c.st.Items[:i], c.st.Items[i+1:]...)
				break
			}
		}
	}
	return accepted && err == nil, err
}
