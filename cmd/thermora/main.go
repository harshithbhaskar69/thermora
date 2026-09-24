package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"thermora.local/thermora/internal/artifacts"
	"thermora.local/thermora/internal/cloud"
	"thermora.local/thermora/internal/local"
	"thermora.local/thermora/internal/manufacturer"
	"thermora.local/thermora/internal/messaging"
	"thermora.local/thermora/internal/storage"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New(
			"usage: thermora provision-local | provision-device NAME | provision-onboarding | provision-cloud | cloud-cluster | cloud-device NAME | cloud-health | renew-local-certificates | seed [NAME] | seed-onboarding [-] | health",
		)
	}
	switch os.Args[1] {
	case "provision-local":
		if err := local.Provision(".local"); err != nil {
			return err
		}
		// Refresh an already-enabled Compose fixture when its transport contract evolves.
		if _, err := os.Stat(".local/onboarding.json"); err == nil {
			return local.ProvisionOnboarding(
				".local",
				"manufacturer",
				"https://localhost:8443",
				"https://manufacturer.localhost:8444",
			)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	case "provision-onboarding":
		if err := local.Provision(".local"); err != nil {
			return err
		}
		if err := local.ProvisionDevice(".local", "manufacturer"); err != nil {
			return err
		}
		return local.ProvisionOnboarding(
			".local",
			"manufacturer",
			"https://localhost:8443",
			"https://manufacturer.localhost:8444",
		)
	case "cloud-origins":
		return local.ConfigureCloudOrigins(os.Getenv("THERMORA_LOCALHOST") == "1")
	case "cloud-local-install", "cloud-local-uninstall":
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return local.InstallCloudDomain(ctx, os.Args[1] == "cloud-local-install")
	case "cloud-dns":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return local.CloudDNS(ctx)
	case "cloud-doctor":
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return local.CloudDoctor(ctx)
	case "cloud-access":
		origins, err := local.ReadCloudOrigins()
		if err != nil {
			return err
		}
		fmt.Println("Thermora Cloud:", origins.Cloud)
		fmt.Println("Sign up in the browser, or use the owner login/password from .local/cloud/seed.json.")
		fmt.Println("Operator login/password: .local/cloud/operator-access.json (Fleet operations).")
		fmt.Println("Connect a heat pump:", origins.Cloud+"/connect", "(choose Connect manufacturer account).")
		fmt.Println("Manufacturer authorization opens at:", origins.Manufacturer)
		fmt.Println("Its separate login/password and partner invitation are in .local/cloud/onboarding-access.json.")
		fmt.Println("onboarding.json is internal provisioning input. Compose uses separate accounts under .local/.")
		return nil
	case "provision-cloud":
		return local.ProvisionCloud()
	case "cloud-fleet":
		if len(os.Args) != 3 {
			return errors.New("usage: thermora cloud-fleet COUNT")
		}
		count, err := strconv.Atoi(os.Args[2])
		if err != nil || count < 1 || count > 25 {
			return errors.New("choose 1 to 25 named fleet devices")
		}
		origins, err := local.ReadCloudOrigins()
		if err != nil {
			return err
		}
		var catalog manufacturer.MockConfig
		if err = local.Read(filepath.Join(local.CloudRoot, "manufacturer", "config.json"), &catalog); err != nil {
			return err
		}
		added := 0
		for i := 1; i <= count; i++ {
			path := filepath.Join(local.CloudRoot, "devices", fmt.Sprintf("fleet-%02d", i), "seed.json")
			var seed local.Seed
			found := false
			if err = local.Read(path, &seed); err == nil {
				for _, device := range catalog.Devices {
					if device.ID == seed.DeviceID {
						found = true
						break
					}
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if !found {
				added++
			}
		}
		if len(catalog.Devices)+added > 25 {
			return errors.New("fleet would exceed the 25-device manufacturer catalog; choose a smaller COUNT")
		}
		for i := 1; i <= count; i++ {
			name := fmt.Sprintf("fleet-%02d", i)
			if err = local.ProvisionDevice(local.CloudRoot, name); err != nil {
				return err
			}
			if err = local.ProvisionOnboarding(local.CloudRoot, name, origins.Cloud, origins.Manufacturer); err != nil {
				return err
			}
		}
		return local.ProvisionCloud()
	case "cloud-device":
		if len(os.Args) != 3 {
			return errors.New("usage: thermora cloud-device NAME")
		}
		if err := local.ProvisionDevice(local.CloudRoot, os.Args[2]); err != nil {
			return err
		}
		origins, err := local.ReadCloudOrigins()
		if err != nil {
			return err
		}
		if err := local.ProvisionOnboarding(
			local.CloudRoot,
			os.Args[2],
			origins.Cloud,
			origins.Manufacturer,
		); err != nil {
			return err
		}
		return local.ProvisionCloud()
	case "cloud-cluster":
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		if err := local.ProvisionCluster(ctx); err != nil {
			return err
		}
		store, err := artifacts.NewLocal("http://127.0.0.1:4566", artifacts.Bucket)
		if err != nil {
			return err
		}
		return store.Provision(ctx)
	case "seed-onboarding":
		return seedOnboarding()
	case "provision-device":
		if len(os.Args) != 3 {
			return errors.New("usage: thermora provision-device NAME")
		}
		return local.ProvisionDevice(".local", os.Args[2])
	case "renew-local-certificates":
		return local.RenewCertificates(".local/pki")
	case "seed":
		seedPath := "/run/seed.json"
		if len(os.Args) == 3 {
			if err := local.ValidateDeviceName(os.Args[2]); err != nil {
				return err
			}
			seedPath = filepath.Join("/run/devices", os.Args[2], "seed.json")
		} else if len(os.Args) != 2 {
			return errors.New("usage: thermora seed [NAME]")
		}
		var cfg local.CloudConfig
		var seed local.Seed
		if err := local.Read("/run/cloud.json", &cfg); err != nil {
			return err
		}
		if err := local.Read(seedPath, &seed); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		db, err := storage.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer db.Close()
		app := &cloud.App{DB: db}
		claim, err := app.Seed(ctx, seed)
		if err != nil {
			return err
		}
		return writeDemoAccess(os.Stdout, os.Stderr, seed, claim)
	case "health", "cloud-health":
		ca, endpoint, timeout := "/run/pki/ca.pem", "https://localhost:8443/health/ready", 3*time.Second
		if os.Args[1] == "cloud-health" {
			origins, err := local.ReadCloudOrigins()
			if err != nil {
				return err
			}
			ca, endpoint, timeout = ".local/cloud/pki/ca.pem", origins.Cloud+"/health/ready", 60*time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			err := health(ctx, ca, endpoint)
			if err == nil || os.Args[1] == "health" {
				return err
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("cloud readiness timed out: %w", err)
			case <-tick.C:
			}
		}
	default:
		return errors.New("unknown command")
	}
}

func health(ctx context.Context, ca, endpoint string) error {
	tls, err := messaging.TLSConfig(ca)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tls}, Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(r)
	if err != nil {
		return err
	}
	closeErr := resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return errors.Join(fmt.Errorf("not ready: HTTP %d", resp.StatusCode), closeErr)
	}
	return closeErr
}

