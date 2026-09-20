package notifyprefs

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// Category names what kind of message a send is. Security and transactional
// mail always send; the rest send unless the user opted out.
type Category string

const (
	// CategorySecurity is account-safety mail: password resets, suspicious
	// logins, verification codes. It always sends and cannot be opted out
	// of; see the package doc.
	CategorySecurity Category = "security"
	// CategoryTransactional is mail the user caused: receipts, confirmations,
	// tour reminders. It always sends and cannot be opted out of.
	CategoryTransactional Category = "transactional"
	// CategoryMarketing is promotional mail the user never asked for. It
	// sends by default and stops when the user opts out.
	CategoryMarketing Category = "marketing"
	// CategoryProduct is product news for an existing user: new features,
	// digests, tips. It sends by default and stops when the user opts out.
	CategoryProduct Category = "product"
)

// Channel names where a message goes.
type Channel string

const (
	// ChannelEmail delivers to the user's mailbox.
	ChannelEmail Channel = "email"
	// ChannelSMS delivers to the user's phone as a text message.
	ChannelSMS Channel = "sms"
	// ChannelPush delivers to the user's devices as a push notification.
	ChannelPush Channel = "push"
)

// Preferences is one user's opt-outs, as (category, channel) pairs that
// should not send. The zero value opts out of nothing. An entry naming
// [CategorySecurity] or [CategoryTransactional] has no effect — see
// [AllowedBy] — and stores drop such entries on write.
type Preferences struct {
	OptOuts map[Category]map[Channel]bool
}

// Set records optedOut for one (category, channel) pair.
func (p *Preferences) Set(category Category, channel Channel, optedOut bool) {
	if p.OptOuts == nil {
		p.OptOuts = make(map[Category]map[Channel]bool)
	}
	if !optedOut {
		if chans, ok := p.OptOuts[category]; ok {
			delete(chans, channel)
			if len(chans) == 0 {
				delete(p.OptOuts, category)
			}
		}
		return
	}
	chans, ok := p.OptOuts[category]
	if !ok {
		chans = make(map[Channel]bool)
		p.OptOuts[category] = chans
	}
	chans[channel] = true
}

// OptedOut reports whether the (category, channel) pair is opted out.
func (p Preferences) OptedOut(category Category, channel Channel) bool {
	return p.OptOuts[category][channel]
}

// MarshalJSON encodes Preferences as its opt-out map directly —
// `{"marketing": {"email": true}}` — rather than under an "OptOuts" key, so
// the Postgres JSONB column holds only the pairs and stays readable.
func (p Preferences) MarshalJSON() ([]byte, error) {
	if len(p.OptOuts) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(p.OptOuts)
}

// UnmarshalJSON decodes the form MarshalJSON writes.
func (p *Preferences) UnmarshalJSON(data []byte) error {
	var m map[Category]map[Channel]bool
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	p.OptOuts = m
	return nil
}

// Normalized returns prefs without the opt-outs that can never take effect
// — security and transactional — so what a store holds matches what
// [AllowedBy] computes. Stores call it on write; callers that build
// Preferences by hand need not, since AllowedBy enforces the same rule on
// read.
func (p Preferences) Normalized() Preferences {
	if len(p.OptOuts) == 0 {
		return p
	}
	out := Preferences{OptOuts: make(map[Category]map[Channel]bool, len(p.OptOuts))}
	for cat, chans := range p.OptOuts {
		if cat == CategorySecurity || cat == CategoryTransactional {
			continue
		}
		cp := make(map[Channel]bool, len(chans))
		for ch, opted := range chans {
			if opted {
				cp[ch] = true
			}
		}
		if len(cp) > 0 {
			out.OptOuts[cat] = cp
		}
	}
	if len(out.OptOuts) == 0 {
		out.OptOuts = nil
	}
	return out
}

// AllowedBy reports whether a message of category over channel sends under
// prefs. Security and transactional always send, even if prefs claims an
// opt-out for them; everything else sends unless opted out. A zero
// Preferences sends everything.
func AllowedBy(prefs Preferences, category Category, channel Channel) bool {
	if category == CategorySecurity || category == CategoryTransactional {
		return true
	}
	return !prefs.OptedOut(category, channel)
}

// errEmptyUserID reports a Store call with no user to look up.
var errEmptyUserID = errors.New("notifyprefs: user id is required")

// Store persists notification preferences across requests, and across
// processes for an implementation backed by something other than memory.
//
// Get returns the stored preferences for userID, or the zero Preferences
// when the user has never set any — missing state means the defaults, which
// send everything. Set replaces them. Allowed reports whether a message of
// category over channel sends for userID: it is Get followed by [AllowedBy],
// for callers that only need the verdict.
type Store interface {
	Get(ctx context.Context, userID string) (Preferences, error)
	Set(ctx context.Context, userID string, prefs Preferences) error
	Allowed(ctx context.Context, userID string, category Category, channel Channel) (bool, error)
}

// MemoryStore is the in-process default [Store]. It does not survive a
// restart and does not coordinate across instances; use notifyprefs/pg
// where either of those matters. It is safe for concurrent use.
type MemoryStore struct {
	mu      sync.Mutex
	entries map[string]Preferences
}

// NewMemoryStore builds an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: make(map[string]Preferences)}
}

// Get implements [Store.Get].
func (s *MemoryStore) Get(_ context.Context, userID string) (Preferences, error) {
	if userID == "" {
		return Preferences{}, errEmptyUserID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Normalized copies: the stored struct shares its inner maps with
	// nothing else once copied, so a caller that mutates the returned
	// value cannot corrupt the store.
	return s.entries[userID].Normalized(), nil
}

// Set implements [Store.Set]. Security and transactional opt-outs are
// dropped before storing, so a later Get never returns a preference that
// cannot take effect.
func (s *MemoryStore) Set(_ context.Context, userID string, prefs Preferences) error {
	if userID == "" {
		return errEmptyUserID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[userID] = prefs.Normalized()
	return nil
}

// Allowed implements [Store.Allowed].
func (s *MemoryStore) Allowed(ctx context.Context, userID string, category Category, channel Channel) (bool, error) {
	prefs, err := s.Get(ctx, userID)
	if err != nil {
		return false, err
	}
	return AllowedBy(prefs, category, channel), nil
}
