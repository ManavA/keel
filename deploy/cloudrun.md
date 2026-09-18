# Running a keel service on Cloud Run

This covers the Cloud Run-specific details in `deploy/Dockerfile` and
`deploy/cloudbuild.yaml`. It assumes the service and health-check
conventions in `httpx`.

## Cloud Run args append to the container's ENTRYPOINT

A `--args` flag passed to `gcloud run deploy` or set on a service is appended
to the image's `ENTRYPOINT`, not used in place of it. Passing a flag the
binary does not understand fails the container's startup, and the resulting
error is a generic "container failed to start" with no indication that the
cause is an unrecognized flag.

## Build-time vs. runtime environment variables

A binary reads environment variables at runtime, so any variable it needs
must be present in the container's runtime environment, not only passed as a
Docker build argument. This matters most for a companion frontend that
inlines some variables into a client-side bundle at build time (Next.js's
`NEXT_PUBLIC_*` convention is one example): a variable inlined into the
bundle is fixed for that build, but the container's own runtime code reads
`process.env` (or the language equivalent) fresh at request time. Set the
value as a build argument at image-build time for the bundle, and as a
runtime environment variable on the Cloud Run service for anything the
server process reads directly. Missing the second one produces a service
that falls back to a default (often `localhost`) for server-side requests
even though the client-side bundle is correct.

## Migrations run as a Cloud Run job, not on service startup

A migration job needs its own image, built from the same source as the
service. If the job's image is rebuilt only occasionally, running the job
against a stale image applies whatever migrations were current when that
image was last built and reports success, without applying anything added
since. Rebuild the job's image immediately before running it, in the same
deploy step, rather than relying on a previously built image still being
current.

## Scheduler wiring

Cloud Scheduler HTTP targets and Cloud Run job executions triggered on a
schedule are both plain resources that must be created and enabled
explicitly; neither is implied by deploying the service or job. Confirm a
scheduled job is both created and in the `ENABLED` state, not only created,
after setting it up.

## Verifying what is actually running

A build succeeding and a deployment serving that build are two different
facts. `scripts/check-deployed-revision.sh` compares a deployed service's
reported build revision (see `httpx/buildinfo`) against a local git
revision, and reports one of three outcomes: the deployed revision matches,
the deployed revision is a known but different commit, or the deployed
revision cannot be determined at all. The third case is deliberately not
folded into the second: "behind" and "unknown" call for different
responses, and a check that reports both as the same thing is itself a gap
in what verification covers.
