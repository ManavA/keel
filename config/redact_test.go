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
			want: "postgres://app:%5Bredacted%5D@db.internal:5432/main?sslmode=disable",
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
			want: "https://api.example.com/v1?access_token=%5Bredacted%5D&page=2",
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
