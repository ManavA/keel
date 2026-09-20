package app

import (
	"errors"

	"github.com/ManavA/keel/admin"
	"github.com/ManavA/keel/auth"
)

// CheckSecretSeparation refuses an auth and admin service pair configured
// with the same JWT signing secret. Both packages sign HS256 tokens of the
// same shape, and while each validator's audience claim ("keel:auth" versus
// "keel:admin") stops a token issued by one from validating against the
// other, two independent secrets is what keeps a compromise of one credential
// from reaching the other's sessions. Call it at startup, before serving,
// whenever a deployment mounts both services:
//
//	if err := app.CheckSecretSeparation(authOpts, adminOpts); err != nil { ... }
//
// A nil return is not an approval of either service on its own: each
// NewService still owns its required-field errors, and an auth service under
// SessionOpaque (the default) signs nothing, so there is no shared credential
// to refuse there whatever its unused Secret holds.
func CheckSecretSeparation(authOpts auth.Options, adminOpts admin.Options) error {
	if authOpts.SessionMode != auth.SessionJWT {
		return nil
	}
	if authOpts.Secret == "" || adminOpts.Secret == "" {
		return nil
	}
	if authOpts.Secret != adminOpts.Secret {
		return nil
	}
	return errors.New("app: auth.Options.Secret and admin.Options.Secret must be different: " +
		"sharing one JWT signing secret lets a compromise of either credential forge sessions for both")
}
