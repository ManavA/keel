package jobs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
)

// Trigger starts another job by name. It is the interface a pipeline stage
// uses to start the next stage directly, instead of waiting for that next
// stage's own independent [Entry] tick — useful when stage B should start
// the moment stage A finishes rather than up to one whole Interval later.
type Trigger interface {
	Run(ctx context.Context, jobName string) error
}

// CloudRunTriggerOptions configures a [CloudRunTrigger].
type CloudRunTriggerOptions struct {
	// ProjectID and Region identify where the target jobs run. Both
	// required.
	ProjectID string
	Region    string
	// HTTPClient overrides the client built from ambient Application
	// Default Credentials. Set this in tests; leave it nil in production.
	HTTPClient *http.Client
}

// CloudRunTrigger starts a Google Cloud Run job execution by calling the
// same Cloud Run Admin API endpoint a Cloud Scheduler job would post to.
type CloudRunTrigger struct {
	projectID string
	region    string
	client    *http.Client
	baseURL   string
}

// NewCloudRunTrigger builds a trigger authenticated as the ambient service
// account (via google.DefaultClient), unless opts.HTTPClient is set. The
// caller's credentials need roles/run.invoker on every job this trigger will
// start.
func NewCloudRunTrigger(ctx context.Context, opts CloudRunTriggerOptions) (*CloudRunTrigger, error) {
	if opts.ProjectID == "" || opts.Region == "" {
		return nil, fmt.Errorf("jobs: ProjectID and Region are required")
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
	return &CloudRunTrigger{
		projectID: opts.ProjectID,
		region:    opts.Region,
		client:    client,
		baseURL:   fmt.Sprintf("https://%s-run.googleapis.com", opts.Region),
	}, nil
}

// Run starts one execution of jobName and returns as soon as the execution
// is accepted; it does not wait for the job to finish.
func (t *CloudRunTrigger) Run(ctx context.Context, jobName string) error {
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

// NoopTrigger stands in when nothing should actually be started — local
// runs, tests, or a pipeline stage that has not been wired to a real
// scheduler yet.
type NoopTrigger struct {
	// Logger, if set, receives a debug line per call. Nil is silent.
	Logger *slog.Logger
}

// Run logs the request (if a Logger is set) and returns nil without
// starting anything.
func (t NoopTrigger) Run(_ context.Context, jobName string) error {
	if t.Logger != nil {
		t.Logger.Debug("job trigger disabled", "job", jobName)
	}
	return nil
}
