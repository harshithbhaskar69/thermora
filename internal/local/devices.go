package local

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/google/uuid"
)

var deviceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidateDeviceName restricts local device aliases to safe path and Compose names.
func ValidateDeviceName(name string) error {
	if !deviceNamePattern.MatchString(name) {
		return errors.New(
			"device name must start with a lowercase letter and contain only lowercase letters, digits or hyphens (1–32 characters)",
		)
	}
	return nil
}

// ProvisionDevice creates an independent equipment/edge identity under the existing demo organization.
// Reusing a name preserves its identity and state; database enrollment remains a separate step.
func ProvisionDevice(root, name string) (err error) {
	if err = ValidateDeviceName(name); err != nil {
		return err
	}
	var base Seed
	if err = Read(filepath.Join(root, "seed.json"), &base); err != nil {
		return err
	}
	devices := filepath.Join(root, "devices")
	if err = os.MkdirAll(devices, 0o700); err != nil {
		return err
	}
	// ponytail: one local provisioner at a time; use crash-released OS locking if unattended provisioning is needed.
	lock := filepath.Join(devices, ".provision-lock")
	if err = os.Mkdir(lock, 0o700); err != nil {
		return fmt.Errorf(
			"device provisioning is locked; after an interrupted command, verify no provisioner is running before removing %s: %w",
			lock,
			err,
		)
	}
	defer func() { err = errors.Join(err, os.Remove(lock)) }()
	dir := filepath.Join(devices, name)
	for _, part := range []string{"", "sim-state", "edge-state"} {
		if err = os.MkdirAll(filepath.Join(dir, part), 0o700); err != nil {
			return err
		}
	}
	seed := base
	path := filepath.Join(dir, "seed.json")
	if _, err = os.Stat(path); errors.Is(err, os.ErrNotExist) {
		seed.DeviceID, seed.RunID, seed.Bootstrap = uuid.NewString(), uuid.NewString(), Secret()
		if err = writeJSON(path, seed); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if err = Read(path, &seed); err != nil {
		return err
	}
	if seed.OrganizationID != base.OrganizationID || seed.UserID != base.UserID {
		return errors.New("device belongs to a different local identity; refusing to replace it")
	}
	var edge EdgeConfig
	if err = Read(filepath.Join(root, "edge.json"), &edge); err != nil {
		return err
	}
	edge.DeviceID, edge.RunID, edge.RunEpoch, edge.Bootstrap, edge.ModbusURL = seed.DeviceID, seed.RunID, 1, seed.Bootstrap, "tcp://sim-"+name+":1502"
	if err = writeJSON(filepath.Join(dir, "edge.json"), edge); err != nil {
		return err
	}
	entries, err := os.ReadDir(devices)
	if err != nil {
		return err
	}
	services, networks := map[string]any{}, map[string]any{}
	for _, entry := range entries {
		if !entry.IsDir() || !deviceNamePattern.MatchString(entry.Name()) {
			continue
		}
		// A partial provision must be repaired by rerunning that name, never started silently.
		var cfg EdgeConfig
		if err = Read(filepath.Join(devices, entry.Name(), "edge.json"), &cfg); err != nil {
			return err
		}
		alias := entry.Name()
		network := "equipment-" + alias
		networks[network] = map[string]bool{"internal": true}
		// All paths in a Compose override resolve relative to deploy/compose/compose.yaml.
		mount := filepath.ToSlash(filepath.Join("..", "..", root, "devices", alias)) + "/"
		for _, kind := range []string{"sim", "edge"} {
			service := map[string]any{
				"image": "thermora:local", "pull_policy": "never", "command": []string{kind},
				"user": "${THERMORA_UID}:${THERMORA_GID}", "read_only": true,
				"cap_drop": []string{"ALL"}, "security_opt": []string{"no-new-privileges:true"},
				"tmpfs": []string{"/tmp"}, "init": true, "restart": "unless-stopped",
				"networks": []string{network}, "volumes": []string{mount + "sim-state:/state"},
			}
			if kind == "edge" {
				service["networks"] = []string{network, "uplink"}
				service["volumes"] = []string{
					mount + "edge-state:/state",
					mount + "edge.json:/run/edge.json:ro",
					filepath.ToSlash(filepath.Join("..", "..", root, "pki", "ca.pem")) + ":/run/pki/ca.pem:ro",
				}
				dependencies := map[string]any{"sim-" + alias: map[string]string{"condition": "service_started"}}
				if root != CloudRoot {
					dependencies["cloud"] = map[string]string{"condition": "service_healthy"}
				}
				service["depends_on"] = dependencies
			}
			services[kind+"-"+alias] = service
		}
	}
	manifest := filepath.Join(devices, "compose.json")
	if err = writeJSON(manifest+".tmp", map[string]any{"services": services, "networks": networks}); err != nil {
		return err
	}
	return os.Rename(manifest+".tmp", manifest)
}
