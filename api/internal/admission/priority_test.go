package admission

import (
	"context"
	"testing"
)

func TestTrustedPriorityPolicyDoesNotTrustIntent(t *testing.T) {
	policy, err := NewTrustedPriorityPolicy("rebuild", map[string][]string{
		"rinspace-publish": {"publish"},
		"rinspace-preview": {"preview"},
		"renderer-admin":   {"publish", "migration"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		principal string
		intent    string
		want      string
		wantError bool
	}{
		{principal: "unknown", intent: "", want: "rebuild"},
		{principal: "unknown", intent: "publish", wantError: true},
		{principal: "rinspace-publish", intent: "publish", want: "publish"},
		{principal: "rinspace-publish", intent: "migration", wantError: true},
		{principal: "rinspace-preview", intent: "preview", want: "preview"},
		{principal: "renderer-admin", intent: "migration", want: "migration"},
		{principal: "renderer-admin", intent: "root", wantError: true},
	}
	for _, test := range tests {
		got, err := policy.AssignPriority(context.Background(), test.principal, test.intent)
		if test.wantError {
			if err == nil {
				t.Fatalf("AssignPriority(%q, %q) = %q", test.principal, test.intent, got)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Fatalf("AssignPriority(%q, %q) = %q, %v; want %q", test.principal, test.intent, got, err, test.want)
		}
	}
}
