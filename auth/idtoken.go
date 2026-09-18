package auth

import "context"

// Identity is what an IDTokenVerifier extracts from a verified identity
// token — a federated provider's own claims, not this package's User type.
type Identity struct {
	UID   string
	Email string
	Name  string
	// EmailVerified reflects the token issuer's own claim. A provider that
	// issues tokens for any syntactically valid email without proving
	// ownership makes this claim meaningless for that email; treat it as
	// authoritative only for providers where it actually is (see the
	// FirebaseVerifier doc comment).
	EmailVerified bool
}

// IDTokenVerifier verifies a bearer identity token issued by an external
// provider (Firebase, an OIDC provider, ...) and extracts the identity it
// carries. VerifyIDToken is the cheap, offline check suitable for the
// per-request hot path; VerifyIDTokenCheckRevoked additionally confirms with
// the issuer that the session has not been revoked and the account is not
// disabled, at the cost of a network round trip, and belongs on a
// session-establishing exchange rather than every request.
type IDTokenVerifier interface {
	VerifyIDToken(ctx context.Context, token string) (Identity, error)
	VerifyIDTokenCheckRevoked(ctx context.Context, token string) (Identity, error)
}
