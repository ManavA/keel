// Package media decodes, resizes, and stores photos, and defines a job
// pattern for doing this in the background: fetch a source, derive
// variants, store them, and record what was stored.
//
// Recording what was stored makes a re-run idempotent: a run that is
// interrupted and retried does not re-download or re-derive variants that
// already landed. Fetch failures are split into permanent (the source will
// never be available again) and transient (retry later), so a permanently
// missing source is marked as such instead of being retried on every run.
// Decode enforces a maximum input size, a maximum decoded pixel count, and
// checks the actual content type of the input rather than a caller-supplied
// one. All three are required together: a small, validly-encoded file can
// still decode to a very large pixel buffer, so the byte limit alone does
// not bound memory use.
//
// Store is the storage interface variants are written through. LocalStore
// is the default implementation, backed by the local filesystem, with
// Handler to serve it over HTTP; it requires no external service. GCSStore
// is an alternative implementation backed by Google Cloud Storage, for a
// caller that has a bucket. The two implementations do not depend on each
// other.
package media
