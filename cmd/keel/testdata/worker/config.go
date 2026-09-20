package main

import (
	"fmt"
	"strings"
	"time"
)

// Config is everything this service reads from its environment. Only
// DATABASE_URL is required.
type Config struct {
	Env string `envconfig:"ENV" default:"development"`

	DatabaseURL string `envconfig:"DATABASE_URL" required:"true"`

	// Convenient for one instance and wrong for several: there is no advisory
	// lock, so two instances starting at once can both reach the same pending
	// file and one will fail. Run migrations as their own step beyond one
	// replica.
	MigrateOnStart bool `envconfig:"MIGRATE_ON_START" default:"true"`

	// How often the heartbeat job runs. The scheduler runs the entry once
	// immediately and then every interval.
	HeartbeatInterval time.Duration `envconfig:"HEARTBEAT_INTERVAL" default:"1m"`

	// The heartbeat row's job name. Name it per deployment when several
	// workers share a database, so their heartbeats can be told apart.
	HeartbeatJob string `envconfig:"HEARTBEAT_JOB" default:"heartbeat"`
}

// Cloud reports whether the service runs outside development, for the logger:
// Cloud Logging reads a "severity" field and ignores slog's "level", so
// without it every entry outside a laptop is DEFAULT severity.
func (c Config) Cloud() bool { return strings.TrimSpace(c.Env) != "development" }

// Validate is called by config.Load, so a configuration this build cannot
// honour stops the process at startup rather than at the first job tick.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.DatabaseURL) == "" {
		return fmt.Errorf("DATABASE_URL must not be empty")
	}
	if c.HeartbeatInterval <= 0 {
		return fmt.Errorf("HEARTBEAT_INTERVAL must be positive, got %s", c.HeartbeatInterval)
	}
	if strings.TrimSpace(c.HeartbeatJob) == "" {
		return fmt.Errorf("HEARTBEAT_JOB must not be empty")
	}
	return nil
}
