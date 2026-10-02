package anthropic

import "time"

// NewWithRequestTimeout is New with the time an unstreamed call is given,
// which New fixes at ten minutes, so that a test can outlast it.
func NewWithRequestTimeout(opts Options, timeout time.Duration) (*Client, error) {
	return newClient(opts, timeout)
}

// StreamIdleLimit is how long c waits for a stream's next event, or zero when
// it waits for ever.
func (c *Client) StreamIdleLimit() time.Duration { return c.idle }
