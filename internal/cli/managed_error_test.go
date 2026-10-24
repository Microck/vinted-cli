package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/Microck/vinted-cli/internal/galleton"
	"github.com/Microck/vinted-cli/internal/vinted"
)

func TestManagedErrorPreservesProtocolAndMissingResourceFailures(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
		kind   vinted.Kind
	}{
		{"protocol_error", 502, vinted.KindAPIChanged},
		{"not_found", 404, vinted.KindGeneric},
		{"reauth_required", 401, vinted.KindAuth},
		{"renewal_uncertain", 409, vinted.KindAuth},
		{"upstream_blocked", 403, vinted.KindBlocked},
	} {
		t.Run(tc.code, func(t *testing.T) {
			err := managedError(&galleton.Error{Code: tc.code, Status: tc.status})
			var got *vinted.Error
			if !errors.As(err, &got) || got.Kind != tc.kind || got.Status != tc.status || got.Retryable {
				t.Fatalf("classification: %#v", err)
			}
			if tc.kind != vinted.KindAuth && strings.Contains(got.Msg, "vinted rejected the stored session") {
				t.Fatal(got.Msg)
			}
			if tc.code == "protocol_error" && (!strings.Contains(got.Msg, "protocol_error") || !strings.Contains(got.Msg, "502")) {
				t.Fatal(got.Msg)
			}
		})
	}
}
