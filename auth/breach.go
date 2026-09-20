package auth

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // G505: SHA-1 is mandated by the HIBP k-anonymity range protocol, not a security choice
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// defaultBreachEndpoint is the have-i-been-pwned password range endpoint. The
// checker only ever sends it the first 5 hex characters of a password's SHA-1,
// never the password or its full hash (see HIBPBreachChecker).
const defaultBreachEndpoint = "https://api.pwnedpasswords.com/range"

// BreachChecker reports whether a password appears in a breach corpus. The
// Service consults it, when configured, on every path that sets a password;
// a nil BreachChecker disables the check entirely (the default, so a service
// with no network access never blocks signup or reset on an unreachable
// endpoint).
type BreachChecker interface {
	// Breached reports whether password is present in the corpus. An error
	// covers transport and protocol failures, never the verdict itself.
	Breached(ctx context.Context, password string) (bool, error)
}

// HIBPBreachChecker is a BreachChecker over the k-anonymity range protocol
// have-i-been-pwned defines: the client hashes the password with SHA-1, sends
// only the first 5 hex characters to Endpoint, and compares the returned
// "SUFFIX:COUNT" lines against the remainder locally. Any endpoint speaking
// that shape works, which is what makes the check unit-testable against a
// fake server.
type HIBPBreachChecker struct {
	// Endpoint is the range base URL (prefix appended as the next path
	// segment). Defaults to defaultBreachEndpoint.
	Endpoint string
	// HTTPClient makes the range requests. Defaults to a client with
	// defaultHTTPTimeout, matching the OIDC verifier's bound.
	HTTPClient *http.Client
}

// Breached implements BreachChecker.
func (c *HIBPBreachChecker) Breached(ctx context.Context, password string) (bool, error) {
	sum := sha1.Sum([]byte(password)) //nolint:gosec // SHA-1 mandated by HIBP k-anonymity, not a choice
	digest := strings.ToUpper(hex.EncodeToString(sum[:]))
	prefix, suffix := digest[:5], digest[5:]

	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = defaultBreachEndpoint
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/"+prefix, nil)
	if err != nil {
		return false, fmt.Errorf("auth: build breach range request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("auth: breach range request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("auth: breach range request returned status %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	// A range response is a few hundred short lines; the default 64KB buffer
	// cap is far above any line this protocol defines.
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(strings.TrimSpace(line), ":", 2)
		if len(parts) != 2 {
			continue
		}
		if !strings.EqualFold(parts[0], suffix) {
			continue
		}
		count, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return false, fmt.Errorf("auth: malformed breach range line: %q", line)
		}
		return count > 0, nil
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("auth: read breach range response: %w", err)
	}
	return false, nil
}

// rejectBreachedPassword consults the Service's BreachChecker, if one is
// configured, and writes a 400 when the password appears in the breach
// corpus. It reports whether the caller should stop: true means the response
// is written. A nil checker (the default) never stops anything, and a checker
// failure fails OPEN with a warning — an unreachable breach endpoint must not
// take down signup or password reset, which are availability-critical paths.
func (s *Service) rejectBreachedPassword(w http.ResponseWriter, r *http.Request, password string) bool {
	if s.breach == nil {
		return false
	}
	breached, err := s.breach.Breached(r.Context(), password)
	if err != nil {
		s.logger(r.Context()).Warn("auth: breach check failed; allowing password", "error", err)
		return false
	}
	if breached {
		http.Error(w, "this password has appeared in a data breach; please choose a different one", http.StatusBadRequest)
		return true
	}
	return false
}
