package identity_test

import (
	"testing"

	"thermora.local/thermora/internal/identity"
)

func TestPasswordVerification(t *testing.T) {
	t.Parallel()
	hash := identity.HashPassword("correct horse")
	if !identity.CheckPassword(hash, "correct horse") || identity.CheckPassword(hash, "wrong horse") ||
		identity.CheckPassword("corrupt", "correct horse") {
		t.Fatal("credential verification failed")
	}
}
