// Package mail sends transactional email over a provider interface, and
// distinguishes "the code did not error" from "the provider accepted the
// message."
//
// # Background
//
// The keighl/postmark client's request path does not check the HTTP status
// code. It unmarshals whatever body Postmark returns into the destination
// struct and returns a nil Go error regardless. Postmark reports an unknown
// template alias as HTTP 422 with `"ErrorCode": 1101` in the response body,
// so a send using that client can log success while the message was never
// accepted. [PostmarkSender.Send] checks `res.ErrorCode` explicitly to close
// this gap. [CheckTemplates] confirms the required template aliases exist
// in the account, so a missing template is a startup failure instead of a
// send that appears to succeed.
//
// [ProviderBackedSender] lets a caller ask whether a given [Sender]'s nil
// error means the provider accepted the message, as opposed to a
// no-op fallback that also returns nil. A Sender that does not implement
// this interface is treated as not provider-backed.
//
// # In-process by default
//
// [LogSender] writes each send through slog and requires no external
// service; use it for local development or when no provider is configured
// yet. Because it is also what runs in production if a provider is never
// configured, it logs only the template alias and a hash of the recipient
// address at Info by default — not reversible for an address nobody
// already suspects, though a specific candidate address can always be
// confirmed by hashing it and comparing. The recipient address and the
// full template model — which for a real template can include a
// password-reset token or another sensitive value — are logged only when
// [LogSenderOptions.LogBodies] is set, at Debug; leave it unset outside
// development. `mail/testing`'s Recorder captures sends in memory for
// tests without logging anything. [PostmarkSender] is the provider-backed
// implementation and is optional.
//
// # Mustache/Mustachio note
//
// Postmark's templating language, Mustachio, treats a template-model key's
// presence as true for `{{#key}}…{{/key}}` sections, independent of its
// value. A section over a scalar value also rebinds `{{.}}` inside the
// section to that scalar, so a sibling key referenced inside the section
// renders empty. Build a template model so that a key is present only when
// it should print, and avoid referencing another key from inside a section
// keyed on a scalar.
//
// # Scope
//
// This package has no Fair Housing or other legal-content filtering; that
// belongs in a shared policy package used by every output channel. It also
// has no sender-persona rotation, brand copy, or template models — those
// are decisions for the service sending the mail.
package mail
