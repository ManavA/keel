package middleware_test

import (
	"testing"
	"time"

	"github.com/ManavA/keel/httpx/middleware"
	"github.com/stretchr/testify/assert"
)

func TestRealIPTrustsHeaders(t *testing.T) {
	assert.False(t, middleware.RealIPOptions{}.TrustsHeaders(),
		"the zero value ignores forwarding headers, so nothing is trusted")

	assert.False(t, middleware.RealIPOptions{TrustedProxies: []string{"not-an-address"}}.TrustsHeaders(),
		"an entry that parses as nothing trusts nothing; a typo must narrow trust, not count as trust")

	assert.True(t, middleware.RealIPOptions{TrustedProxies: []string{"10.0.0.0/8"}}.TrustsHeaders())
	assert.True(t, middleware.RealIPOptions{TrustAnyPeer: true}.TrustsHeaders())
}

func TestRateLimitSharedBucketWarning(t *testing.T) {
	limited := &middleware.RateLimitOptions{Requests: 100, Window: time.Minute}

	assert.Empty(t, middleware.RateLimitSharedBucketWarning(middleware.RealIPOptions{}, nil),
		"no rate limit means no shared bucket, whatever RealIP says")

	assert.NotEmpty(t, middleware.RateLimitSharedBucketWarning(middleware.RealIPOptions{}, limited),
		"a default-key limit that trusts no forwarding header buckets every client behind a proxy together")

	assert.Empty(t,
		middleware.RateLimitSharedBucketWarning(
			middleware.RealIPOptions{TrustedProxies: []string{"10.0.0.0/8"}}, limited),
		"a trusted proxy header recovers the client address before the limiter keys on it")

	assert.Empty(t,
		middleware.RateLimitSharedBucketWarning(
			middleware.RealIPOptions{TrustAnyPeer: true}, limited))

	withKey := &middleware.RateLimitOptions{
		Requests: 100,
		Window:   time.Minute,
		Key:      middleware.KeyByHeader("X-Api-Key"),
	}
	assert.Empty(t, middleware.RateLimitSharedBucketWarning(middleware.RealIPOptions{}, withKey),
		"a custom key does not bucket by the client address, so RealIP cannot blind it")
}
