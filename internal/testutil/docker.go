//go:build integration

// Package testutil starts only uniquely named, disposable integration-test containers.
package testutil

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func Container(t *testing.T, args ...string) string {
	t.Helper()
	name := "thermora-test-" + uuid.NewString()
	// A fresh checkout may need to pull the pinned image before starting it.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	command := append([]string{"run", "--rm", "-d", "--name", name}, args...)
	// #nosec G204 -- arguments are test-authored values passed directly to Docker without a shell.
	out, err := exec.CommandContext(ctx, "docker", command...).CombinedOutput()
	if err != nil {
		t.Fatalf("start test container: %v: %s", err, out)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// #nosec G204 -- cleanup can target only the unique container name generated above.
		if out, err := exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput(); err != nil {
			t.Errorf("remove %s: %v %s", name, err, out)
		}
	})
	return name
}

func Port(t *testing.T, name, port string) string {
	t.Helper()
	// #nosec G204 -- container name and port come from the integration test, with no shell expansion.
	out, err := exec.CommandContext(t.Context(), "docker", "port", name, port).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect test port: %v %s", err, out)
	}
	return strings.TrimSpace(string(out))
}
