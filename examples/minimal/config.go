package main

import (
	"fmt"
	"slices"
	"strings"
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

	// Read even though only "local" works without further configuration, so a
	// deployment can be written against the variable and is refused rather
	// than ignored when it names something this build cannot do.
	AuthSources []string `envconfig:"AUTH_SOURCES" default:"local"`

	// Base URL used to build the verification and password-reset links mailed
	// to users. It must be reachable from an inbox, not from this process.
	SiteURL string `envconfig:"SITE_URL" default:"http://localhost:8080"`

	// How long an issued session token stays valid. Zero keeps the auth
	// package's own default of seven days.
	AuthTokenTTL time.Duration `envconfig:"AUTH_TOKEN_TTL"`

	// Bounds requests per client IP to the auth routes. Zero keeps the auth
	// package's own default of 15 requests per minute.
	AuthRateLimitRequests int           `envconfig:"AUTH_RATE_LIMIT_REQUESTS"`
	AuthRateLimitWindow   time.Duration `envconfig:"AUTH_RATE_LIMIT_WINDOW"`

	// Enables the Firebase source: the GCP project whose ID tokens /auth/exchange
	// verifies. Empty means the firebase source cannot be selected. Credentials
	// come from the environment the usual way (GOOGLE_APPLICATION_CREDENTIALS
	// or ambient GCP metadata), so there is no secret to configure here.
	FirebaseProjectID string `envconfig:"FIREBASE_PROJECT_ID"`

	// Enable the OIDC source: the issuer and audience /auth/exchange verifies
	// ID tokens against (Auth0, Google, Apple, Cognito, Clerk, or any other
	// standard OIDC provider). JWKSURL overrides discovery when set; it is
	// empty by default, which takes the standard discovery path. Both issuer
	// and audience are required to select the oidc source.
	OIDCIssuerURL string `envconfig:"OIDC_ISSUER_URL"`
	OIDCAudience  string `envconfig:"OIDC_AUDIENCE"`
	OIDCJWKSURL   string `envconfig:"OIDC_JWKS_URL"`

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

var supportedAuthSources = []string{"local", "firebase", "oidc"}

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
	if slices.Contains(c.AuthSources, "firebase") && strings.TrimSpace(c.FirebaseProjectID) == "" {
		return fmt.Errorf("AUTH_SOURCES names %q but FIREBASE_PROJECT_ID is empty", "firebase")
	}
	if slices.Contains(c.AuthSources, "oidc") {
		if strings.TrimSpace(c.OIDCIssuerURL) == "" || strings.TrimSpace(c.OIDCAudience) == "" {
			return fmt.Errorf("AUTH_SOURCES names %q but OIDC_ISSUER_URL or OIDC_AUDIENCE is empty", "oidc")
		}
	}
	// One service holds one identity-token verifier, so the two federated
	// sources cannot be selected together. Each composes with local.
	if slices.Contains(c.AuthSources, "firebase") && slices.Contains(c.AuthSources, "oidc") {
		return fmt.Errorf("AUTH_SOURCES names both %q and %q, which cannot be selected together; pick one", "firebase", "oidc")
	}
	if strings.TrimSpace(c.SiteURL) == "" {
		return fmt.Errorf("SITE_URL must not be empty: it is used to build the verification and password-reset links")
	}
	return nil
}

// Addr binds every interface: binding 127.0.0.1 inside a container makes the
// service unreachable from outside it, with nothing in the logs to say why.
func (c *Config) Addr() string { return fmt.Sprintf(":%d", c.Port) }
