package main

import (
	"fmt"
	"slices"
	"time"
)

// Config is everything this service reads from its environment.
//
// Only DATABASE_URL is required. Nothing else has to be set for the service to
// run correctly, which is what lets it come up under a compose file holding an
// app and a Postgres and nothing else.
type Config struct {
	Port int    `envconfig:"PORT" default:"8080"`
	Env  string `envconfig:"ENV" default:"development"`

	DatabaseURL string `envconfig:"DATABASE_URL" required:"true"`

	// MigrateOnStart applies pending migrations before serving. Convenient for
	// one instance and wrong for several: there is no advisory lock yet, so two
	// instances starting at once can both reach the same pending file and one
	// will fail loudly. Set it false and run migrations as their own step once
	// you run more than one replica.
	MigrateOnStart bool `envconfig:"MIGRATE_ON_START" default:"true"`

	// TrustedProxies are the proxies in front of this service. Empty means
	// forwarding headers are ignored entirely, which is correct when nothing is
	// in front of it.
	TrustedProxies []string `envconfig:"TRUSTED_PROXIES"`

	// CORSOrigins are the browser origins allowed to call this API. Empty
	// allows none, which is correct for an API no browser calls directly.
	CORSOrigins []string `envconfig:"CORS_ORIGINS"`

	// AuthSources selects the authentication sources. Only "local" exists
	// today; "firebase" and "oidc" arrive with the auth package. The variable
	// is read now so that a deployment can be written against it, and refused
	// rather than ignored when it names something this build cannot do.
	AuthSources []string `envconfig:"AUTH_SOURCES" default:"local"`

	// ReconcileInterval is how often the search index is rebuilt from the
	// notes table. Zero disables the job.
	ReconcileInterval time.Duration `envconfig:"RECONCILE_INTERVAL" default:"15m"`

	// ShutdownTimeout bounds the wait for in-flight requests. Keep it under the
	// platform's own termination grace period.
	ShutdownTimeout time.Duration `envconfig:"SHUTDOWN_TIMEOUT" default:"20s"`

	// ReadinessCacheTTL is how long a readiness result is reused. Every prober
	// and load balancer polls on its own schedule, and without a cache all of
	// it reaches the database.
	ReadinessCacheTTL time.Duration `envconfig:"READINESS_CACHE_TTL" default:"5s"`
}

// supportedAuthSources is what this build can actually do.
var supportedAuthSources = []string{"local"}

// Validate is called by config.Load, so a configuration this build cannot
// honour stops the process at startup rather than at the first request that
// depends on it.
func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("PORT must be between 1 and 65535, got %d", c.Port)
	}
	for _, source := range c.AuthSources {
		if !slices.Contains(supportedAuthSources, source) {
			return fmt.Errorf(
				"AUTH_SOURCES names %q, which this build does not have; "+
					"only %v is available until the auth package lands",
				source, supportedAuthSources)
		}
	}
	return nil
}

// Addr is the listen address. A bare port binds every interface, which is what
// a container needs: binding 127.0.0.1 inside one makes the service
// unreachable from outside it, with nothing in the logs to say why.
func (c *Config) Addr() string { return fmt.Sprintf(":%d", c.Port) }
