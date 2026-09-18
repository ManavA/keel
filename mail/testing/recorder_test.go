package testing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecorder_RecordsSends(t *testing.T) {
	r := New()

	require.NoError(t, r.Send(context.Background(), "a@b.com", "welcome", map[string]any{"name": "Jane"}))
	require.NoError(t, r.Send(context.Background(), "c@d.com", "goodbye", nil))

	sent := r.Sent()
	require.Len(t, sent, 2)
	assert.Equal(t, "a@b.com", sent[0].To)
	assert.Equal(t, "welcome", sent[0].TemplateAlias)
	assert.Equal(t, "Jane", sent[0].TemplateModel["name"])
	assert.Equal(t, "goodbye", sent[1].TemplateAlias)
}

func TestRecorder_SentReturnsACopy(t *testing.T) {
	r := New()
	require.NoError(t, r.Send(context.Background(), "a@b.com", "welcome", nil))

	sent := r.Sent()
	sent[0].To = "mutated@example.com"

	assert.Equal(t, "a@b.com", r.Sent()[0].To, "mutating the returned slice must not affect the recorder's own state")
}

func TestRecorder_DeliversToProvider_IsFalse(t *testing.T) {
	r := New()
	assert.False(t, r.DeliversToProvider())
}
