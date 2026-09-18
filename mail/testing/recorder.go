// Package testing provides a [Recorder], a [mail.Sender] that captures
// every send in memory instead of delivering it, for use in tests and local
// development.
//
// It lives in its own package rather than as an exported type in mail
// itself so that a production binary's dependency graph never has to
// include test-only code merely because it imports mail — and so this
// package's own name can say plainly what it is for, the way `net/http/httptest`
// does.
package testing

import (
	"context"
	"sync"
)

// Sent records one email a [Recorder] captured.
type Sent struct {
	To            string
	TemplateAlias string
	TemplateModel map[string]any
}

// Recorder is a [mail.Sender] that captures sends in memory rather than
// delivering them. It is safe for concurrent use.
type Recorder struct {
	mu   sync.Mutex
	sent []Sent
}

// New builds an empty Recorder.
func New() *Recorder {
	return &Recorder{}
}

// Send records the message and always returns nil.
func (r *Recorder) Send(_ context.Context, to, templateAlias string, templateModel map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, Sent{To: to, TemplateAlias: templateAlias, TemplateModel: templateModel})
	return nil
}

// Sent returns every message recorded so far, in send order.
func (r *Recorder) Sent() []Sent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Sent, len(r.sent))
	copy(out, r.sent)
	return out
}

// DeliversToProvider reports false: a Recorder never contacts a real
// provider, so a caller counting confirmed deliveries (via
// mail.DeliversToProvider) must not count these sends. See the mail
// package's ProviderBackedSender doc for why this has to be queryable
// rather than assumed from a nil error.
func (r *Recorder) DeliversToProvider() bool { return false }
