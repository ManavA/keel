package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ManavA/keel/events"
	"github.com/ManavA/keel/retry"
)

// SignatureHeader carries the HMAC-SHA256 of the request body, as
// `sha256=<hex>`. TopicHeader names the topic the event was published to.
const (
	SignatureHeader = "X-Keel-Signature"
	TopicHeader     = "X-Keel-Topic"
)

// signaturePrefix is the scheme prefix on every signature header value.
const signaturePrefix = "sha256="

// defaultTimeout bounds one HTTP delivery attempt.
const defaultTimeout = 10 * time.Second

// Endpoint is one external URL subscribed to events. Topics selects which
// topics are delivered to URL; empty means every topic. Secret signs each
// delivery and is never returned by [Dispatcher.Endpoints].
type Endpoint struct {
	ID     string
	URL    string
	Secret string
	Topics []string
}

// Options configures a [Dispatcher]. The zero value delivers with a 10-second
// per-attempt timeout and retry's own defaults.
type Options struct {
	// Client performs the deliveries. Its Timeout bounds one attempt; the
	// default client uses 10 seconds.
	Client *http.Client

	// Retry configures the backoff around each endpoint's delivery. The
	// zero value uses retry's own defaults.
	Retry retry.Options

	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Dispatcher delivers published events to registered endpoints. It
// implements [events.Publisher]. The zero value is not usable; build one
// with [NewDispatcher].
type Dispatcher struct {
	mu        sync.Mutex
	endpoints map[string]Endpoint
	failures  map[string]int

	client *http.Client
	retry  retry.Options
	logger *slog.Logger
}

// NewDispatcher builds a Dispatcher from opts.
func NewDispatcher(opts Options) *Dispatcher {
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{
		endpoints: map[string]Endpoint{},
		failures:  map[string]int{},
		client:    client,
		retry:     opts.Retry,
		logger:    logger,
	}
}

// Register subscribes an endpoint. It validates URL and Secret, assigns an id
// when Endpoint.ID is empty, and reports an error when the id is already
// registered. It returns the id the endpoint is registered under.
func (d *Dispatcher) Register(ep Endpoint) (string, error) {
	u, err := url.Parse(ep.URL)
	if err != nil {
		return "", fmt.Errorf("webhooks: invalid endpoint URL %q: %w", ep.URL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("webhooks: endpoint URL %q must use http or https", ep.URL)
	}
	if u.Host == "" {
		return "", fmt.Errorf("webhooks: endpoint URL %q has no host", ep.URL)
	}
	if ep.Secret == "" {
		return "", fmt.Errorf("webhooks: endpoint secret is empty")
	}

	id := ep.ID
	if id == "" {
		id = uuid.NewString()
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if _, taken := d.endpoints[id]; taken {
		return "", fmt.Errorf("webhooks: endpoint %q is already registered", id)
	}
	ep.ID = id
	d.endpoints[id] = ep
	return id, nil
}

// Unregister removes an endpoint and its failure count. Removing an id that
// was never registered does nothing.
func (d *Dispatcher) Unregister(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.endpoints, id)
	delete(d.failures, id)
}

// Endpoints lists the registered endpoints ordered by id, with secrets
// redacted: the returned Endpoints carry an empty Secret.
func (d *Dispatcher) Endpoints() []Endpoint {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Endpoint, 0, len(d.endpoints))
	for _, ep := range d.endpoints {
		ep.Secret = ""
		out = append(out, ep)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Failures reports an endpoint's consecutive Publish failures, reset by the
// next success. ok is false when id is not a registered endpoint.
func (d *Dispatcher) Failures(id string) (n int, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, known := d.endpoints[id]; !known {
		return 0, false
	}
	return d.failures[id], true
}

// Publish delivers event to every endpoint subscribed to topic and implements
// [events.Publisher]. A topic with no subscribers is delivered vacuously and
// returns nil. An endpoint whose delivery fails every retry contributes one
// error mentioning its id, without stopping the other endpoints; the joined
// error is non-nil, so an outbox relay using this dispatcher leaves the row
// unpublished and attempts it again.
func (d *Dispatcher) Publish(ctx context.Context, topic string, event any) error {
	body, err := events.Marshal(event)
	if err != nil {
		return fmt.Errorf("webhooks: marshal event for topic %s: %w", topic, err)
	}

	d.mu.Lock()
	var matched []Endpoint
	for _, ep := range d.endpoints {
		if subscribed(ep, topic) {
			matched = append(matched, ep)
		}
	}
	retryOpts := d.retry
	d.mu.Unlock()
	sort.Slice(matched, func(i, j int) bool { return matched[i].ID < matched[j].ID })

	var errs []error
	for _, ep := range matched {
		if err := retry.Do(ctx, func() error {
			return d.post(ctx, ep, topic, body)
		}, retryOpts); err != nil {
			d.mu.Lock()
			d.failures[ep.ID]++
			d.mu.Unlock()
			d.logger.WarnContext(ctx, "webhooks: delivery failed",
				"endpoint", ep.ID, "topic", topic, "error", err)
			errs = append(errs, fmt.Errorf("webhooks: deliver to endpoint %s: %w", ep.ID, err))
			continue
		}
		d.mu.Lock()
		delete(d.failures, ep.ID)
		d.mu.Unlock()
	}
	return errors.Join(errs...)
}

// subscribed reports whether an endpoint wants a topic: an endpoint with no
// topics takes every topic.
func subscribed(ep Endpoint, topic string) bool {
	if len(ep.Topics) == 0 {
		return true
	}
	for _, want := range ep.Topics {
		if want == topic {
			return true
		}
	}
	return false
}

// post makes one signed delivery attempt to an endpoint. Any non-2xx status
// is an error, so the retry around it treats a rejection like a failure.
func (d *Dispatcher) post(ctx context.Context, ep Endpoint, topic string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhooks: build request for endpoint %s: %w", ep.ID, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(TopicHeader, topic)
	req.Header.Set(SignatureHeader, Sign(ep.Secret, body))

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhooks: post to endpoint %s: %w", ep.ID, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhooks: post to endpoint %s: unexpected status %s", ep.ID, resp.Status)
	}
	return nil
}

// Sign returns the signature header value for body under secret:
// `sha256=<hex of HMAC-SHA256(secret, body)>`.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether signature is the value [Sign] produced for body
// under secret. The comparison runs in constant time, and anything malformed
// simply does not verify.
func Verify(secret string, body []byte, signature string) bool {
	if !strings.HasPrefix(signature, signaturePrefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(signature, signaturePrefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
