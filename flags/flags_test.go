package flags_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/flags"
)

// The issue's acceptance check: a 50 percent flag assigns the same subjects
// consistently, and allowlisted subjects always pass.
func TestEvaluate_HalfFlagIsStableAndAllowlistPasses(t *testing.T) {
	f := flags.Flag{Key: "new-checkout", Enabled: true, Percentage: 50}

	first := map[string]bool{}
	for i := 0; i < 200; i++ {
		subject := fmt.Sprintf("user-%d", i)
		first[subject] = flags.Evaluate(f, subject)
	}

	on := 0
	for subject, want := range first {
		assert.Equal(t, want, flags.Evaluate(f, subject), "subject %q changed buckets", subject)
		if want {
			on++
		}
	}
	// 200 subjects at 50 percent land near half; a broken hash that admits
	// everyone or no one must fail loudly instead of passing on stability
	// alone.
	assert.InDelta(t, 100, on, 30, "expected about half of 200 subjects enabled, got %d", on)

	allowed := f
	allowed.Allow = []string{"user-7", "user-199"}
	for _, subject := range []string{"user-7", "user-199"} {
		assert.True(t, flags.Evaluate(allowed, subject), "allowlisted subject %q must pass", subject)
	}

	// Allowlist passes even when the percentage bucket says no: a flag at 0
	// percent admits nobody by rollout, but an allowlisted subject still
	// passes.
	require.True(t, flags.Evaluate(flags.Flag{Key: "x", Enabled: true, Allow: []string{"vip-1"}}, "vip-1"))
	assert.False(t, flags.Evaluate(flags.Flag{Key: "x", Enabled: true}, "vip-1"))
}

func TestEvaluate_DisabledIsOffForEveryone(t *testing.T) {
	f := flags.Flag{Key: "k", Enabled: false, Percentage: 100, Allow: []string{"vip-1"}}
	assert.False(t, flags.Evaluate(f, "vip-1"))
	assert.False(t, flags.Evaluate(f, "anyone"))
}

func TestMemoryStore_RoundTrip(t *testing.T) {
	s := flags.NewMemoryStore()
	ctx := t.Context()

	_, err := s.Get(ctx, "missing")
	assert.ErrorIs(t, err, flags.ErrNotFound)

	require.NoError(t, s.Upsert(ctx, flags.Flag{Key: "a", Enabled: true, Percentage: 10}))
	require.NoError(t, s.Upsert(ctx, flags.Flag{Key: "b", Enabled: true, Percentage: 100, Allow: []string{"vip"}}))

	got, err := s.Get(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, []string{"vip"}, got.Allow)

	all, err := s.List(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

func TestFlag_Validate(t *testing.T) {
	assert.Error(t, (flags.Flag{Enabled: true}).Validate(), "empty key")
	assert.Error(t, (flags.Flag{Key: "k", Percentage: -1}).Validate())
	assert.Error(t, (flags.Flag{Key: "k", Percentage: 101}).Validate())
	assert.NoError(t, (flags.Flag{Key: "k", Percentage: 50}).Validate())
}
