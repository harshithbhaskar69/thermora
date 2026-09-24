package local

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"thermora.local/thermora/internal/manufacturer"
)

func TestDomainHostsPreservesUnrelatedEntriesAndRejectsConflicts(t *testing.T) {
	t.Parallel()
	original := []byte("127.0.0.1 localhost\n::1 localhost\n# user comment\n")
	installed, err := domainHosts(original, "fixture", true)
	if err != nil {
		t.Fatal(err)
	}
	again, err := domainHosts(installed, "fixture", true)
	if err != nil || !bytes.Equal(installed, again) {
		t.Fatal("installation is not idempotent", err)
	}
	removed, err := domainHosts(installed, "fixture", false)
	if err != nil || !bytes.Equal(original, removed) {
		t.Fatal("uninstall changed unrelated entries", err)
	}
	for _, conflict := range []string{
		"1.2.3.4 cloud.thermora.io\n",
		"::1 manufacturer.thermora.io\n",
		string(installed),
		"127.0.0.1 cloud.thermora.io manufacturer.thermora.io # manual entry\n",
		"127.0.0.1 cloud.thermora.io manufacturer.thermora.io extra.local # thermora-local:" + strings.Repeat("a", 64) + "\n",
		"192.0.2.1 cloud.thermora.io manufacturer.thermora.io # thermora-local:" + strings.Repeat("a", 64) + "\n",
		"127.0.0.1 cloud.thermora.io manufacturer.thermora.io # thermora-local:" + strings.Repeat("z", 64) + "\n",
	} {
		if _, err := domainHosts([]byte(conflict), "other-installation", true); err == nil {
			t.Fatal("accepted conflicting host entry")
		}
	}
}

func TestDomainHostsReclaimsGeneratedEntryAfterLocalReset(t *testing.T) {
	t.Parallel()
	original := []byte("127.0.0.1 localhost\n# preserved comment\n")
	previousCA, currentCA := strings.Repeat("a", 64), strings.Repeat("b", 64)
	previous, err := domainHosts(original, previousCA, true)
	if err != nil {
		t.Fatal(err)
	}
	current, err := domainHosts(previous, currentCA, true)
	if err != nil {
		t.Fatal("reinstall after local reset failed", err)
	}
	want, err := domainHosts(original, currentCA, true)
	if err != nil || !bytes.Equal(current, want) {
		t.Fatalf("reinstall produced %q, want %q: %v", current, want, err)
	}
	oldUninstall, err := domainHosts(current, previousCA, false)
	if err != nil || !bytes.Equal(oldUninstall, current) {
		t.Fatal("old installation removed the current hosts entry", err)
	}
	removed, err := domainHosts(current, currentCA, false)
	if err != nil || !bytes.Equal(removed, original) {
		t.Fatal("current uninstall changed unrelated entries", err)
	}
}

func TestPublicOriginMigrationPreservesIdentity(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := ConfigureCloudOrigins(true); err != nil {
		t.Fatal(err)
	}
	if err := ProvisionOnboarding(CloudRoot, "", localhostOrigins.Cloud, localhostOrigins.Manufacturer); err != nil {
		t.Fatal(err)
	}
	var before CloudConfig
	var mockBefore manufacturer.MockConfig
	if err := Read(filepath.Join(CloudRoot, "cloud.json"), &before); err != nil {
		t.Fatal(err)
	}
	if err := Read(filepath.Join(CloudRoot, "manufacturer/config.json"), &mockBefore); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureCloudOrigins(false); err != nil {
		t.Fatal(err)
	}
	if err := ProvisionOnboarding(CloudRoot, "", domainOrigins.Cloud, domainOrigins.Manufacturer); err != nil {
		t.Fatal(err)
	}
	var after CloudConfig
	var mockAfter manufacturer.MockConfig
	if err := Read(filepath.Join(CloudRoot, "cloud.json"), &after); err != nil {
		t.Fatal(err)
	}
	if err := Read(filepath.Join(CloudRoot, "manufacturer/config.json"), &mockAfter); err != nil {
		t.Fatal(err)
	}
	if after.Manufacturer.RedirectURL != domainOrigins.Cloud+"/api/v1/manufacturer-authorizations/callback" ||
		mockAfter.RedirectURL != after.Manufacturer.RedirectURL {
		t.Fatal("callbacks diverged")
	}
	if before.Manufacturer.EncryptionKey != after.Manufacturer.EncryptionKey ||
		mockBefore.Subject != mockAfter.Subject ||
		mockBefore.Password != mockAfter.Password ||
		mockBefore.ClientSecret != mockAfter.ClientSecret {
		t.Fatal("origin migration changed identity")
	}
	if _, err := os.Stat(filepath.Join(CloudRoot, "gateway.json")); err != nil {
		t.Fatal(err)
	}
}
