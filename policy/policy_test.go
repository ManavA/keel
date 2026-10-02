package policy_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ManavA/keel/policy"
)

func TestEffect_Valid(t *testing.T) {
	tests := []struct {
		name string
		e    policy.Effect
		want bool
	}{
		{name: "allow", e: policy.Allow, want: true},
		{name: "ask", e: policy.Ask, want: true},
		{name: "block", e: policy.Block, want: true},
		{name: "the zero effect", e: "", want: false},
		{name: "approve is read by the decoder but is not an effect", e: "approve", want: false},
		{name: "another case", e: "Allow", want: false},
		{name: "another word", e: "deny", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.e.Valid())
		})
	}
}

func TestEffects_AreWrittenAsThePlanSays(t *testing.T) {
	assert.Equal(t, "allow", string(policy.Allow))
	assert.Equal(t, "ask", string(policy.Ask))
	assert.Equal(t, "block", string(policy.Block))
}

func TestStrictest(t *testing.T) {
	tests := []struct {
		name    string
		effects []policy.Effect
		want    policy.Effect
	}{
		{name: "none", effects: nil, want: ""},
		{name: "one allow", effects: []policy.Effect{policy.Allow}, want: policy.Allow},
		{name: "one ask", effects: []policy.Effect{policy.Ask}, want: policy.Ask},
		{name: "one block", effects: []policy.Effect{policy.Block}, want: policy.Block},
		{name: "ask over allow", effects: []policy.Effect{policy.Allow, policy.Ask}, want: policy.Ask},
		{name: "ask over allow, other order", effects: []policy.Effect{policy.Ask, policy.Allow}, want: policy.Ask},
		{name: "block over ask", effects: []policy.Effect{policy.Block, policy.Ask}, want: policy.Block},
		{name: "block over ask, other order", effects: []policy.Effect{policy.Ask, policy.Block}, want: policy.Block},
		{name: "block over allow", effects: []policy.Effect{policy.Allow, policy.Block}, want: policy.Block},
		{name: "all three, strictest last", effects: []policy.Effect{policy.Allow, policy.Ask, policy.Block}, want: policy.Block},
		{name: "all three, strictest first", effects: []policy.Effect{policy.Block, policy.Ask, policy.Allow}, want: policy.Block},
		{name: "repeats", effects: []policy.Effect{policy.Allow, policy.Allow, policy.Allow}, want: policy.Allow},
		{name: "an effect that is none of the three counts as block", effects: []policy.Effect{policy.Allow, "bogus"}, want: policy.Block},
		{name: "the zero effect counts as block", effects: []policy.Effect{policy.Ask, ""}, want: policy.Block},
		{name: "an unknown effect alone is reported as block", effects: []policy.Effect{"bogus"}, want: policy.Block},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, policy.Strictest(tt.effects...))
		})
	}
}

func TestAction_JSON(t *testing.T) {
	tests := []struct {
		name   string
		action policy.Action
		want   string
	}{
		{
			name:   "a kind alone",
			action: policy.Action{Kind: "read"},
			want:   `{"kind":"read"}`,
		},
		{
			name: "every field",
			action: policy.Action{
				Kind: "send", Target: "email:ap@example.com",
				Attrs: map[string]any{"external": true, "amount": 12.5},
			},
			want: `{"kind":"send","target":"email:ap@example.com","attrs":{"amount":12.5,"external":true}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.action)
			assert.NoError(t, err)
			assert.JSONEq(t, tt.want, string(got))
			assert.Equal(t, tt.want, string(got))
		})
	}
}
