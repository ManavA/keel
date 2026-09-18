package mail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
// own behavior: see the mail package doc.
func postmarkStub(t *testing.T, body postmark.EmailResponse) (*httptest.Server, *postmark.TemplatedEmail) {
	t.Helper()
	var captured postmark.TemplatedEmail
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(body)
	}))
	return srv, &captured
}

func newTestSender(t *testing.T, opts PostmarkSenderOptions, srvURL string) *PostmarkSender {
	t.Helper()
	s := NewPostmarkSender(opts)
	s.client.BaseURL = srvURL
	return s
}

func TestPostmarkSender_Send_Accepted(t *testing.T) {
	srv, captured := postmarkStub(t, postmark.EmailResponse{MessageID: "msg-1", ErrorCode: 0})
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{ServerToken: "tok", FromEmail: "hello@example.com"}, srv.URL)

	err := s.Send(context.Background(), "buyer@example.com", "welcome", map[string]any{"name": "Jane"})
	require.NoError(t, err)
	assert.Equal(t, "hello@example.com", captured.From)
	assert.Equal(t, "buyer@example.com", captured.To)
	assert.Equal(t, "welcome", captured.TemplateAlias)
}

func TestPostmarkSender_Send_UsesFromName(t *testing.T) {
	srv, captured := postmarkStub(t, postmark.EmailResponse{MessageID: "msg-1"})
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{
		ServerToken: "tok",
		FromEmail:   "hello@example.com",
		FromName:    "Acme",
	}, srv.URL)

	require.NoError(t, s.Send(context.Background(), "buyer@example.com", "welcome", nil))
	assert.Equal(t, "Acme <hello@example.com>", captured.From)
}

// TestPostmarkSender_Send_APIErrorCodeInA200BodyIsAnError is the central
// regression test: the client library returns a nil Go error for this
// shape, and PostmarkSender.Send must not let that read as success.
func TestPostmarkSender_Send_APIErrorCodeInA200BodyIsAnError(t *testing.T) {
	srv, _ := postmarkStub(t, postmark.EmailResponse{ErrorCode: 1101, Message: "template not found"})
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{ServerToken: "tok", FromEmail: "hello@example.com"}, srv.URL)

	err := s.Send(context.Background(), "buyer@example.com", "missing-template", nil)
	require.Error(t, err, "a nonzero ErrorCode inside a 200 response must be treated as a failed send")
	assert.Contains(t, err.Error(), "1101")
}

func TestPostmarkSender_Send_InactiveRecipientWrapsErrRecipientUndeliverable(t *testing.T) {
	srv, _ := postmarkStub(t, postmark.EmailResponse{ErrorCode: 406, Message: "inactive recipient"})
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{ServerToken: "tok", FromEmail: "hello@example.com"}, srv.URL)

	err := s.Send(context.Background(), "gone@example.com", "welcome", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRecipientUndeliverable)
}

func TestPostmarkSender_Send_OtherErrorCodesAreNotTreatedAsPermanent(t *testing.T) {
	srv, _ := postmarkStub(t, postmark.EmailResponse{ErrorCode: 300, Message: "invalid email request"})
	defer srv.Close()

	s := newTestSender(t, PostmarkSenderOptions{ServerToken: "tok", FromEmail: "hello@example.com"}, srv.URL)

	err := s.Send(context.Background(), "buyer@example.com", "welcome", nil)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrRecipientUndeliverable,
		"only 406 is a permanent failure; other codes must retry rather than be treated as a dead address")
}

func TestPostmarkSender_DeliversToProvider(t *testing.T) {
	s := NewPostmarkSender(PostmarkSenderOptions{ServerToken: "tok", FromEmail: "hello@example.com"})
	assert.True(t, s.DeliversToProvider())
	assert.True(t, DeliversToProvider(s))
}