func seedOnboarding() error {
	var cfg local.CloudConfig
	var onboarding local.OnboardingSeed
	if err := local.Read("/run/cloud.json", &cfg); err != nil {
		return err
	}
	if len(os.Args) == 3 && os.Args[2] == "-" {
		if err := json.NewDecoder(io.LimitReader(os.Stdin, 64*1024)).Decode(&onboarding); err != nil {
			return err
		}
	} else if len(os.Args) == 2 {
		if err := local.Read("/run/onboarding.json", &onboarding); err != nil {
			return err
		}
	} else {
		return errors.New("usage: thermora seed-onboarding [-]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := storage.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	app := &cloud.App{DB: db}
	if _, err = app.Seed(ctx, onboarding.Seed); err != nil {
		return err
	}
	if err = app.SeedCloudOnboarding(ctx, onboarding.Seed, onboarding.Subject, onboarding.AccessCode); err != nil {
		return err
	}
	var operator local.OperatorAccess
	if err = local.Read("/run/operator-access.json", &operator); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return app.SeedOperator(ctx, operator)
}

func writeDemoAccess(output, diagnostics io.Writer, seed local.Seed, claim string) error {
	status := "available"
	message := "Claim code refreshed for 10 minutes. Use claim_code, not device_id, from the private access file."
	if claim == "" {
		status = "already_claimed"
		message = "This heat pump is already claimed; no new claim code was issued. Open its existing building, or add a separate pump with task demo:device:add DEVICE=<name>."
	}
	if err := json.NewEncoder(output).Encode(map[string]string{
		"url":             "https://localhost:8443",
		"login":           seed.Login,
		"password":        seed.Password,
		"claim_code":      claim,
		"claim_status":    status,
		"device_id":       seed.DeviceID,
		"organization_id": seed.OrganizationID,
	}); err != nil {
		return err
	}
	_, err := fmt.Fprintln(diagnostics, message)
	return err
}
