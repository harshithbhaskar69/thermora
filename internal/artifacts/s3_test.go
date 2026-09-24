package artifacts

import "testing"

func TestLocalEndpointGuard(t *testing.T) {
	for _, endpoint := range []string{"https://s3.amazonaws.com", "http://floci:4566.evil.invalid", "http://127.0.0.1:4566/redirect", ""} {
		if _, err := NewLocal(endpoint, Bucket); err == nil {
			t.Fatalf("accepted non-local endpoint %q", endpoint)
		}
	}
	if _, err := NewLocal("http://127.0.0.1:4566", "arn:aws:s3:::bucket"); err == nil {
		t.Fatal("accepted bucket ARN")
	}
}
