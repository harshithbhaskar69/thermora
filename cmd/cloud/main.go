package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"thermora.local/thermora/internal/artifacts"
	"thermora.local/thermora/internal/cloud"
	"thermora.local/thermora/internal/local"
	"thermora.local/thermora/internal/manufacturer"
	"thermora.local/thermora/internal/messaging"
	"thermora.local/thermora/internal/storage"
	"thermora.local/thermora/web"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		slog.Error("cloud stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	path := flag.String("config", "/run/cloud.json", "local cloud configuration file")
	migrate := flag.Bool("migrate", false, "apply embedded database migrations and exit")
	flag.Parse()
	if *migrate {
		var cfg local.MigrationConfig
		if err := local.Read(*path, &cfg); err != nil {
			return err
		}
		if err := storage.Migrate(ctx, cfg.DatabaseURL); err != nil {
			return err
		}
		return storage.ProvisionAppRole(ctx, cfg.DatabaseURL, cfg.ApplicationPassword)
	}
	var cfg local.CloudConfig
	if err := local.Read(*path, &cfg); err != nil {
		return err
	}
	db, err := storage.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	admin, err := messaging.Connect(workerCtx, cfg.Admin, []string{messaging.ControlTopic + "/response"}, nil)
	if err != nil {
		return err
	}
	brokerAdmin := &messaging.Admin{Client: admin}
	provision, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = admin.Wait(provision); err != nil {
		return err
	}
	if err = brokerAdmin.ProvisionCloudRole(provision); err != nil {
		return err
	}
	rows, err := db.Query(provision, `SELECT organization_id,id FROM devices WHERE enrollment='active' ORDER BY id`)
	if err != nil {
		return err
	}
	type deviceRole struct{ org, device string }
	roles := []deviceRole{}
	for rows.Next() {
		var r deviceRole
		if err = rows.Scan(&r.org, &r.device); err != nil {
			rows.Close()
			return err
		}
		roles = append(roles, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, r := range roles {
		if err = brokerAdmin.ProvisionDeviceRole(provision, r.org, r.device); err != nil {
			return err
		}
	}
	topics := []string{}
	for _, suffix := range []string{"telemetry", "command/event", "config/reported", "demo/event", "presence", "hello", "heartbeat"} {
		topics = append(topics, "thermora/v1/org/+/device/+/"+suffix)
	}
	mqtt, err := messaging.Connect(workerCtx, cfg.MQTT, topics, nil)
	if err != nil {
		return err
	}
	app := &cloud.App{DB: db, MQTT: mqtt, Admin: brokerAdmin}
	if cfg.ArtifactsEndpoint != "" {
		app.Artifacts, err = artifacts.NewLocal(cfg.ArtifactsEndpoint, artifacts.Bucket)
		if err != nil {
			return err
		}
	}
	if cfg.Manufacturer != nil {
		app.Manufacturer, err = manufacturer.NewClient(*cfg.Manufacturer)
		if err != nil {
			return err
		}
		defer app.Manufacturer.HTTP.CloseIdleConnections()
		if cfg.Manufacturer.Broker.Username != "" {
			provision, cancel := context.WithTimeout(ctx, 30*time.Second)
			err = admin.Wait(provision)
			if err == nil {
				err = app.Admin.ProvisionManufacturer(
					provision,
					cfg.Manufacturer.OrganizationID,
					cfg.Manufacturer.Broker,
				)
			}
			cancel()
			if err != nil {
				return err
			}
		}
	}
	assets, err := web.Assets()
	if err != nil {
		return err
	}
	handler, err := app.Handler(assets)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              ":8443",
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	workerDone := make(chan error, 1)
	go func() { workerDone <- app.Run(workerCtx) }()
	serverDone := make(chan error, 1)
	go func() {
		slog.Info("cloud listening", "address", server.Addr)
		serverDone <- server.ListenAndServeTLS(cfg.Certificate, cfg.Key)
	}()
	select {
	case <-ctx.Done():
	case err = <-serverDone:
	}
	shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	shutdownErr := server.Shutdown(shutdownCtx)
	cancelWorkers()
	workerErr := <-workerDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return errors.Join(shutdownErr, workerErr)
}
