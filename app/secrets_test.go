package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/admin"
	"github.com/ManavA/keel/auth"
)

// TestSharedJWTSecretRefused is the startup check issue #34 asks for: an auth
// service and an admin service configured with the same JWT signing secret
// must fail before serving, with an error naming both fields. Each service
// builds fine on its own — the failure comes only from the shared secret.
func TestSharedJWTSecretRefused(t *testing.T) {
	const shared = "shared-secret-long-enough"

	authOpts := auth.Options{
		SessionMode:              auth.SessionJWT,
		AllowUnrevocableSessions: true,
		Secret:                   shared,
	}
	adminOpts := admin.Options{
		Users:  admin.NewMemoryAdminStore(),
		Secret: shared,
	}

	_, err := auth.NewService(authOpts)
	require.NoError(t, err)
	_, err = admin.NewService(adminOpts)
	require.NoError(t, err)

	err = CheckSecretSeparation(authOpts, adminOpts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth.Options.Secret")
	assert.Contains(t, err.Error(), "admin.Options.Secret")
}

func TestDistinctJWTSecretsPass(t *testing.T) {
	authOpts := auth.Options{
		SessionMode:              auth.SessionJWT,
		AllowUnrevocableSessions: true,
		Secret:                   "auth-secret-long-enough",
	}
	adminOpts := admin.Options{
		Users:  admin.NewMemoryAdminStore(),
		Secret: "admin-secret-long-enough",
	}

	assert.NoError(t, CheckSecretSeparation(authOpts, adminOpts))
}

func TestOpaqueAuthNeedsNoSeparation(t *testing.T) {
	// Under SessionOpaque the auth secret signs nothing (NewService ignores
	// it), so there is no shared credential to refuse even when the two
	// strings happen to match.
	authOpts := auth.Options{Secret: "admin-secret-long-enough"}
	adminOpts := admin.Options{
		Users:  admin.NewMemoryAdminStore(),
		Secret: "admin-secret-long-enough",
	}

	assert.NoError(t, CheckSecretSeparation(authOpts, adminOpts))
}

func TestMissingSecretsBelongToEachService(t *testing.T) {
	// An empty secret on either side is that service's own required-field
	// error from its NewService, not a separation failure here.
	authOpts := auth.Options{
		SessionMode:              auth.SessionJWT,
		AllowUnrevocableSessions: true,
	}
	adminOpts := admin.Options{Users: admin.NewMemoryAdminStore()}

	assert.NoError(t, CheckSecretSeparation(authOpts, adminOpts))
}
