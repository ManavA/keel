package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ManavA/keel/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type nested struct {
	APIToken string `envconfig:"NESTED_API_TOKEN"`
}

type testConfig struct {
	Port        int    `envconfig:"PORT" default:"8080"`
	Env         string `envconfig:"ENV" default:"development"`
	DatabaseURL string `envconfig:"DATABASE_URL"`
	JWTSecret   string `envconfig:"JWT_SECRET"`
	Banner      string `envconfig:"BANNER"`
	SortKey     string `envconfig:"SORT_KEY"`
	PlainToken  string `envconfig:"PLAIN_TOKEN" secret:"false"`
	Nested      nested
	unexported  string //nolint:unused // present so reflection has to skip it
}

type validatingConfig struct {
	Port int `envconfig:"PORT" default:"0"`
}

var errNoPort = errors.New("PORT must be set")

func (c *validatingConfig) Validate() error {
	if c.Port == 0 {
		return errNoPort
	}
	return nil
}

func TestLoadDefaults(t *testing.T) {
	var cfg testConfig
	require.NoError(t, config.LoadWith(&cfg, config.Options{Files: []string{}}))

	assert.Equal(t, 8080, cfg.Port)
	assert.Equal(t, "development", cfg.Env)
}

func TestLoadTrimsSecrets(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		check func(*testing.T, testConfig)
	}{
		{
			name: "a trailing newline on a secret is removed",
			env:  map[string]string{"JWT_SECRET": "s3cret\n"},
			check: func(t *testing.T, c testConfig) {
				assert.Equal(t, "s3cret", c.JWTSecret)
			},
		},
		{
			name: "surrounding whitespace on a URL is removed",
			env:  map[string]string{"DATABASE_URL": "  postgres://h/db\r\n"},
			check: func(t *testing.T, c testConfig) {
				assert.Equal(t, "postgres://h/db", c.DatabaseURL)
			},
		},
		{
			name: "a nested struct is walked",
			env:  map[string]string{"NESTED_API_TOKEN": "abc\n"},
			check: func(t *testing.T, c testConfig) {
				assert.Equal(t, "abc", c.Nested.APIToken)
			},
		},
		{
			name: "a non-secret field keeps the whitespace it was given",
			env:  map[string]string{"BANNER": "  welcome  "},
			check: func(t *testing.T, c testConfig) {
				assert.Equal(t, "  welcome  ", c.Banner)
			},
		},
		{
			name: "a field named like a sort key is not treated as a secret",
			env:  map[string]string{"SORT_KEY": " created_at "},
			check: func(t *testing.T, c testConfig) {
				assert.Equal(t, " created_at ", c.SortKey)
			},
		},
		{
			name: `secret:"false" overrides the name heuristic`,
			env:  map[string]string{"PLAIN_TOKEN": " not-a-secret "},
			check: func(t *testing.T, c testConfig) {
				assert.Equal(t, " not-a-secret ", c.PlainToken)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			var cfg testConfig
			require.NoError(t, config.LoadWith(&cfg, config.Options{Files: []string{}}))
			tt.check(t, cfg)
		})
	}
}

func TestLoadCallsValidate(t *testing.T) {
	var cfg validatingConfig
	err := config.LoadWith(&cfg, config.Options{Files: []string{}})
	require.Error(t, err)
	assert.ErrorIs(t, err, errNoPort)

	t.Setenv("PORT", "9000")
	require.NoError(t, config.LoadWith(&cfg, config.Options{Files: []string{}}))
	assert.Equal(t, 9000, cfg.Port)
}

func TestLoadSkipValidate(t *testing.T) {
	var cfg validatingConfig
	require.NoError(t, config.LoadWith(&cfg, config.Options{Files: []string{}, SkipValidate: true}))
}

func TestLoadDotenv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(path, []byte("ENV=staging\nJWT_SECRET=from-file\n"), 0o600))

	var cfg testConfig
	require.NoError(t, config.LoadWith(&cfg, config.Options{Files: []string{path}}))
	assert.Equal(t, "staging", cfg.Env)
	assert.Equal(t, "from-file", cfg.JWTSecret)
}

func TestLoadDotenvDoesNotOverrideTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(path, []byte("ENV=from-file\n"), 0o600))
	t.Setenv("ENV", "from-deployment")

	var cfg testConfig
	require.NoError(t, config.LoadWith(&cfg, config.Options{Files: []string{path}}))
	assert.Equal(t, "from-deployment", cfg.Env)
}

func TestLoadMissingDotenvIsNotAnError(t *testing.T) {
	var cfg testConfig
	require.NoError(t, config.LoadWith(&cfg, config.Options{
		Files: []string{filepath.Join(t.TempDir(), "absent.env")},
	}))
}

func TestLoadRejectsBadDestination(t *testing.T) {
	var notAStruct int
	assert.Error(t, config.Load(nil))
	assert.Error(t, config.Load(&notAStruct))
	assert.Error(t, config.Load(testConfig{}))
}

func TestIsSecretName(t *testing.T) {
	secret := []string{
		"JWT_SECRET", "AdminJWTSecret", "POSTMARK_SERVER_TOKEN", "password",
		"DB_PASSWD", "GOOGLE_CREDENTIALS", "API_KEY", "ApiKey", "private-key",
	}
	for _, name := range secret {
		assert.True(t, config.IsSecretName(name), "%q should read as a secret", name)
	}

	// The control. These all contain "key" or look credential-adjacent, and
	// hiding them would make the rule useless in practice.
	plain := []string{"SortKey", "CACHE_KEY", "PartitionKey", "IdempotencyKey", "Port", "Env", "Keyboard"}
	for _, name := range plain {
		assert.False(t, config.IsSecretName(name), "%q should not read as a secret", name)
	}
}
