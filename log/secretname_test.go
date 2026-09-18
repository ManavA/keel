package log_test

import (
	"log/slog"
	"testing"

	"github.com/ManavA/keel/log"
	"github.com/stretchr/testify/assert"
)

// SecretNames is the table both log and config are checked against. config has
// its own copy; the two packages cannot share one because neither imports the
// other, so the copies are what keeps the rule from drifting apart.
//
// Keep this list and config's identical.
var SecretNames = []struct {
	Name   string
	Secret bool
}{
	{"password", true},
	{"Password", true},
	{"DB_PASSWD", true},
	{"api_token", true},
	{"apiToken", true},
	{"APIToken", true},
	{"access_token", true},
	{"refresh_token", true},
	{"JWT_SECRET", true},
	{"ClientSecret", true},
	{"authorization", true},
	{"Authorization", true},
	{"api_key", true},
	{"ApiKey", true},
	{"X-Api-Key", true},
	{"private-key", true},
	{"PrivateKey", true},
	{"GOOGLE_CREDENTIALS", true},
	{"user_credentials", true},
	{"session_id", true},
	{"sessionid", true},
	{"cookie", true},
	{"token", true},
	{"jwt", true},
	{"bearer", true},
	{"Secrets", true},
	{"Tokens", true},
	{"access_tokens", true},

	// The control. Substring matching hides every one of these.
	{"sort_key", false},
	{"cache_key", false},
	{"PartitionKey", false},
	{"IdempotencyKey", false},
	{"session_count", false},
	{"sessions_active", false},
	{"cookie_consent", false},
	{"tokens_used", false},
	{"status_code", false},
	{"state", false},
	{"authored_by", false},
	{"user_id", false},
	{"Port", false},
	{"Env", false},
	{"Keyboard", false},
	{"monkey", false},
}

func TestSecretNameAgreement(t *testing.T) {
	for _, tt := range SecretNames {
		t.Run(tt.Name, func(t *testing.T) {
			assert.Equal(t, tt.Secret, log.IsSecretKey(tt.Name))
		})
	}
}

func TestSegments(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"api_token", []string{"api", "token"}},
		{"apiToken", []string{"api", "token"}},
		{"APIToken", []string{"api", "token"}},
		{"X-Api-Key", []string{"x", "api", "key"}},
		{"HTTPSProxy", []string{"https", "proxy"}},
		{"port", []string{"port"}},
		{"", nil},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			assert.Equal(t, tt.want, log.Segments(tt.in))
		})
	}
}

func TestRedactedMetricsKeepTheirType(t *testing.T) {
	// These went in as a number and a boolean, and a metrics pipeline reading
	// them breaks if they come back as strings.
	rec := capture(t, log.Options{}, func(l *slog.Logger) {
		l.Info("m", "tokens_used", 42, "session_count", 7, "cookie_consent", true)
	})
	assert.Equal(t, float64(42), rec["tokens_used"])
	assert.Equal(t, float64(7), rec["session_count"])
	assert.Equal(t, true, rec["cookie_consent"])
}
