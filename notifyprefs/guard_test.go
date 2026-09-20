package notifyprefs_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/mail"
	mailtesting "github.com/ManavA/keel/mail/testing"
	"github.com/ManavA/keel/notifyprefs"
)

// resolveByAlias maps each template alias to a fixed category for one user,
// which is all the acceptance check needs.
func resolveByAlias(userID string, cats map[string]notifyprefs.Category) notifyprefs.ResolveFunc {
	return func(_ context.Context, _, templateAlias string) (string, notifyprefs.Category) {
		return userID, cats[templateAlias]
	}
}

func TestGuardedSender_SuppressesOptOutButNotSecurity(t *testing.T) {
	ctx := context.Background()
	store := notifyprefs.NewMemoryStore()
	rec := mailtesting.New()
	sender := notifyprefs.NewGuardedSender(rec, store, resolveByAlias("user-1", map[string]notifyprefs.Category{
		"marketing-alias": notifyprefs.CategoryMarketing,
		"security-alias":  notifyprefs.CategorySecurity,
	}))
	var _ mail.Sender = sender

	prefs := notifyprefs.Preferences{}
	prefs.Set(notifyprefs.CategoryMarketing, notifyprefs.ChannelEmail, true)
	require.NoError(t, store.Set(ctx, "user-1", prefs))

	err := sender.Send(ctx, "user@example.com", "marketing-alias", nil)
	assert.ErrorIs(t, err, notifyprefs.ErrSuppressed)
	assert.Empty(t, rec.Sent(), "suppressed send must not reach the inner sender")

	require.NoError(t, sender.Send(ctx, "user@example.com", "security-alias", nil))
	require.Len(t, rec.Sent(), 1)
	assert.Equal(t, "security-alias", rec.Sent()[0].TemplateAlias)
}

func TestGuardedSender_UnknownUserSends(t *testing.T) {
	ctx := context.Background()
	store := notifyprefs.NewMemoryStore()
	rec := mailtesting.New()
	sender := notifyprefs.NewGuardedSender(rec, store, resolveByAlias("nobody", map[string]notifyprefs.Category{
		"marketing-alias": notifyprefs.CategoryMarketing,
	}))

	// No preferences row exists for this user: the safe default is to send.
	require.NoError(t, sender.Send(ctx, "new@example.com", "marketing-alias", nil))
	assert.Len(t, rec.Sent(), 1)
}

func TestGuardedSender_EmptyUserSends(t *testing.T) {
	ctx := context.Background()
	store := notifyprefs.NewMemoryStore()
	rec := mailtesting.New()
	sender := notifyprefs.NewGuardedSender(rec, store, resolveByAlias("", map[string]notifyprefs.Category{
		"marketing-alias": notifyprefs.CategoryMarketing,
	}))

	// An unresolvable user cannot be checked against the store; fail open.
	require.NoError(t, sender.Send(ctx, "user@example.com", "marketing-alias", nil))
	assert.Len(t, rec.Sent(), 1)
}
