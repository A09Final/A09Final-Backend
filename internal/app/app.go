// Package app connects the MQTT client, the alert relay, the command service,
// and the HTTP API into one running backend.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/alert"
	"github.com/dericknorlan/beforefire-backend/internal/command"
	"github.com/dericknorlan/beforefire-backend/internal/config"
	"github.com/dericknorlan/beforefire-backend/internal/device"
	"github.com/dericknorlan/beforefire-backend/internal/httpapi"
	"github.com/dericknorlan/beforefire-backend/internal/liveactivity"
	"github.com/dericknorlan/beforefire-backend/internal/mqtt"
	"github.com/dericknorlan/beforefire-backend/internal/telemetry"
)

const (
	// Time for one alert report, including APNs retries.
	alertHandlingTimeout = 30 * time.Second
	shutdownTimeout      = 10 * time.Second
	readHeaderTimeout    = 5 * time.Second
	subscriptionQoS      = 1
)

// App is the complete backend.
type App struct {
	config         config.Config
	logger         *slog.Logger
	mqtt           *mqtt.Client
	relay          *alert.Relay
	commands       *command.Service
	statuses       *device.StatusTracker
	readings       *telemetry.Store
	liveActivities *liveactivity.Service
	server         *http.Server
}

// New builds the backend. sender is *apns.Client or apns.DryRunSender.
func New(cfg config.Config, sender alert.Sender, logger *slog.Logger) *App {
	application := &App{config: cfg, logger: logger, statuses: device.NewStatusTracker(), readings: telemetry.NewStore()}

	var tlsConfig *tls.Config
	if cfg.MQTTUseTLS {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	application.mqtt = mqtt.NewClient(mqtt.Options{
		Address:   cfg.MQTTAddress,
		TLSConfig: tlsConfig,
		ClientID:  cfg.MQTTClientID,
		Username:  cfg.MQTTUsername,
		Password:  cfg.MQTTPassword,
		KeepAlive: cfg.MQTTKeepAlive,
		Subscriptions: []mqtt.Subscription{
			{Filter: device.Filter(device.KindAlert), QoS: subscriptionQoS},
			{Filter: device.Filter(device.KindAck), QoS: subscriptionQoS},
			{Filter: device.Filter(device.KindStatus), QoS: subscriptionQoS},
			{Filter: device.Filter(device.KindTelemetry), QoS: subscriptionQoS},
		},
		OnMessage: application.handleMessage,
		OnConnectionChange: func(isConnected bool) {
			if !isConnected {
				logger.Warn("mqtt disconnected")
			}
		},
		Logger: logger,
	})

	application.commands = command.NewService(command.ServiceConfig{
		Publisher:        application.mqtt,
		Statuses:         application.statuses,
		AnswerTimeout:    cfg.CommandAnswerTimeout,
		AllowedClockSkew: cfg.CommandClockSkew,
		Logger:           logger,
	})

	var onAlarm func(context.Context, string, alert.Report)
	if cfg.LiveActivityStateFile != "" {
		activities, err := liveactivity.Open(cfg.LiveActivityStateFile, sender, logger)
		if err != nil {
			logger.Error("Live Activity store unavailable", "error", err)
		} else {
			application.liveActivities = activities
			onAlarm = func(ctx context.Context, deviceID string, report alert.Report) {
				now := time.Now()
				if snapshot, ok := application.readings.Latest(deviceID); ok && snapshot.Report.MeasuredAt > report.IssuedAt &&
					application.statuses.Get(deviceID).Connection == device.ConnectionOnline &&
					snapshot.ConfirmsNormal() && !snapshot.MeasuredTime().After(now) && now.Sub(snapshot.MeasuredTime()) <= cfg.TelemetryStaleAfter {
					return
				}
				activities.Alarm(ctx, deviceID, report)
				// The MQTT alert and the newer telemetry can be handled concurrently.
				if snapshot, ok := application.readings.Latest(deviceID); ok && snapshot.Report.MeasuredAt > 0 &&
					application.statuses.Get(deviceID).Connection == device.ConnectionOnline {
					activities.ObserveTelemetry(deviceID, snapshot, false, time.Now(), cfg.TelemetryStaleAfter)
				}
			}
		}
	}
	application.relay = alert.NewRelay(alert.RelayConfig{
		Sender:                 sender,
		Deduplicator:           alert.NewDeduplicator(cfg.AlertDedupeWindow),
		Expiry:                 cfg.AlertExpiry,
		AlarmInterruptionLevel: cfg.AlarmInterruptionLevel,
		OnUnregisteredToken:    application.commands.RemovePushToken,
		OnAlarm:                onAlarm,
		Logger:                 logger,
	})

	application.server = &http.Server{
		Addr: cfg.HTTPAddress,
		Handler: httpapi.NewHandler(httpapi.Dependencies{
			Commands:            application.commands,
			Statuses:            application.statuses,
			Devices:             application.statuses,
			Readings:            application.readings,
			LiveActivities:      application.liveActivities,
			IsMQTTConnected:     application.mqtt.Connected,
			TelemetryStaleAfter: cfg.TelemetryStaleAfter,
			Logger:              logger,
		}),
		ReadHeaderTimeout: readHeaderTimeout,
		// Must be longer than the command answer timeout.
		WriteTimeout: cfg.CommandAnswerTimeout + 10*time.Second,
	}
	return application
}

// Run starts MQTT and HTTP and stops both when ctx ends.
func (a *App) Run(ctx context.Context) error {
	mqttStopped := make(chan struct{})
	go func() {
		defer close(mqttStopped)
		a.mqtt.Run(ctx)
	}()

	serverFailed := make(chan error, 1)
	go func() {
		a.logger.Info("http listening", "address", a.config.HTTPAddress)
		if err := a.server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serverFailed <- err
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-serverFailed:
	}
	shutdownContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	_ = a.server.Shutdown(shutdownContext)
	<-mqttStopped
	return runErr
}

func (a *App) handleMessage(message mqtt.Message) {
	deviceID, kind, ok := device.ParseTopic(message.Topic)
	if !ok {
		a.logger.Warn("message on an unknown topic ignored", "topic", message.Topic)
		return
	}
	switch kind {
	case device.KindAlert:
		ctx, cancel := context.WithTimeout(context.Background(), alertHandlingTimeout)
		defer cancel()
		if _, err := a.relay.Handle(ctx, deviceID, message.Payload); err != nil {
			a.logger.Warn("alert report rejected", "deviceId", deviceID, "error", err)
		}
	case device.KindAck:
		a.commands.HandleAck(deviceID, message.Payload)
	case device.KindStatus:
		a.statuses.Record(deviceID, message.Payload)
	case device.KindTelemetry:
		if err := a.readings.Record(deviceID, message.Payload); err != nil {
			a.logger.Warn("telemetry rejected", "deviceId", deviceID, "error", err)
			return
		}
		if a.liveActivities != nil && a.statuses.Get(deviceID).Connection == device.ConnectionOnline {
			if snapshot, ok := a.readings.Latest(deviceID); ok {
				a.liveActivities.ObserveTelemetry(deviceID, snapshot, message.Retained, time.Now(), a.config.TelemetryStaleAfter)
			}
		}
	}
}
