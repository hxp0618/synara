package httpapi

import "testing"

func TestResolveSSOReturnLocationUsesOnlyConfiguredPlatformAdminURL(t *testing.T) {
	if got := resolveSSOReturnLocation("/settings", "https://admin.synara.example"); got != "/settings" {
		t.Fatalf("ordinary return location = %q", got)
	}
	if got := resolveSSOReturnLocation(platformAdminSSOReturnTo, "https://admin.synara.example"); got != "https://admin.synara.example" {
		t.Fatalf("Platform Admin return location = %q", got)
	}
	if got := resolveSSOReturnLocation(platformAdminSSOReturnTo, ""); got != "/" {
		t.Fatalf("missing Platform Admin fallback = %q", got)
	}
}
