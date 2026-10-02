package agenttest_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ManavA/keel/agent/agenttest"
)

var kitStart = time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)

func TestClock_ReadsItsStartUntilMoved(t *testing.T) {
	clock := agenttest.NewClock(kitStart)

	assert.Equal(t, kitStart, clock.Now())
	assert.Equal(t, kitStart, clock.Now(), "reading the clock does not move it")
}

func TestClock_Advance(t *testing.T) {
	tests := []struct {
		name  string
		steps []time.Duration
		want  time.Time
	}{
		{name: "one step", steps: []time.Duration{time.Second}, want: kitStart.Add(time.Second)},
		{
			name:  "steps add up",
			steps: []time.Duration{time.Second, 30 * time.Second, time.Millisecond},
			want:  kitStart.Add(31*time.Second + time.Millisecond),
		},
		{name: "a step of nothing", steps: []time.Duration{0}, want: kitStart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := agenttest.NewClock(kitStart)
			for _, d := range tt.steps {
				clock.Advance(d)
			}
			assert.Equal(t, tt.want, clock.Now())
		})
	}
}

// Run under -race: an engine's heartbeat reads the clock from its own
// goroutine while the test moves it.
func TestClock_IsSafeForConcurrentUse(t *testing.T) {
	const (
		goroutines = 8
		steps      = 200
	)
	clock := agenttest.NewClock(kitStart)

	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range steps {
				clock.Advance(time.Millisecond)
				_ = clock.Now()
			}
		})
	}
	wg.Wait()

	assert.Equal(t, kitStart.Add(goroutines*steps*time.Millisecond), clock.Now())
}
