package main

import (
	"fmt"
	"strings"

	"github.com/ManavA/keel/app"
)

// Config is everything this service reads from its environment.
//
// The lifecycle-owned settings — PORT, DATABASE_URL and the rest — come from
// the embedded app.Config. Only DATABASE_URL and WEBHOOK_SECRET are required,
// which is what lets the service come up under a compose file holding an app
// and a database and nothing else.
type Config struct {
	app.Config

	// Convenient for one instance and wrong for several: there is no advisory
	// lock, so two instances starting at once can both reach the same pending
	// file and one will fail. Run migrations as their own step beyond one
	// replica.
	MigrateOnStart bool `envconfig:"MIGRATE_ON_START" default:"true"`

	// Empty ignores forwarding headers entirely, which is correct when nothing
	// is in front of the service.
	TrustedProxies []string `envconfig:"TRUSTED_PROXIES"`

	// The secret deliveries are signed with. The sender puts
	// HMAC-SHA256(body) in X-Keel-Signature and the topic in X-Keel-Topic;
	// a delivery without a valid signature is refused before it reaches the
	// database. It has no default: a receiver that accepts anything is a
	// receiver that stores anything.
	WebhookSecret string `envconfig:"WEBHOOK_SECRET" required:"true"`
}

// Validate is called by config.Load, so a configuration this build cannot
// honour stops the process at startup rather than at the first delivery.
func (c *Config) Validate() error {
	if err := c.Config.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.WebhookSecret) == "" {
		return fmt.Errorf("WEBHOOK_SECRET must not be empty: unsigned deliveries would be stored")
	}
	return nil
}
