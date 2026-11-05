// Command beforefire-backend relays alerts from the BeforeFire sensor unit to
// APNs and forwards app commands to the sensor unit over MQTT.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/alert"
	"github.com/dericknorlan/beforefire-backend/internal/apns"
	"github.com/dericknorlan/beforefire-backend/internal/app"
	"github.com/dericknorlan/beforefire-backend/internal/config"
)

func main() {
	healthCheck := flag.Bool("healthcheck", false, "call GET /healthz on this container and exit (for the Docker HEALTHCHECK)")
	flag.Parse()

	cfg, err := config.Load()
	if *healthCheck {
		os.Exit(runHealthCheck(cfg.HTTPAddress))
	}
	logger := newLogger(cfg.LogLevel)
	if err != nil {
		logger.Error("configuration is not correct", "error", err)
		os.Exit(2)
	}

	sender, err := newSender(cfg, logger)
	if err != nil {
		logger.Error("cannot start the APNs client", "error", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.New(cfg, sender, logger).Run(ctx); err != nil {
		logger.Error("backend stopped", "error", err)
		os.Exit(1)
	}
}

func newSender(cfg config.Config, logger *slog.Logger) (alert.Sender, error) {
	if cfg.APNsDryRun {
		logger.Warn("APNS_DRY_RUN is true: notifications are written to the log and not sent")
		return apns.DryRunSender{Logger: logger}, nil
	}
	pemBytes, err := os.ReadFile(cfg.APNsKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read APNS_KEY_FILE: %w", err)
	}
	key, err := apns.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, err
	}
	logger.Info("APNs ready", "endpoint", cfg.APNsURL(), "topic", cfg.APNsTopic)
	return apns.NewClient(apns.Config{
		BaseURL: cfg.APNsURL(),
		Topic:   cfg.APNsTopic,
		KeyID:   cfg.APNsKeyID,
		TeamID:  cfg.APNsTeamID,
		Key:     key,
	})
}

func newLogger(level string) *slog.Logger {
	var slogLevel slog.Level
	if err := slogLevel.UnmarshalText([]byte(level)); err != nil {
		slogLevel = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slogLevel}))
}

// runHealthCheck returns 0 when /healthz answers 200 (MQTT connected).
// The runtime image has no shell or curl, so the binary does the check itself.
func runHealthCheck(httpAddress string) int {
	address := httpAddress
	if strings.HasPrefix(address, ":") {
		address = "127.0.0.1" + address
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + address + "/healthz")
	if err != nil {
		return 1
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
