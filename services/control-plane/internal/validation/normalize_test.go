package validation

import "testing"

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
