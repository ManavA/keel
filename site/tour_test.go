// Executable copies of the samples quoted in tour.html. If a sample stops
// compiling or its output changes, this test fails first; update the page
// with it. TestTourQuotesKeptInSync in site_test.go guards the other
// direction, that the page keeps quoting each API.
package site_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/config"
	"github.com/ManavA/keel/flags"
	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/jobs"
	"github.com/ManavA/keel/retry"
	"github.com/ManavA/keel/textpolicy"
)

var errTourFlaky = errors.New("upstream reset the connection")

func TestTourRetry(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := retry.Do(ctx, func() error {
		calls++
		if calls < 3 {
			return errTourFlaky
		}
		return nil
	}, retry.Options{MaxAttempts: 5, BaseDelay: time.Millisecond})
	require.NoError(t, err)
	require.Equal(t, 3, calls)
}

func TestTourTextPolicy(t *testing.T) {
	policy := textpolicy.New(textpolicy.Rule{
		Name:    "spam-payout",
		Pattern: regexp.MustCompile(`(?i)\bfree-money-now\b`),
	})
	require.NoError(t, policy.Check("a note about the roof repair quote"))
	err := policy.Check("claim your free-money-now prize today")
	var violation *textpolicy.ViolationError
	require.ErrorAs(t, err, &violation)
	require.Equal(t, "spam-payout", violation.Rule)
}

func TestTourFlags(t *testing.T) {
	ctx := context.Background()
	store := flags.NewMemoryStore()
	require.NoError(t, store.Upsert(ctx, flags.Flag{
		Key:     "new-editor",
		Enabled: true,
		Allow:   []string{"you@example.com"},
	}))
	flag, err := store.Get(ctx, "new-editor")
	require.NoError(t, err)
	require.True(t, flags.Evaluate(flag, "you@example.com"))
	require.False(t, flags.Evaluate(flag, "stranger@example.com"))
}

func TestTourJobsOutcome(t *testing.T) {
	run := jobs.Outcome{Attempted: 3, Succeeded: 2, Failed: 1}
	require.Equal(t, "partial", run.Status())
	require.True(t, run.OK())
	require.Equal(t, "idle", jobs.Outcome{}.Status())
	require.Equal(t, "did-nothing", jobs.Outcome{Attempted: 2}.Status())
}

func TestTourHttpx(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.JSON(rec, http.StatusOK, map[string]string{"title": "Roof repair quote"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"title":"Roof repair quote"}`, rec.Body.String())

	// A nil slice reaches the browser as [], never null.
	empty := httptest.NewRecorder()
	httpx.JSON(empty, http.StatusOK, []string(nil))
	require.Equal(t, "[]\n", empty.Body.String())

	// Errors carry the status phrase and a request id, never the input.
	missing := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/notes/nope", nil)
	httpx.NotFound(missing, req)
	require.Equal(t, http.StatusNotFound, missing.Code)
	require.JSONEq(t, `{"error":"not found"}`, missing.Body.String())
}

func TestTourConfigRedact(t *testing.T) {
	require.Equal(t,
		"postgres://keel:[redacted]@127.0.0.1:5544/keel?sslmode=disable",
		config.RedactURL("postgres://keel:s3cret@127.0.0.1:5544/keel?sslmode=disable"))
	require.Equal(t, "[unset]", config.RedactURL(""))
}
