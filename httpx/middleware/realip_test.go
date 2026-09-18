package middleware_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ManavA/keel/httpx/middleware"
	"github.com/stretchr/testify/assert"
)

// seen runs one request through RealIP and reports the RemoteAddr the handler
// beneath it saw.
func seen(t *testing.T, opts middleware.RealIPOptions, remoteAddr string, forwarded ...string) string {
	t.Helper()

	var got string
	h := middleware.RealIP(opts)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.RemoteAddr
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remoteAddr
	for _, f := range forwarded {
		req.Header.Add("X-Forwarded-For", f)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

func TestRealIPIgnoresHeadersByDefault(t *testing.T) {
	// The whole reason the zero value exists. A service with no proxy in front
	// must not let a client name itself.
	got := seen(t, middleware.RealIPOptions{}, "203.0.113.9:4444", "1.2.3.4")
	assert.Equal(t, "203.0.113.9:4444", got)
}

func TestRealIPTakesTheRightmostUntrustedEntry(t *testing.T) {
	opts := middleware.RealIPOptions{TrustedProxies: []string{"10.0.0.0/8"}}

	tests := []struct {
		name      string
		forwarded []string
		want      string
	}{
		{
			// The defect this middleware exists for. A client that sends its
			// own X-Forwarded-For gets the leftmost slot, and an implementation
			// reading from the left believes it.
			name:      "a client-supplied entry on the left is not believed",
			forwarded: []string{"198.51.100.1, 203.0.113.9, 10.0.0.5"},
			want:      "203.0.113.9",
		},
		{
			name:      "one client and one proxy",
			forwarded: []string{"203.0.113.9, 10.0.0.5"},
			want:      "203.0.113.9",
		},
		{
			name:      "several trusted hops are skipped",
			forwarded: []string{"203.0.113.9, 10.0.0.5, 10.1.2.3, 10.4.5.6"},
			want:      "203.0.113.9",
		},
		{
			// A front end that appends the connecting client as the last entry
			// rather than prepending. The rightmost untrusted entry is still
			// the right answer.
			name:      "an appended client address is taken over the spoofed prefix",
			forwarded: []string{"9.9.9.9, 8.8.8.8, 203.0.113.9"},
			want:      "203.0.113.9",
		},
		{
			name:      "the header split across repeated header lines",
			forwarded: []string{"203.0.113.9", "10.0.0.5"},
			want:      "203.0.113.9",
		},
		{
			name:      "an unparseable entry does not hide the ones behind it",
			forwarded: []string{"203.0.113.9, not-an-ip, 10.0.0.5"},
			want:      "203.0.113.9",
		},
		{
			name:      "an entry carrying a port is accepted",
			forwarded: []string{"203.0.113.9:1234, 10.0.0.5"},
			want:      "203.0.113.9",
		},
		{
			name:      "an IPv6 client",
			forwarded: []string{"2001:db8::1, 10.0.0.5"},
			want:      "2001:db8::1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := seen(t, opts, "10.0.0.5:9999", tt.forwarded...)
			assert.Equal(t, net.JoinHostPort(tt.want, "0"), got)
		})
	}
}

func TestRealIPRequiresATrustedPeer(t *testing.T) {
	opts := middleware.RealIPOptions{TrustedProxies: []string{"10.0.0.0/8"}}

	// A connection from somewhere that is not a configured proxy: the header
	// could have been written by anyone, so it is not read at all.
	got := seen(t, opts, "203.0.113.50:5555", "1.2.3.4, 10.0.0.5")
	assert.Equal(t, "203.0.113.50:5555", got)
}

func TestRealIPTrustAnyPeer(t *testing.T) {
	opts := middleware.RealIPOptions{
		TrustedProxies: []string{"10.0.0.0/8"},
		TrustAnyPeer:   true,
	}
	got := seen(t, opts, "198.51.100.77:5555", "1.2.3.4, 203.0.113.9, 10.0.0.5")
	assert.Equal(t, "203.0.113.9:0", got)
}

func TestRealIPEveryEntryTrusted(t *testing.T) {
	// Nothing in the header is a client address, so there is no answer and
	// RemoteAddr is left alone rather than being set to a proxy.
	opts := middleware.RealIPOptions{TrustedProxies: []string{"10.0.0.0/8"}}
	got := seen(t, opts, "10.0.0.5:9999", "10.1.1.1, 10.0.0.5")
	assert.Equal(t, "10.0.0.5:9999", got)
}

func TestRealIPCustomHeader(t *testing.T) {
	var got string
	h := middleware.RealIP(middleware.RealIPOptions{
		TrustedProxies: []string{"10.0.0.0/8"},
		Header:         "X-Real-Ip",
	})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.RemoteAddr
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:9999"
	req.Header.Set("X-Real-Ip", "203.0.113.9")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	h.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, "203.0.113.9:0", got)
}

func TestRealIPRejectsAMalformedProxyEntry(t *testing.T) {
	// A typo in the proxy list narrows what is trusted; it must never widen it.
	opts := middleware.RealIPOptions{TrustedProxies: []string{"10.0.0.0/8", "not-a-cidr", ""}}
	got := seen(t, opts, "10.0.0.5:9999", "203.0.113.9, 10.0.0.5")
	assert.Equal(t, "203.0.113.9:0", got)
}
