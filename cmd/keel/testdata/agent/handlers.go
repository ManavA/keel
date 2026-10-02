package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/httpx"
)

// operatorName is who an approval or a cancellation is recorded as decided
// by. There is one token and so one name. A real service mounts these routes
// behind its admin service and names the admin who is logged in.
const operatorName = "operator"

// batchInput is what the coordinator is asked to do.
const batchInput = "Review the current batch of documents and send one digest of it to " + digestRecipient + "."

// maxStartKey bounds an Idempotency-Key.
const maxStartKey = 200

// requireOperator lets a request through when it carries the operator token
// as a bearer token. Both sides are hashed first, so the comparison takes the
// same time whatever the lengths, and then compared in constant time.
func requireOperator(token string) func(http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scheme, given, ok := strings.Cut(r.Header.Get("Authorization"), " ")
			got := sha256.Sum256([]byte(strings.TrimSpace(given)))
			if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				httpx.Unauthorized(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// operatorActor is httpapi's Actor. It names whoever passed requireOperator;
// it is a name for the record, and the middleware is the check.
func operatorActor(*http.Request) string { return operatorName }

// withDeadline bounds a request. The router is built with no timeout, since
// one would cut the event stream off, so a route that should have one takes
// it here.
func withDeadline(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// startBatch handles POST /api/batches: it records a coordinator run and
// answers 202 with it. Nothing is executed here; the worker picks the run up.
// An Idempotency-Key makes the start safe to repeat: the same key returns the
// run the first request started.
func startBatch(engine *agent.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if len(key) > maxStartKey {
			httpx.BadRequest(w, r, errors.New("Idempotency-Key is too long"))
			return
		}
		run, err := engine.Start(r.Context(), agent.StartRequest{
			Agent:    agentCoordinator,
			Input:    batchInput,
			Key:      key,
			Metadata: map[string]string{"started_by": operatorName},
		})
		if err != nil {
			httpx.InternalError(w, r, err)
			return
		}
		httpx.JSON(w, http.StatusAccepted, run)
	}
}
