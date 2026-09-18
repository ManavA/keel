package config_test

import (
	"testing"

	"github.com/ManavA/keel/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedact(t *testing.T) {
	assert.Equal(t, config.Unset, config.Redact(""))
	assert.Equal(t, config.Placeholder, config.Redact("hunter2"))
	assert.NotEqual(t, config.Redact(""), config.Redact("hunter2"),
		"an unset secret and a hidden one must not print the same")
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// The failure this function exists for: cutting at the first "@"
			// keeps the userinfo and drops the host, which is exactly backwards.
			name: "the password goes and the host stays",
			in:   "postgres://app:hunter2@db.internal:5432/main?sslmode=disable",
			want: "postgres://app:[redacted]@db.internal:5432/main?sslmode=disable",
		},
		{
			name: "a URL with no password is untouched apart from parsing",
			in:   "postgres://db.internal:5432/main",
			want: "postgres://db.internal:5432/main",
		},
		{
			name: "a username with no password survives",
			in:   "redis://cache@localhost:6379",
			want: "redis://cache@localhost:6379",
		},
		{
			name: "a credential in the query string is masked",
			in:   "https://api.example.com/v1?access_token=abc123&page=2",
			want: "https://api.example.com/v1?access_token=[redacted]&page=2",
		},
		{
			name: "an empty value reads as unset",
			in:   "",
			want: config.Unset,
		},
		{
			name: "something unparseable is redacted whole rather than guessed at",
			in:   "not a url at all",
			want: config.Placeholder,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := config.RedactURL(tt.in)
			assert.Equal(t, tt.want, got)
			if tt.in != "" && tt.in != "not a url at all" {
				assert.NotContains(t, got, "hunter2")
				assert.NotContains(t, got, "abc123")
			}
		})
	}
}

func TestRedacted(t *testing.T) {
	cfg := testConfig{
		Port:        8080,
		Env:         "staging",
		DatabaseURL: "postgres://app:hunter2@db.internal:5432/main",
		JWTSecret:   "s3cret",
		Banner:      "welcome",
		SortKey:     "created_at",
		Nested:      nested{APIToken: "nested-secret"},
	}

	byName := map[string]config.Field{}
	for _, f := range config.Redacted(&cfg) {
		byName[f.Name] = f
	}
	require.Contains(t, byName, "Port")

	assert.Equal(t, "8080", byName["Port"].Value)
	assert.Equal(t, "staging", byName["Env"].Value)
	assert.Equal(t, "welcome", byName["Banner"].Value)
	assert.Equal(t, "created_at", byName["SortKey"].Value)

	assert.Equal(t, config.Placeholder, byName["JWTSecret"].Value)
	assert.True(t, byName["JWTSecret"].Secret)
	assert.Equal(t, config.Placeholder, byName["Nested.APIToken"].Value)

	assert.Contains(t, byName["DatabaseURL"].Value, "db.internal:5432")
	assert.NotContains(t, byName["DatabaseURL"].Value, "hunter2")

	for _, f := range byName {
		assert.NotContains(t, f.Value, "hunter2")
		assert.NotContains(t, f.Value, "s3cret")
		assert.NotContains(t, f.Value, "nested-secret")
	}
}

func TestRedactedIgnoresNonStructs(t *testing.T) {
	assert.Nil(t, config.Redacted(nil))
	assert.Nil(t, config.Redacted(42))
	assert.Nil(t, config.Redacted((*testConfig)(nil)))
}

type collectionSecrets struct {
	Secrets       []string          `envconfig:"SECRETS"`
	Credentials   map[string]string `envconfig:"CREDENTIALS"`
	WebhookSecret int               `envconfig:"WEBHOOK_SECRET"`
	Hosts         []string          `envconfig:"HOSTS"`
	Empty         []string          `envconfig:"EMPTY_SECRETS" secret:"true"`
}

