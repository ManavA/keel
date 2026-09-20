package main

import (
	"errors"
	"strings"
	"time"

	"github.com/ManavA/keel/app"
)

// Config is everything this service reads from its environment.
//
// The lifecycle-owned settings — PORT, DATABASE_URL and the rest — come from
// the embedded app.Config. Only DATABASE_URL is required, which is what lets
// the service come up under a compose file holding an app and a database and
// nothing else.
type Config struct {
	app.Config

	// Convenient for one instance and wrong for several: there is no advisory
	// lock, so two instances starting at once can both reach the same pending
	// file and one will fail. Run migrations as their own step beyond one
	// replica.
	MigrateOnStart bool `envconfig:"MIGRATE_ON_START" default:"true"`

	// Base URL used to build the verification and password-reset links, once
	// a mail sender is configured. It must be reachable from an inbox, not
	// from this process.
	SiteURL string `envconfig:"SITE_URL" default:"http://localhost:8080"`

	// Signs the admin sessions. Required, and must differ from any secret
	// the end-user auth service uses.
	AdminSecret string `envconfig:"ADMIN_SECRET"`

	// Seeds the first admin account at startup when both are set. Empty
	// leaves the admin table alone.
	AdminSeedEmail    string `envconfig:"ADMIN_SEED_EMAIL"`
	AdminSeedPassword string `envconfig:"ADMIN_SEED_PASSWORD"`

	// How often the outbox relay polls for unpublished rows. Zero disables
	// it, for a deployment that runs the relay as its own process.
	RelayInterval time.Duration `envconfig:"RELAY_INTERVAL" default:"5s"`
}

// Validate is called by config.Load, so a configuration this build cannot
// honour stops the process at startup rather than at the first request that
// needs it.
func (c *Config) Validate() error {
	if err := c.Config.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.AdminSecret) == "" {
		return errors.New("ADMIN_SECRET must not be empty: it signs the admin sessions")
	}
	return nil
}
