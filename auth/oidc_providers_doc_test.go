package auth

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOIDCProviderMatrixDocCoversFiveProviders pins issue #43: the provider
// matrix doc must cover Auth0, Google, Apple, Cognito and Clerk, with the
// issuer, audience and verification-claim rows filled in each section. A
// provider row deleted from the doc fails here rather than silently.
func TestOIDCProviderMatrixDocCoversFiveProviders(t *testing.T) {
	raw, err := os.ReadFile("../docs/oidc-providers.md")
	require.NoError(t, err, "the OIDC provider matrix doc must exist at docs/oidc-providers.md")

	sections := splitProviderSections(t, string(raw))
	for _, provider := range []string{"Auth0", "Google", "Apple", "Cognito", "Clerk"} {
		body, ok := sections[provider]
		require.True(t, ok, "the matrix doc must have a section for %s", provider)
		for _, row := range []string{"Issuer", "Audience", "email_verified", "http"} {
			require.Contains(t, body, row,
				"%s's section must fill in the %s row (with a provider docs link)", provider, row)
		}
	}
}

// splitProviderSections indexes the doc's "## <provider>" sections by the
// provider name in the heading.
func splitProviderSections(t *testing.T, doc string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, chunk := range strings.Split(doc, "\n## ") {
		for _, provider := range []string{"Auth0", "Google", "Apple", "Cognito", "Clerk"} {
			heading, _, _ := strings.Cut(chunk, "\n")
			if strings.Contains(heading, provider) {
				out[provider] = chunk
			}
		}
	}
	return out
}
