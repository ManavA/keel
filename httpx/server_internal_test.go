package httpx

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The timeouts are applied inside NewServer and are not readable from outside
// the package, so this test lives here. Without it the defaults are unasserted:
// the slowloris test passes an explicit ReadHeaderTimeout, so dropping the
// default would leave the suite green.
func TestServerDefaultsAreApplied(t *testing.T) {
	srv := NewServer(ServerOptions{Addr: "127.0.0.1:0"})

	assert.Equal(t, DefaultReadHeaderTimeout, srv.http.ReadHeaderTimeout)
	assert.Equal(t, DefaultReadTimeout, srv.http.ReadTimeout)
	assert.Equal(t, DefaultWriteTimeout, srv.http.WriteTimeout)
	assert.Equal(t, DefaultIdleTimeout, srv.http.IdleTimeout)
	assert.Equal(t, DefaultShutdownTimeout, srv.shutdown)
	assert.Positive(t, DefaultReadHeaderTimeout,
		"net/http leaves this unset, and unset means a client can hold a connection open forever")
}

func TestServerOptionsOverrideTheDefaults(t *testing.T) {
	srv := NewServer(ServerOptions{
		Addr:              "127.0.0.1:0",
		ReadHeaderTimeout: 1,
		ReadTimeout:       2,
		WriteTimeout:      3,
		IdleTimeout:       4,
		ShutdownTimeout:   5,
	})

	assert.EqualValues(t, 1, srv.http.ReadHeaderTimeout)
	assert.EqualValues(t, 2, srv.http.ReadTimeout)
	assert.EqualValues(t, 3, srv.http.WriteTimeout)
	assert.EqualValues(t, 4, srv.http.IdleTimeout)
	assert.EqualValues(t, 5, srv.shutdown)
}
