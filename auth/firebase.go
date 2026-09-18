package auth

import (
	"context"
	"fmt"

	firebase "firebase.google.com/go/v4"
	firebaseAuth "firebase.google.com/go/v4/auth"
)

// FirebaseVerifier is an [IDTokenVerifier] backed by the Firebase Admin SDK.
//
// EmailVerified on the returned Identity reflects Firebase's own
// email_verified claim. Firebase's password provider issues tokens for any
// syntactically valid email without proving ownership, so an unverified
// email must never be treated as belonging to the account holder — for
// example, never send it a notification on the strength of this claim alone.
type FirebaseVerifier struct {
	client *firebaseAuth.Client
}

// NewFirebaseVerifier initializes the Firebase Admin SDK for the given
// project and returns a FirebaseVerifier. Credentials are discovered the
// usual way: GOOGLE_APPLICATION_CREDENTIALS, or ambient GCP metadata.
func NewFirebaseVerifier(ctx context.Context, projectID string) (*FirebaseVerifier, error) {
	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: projectID})
	if err != nil {
		return nil, fmt.Errorf("auth: initialize firebase app: %w", err)
	}
	client, err := app.Auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: initialize firebase auth client: %w", err)
	}
	return &FirebaseVerifier{client: client}, nil
}

// VerifyIDToken verifies a Firebase ID token offline (signature and standard
// claims only). Suitable for the per-request hot path.
func (f *FirebaseVerifier) VerifyIDToken(ctx context.Context, token string) (Identity, error) {
	t, err := f.client.VerifyIDToken(ctx, token)
	if err != nil {
		return Identity{}, fmt.Errorf("auth: verify firebase token: %w", err)
	}
	return identityFromToken(t), nil
}

// VerifyIDTokenCheckRevoked additionally checks with Firebase that the
// session has not been revoked and the account is not disabled or deleted.
// Reserved for a session-establishing exchange, not every request.
func (f *FirebaseVerifier) VerifyIDTokenCheckRevoked(ctx context.Context, token string) (Identity, error) {
	t, err := f.client.VerifyIDTokenAndCheckRevoked(ctx, token)
	if err != nil {
		return Identity{}, fmt.Errorf("auth: verify firebase token: %w", err)
	}
	return identityFromToken(t), nil
}

func identityFromToken(t *firebaseAuth.Token) Identity {
	id := Identity{UID: t.UID}
	if email, ok := t.Claims["email"].(string); ok {
		id.Email = email
	}
	if name, ok := t.Claims["name"].(string); ok {
		id.Name = name
	}
	if verified, ok := t.Claims["email_verified"].(bool); ok {
		id.EmailVerified = verified
	}
	return id
}
