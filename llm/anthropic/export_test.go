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

// NewWithBound is New with the bound on a body read whole, and on what a
// streamed reply may keep, set to bound in place of 32 MiB, so that a test
// of the bound need not move 33 MiB to reach it.
func NewWithBound(opts Options, bound int64) (*Client, error) {
	c, err := newClient(opts, defaultTimeout)
	if err != nil {
		return nil, err
	}
	c.bound = bound
	return c, nil
}

// Bound is the bound c holds a body and a streamed reply to.
func (c *Client) Bound() int64 { return c.bound }
