// Package cloudrun implements [jobs.Trigger] for Google Cloud Run.
//
// It is a separate package from jobs, rather than a type inside it, so that
// a caller who only needs [jobs.Outcome], [jobs.Complete] and
// [jobs.Scheduler] — the in-process default — does not pull in
// golang.org/x/oauth2/google and its dependency tree merely by importing
// jobs. Import this package only when wiring a [Trigger] to start another
// job directly on Cloud Run; jobs itself still declares the [jobs.Trigger]
// interface and [jobs.NoopTrigger], the default when no such wiring exists.
package cloudrun

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2/google"

	"github.com/ManavA/keel/jobs"
)

// TriggerOptions configures a [Trigger].
type TriggerOptions struct {
	// ProjectID and Region identify where the target jobs run. Both
	// required.
	ProjectID string
	Region    string
	// HTTPClient overrides the client built from ambient Application
	// Default Credentials. Set this in tests; leave it nil in production.
	HTTPClient *http.Client
}

// Trigger starts a Google Cloud Run job execution by calling the same Cloud
// Run Admin API endpoint a Cloud Scheduler job would post to.
type Trigger struct {
	projectID string
	region    string
	client    *http.Client
	baseURL   string
}

var _ jobs.Trigger = (*Trigger)(nil)

// NewTrigger builds a Trigger authenticated as the ambient service account
// (via google.DefaultClient), unless opts.HTTPClient is set. The caller's
// credentials need roles/run.invoker on every job this Trigger will start.
func NewTrigger(ctx context.Context, opts TriggerOptions) (*Trigger, error) {
	if opts.ProjectID == "" || opts.Region == "" {
		return nil, fmt.Errorf("cloudrun: ProjectID and Region are required")
	}
	client := opts.HTTPClient
	if client == nil {
		var err error
		client, err = google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			return nil, fmt.Errorf("default credentials: %w", err)
		}
		client.Timeout = 30 * time.Second
	}
	return &Trigger{
		projectID: opts.ProjectID,
		region:    opts.Region,
		client:    client,
		baseURL:   fmt.Sprintf("https://%s-run.googleapis.com", opts.Region),
	}, nil
}

// Run starts one execution of jobName and returns as soon as the execution
// is accepted; it does not wait for the job to finish.
func (t *Trigger) Run(ctx context.Context, jobName string) error {
	url := fmt.Sprintf("%s/apis/run.googleapis.com/v1/namespaces/%s/jobs/%s:run",
		strings.TrimSuffix(t.baseURL, "/"), t.projectID, jobName)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("run job %s: %w", jobName, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The body is read only to DECORATE an error that is already
		// certain from the status code; a read failure here yields "" and
		// a slightly less informative message rather than replacing a real
		// failure with a body-read failure.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("run job %s: status %d: %s", jobName, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
