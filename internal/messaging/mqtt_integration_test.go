//go:build integration

package messaging_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"thermora.local/thermora/internal/local"
	"thermora.local/thermora/internal/messaging"
	"thermora.local/thermora/internal/protocol"
	"thermora.local/thermora/internal/testutil"
)

func TestMQTTAuthenticationAndTenantACL(t *testing.T) {
	root := t.TempDir()
	if err := local.Provision(root); err != nil {
		t.Fatal(err)
	}
	config := []byte(
		"listener 8883\nallow_anonymous false\ncafile /run/pki/ca.pem\ncertfile /run/pki/mqtt.pem\nkeyfile /run/pki/mqtt.key\nplugin /usr/lib/mosquitto_dynamic_security.so\nplugin_opt_config_file /mosquitto/data/dynamic-security.json\n",
	)
	if err := os.WriteFile(filepath.Join(root, "mosquitto.conf"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	name := testutil.Container(
		t,
		// Match Compose: root's entrypoint would chown the private fixture on Linux.
		"--user",
		fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-p",
		"127.0.0.1::8883",
		"-v",
		root+"/pki:/run/pki:ro",
		"-v",
		root+"/mqtt:/mosquitto/data",
		"-v",
		root+"/mosquitto.conf:/mosquitto/config/mosquitto.conf:ro",
		"eclipse-mosquitto:2.0.22@sha256:199ea8ef2e35ec2b1b37e59cfd1dbae538ed4dfa4a2251a121a52215a6248a21",
	)
	var cfg local.CloudConfig
	if err := local.Read(filepath.Join(root, "cloud.json"), &cfg); err != nil {
		t.Fatal(err)
	}
	address := "tls://" + testutil.Port(t, name, "8883")
	cfg.Admin.URL = address
	cfg.Admin.CAFile = filepath.Join(root, "pki", "ca.pem")
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	adminClient, err := messaging.Connect(ctx, cfg.Admin, []string{messaging.ControlTopic + "/response"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = adminClient.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	admin := &messaging.Admin{Client: adminClient}
	org, device, password := uuid.NewString(), uuid.NewString(), local.Secret()
	if err = admin.ProvisionDevice(ctx, org, device, password); err != nil {
		t.Fatal(err)
	}
	if err = admin.ProvisionDevice(ctx, org, device, password); err != nil {
		t.Fatal("activation reconciliation failed", err)
	}
	settings := messaging.Settings{
		URL:      address,
		Username: device,
		ClientID: device,
		Password: password,
		CAFile:   cfg.Admin.CAFile,
	}
	edgeCtx, stopEdge := context.WithCancel(ctx)
	defer stopEdge()
	edge, err := messaging.Connect(edgeCtx, settings, []string{protocol.Topic(org, device, "command")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = edge.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err = edge.Publish(ctx, protocol.Topic(org, device, "telemetry"), []byte(`{"probe":true}`), false); err != nil {
		t.Fatal("own topic denied", err)
	}
	for _, topic := range []string{protocol.Topic(uuid.NewString(), uuid.NewString(), "telemetry"), messaging.ControlTopic, protocol.Topic(org, device, "command")} {
		if err = edge.Publish(ctx, topic, []byte(`{}`), false); err == nil {
			t.Fatalf("unauthorized publish accepted on %s", topic)
		}
	}
	if err = admin.DisableDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for edge.Ready.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if edge.Ready.Load() {
		t.Fatal("revocation did not disconnect existing session")
	}
	revokedCtx, stopRevoked := context.WithTimeout(ctx, time.Second)
	if err = edge.Wait(revokedCtx); err == nil {
		t.Fatal("revoked principal reconnected")
	}
	stopRevoked()
	stopEdge()
	// Re-enable only this test fixture to test independent ACL boundaries below.
	// Fresh identities are required by the Thermora retirement API.
	device = uuid.NewString()
	settings.Username = device
	settings.ClientID = device
	if err = admin.ProvisionDevice(ctx, org, device, password); err != nil {
		t.Fatal(err)
	}
	wildcardCtx, stopWildcard := context.WithTimeout(t.Context(), 2*time.Second)
	defer stopWildcard()
	wildcard, err := messaging.Connect(wildcardCtx, settings, []string{"#"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = wildcard.Wait(wildcardCtx); err == nil {
		t.Fatal("device principal subscribed to wildcard")
	}
	stopWildcard()
	badCtx, stop := context.WithTimeout(t.Context(), 2*time.Second)
	defer stop()
	settings.Password = "incorrect"
	bad, err := messaging.Connect(badCtx, settings, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = bad.Wait(badCtx); err == nil {
		t.Fatal("invalid principal authenticated")
	}
}
