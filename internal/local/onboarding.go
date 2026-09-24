package local

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"thermora.local/thermora/internal/manufacturer"
)

type OnboardingAccess struct {
	ManufacturerLogin    string `json:"manufacturer_login"`
	ManufacturerPassword string `json:"manufacturer_password"`
	AccessCode           string `json:"partner_access_code"`
}

type OnboardingSeed struct {
	Seed                Seed
	Subject, AccessCode string
}

// ProvisionOnboarding preserves both equipment identity and the mock household on retries.
func ProvisionOnboarding(root, deviceName, origin, manufacturerOrigin string) error {
	var seed Seed
	seedPath := filepath.Join(root, "seed.json")
	if deviceName != "" {
		if err := ValidateDeviceName(deviceName); err != nil {
			return err
		}
		seedPath = filepath.Join(root, "devices", deviceName, "seed.json")
	}
	if err := Read(seedPath, &seed); err != nil {
		return err
	}
	dir := filepath.Join(root, "manufacturer")
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		return err
	}
	var access OnboardingAccess
	path := filepath.Join(root, "onboarding-access.json")
	if err := Read(path, &access); errors.Is(err, os.ErrNotExist) {
		access = OnboardingAccess{
			ManufacturerLogin:    "household@manufacturer.local",
			ManufacturerPassword: Secret(),
			AccessCode:           Secret(),
		}
		if err = writeJSON(path, access); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	seedOutput := filepath.Join(root, "onboarding.json")
	if root == CloudRoot && deviceName != "" {
		path = filepath.Join(root, "devices", deviceName, "onboarding-access.json")
		seedOutput = filepath.Join(root, "devices", deviceName, "onboarding.json")
		var existing OnboardingAccess
		if err := Read(path, &existing); errors.Is(err, os.ErrNotExist) {
			access.AccessCode = Secret()
			if err = writeJSON(path, access); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			access = existing
		}
	}
	var cfg CloudConfig
	if err := Read(filepath.Join(root, "cloud.json"), &cfg); err != nil {
		return err
	}
	if cfg.Manufacturer == nil {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		cfg.Manufacturer = &manufacturer.Config{
			APIURL:        "https://manufacturer:8444",
			AuthorizeURL:  manufacturerOrigin + "/oauth/authorize",
			RedirectURL:   origin + "/api/v1/manufacturer-authorizations/callback",
			ClientSecret:  Secret(),
			EncryptionKey: base64.RawStdEncoding.EncodeToString(key),
			CAFile:        "/run/pki/ca.pem",
		}
	}
	cfg.Manufacturer.AuthorizeURL = manufacturerOrigin + "/oauth/authorize"
	cfg.Manufacturer.RedirectURL = origin + "/api/v1/manufacturer-authorizations/callback"
	cfg.Manufacturer.OrganizationID = seed.OrganizationID
	if cfg.Manufacturer.Broker.Username == "" {
		cfg.Manufacturer.Broker = cfg.MQTT
		cfg.Manufacturer.Broker.Username = "manufacturer"
		cfg.Manufacturer.Broker.ClientID = "manufacturer"
		cfg.Manufacturer.Broker.Password = Secret()
	}
	if err := writeJSON(filepath.Join(root, "cloud.json"), cfg); err != nil {
		return err
	}
	var mock manufacturer.MockConfig
	mockPath := filepath.Join(dir, "config.json")
	if err := Read(mockPath, &mock); errors.Is(err, os.ErrNotExist) {
		mock = manufacturer.MockConfig{
			Login:        access.ManufacturerLogin,
			Password:     access.ManufacturerPassword,
			Subject:      uuid.NewString(),
			ClientSecret: cfg.Manufacturer.ClientSecret,
			RedirectURL:  cfg.Manufacturer.RedirectURL,
			Certificate:  "/run/pki/manufacturer.pem",
			Key:          "/run/pki/manufacturer.key",
			StatePath:    "/state/manufacturer.db",
			Devices:      []manufacturer.Device{{ID: seed.DeviceID, Name: "Demo space-heating pump"}},
		}
		if err = writeJSON(mockPath, mock); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	found := false
	for _, device := range mock.Devices {
		if device.ID == seed.DeviceID {
			found = true
		}
	}
	if !found {
		if len(mock.Devices) >= 25 {
			return errors.New("local manufacturer catalog is limited to 25 devices")
		}
		mock.Devices = append(
			mock.Devices,
			manufacturer.Device{ID: seed.DeviceID, Name: "Demo heat pump " + deviceName},
		)
		if err := writeJSON(mockPath, mock); err != nil {
			return err
		}
	}
	mock.OrganizationID = seed.OrganizationID
	mock.RedirectURL = cfg.Manufacturer.RedirectURL
	mock.MQTT = cfg.Manufacturer.Broker
	if err := writeJSON(mockPath, mock); err != nil {
		return err
	}
	if err := writeJSON(
		seedOutput,
		OnboardingSeed{Seed: seed, Subject: mock.Subject, AccessCode: access.AccessCode},
	); err != nil {
		return err
	}
	pair, err := tls.LoadX509KeyPair(
		filepath.Join(root, "pki", "manufacturer.pem"),
		filepath.Join(root, "pki", "manufacturer.key"),
	)
	if errors.Is(err, os.ErrNotExist) || (err == nil && pair.Leaf.VerifyHostname("manufacturer.localhost") != nil) {
		return serverCertificates(filepath.Join(root, "pki"), []string{"manufacturer"})
	}
	return err
}
