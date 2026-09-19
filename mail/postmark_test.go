package mail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/keighl/postmark"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postmarkStub serves body with an HTTP 200 status at the templated-send
// endpoint and records the last request's From header, so tests can assert
// on both the outcome and what was actually sent. Every case this package
// needs to cover — an accepted send, an ErrorCode in the body, an inactive
// recipient — arrives as a 200 with a different body, matching Postmark's
// own behavior: see the mail package doc. calls counts how many times the
// server was hit, so a test can pin exactly how many attempts a given
// ErrorCode caused, not just the final outcome.
func postmarkStub(t *testing.T, body postmark.EmailResponse) (srv *httptest.Server, captured *postmark.TemplatedEmail, calls *int32) {
	t.Helper()
	captured = &postmark.TemplatedEmail{}
	calls = new(int32)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		require.NoError(t, json.NewDecoder(r.Body).Decode(captured))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(body)
	}))
	return srv, captured, calls
}

func newTestSender(t *testing.T, opts PostmarkSenderOptions, srvURL string) *PostmarkSender {
	t.Helper()
	s := NewPostmarkSender(opts)
	s.client.BaseURL = srvURL
	return s
}

func TestPostmarkSender_Send_Accepted(t *testing.T) {
	srv, captured, _ := postmarkStub(t, postmark.EmailResponse{MessageID: "msg-1", ErrorCode: 0})
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{ServerToken: testServerToken, FromEmail: testFromEmail}, srv.URL)

	err := s.Send(context.Background(), "buyer@example.com", "welcome", map[string]any{testNameKey: testName})
	require.NoError(t, err)
	assert.Equal(t, "hello@example.com", captured.From)
	assert.Equal(t, "buyer@example.com", captured.To)
	assert.Equal(t, "welcome", captured.TemplateAlias)
}

func TestPostmarkSender_Send_UsesFromName(t *testing.T) {
	srv, captured, _ := postmarkStub(t, postmark.EmailResponse{MessageID: "msg-1"})
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{
		ServerToken: testServerToken,
		FromEmail:   testFromEmail,
		FromName:    testBrand,
	}, srv.URL)

	require.NoError(t, s.Send(context.Background(), "buyer@example.com", "welcome", nil))
	assert.Equal(t, "Acme <hello@example.com>", captured.From)
}

// TestPostmarkSender_Send_APIErrorCodeInA200BodyIsAnError is the central
// regression test: the client library returns a nil Go error for this
// shape, and PostmarkSender.Send must not let that read as success.
func TestPostmarkSender_Send_APIErrorCodeInA200BodyIsAnError(t *testing.T) {
	srv, _, calls := postmarkStub(t, postmark.EmailResponse{ErrorCode: 1101, Message: "template not found"})
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{ServerToken: testServerToken, FromEmail: testFromEmail}, srv.URL)

	err := s.Send(context.Background(), "buyer@example.com", "missing-template", nil)
	require.Error(t, err, "a nonzero ErrorCode inside a 200 response must be treated as a failed send")
	assert.Contains(t, err.Error(), "1101")
	assert.Equal(t, int32(1), atomic.LoadInt32(calls),
		"a parsed ErrorCode is a permanent API error, sent exactly once, not retried")
}

func TestPostmarkSender_Send_InactiveRecipientWrapsErrRecipientUndeliverable(t *testing.T) {
	srv, _, calls := postmarkStub(t, postmark.EmailResponse{ErrorCode: 406, Message: "inactive recipient"})
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{ServerToken: testServerToken, FromEmail: testFromEmail}, srv.URL)

	err := s.Send(context.Background(), "gone@example.com", "welcome", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRecipientUndeliverable)
	assert.Equal(t, int32(1), atomic.LoadInt32(calls), "406 is permanent, sent exactly once, never retried")
}

func TestPostmarkSender_Send_OtherErrorCodesAreNotTreatedAsPermanent(t *testing.T) {
	srv, _, calls := postmarkStub(t, postmark.EmailResponse{ErrorCode: 300, Message: "invalid email request"})
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{ServerToken: testServerToken, FromEmail: testFromEmail}, srv.URL)

	err := s.Send(context.Background(), "buyer@example.com", "welcome", nil)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrRecipientUndeliverable,
		"only 406 is a permanent failure; other codes must retry rather than be treated as a dead address")
	assert.Equal(t, int32(1), atomic.LoadInt32(calls),
		"a parsed, non-406 ErrorCode from a 200 response is not a transport failure, so it is not retried either")
}

