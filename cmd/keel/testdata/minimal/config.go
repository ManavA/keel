package main

import (
	"fmt"
	"slices"
	"time"
)

// Config is everything this service reads from its environment.
//
// Only DATABASE_URL is required, which is what lets the service come up under a
// compose file holding an app and a Postgres and nothing else.
type Config struct {
	Port int    `envconfig:"PORT" default:"8080"`
	Env  string `envconfig:"ENV" default:"development"`

	DatabaseURL string `envconfig:"DATABASE_URL" required:"true"`

	// Convenient for one instance and wrong for several: there is no advisory
	// lock, so two instances starting at once can both reach the same pending
	// file and one will fail. Run migrations as their own step beyond one
	// replica.
	MigrateOnStart bool `envconfig:"MIGRATE_ON_START" default:"true"`

	// Empty ignores forwarding headers entirely, which is correct when nothing
	// is in front of the service.
	TrustedProxies []string `envconfig:"TRUSTED_PROXIES"`

	// Empty allows no origin, which is correct for an API no browser calls.
	CORSOrigins []string `envconfig:"CORS_ORIGINS"`

	// Read even though only "local" is supported, so a deployment can be
	// written against the variable and is refused rather than ignored when it
	// names something this build cannot do.
	AuthSources []string `envconfig:"AUTH_SOURCES" default:"local"`

	// How often the search index is rebuilt from the notes table. Zero disables
	// the job.
	ReconcileInterval time.Duration `envconfig:"RECONCILE_INTERVAL" default:"15m"`

	// Keep under the platform's own termination grace period, or the platform
	// kills the shutdown partway through.
	ShutdownTimeout time.Duration `envconfig:"SHUTDOWN_TIMEOUT" default:"20s"`

	// Every prober and load balancer polls on its own schedule, and without a
	// cache all of it reaches the database.
	ReadinessCacheTTL time.Duration `envconfig:"READINESS_CACHE_TTL" default:"5s"`
}

var supportedAuthSources = []string{"local"}

// Validate is called by config.Load, so a configuration this build cannot
// honour stops the process at startup rather than at the first request that
// needs it.
func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("PORT must be between 1 and 65535, got %d", c.Port)
	}
	for _, source := range c.AuthSources {
		if !slices.Contains(supportedAuthSources, source) {
			return fmt.Errorf("AUTH_SOURCES names %q, which this build does not have; only %v is available",
				source, supportedAuthSources)
		}
	}
	return nil
}

// Addr binds every interface: binding 127.0.0.1 inside a container makes the
// service unreachable from outside it, with nothing in the logs to say why.
func (c *Config) Addr() string { return fmt.Sprintf(":%d", c.Port) }
