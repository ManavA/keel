package config_test

import (
	"testing"

	"github.com/ManavA/keel/config"
	"github.com/stretchr/testify/assert"
)

// secretNames must stay identical to log's SecretNames. The two packages cannot
// share one table because neither imports the other, so the copies are what
// keeps the rule from drifting apart: a struct field and the log attribute
// named after it have to be treated the same way.
var secretNames = []struct {
	name   string
	secret bool
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
	for _, tt := range secretNames {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.secret, config.IsSecretName(tt.name))
		})
	}
}
