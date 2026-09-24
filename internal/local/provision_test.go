package local

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"

	"thermora.local/thermora/internal/manufacturer"
)

func TestProvisionPreservesIdentityAndIssuesVerifiableCertificates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := Provision(root); err != nil {
		t.Fatal(err)
	}
	read := func(name string) []byte {
		t.Helper()
		// #nosec G304 -- both the temporary directory and fixture filenames are test-controlled.
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	seed, ca := read("seed.json"), read("pki/ca.pem")
	if err := Provision(root); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seed, read("seed.json")) || !bytes.Equal(ca, read("pki/ca.pem")) {
		t.Fatal("idempotent provisioning rotated identity")
	}
	if err := RenewCertificates(filepath.Join(root, "pki")); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("invalid root certificate")
	}
	for _, name := range []string{"cloud", "mqtt"} {
		pair, err := tls.X509KeyPair(read("pki/"+name+".pem"), read("pki/"+name+".key"))
		if err != nil {
			t.Fatal(err)
		}
		for _, host := range []string{name, "localhost", "127.0.0.1"} {
			if _, err = pair.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: host}); err != nil {
				t.Fatal(err)
			}
		}
		if len(pair.Leaf.AuthorityKeyId) == 0 {
			t.Fatal("leaf lacks authority key identifier required by strict TLS clients")
		}
	}
	for _, name := range []string{"cloud.json", "migrate.json", "seed.json", "pki/ca.key", "pki/cloud.key", "pki/mqtt.key"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s is not a private local file: %v", name, err)
		}
	}
	var cloud CloudConfig
	var migration MigrationConfig
	if err := Read(filepath.Join(root, "cloud.json"), &cloud); err != nil {
		t.Fatal(err)
	}
	if err := Read(filepath.Join(root, "migrate.json"), &migration); err != nil {
		t.Fatal(err)
	}
	if cloud.DatabaseURL == migration.DatabaseURL {
		t.Fatal("application and migration database identities are not separated")
	}
}

func TestCloudOnboardingPreservesHouseholdAndIndependentInvitations(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := Provision(CloudRoot); err != nil {
		t.Fatal(err)
	}
	provision := func(name string) OnboardingAccess {
		t.Helper()
		if name != "" {
			if err := ProvisionDevice(CloudRoot, name); err != nil {
				t.Fatal(err)
			}
		}
		if err := ProvisionOnboarding(
			CloudRoot,
			name,
			"https://localhost:9443",
			"https://manufacturer.localhost:9444",
		); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(CloudRoot, "onboarding-access.json")
		if name != "" {
			path = filepath.Join(CloudRoot, "devices", name, "onboarding-access.json")
		}
		var access OnboardingAccess
		if err := Read(path, &access); err != nil {
			t.Fatal(err)
		}
		return access
	}
	primary, named := provision(""), provision("e2e")
	if primary.AccessCode == named.AccessCode || primary.ManufacturerPassword != named.ManufacturerPassword {
		t.Fatal("devices need separate invitations and the same mock household")
	}
	if provision("") != primary || provision("e2e") != named {
		t.Fatal("reprovisioning rotated private access")
	}
	var mock manufacturer.MockConfig
	if err := Read(filepath.Join(CloudRoot, "manufacturer/config.json"), &mock); err != nil {
		t.Fatal(err)
	}
	if len(mock.Devices) != 2 || mock.Devices[0].ID == mock.Devices[1].ID {
		t.Fatal("catalog does not retain independent equipment")
	}
	pair, err := tls.LoadX509KeyPair(
		filepath.Join(CloudRoot, "pki/manufacturer.pem"),
		filepath.Join(CloudRoot, "pki/manufacturer.key"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err = pair.Leaf.VerifyHostname("manufacturer.localhost"); err != nil {
		t.Fatal(err)
	}
}
