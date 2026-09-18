package mail

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatAddress(t *testing.T) {
	tests := []struct {
		name  string
		addr  string
		email string
		want  string
	}{
		{"empty name uses bare address", "", testRecipient, testRecipient},
		{"simple name is not quoted", testBrand, "hello@acme.com", "Acme <hello@acme.com>"},
		{"comma in name is quoted, not left to split the address", "Smith, Jane", testRecipient, `"Smith, Jane" <a@b.com>`},
		{"quote in name is escaped", `Say "hi"`, testRecipient, `"Say \"hi\"" <a@b.com>`},
		{"backslash in name is escaped", `back\slash`, testRecipient, `"back\\slash" <a@b.com>`},
		{"whitespace around both is trimmed", "  Acme  ", "  hello@acme.com  ", "Acme <hello@acme.com>"},
		{"empty email with a name yields empty", testBrand, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, FormatAddress(tt.addr, tt.email))
		})
	}
}

type fakeProviderBackedSender struct{ delivers bool }

func (fakeProviderBackedSender) Send(context.Context, string, string, map[string]any) error {
	return nil
}
func (f fakeProviderBackedSender) DeliversToProvider() bool { return f.delivers }

type plainSender struct{}

func (plainSender) Send(context.Context, string, string, map[string]any) error { return nil }

func TestDeliversToProvider(t *testing.T) {
	tests := []struct {
		name   string
		sender Sender
		want   bool
	}{
		{"a sender that declares itself provider-backed", fakeProviderBackedSender{delivers: true}, true},
		{"a sender that declares itself NOT provider-backed", fakeProviderBackedSender{delivers: false}, false},
		{"a sender with no opinion defaults to NOT provider-backed", plainSender{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, DeliversToProvider(tt.sender))
		})
	}
}

func TestErrRecipientUndeliverable_IsDistinctSentinel(t *testing.T) {
	assert.NotNil(t, ErrRecipientUndeliverable)
	assert.Contains(t, ErrRecipientUndeliverable.Error(), "undeliverable")
}
