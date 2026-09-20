// Package auth provides authentication for an HTTP API: password accounts,
// session tokens, email verification, password reset, and identity-token
// exchange for Firebase and generic OpenID Connect providers. A project using
// only a database gets complete authentication without configuring any
// external service.
//
// # Sources
//
// A [Service] is configured with one or more [Source] values (Options.Sources).
// The default, and the only source that needs nothing outside the process, is
// [SourceLocal]: email and password against [UserStore], with email
// verification and password reset tokens.
//
//	Source           Needs                                   Verifier
//	SourceLocal      nothing (Options.Users defaults to an    none
//	                 in-memory store)
//	SourceFirebase   a Firebase project                       FirebaseVerifier
//	SourceOIDC       an issuer URL and audience (Auth0,        OIDCVerifier
//	                 Google, Apple, Cognito, Clerk, or any
//	                 other standard OIDC provider)
//
// Sources compose. A Service configured with SourceLocal and SourceFirebase
// (or SourceOIDC) links a federated sign-in to an existing local account when
// the two share the same email AND the provider marks that email verified.
// An unverified email claim is never used to link accounts, because it is not
// evidence that the signer controls the address.
//
// # Storage
//
// [UserStore], [VerificationStore], [PasswordResetStore], [SessionStore]
// and [AttemptStore] are the interfaces this package persists through. Each
// has an in-memory implementation, sufficient for tests and for a service
// with no database. The auth/pg package (a separate package, so this one
// never imports database/sql) provides a Postgres-backed implementation of
// all five, along with the migrations it needs.
//
// # Sessions and revocation
//
// Options.SessionMode selects how a session token is represented.
// [SessionOpaque] (the default) stores the session in a [SessionStore] and
// hands the caller a random reference token; [Service.Logout],
// [Service.ResetPassword] and [Service.DeleteAccount] all revoke through the
// same store, so a token stolen before a password reset stops working the
// moment the reset succeeds. [SessionJWT] issues a signed, self-contained
// token that needs no storage lookup to validate; the trade-off is that
// those same revocation calls become a no-op, since a JWT keeps validating
// on any server holding the secret until it expires (see SessionJWT's own
// doc comment).
//
// # Identity-token verifiers and typed nil
//
// [Service.SetIDTokenVerifier] is a method rather than a public field. A
// caller that builds a *FirebaseVerifier only under some condition, and
// always assigns the result to an IDTokenVerifier-typed variable, produces a
// non-nil interface value wrapping a nil pointer whenever the condition was
// false: a plain `verifier != nil` check is true, and any call through it
// panics on the nil receiver. SetIDTokenVerifier detects that case with
// reflection and returns an error instead of storing it, so the failure
// happens at configuration time rather than on the first request.
//
// # Rate limiting
//
// Router rate-limits its own routes per client IP (15 requests/minute by
// default, configurable through Options.RateLimit) using keel's
// httpx/middleware package; Options.RealIP configures which forwarding
// headers, if any, that limiter trusts. POST /login additionally throttles
// failed attempts per account email (10 failures per 15 minutes by default,
// see Options.AccountRateLimit), which is what stops a spray that spreads a
// few attempts per IP across many IPs: the IPs each stay under their own
// limit while the targeted account shares one bucket. Every login outcome is
// recorded to Options.Attempts — success and failure, never password
// material — with a Postgres implementation in auth/pg alongside the other
// stores.
//
// # Breached-password screening
//
// Options.BreachChecker optionally rejects passwords present in a breach
// corpus on signup and password reset (see HIBPBreachChecker, which speaks
// the k-anonymity range protocol: only the first 5 hex characters of the
// password's SHA-1 leave the process). Nil — the default — disables the
// check, so an offline service never blocks a password set on an unreachable
// endpoint; a checker failure likewise fails open with a warning.
//
// # Session tokens are not interchangeable with admin's
//
// Under SessionJWT, this package's tokens carry an "aud" claim
// ("keel:auth") that the sibling admin package's tokens do not share
// ("keel:admin"); each package's validator requires its own audience. That
// stops a token from validating against the wrong package even when both
// happen to be configured with the same signing secret — which they should
// not be regardless; see Options's doc comment.
//
// # Error responses
//
// Every error response this package writes is a fixed string chosen by
// status code, never built from request data. A 401 for an unregistered
// email and a 401 for a wrong password are byte-for-byte identical, so the
// login endpoint cannot be used to determine which emails are registered.
package auth
