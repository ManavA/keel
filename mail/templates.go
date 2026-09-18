package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
// missing-alias response. This function reads the response body's
// ErrorCode field directly instead.
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

func templateExists(ctx context.Context, client *http.Client, baseURL, serverToken, alias string) (bool, error) {
	url := fmt.Sprintf("%s/templates/%s", strings.TrimRight(baseURL, "/"), alias)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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

	// A non-2xx with no readable body is still a definite "not found" or
	// "not authorized" — either way, not a confirmed template.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, nil
	}

	var body templateProbeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, fmt.Errorf("decode response for template %q: %w", alias, err)
	}
	// The load-bearing check. A 200 status with a nonzero ErrorCode in the
	// body IS Postmark's shape for "no such template" — see the function
	// doc.
	return body.ErrorCode == 0, nil
}