func TestRedactedHidesNonStringSecrets(t *testing.T) {
	// Requiring a string field prints a []string of API keys in full.
	cfg := collectionSecrets{
		Secrets:       []string{"hunter2", "swordfish"},
		Credentials:   map[string]string{"api_key": "AKIA-REAL"},
		WebhookSecret: 3600,
		Hosts:         []string{"a.example.com", "b.example.com"},
	}

	byName := map[string]config.Field{}
	for _, f := range config.Redacted(&cfg) {
		byName[f.Name] = f
	}

	assert.True(t, byName["Secrets"].Secret)
	assert.NotContains(t, byName["Secrets"].Value, "hunter2")
	assert.NotContains(t, byName["Secrets"].Value, "swordfish")
	assert.Contains(t, byName["Secrets"].Value, "2 entries",
		"a count is not a credential, and it answers whether the deployment supplied any")

	assert.True(t, byName["Credentials"].Secret)
	assert.NotContains(t, byName["Credentials"].Value, "AKIA-REAL")

	assert.True(t, byName["WebhookSecret"].Secret)
	assert.NotContains(t, byName["WebhookSecret"].Value, "3600")

	assert.Equal(t, config.Unset, byName["Empty"].Value,
		"an unset secret must read differently from a hidden one")

	// The control: a non-secret collection is still readable.
	assert.Contains(t, byName["Hosts"].Value, "a.example.com")
}

type intSecretConfig struct {
	WebhookSecret int `envconfig:"WEBHOOK_SECRET"`
}

func TestLoadErrorDoesNotEchoASecretValue(t *testing.T) {
	// envconfig quotes the offending value: "converting 's3cr3t' to type int".
	// The README's own slog.Error("load config", "error", err) would print it,
	// and log's redaction cannot reach inside an error string.
	t.Setenv("WEBHOOK_SECRET", "s3cr3t-value")

	var cfg intSecretConfig
	err := config.LoadWith(&cfg, config.Options{Files: []string{}})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "s3cr3t-value")
	assert.Contains(t, err.Error(), "WEBHOOK_SECRET", "the field must still be named")
}

func TestLoadErrorStillNamesANonSecretValue(t *testing.T) {
	// The control: scrubbing must not blank out ordinary parse errors.
	type plainConfig struct {
		Port int `envconfig:"PORT"`
	}
	t.Setenv("PORT", "eighty")

	var cfg plainConfig
	err := config.LoadWith(&cfg, config.Options{Files: []string{}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "eighty")
}

type shortSecretConfig struct {
	WebhookSecret int `envconfig:"WEBHOOK_SECRET"`
}

func TestLoadErrorSurvivesAShortSecret(t *testing.T) {
	// A global replace of a one-character value rewrites every occurrence of
	// that letter, including the ones inside the placeholder it just inserted,
	// which destroys the message this scrubbing exists to keep readable.
	t.Setenv("WEBHOOK_SECRET", "t")

	var cfg shortSecretConfig
	err := config.LoadWith(&cfg, config.Options{Files: []string{}})
	require.Error(t, err)

	msg := err.Error()
	assert.NotContains(t, msg, "'t'", "the quoted value is scrubbed whatever its length")
	assert.Contains(t, msg, config.Placeholder)
	assert.Contains(t, msg, "WEBHOOK_SECRET", "the field must still be named")
	assert.Contains(t, msg, "converting", "the message must still read as a sentence")
	assert.NotContains(t, msg, `"t"`, "strconv quotes it again in the details")
	assert.NotContains(t, msg, "[redac[", "the placeholder must not be rewritten into itself")
}

type twoSecretConfig struct {
	APIToken      string `envconfig:"API_TOKEN"`
	WebhookSecret int    `envconfig:"WEBHOOK_SECRET"`
}

func TestLoadErrorScrubsTheLongestSecretFirst(t *testing.T) {
	// One secret is a prefix of the other. Replacing the short one first would
	// leave the tail of the long one in the message.
	t.Setenv("API_TOKEN", "abcdef")
	t.Setenv("WEBHOOK_SECRET", "abcdefghijkl")

	var cfg twoSecretConfig
	err := config.LoadWith(&cfg, config.Options{Files: []string{}})
	require.Error(t, err)

	assert.NotContains(t, err.Error(), "abcdef")
	assert.NotContains(t, err.Error(), "ghijkl")
}
