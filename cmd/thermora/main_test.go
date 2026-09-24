package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"thermora.local/thermora/internal/local"
	"thermora.local/thermora/internal/messaging"
)

func TestDemoAccessExplainsClaimAvailabilityWithoutPrintingSecrets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, claim, status, message string }{
		{"inventory", "synthetic-claim-value", "available", "Use claim_code, not device_id"},
		{"claimed", "", "already_claimed", "no new claim code was issued"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var output, diagnostics bytes.Buffer
			seed := local.Seed{Login: "owner@example.invalid", Password: "synthetic-password"}
			if err := writeDemoAccess(&output, &diagnostics, seed, tc.claim); err != nil {
				t.Fatal(err)
			}
			var access map[string]string
			if err := json.Unmarshal(output.Bytes(), &access); err != nil {
				t.Fatal("private access output is not valid JSON", err)
			}
			if access["claim_status"] != tc.status || access["claim_code"] != tc.claim ||
				access["password"] != seed.Password {
				t.Fatal("incorrect private access document")
			}
			if !strings.Contains(diagnostics.String(), tc.message) {
				t.Fatal("missing actionable claim status")
			}
			for _, secret := range []string{seed.Password, seed.Login, tc.claim} {
				if secret != "" && strings.Contains(diagnostics.String(), secret) {
					t.Fatal("credentials printed in diagnostics")
				}
			}
		})
	}
}

func TestComposeUpgradeRefreshesManufacturerTransport(t *testing.T) {
	t.Chdir(t.TempDir())
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	os.Args = []string{"thermora", "provision-onboarding"}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	var config local.CloudConfig
	if err := local.Read(".local/cloud.json", &config); err != nil {
		t.Fatal(err)
	}
	secret := config.Manufacturer.ClientSecret
	config.Manufacturer.Broker = messaging.Settings{} // Existing C1 installations had no fixture principal.
	// #nosec G117 -- isolated synthetic provisioning credentials stay inside this private test directory.
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(".local/cloud.json", data, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"thermora", "provision-local"}
	if err = run(); err != nil {
		t.Fatal(err)
	}
	if err = local.Read(".local/cloud.json", &config); err != nil {
		t.Fatal(err)
	}
	if config.Manufacturer.ClientSecret != secret || config.Manufacturer.Broker.Username == "" {
		t.Fatal("upgrade rotated OAuth trust or did not repair transport")
	}
}
