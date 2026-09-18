package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// defaultPostmarkBaseURL is Postmark's production API root.
const defaultPostmarkBaseURL = "https://api.postmarkapp.com"

// CheckTemplatesOptions configures [CheckTemplates].
type CheckTemplatesOptions struct {
	// ServerToken authenticates against one Postmark server. Required.
	ServerToken string
	// HTTPClient overrides the client used to reach Postmark. Nil uses
	// http.DefaultClient.
	HTTPClient *http.Client
	// BaseURL overrides Postmark's API root. Empty uses the production
	// API; set this in tests to point at an httptest server.
	BaseURL string
}

// templateProbeResponse is the subset of Postmark's template-lookup
// response this check reads. See CheckTemplates for why it is read this
// way instead of through the keighl/postmark client's GetTemplate.
type templateProbeResponse struct {
	ErrorCode int64  `json:"ErrorCode"`
	Message   string `json:"Message"`
}

// CheckTemplates confirms that every alias in required exists as a template
// in the Postmark account authenticated by opts.ServerToken, and returns one
// error naming every alias that does not.
//
// Call this once at process startup, before the process accepts traffic
// that could trigger a send. This turns a missing template into a startup
// failure instead of a send that fails silently the first time that
// template is used. See the package doc.
//
// This makes its own HTTP call to `GET /templates/{alias}` rather than
// using the keighl/postmark client's GetTemplate, because that client
// ignores the HTTP status code and returns a nil Go error even for a
// missing-alias response. This function reads the response status and
// body directly instead, and — unlike a check built on that client — it
// distinguishes a confirmed missing template (404 or 422) from a check
// that could not be completed (401/403, a rate limit, a 5xx, or any other
// unexpected status), returning an error for the latter rather than
// reporting the template as missing. An invalid server token should not
// read as a template problem.
func CheckTemplates(ctx context.Context, required []string, opts CheckTemplatesOptions) error {
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = defaultPostmarkBaseURL
	}

	var missing []string
	for _, alias := range required {
		ok, err := templateExists(ctx, client, baseURL, opts.ServerToken, alias)
		if err != nil {
			return fmt.Errorf("check template %q: %w", alias, err)
		}
		if !ok {
			missing = append(missing, alias)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("mail: %d required template(s) not found in this Postmark account: %s",
			len(missing), strings.Join(missing, ", "))
	}
	return nil
}

// templateExists reports whether alias is a confirmed template in the
// account, or returns an error when the question could not be answered at
// all — see CheckTemplates for why those two outcomes are kept distinct.
func templateExists(ctx context.Context, client *http.Client, baseURL, serverToken, alias string) (bool, error) {
	reqURL := fmt.Sprintf("%s/templates/%s", strings.TrimRight(baseURL, "/"), url.PathEscape(alias))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Postmark-Server-Token", serverToken)

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("request template %q: %w", alias, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var body templateProbeResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return false, fmt.Errorf("decode response for template %q: %w", alias, err)
		}
		// A 200 status with a nonzero ErrorCode in the body is also
		// Postmark's shape for "no such template" in some cases; see the
		// package doc's account of the 1101 error code.
		return body.ErrorCode == 0, nil

	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity:
		// A confirmed, definite answer: this alias does not exist in the
		// account.
		return false, nil

	default:
		// Every other status — 401/403 (an invalid or missing server
		// token), 429 (rate limited), a 5xx, or anything unexpected —
		// means this check could not be completed. Reporting it as
		// "missing" would send an operator looking for a template that in
		// fact exists, while the real problem goes unreported.
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return false, fmt.Errorf("could not check template %q: unexpected status %d: %s",
			alias, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}
