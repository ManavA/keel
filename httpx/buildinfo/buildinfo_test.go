package buildinfo

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolve(t *testing.T) {
	stamp := []debug.BuildSetting{
		{Key: "vcs.revision", Value: "stamped0000000000000000000000000000abcd"},
		{Key: "vcs.time", Value: "2026-01-02T03:04:05Z"},
		{Key: "vcs.modified", Value: "false"},
	}

	tests := []struct {
		name        string
		injectedRev string
		injectedAt  string
		settings    []debug.BuildSetting
		want        Info
	}{
		{
			name:        "the linker flag outranks the VCS stamp",
			injectedRev: "injected00000000000000000000000000001234",
			injectedAt:  "2026-02-03T04:05:06Z",
			settings:    stamp,
			want: Info{
				Revision: "injected00000000000000000000000000001234",
				BuiltAt:  "2026-02-03T04:05:06Z",
			},
		},
		{
			name:     "the VCS stamp is used when nothing was injected",
			settings: stamp,
			want: Info{
				Revision: "stamped0000000000000000000000000000abcd",
				BuiltAt:  "2026-01-02T03:04:05Z",
			},
		},
		{
			name: "a dirty tree is reported",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abc"},
				{Key: "vcs.modified", Value: "true"},
			},
			want: Info{Revision: "abc", Dirty: true},
		},
		{
			name: "nothing at all reads as unknown, not as empty",
			want: Info{Revision: Unknown},
		},
		{
			name:        "surrounding whitespace on an injected value is removed",
			injectedRev: "  abc\n",
			want:        Info{Revision: "abc"},
		},
		{
			name:        "an injected revision with a stamped time keeps its own empty time",
			injectedRev: "abc",
			settings:    stamp,
			want:        Info{Revision: "abc"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resolve(tt.injectedRev, tt.injectedAt, tt.settings))
		})
	}
}

func TestKnown(t *testing.T) {
	assert.False(t, Info{}.Known())
	assert.False(t, Info{Revision: Unknown}.Known())
	assert.True(t, Info{Revision: "abc"}.Known())
}

func TestShort(t *testing.T) {
	assert.Equal(t, Unknown, Info{}.Short())
	assert.Equal(t, Unknown, Info{Revision: Unknown}.Short())
	assert.Equal(t, "abc", Info{Revision: "abc"}.Short())
	assert.Equal(t, "0123456789ab", Info{Revision: "0123456789abcdef"}.Short())
}

func TestGetDoesNotPanic(t *testing.T) {
	// A test binary carries no vcs stamp, so this is only a smoke check that
	// Get reaches resolve and returns something comparable.
	assert.NotEmpty(t, Get().Revision)
}
