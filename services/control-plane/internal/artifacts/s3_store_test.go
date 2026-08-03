package artifacts

import "testing"

func TestNormalizeS3EndpointRejectsCredentialAndRouteInjection(t *testing.T) {
	for _, value := range []string{
		"https://operator:secret@s3.example.com",
		"https://s3.example.com/tenant",
		"https://s3.example.com?token=secret",
		"https://s3.example.com#tenant",
		"file:///tmp/artifacts",
	} {
		if _, _, err := normalizeS3Endpoint(value); err == nil {
			t.Fatalf("normalizeS3Endpoint accepted %q", value)
		}
	}
}

func TestNormalizeS3EndpointAcceptsOriginsOnly(t *testing.T) {
	for _, value := range []string{"https://s3.example.com", "http://minio:9000", "s3.example.com"} {
		if _, _, err := normalizeS3Endpoint(value); err != nil {
			t.Fatalf("normalizeS3Endpoint rejected %q: %v", value, err)
		}
	}
}