// TestPostmarkSender_Send_RetriesTransportFailures covers the retry this
// package added around the HTTP call itself: a response the client
// library cannot even parse — unlike the ErrorCode cases above, which are
// parsed successfully and must not retry.
func TestPostmarkSender_Send_RetriesTransportFailures(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("not json"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(postmark.EmailResponse{MessageID: "msg-1"})
	}))
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{ServerToken: testServerToken, FromEmail: testFromEmail}, srv.URL)

	err := s.Send(context.Background(), "buyer@example.com", "welcome", nil)
	require.NoError(t, err)
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls), "the first two transport failures must be retried")
}

func TestPostmarkSender_Send_GivesUpAfterTransportRetriesExhausted(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{ServerToken: testServerToken, FromEmail: testFromEmail}, srv.URL)

	err := s.Send(context.Background(), "buyer@example.com", "welcome", nil)
	require.Error(t, err)
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls), "postmarkTransportRetry.MaxAttempts bounds the retrying")
}

// The client library parses a 5xx body like any other, so a 500 carrying a
// well-formed ErrorCode would read as a permanent API error and be sent once.
// The HTTP status must win: it is retried.
func TestPostmarkSender_Send_RetriesA5xxWithAParseableBody(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(postmark.EmailResponse{ErrorCode: 300, Message: "server error, try again"})
	}))
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{ServerToken: testServerToken, FromEmail: testFromEmail}, srv.URL)

	err := s.Send(context.Background(), "buyer@example.com", "welcome", nil)
	require.Error(t, err)
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls),
		"a 5xx must be retried even though its body parses")
}

// A non-2xx whose body parses as JSON but carries no Postmark ErrorCode
// must still be a send failure: unmarshalling leaves the zero value,
// ErrorCode reads 0, and without a status check Send would report success
// for a message that was never accepted.
func TestPostmarkSender_Send_Non2xxJsonWithoutErrorCodeIsAnError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	}))
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{ServerToken: testServerToken, FromEmail: testFromEmail}, srv.URL)

	err := s.Send(context.Background(), "buyer@example.com", "welcome", nil)
	require.Error(t, err, "a 429 with no Postmark ErrorCode must be a failed send, not success")
}

// The issue's acceptance check: a 502 carrying an HTML outage page is a
// send failure, not success.
func TestPostmarkSender_Send_502WithHTMLBodyIsAnError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html><body>Bad Gateway</body></html>`))
	}))
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{ServerToken: testServerToken, FromEmail: testFromEmail}, srv.URL)

	err := s.Send(context.Background(), "buyer@example.com", "welcome", nil)
	require.Error(t, err, "a 502 with an HTML body must be a failed send, not success")
}

// A genuine Postmark API error arrives as a non-2xx (422) with a nonzero
// ErrorCode in the body. The status must not turn it into a retried
// transport failure: it is classified once, like its 200 counterpart.
func TestPostmarkSender_Send_Non2xxWithErrorCodeIsClassifiedOnce(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(postmark.EmailResponse{ErrorCode: 1101, Message: "template not found"})
	}))
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{ServerToken: testServerToken, FromEmail: testFromEmail}, srv.URL)

	err := s.Send(context.Background(), "buyer@example.com", "missing-template", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1101")
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls),
		"a parsed ErrorCode is a permanent API error, sent exactly once, not retried")
}

// Send's context reaches the retry loop: a canceled one stops before any
// request is made.
func TestPostmarkSender_Send_CanceledContextSendsNothing(t *testing.T) {
	srv, _, calls := postmarkStub(t, postmark.EmailResponse{MessageID: "msg-1"})
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{ServerToken: testServerToken, FromEmail: testFromEmail}, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.Send(ctx, "buyer@example.com", "welcome", nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int32(0), atomic.LoadInt32(calls))
}

func TestPostmarkSender_DeliversToProvider(t *testing.T) {
	s := NewPostmarkSender(PostmarkSenderOptions{ServerToken: testServerToken, FromEmail: testFromEmail})
	assert.True(t, s.DeliversToProvider())
	assert.True(t, DeliversToProvider(s))
}
