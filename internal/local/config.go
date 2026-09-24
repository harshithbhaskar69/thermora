// Package local manages explicitly local configuration and demo provisioning.
package local

import (
	"encoding/json"
	"fmt"
	"os"

	"thermora.local/thermora/internal/manufacturer"
	"thermora.local/thermora/internal/messaging"
)

type CloudConfig struct {
	ArtifactsEndpoint string               `json:"artifacts_endpoint,omitempty"`
	Manufacturer      *manufacturer.Config `json:",omitempty"`
	DatabaseURL       string               `json:"database_url"`
	MQTT              messaging.Settings   `json:"mqtt"`
	Admin             messaging.Settings   `json:"admin"`
	Certificate       string               `json:"certificate"`
	Key               string               `json:"key"`
}

// MigrationConfig is mounted only in the one-shot migration container.
type MigrationConfig struct {
	DatabaseURL         string `json:"database_url"`
	ApplicationPassword string `json:"application_password"`
}
type EdgeConfig struct {
	DeviceID  string `json:"device_id"`
	RunID     string `json:"run_id"`
	RunEpoch  uint32 `json:"run_epoch"`
	Bootstrap string `json:"bootstrap"`
	CloudURL  string `json:"cloud_url"`
	CAFile    string `json:"ca_file"`
	MQTTURL   string `json:"mqtt_url"`
	ModbusURL string `json:"modbus_url"`
}
type Seed struct {
	OrganizationID string `json:"organization_id"`
	UserID         string `json:"user_id"`
	Login          string `json:"login"`
	Password       string `json:"password"`
	DeviceID       string `json:"device_id"`
	RunID          string `json:"run_id"`
	Bootstrap      string `json:"bootstrap"`
}

func Read(path string, value any) error {
	// #nosec G304 -- the local operator chooses this config path; no network input reaches it.
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read configuration: %w", err)
	}
	if len(data) > 64*1024 {
		return fmt.Errorf("configuration too large")
	}
	if err = json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode configuration: %w", err)
	}
	return nil
}
