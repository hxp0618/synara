package validation

import "testing"

func TestOpaqueIdentifier(t *testing.T) {
	for _, value := range []string{"incident-hmac-v2", " relay/key@2026.08 "} {
		if normalized, ok := OpaqueIdentifier(value, 2, 200); !ok || normalized == "" {
			t.Fatalf("OpaqueIdentifier(%q) = %q, %v", value, normalized, ok)
		}
	}
	for _, value := range []string{"", "a", "-leading", "incident key", "incident\nkey"} {
		if _, ok := OpaqueIdentifier(value, 2, 200); ok {
			t.Fatalf("OpaqueIdentifier accepted %q", value)
		}
	}
}

func TestEmailNormalizesCaseAndWhitespace(t *testing.T) {
	value, err := Email("  Owner@Example.COM ")
	if err != nil {
		t.Fatal(err)
	}
	if value != "owner@example.com" {
		t.Fatalf("got %q", value)
	}
}

func TestSlugRejectsUnsafeValues(t *testing.T) {
	if _, err := Slug("Cross Tenant!", "invalid_slug", "Slug"); err == nil {
		t.Fatal("expected unsafe slug to be rejected")
	}
}
