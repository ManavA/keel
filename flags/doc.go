// Package flags evaluates per-subject feature flags: a percentage rollout
// plus an explicit allowlist, over flag definitions kept in a Store.
//
// A subject is usually a user or tenant id. Evaluation is deterministic: the
// same flag key and subject always land in the same percentage bucket, so a
// rollout neither flickers between requests nor needs sticky state. The hash
// is over the flag key and the subject together, so two flags at the same
// percentage admit different subjects.
//
// A disabled flag is off for everyone, including allowlisted subjects, so
// flipping Enabled is a kill switch. Subject keys are opaque to this package:
// matching is exact, and any normalization is the caller's job.
package flags
