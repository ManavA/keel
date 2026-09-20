# OIDC provider matrix

How to configure `auth.OIDCVerifier` for each supported issuer: the exact
issuer string, what the audience must be, how the `email_verified` claim
behaves, and the gotcha that bites with this verifier. Each row was checked
against the provider's own docs (linked) and, where noted, against the live
discovery document.

What the verifier itself enforces, for every provider: RS256 signatures only,
an `iss` match that is byte for byte (a trailing slash counts), an `aud` that
must be exactly the configured audience and nothing else, a required `exp`
with no clock-skew leeway, and discovery whose `issuer` field must equal the
configured issuer while `jwks_uri` must share the issuer's origin. Pass
`OIDCOptions.JWKSURL` when it does not (Google).

| Provider | Issuer | Audience | `email_verified` |
|---|---|---|---|
| Auth0 | `https://<tenant>.auth0.com/` (slash included) | Application Client ID (ID tokens) | Boolean, present |
| Google | `https://accounts.google.com` | OAuth client ID | Boolean, present |
| Apple | `https://appleid.apple.com` | Services ID (client ID) | String `"true"`/`"false"`, not boolean; and Apple signs ES256, which the verifier rejects (see below) |
| Cognito | `https://cognito-idp.<region>.amazonaws.com/<poolId>` | App client ID (ID tokens) | Boolean, present |
| Clerk | `https://<instance>.clerk.accounts.dev` (custom domain in production) | Must be present in the token as `aud`; Clerk checks `azp` by default | Not promised on stock session tokens; missing means unverified to this verifier |

## Auth0

Issuer: `https://<tenant>.auth0.com/`, trailing slash included. That slash is
part of the `iss` claim in real tokens, so configure the issuer exactly like
that; trimming it makes every token fail issuer validation. With a custom
domain the issuer is that domain instead; read it from your tenant's discovery
document. Docs: https://auth0.com/docs/secure/tokens/id-tokens/id-token-structure

Audience: your application's Client ID. Note the split Auth0 draws: an ID
token's `aud` is the Client ID, while an access token's `aud` is the API
identifier. This verifier checks ID tokens, so configure the Client ID; the
API identifier never matches an ID token. (The preset table on
`OIDCVerifier` previously said API identifier; that is corrected here.)

email_verified: present as a boolean. Account linking follows the usual rule:
only a literal `true` links to an existing local account.

## Google

Issuer: `https://accounts.google.com`. Google accepts two `iss` values for its
tokens, `accounts.google.com` and `https://accounts.google.com`, but this
verifier matches exactly one string, so configure the form your tokens carry.
Docs: https://developers.google.com/identity/gsi/web/guides/verify-google-id-token

Audience: your OAuth client ID.

email_verified: present as a boolean.

Gotcha: Google's discovery document points `jwks_uri` at
`https://www.googleapis.com/oauth2/v3/certs`, a different host from the
issuer (confirmed against the live discovery document). The verifier's
same-origin rule rejects that, so discovery fails unless you set
`OIDCOptions.JWKSURL` to the certs URL explicitly.

## Apple

Issuer: `https://appleid.apple.com`. Docs: https://developer.apple.com/documentation/sign_in_with_apple/sign_in_with_apple_rest_api/authenticating_users_with_sign_in_with_apple

Audience: your Services ID (the client ID), e.g. `com.example.my-app`.

email_verified: a String `"true"` or `"false"`, not a boolean. The verifier
reads only a boolean claim, so an Apple email always looks unverified and
never links accounts.

Gotcha, the blocking one: Apple signs its identity tokens with ES256 and this
verifier accepts RS256 only, so Apple ID tokens are rejected at the signature
step. Apple cannot be used with `OIDCVerifier` as written; supporting it
needs ES256 key handling first. Apple's keys live at
`https://appleid.apple.com/auth/keys` (same origin, so discovery is fine).

Also note Apple sends the email claim only on first authorization; store it
when you see it.

## Cognito

Issuer: `https://cognito-idp.<region>.amazonaws.com/<poolId>`.
Docs: https://docs.aws.amazon.com/cognito/latest/developerguide/amazon-cognito-user-pools-using-the-id-token.html

Audience: the app client ID, read from the ID token's `aud` claim. Pass ID
tokens (`token_use` `id`): access tokens carry the client as `client_id` with
no `aud` claim, so the verifier rejects them. Claim checks:
https://docs.aws.amazon.com/cognito/latest/developerguide/amazon-cognito-user-pools-using-tokens-verifying-a-jwt.html

email_verified: present as a boolean.

Keys are RS256 under
`https://cognito-idp.<region>.amazonaws.com/<poolId>/.well-known/jwks.json`
(same origin, so discovery works). AWS tells integrators to cache keys by
`kid` and refresh on an unknown one because signing keys rotate; that is what
this verifier already does (unknown-`kid` refetch plus `CacheTTL` refresh).

## Clerk

Issuer: the Frontend API URL, `https://<instance>.clerk.accounts.dev` in
development or your custom domain in production.

Audience: Clerk's own verifier checks `azp` (authorized parties) and only
checks `aud` when an audience is configured; this verifier always requires an
exact single `aud` match. So the Clerk token must carry the `aud` you
configure (add one with a JWT template if your tokens lack it), or
verification fails. Reference: https://clerk.com/docs/references/backend/verify-token
and https://github.com/clerk/javascript/blob/main/packages/backend/src/tokens/verify.ts

email_verified: stock Clerk session tokens do not promise this claim, and a
missing claim reads as unverified, so expect no account linking from Clerk
tokens unless your template adds it.

Clock skew: Clerk's own verifier allows 5 seconds of skew by default
(`clockSkewInMs`); this verifier allows none. Keep the server clock on NTP,
especially with short-lived tokens (Cognito allows 5-minute expiries).
