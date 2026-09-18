package main

import (
	"fmt"
	"slices"
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
}

var supportedAuthSources = []string{"local", "firebase", "oidc"}

// Validate is called by config.Load, so a configuration this build cannot
// honour stops the process at startup rather than at the first request that
// needs it.
func (c *Config) Validate() error {
	if err := c.Config.Validate(); err != nil {
		return err
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
