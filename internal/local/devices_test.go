package local

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestProvisionDevicePreservesIdentityAndSeparatesEquipment(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := Provision(root); err != nil {
		t.Fatal(err)
	}
	var original Seed
	if err := Read(filepath.Join(root, "seed.json"), &original); err != nil {
		t.Fatal(err)
	}
	seeds := map[string]Seed{}
	for _, name := range []string{"helsinki", "tampere", "helsinki"} {
		if err := ProvisionDevice(root, name); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, "devices", name)
		var seed Seed
		var edge EdgeConfig
		if err := Read(filepath.Join(dir, "seed.json"), &seed); err != nil {
			t.Fatal(err)
		}
		if err := Read(filepath.Join(dir, "edge.json"), &edge); err != nil {
			t.Fatal(err)
		}
		if previous, ok := seeds[name]; ok && previous != seed {
			t.Fatal("retry rotated device identity")
		}
		seeds[name] = seed
		if seed.OrganizationID != original.OrganizationID || seed.UserID != original.UserID ||
			seed.DeviceID == original.DeviceID ||
			seed.RunID == original.RunID ||
			seed.Bootstrap == original.Bootstrap {
			t.Fatal("additional equipment must share the owner but have its own device trust and run")
		}
		if edge.DeviceID != seed.DeviceID || edge.RunID != seed.RunID || edge.Bootstrap != seed.Bootstrap ||
			edge.ModbusURL != "tcp://sim-"+name+":1502" {
			t.Fatal("edge does not address its own equipment")
		}
		for _, file := range []string{"seed.json", "edge.json"} {
			info, err := os.Stat(filepath.Join(dir, file))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatal("device credentials are not private", err)
			}
		}
	}
	if seeds["helsinki"].DeviceID == seeds["tampere"].DeviceID ||
		seeds["helsinki"].Bootstrap == seeds["tampere"].Bootstrap {
		t.Fatal("device aliases reused an identity")
	}
	var unchanged Seed
	if err := Read(filepath.Join(root, "seed.json"), &unchanged); err != nil || original != unchanged {
		t.Fatal("primary device changed", err)
	}
	// Read only generated, synthetic test data; never report credential-bearing bytes.
	data, err := os.ReadFile(
		filepath.Join(root, "devices", "compose.json"),
	) // #nosec G304 -- root is a test-owned temporary directory.
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Services map[string]struct {
			Networks []string `json:"networks"`
			Volumes  []string `json:"volumes"`
			Ports    []string `json:"ports"`
		} `json:"services"`
		Networks map[string]struct{ Internal bool } `json:"networks"`
	}
	if err = json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Services) != 4 || len(manifest.Networks) != 2 {
		t.Fatal("manifest lost an existing device or duplicated a retry")
	}
	for name, seed := range seeds {
		network := "equipment-" + name
		if !manifest.Networks[network].Internal || len(manifest.Services["sim-"+name].Networks) != 1 ||
			manifest.Services["sim-"+name].Networks[0] != network ||
			len(manifest.Services["sim-"+name].Ports) != 0 {
			t.Fatal("equipment network is not private and isolated")
		}
		if bytes.Contains(data, []byte(seed.Bootstrap)) || bytes.Contains(data, []byte(seed.Password)) {
			t.Fatal("Compose manifest contains credentials")
		}
	}
}

func TestProvisionDeviceRejectsUnsafeNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "../other", "a/b", "a;touch", "a$(id)", "UPPER", "-flag", "a123456789012345678901234567890123"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := ProvisionDevice(t.TempDir(), name); err == nil {
				t.Fatal("invalid device alias accepted")
			}
		})
	}
}
