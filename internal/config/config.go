// Package config reads the backend settings from environment variables.
// .env.example lists every variable with a description.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/alert"
	"github.com/dericknorlan/beforefire-backend/internal/apns"
)

// Config holds all settings of the backend.
type Config struct {
	HTTPAddress string

	MQTTAddress   string
	MQTTUseTLS    bool
	MQTTClientID  string
	MQTTUsername  string
	MQTTPassword  string
	MQTTKeepAlive time.Duration

	APNsDryRun      bool
	APNsBaseURL     string
	APNsKeyFile     string
	APNsKeyID       string
	APNsTeamID      string
	APNsTopic       string
	APNsEnvironment string

	AlertExpiry            time.Duration
	AlertDedupeWindow      time.Duration
	AlarmInterruptionLevel string

	CommandAnswerTimeout time.Duration
	CommandClockSkew     time.Duration

	TelemetryStaleAfter time.Duration

	LogLevel string
}

// Default values. The APNs topic is the bundle ID of the BeforeFire app target.
const (
	defaultHTTPAddress            = ":8080"
	defaultMQTTAddress            = "mosquitto:1883"
	defaultMQTTClientID           = "beforefire-backend"
	defaultMQTTUsername           = "backend"
	defaultMQTTKeepAlive          = 30 * time.Second
	defaultAPNsTopic              = "com.dericknorlan.BeforeFire"
	defaultAPNsEnvironment        = "development"
	defaultAlertExpiry            = 15 * time.Minute
	defaultAlertDedupeWindow      = 30 * time.Minute
	defaultCommandAnswerTimeout   = 8 * time.Second
	defaultCommandClockSkew       = 5 * time.Minute
	defaultTelemetryStaleAfter    = 30 * time.Second
	defaultLogLevel               = "info"
	defaultAlarmInterruptionLevel = alert.InterruptionTimeSensitive
)

// Load reads the environment and returns all problems in one error.
func Load() (Config, error) {
	reader := &environmentReader{}
	config := Config{
		HTTPAddress: reader.text("HTTP_ADDRESS", defaultHTTPAddress),

		MQTTAddress:   reader.text("MQTT_ADDRESS", defaultMQTTAddress),
		MQTTUseTLS:    reader.boolean("MQTT_TLS", false),
		MQTTClientID:  reader.text("MQTT_CLIENT_ID", defaultMQTTClientID),
		MQTTUsername:  reader.text("MQTT_USERNAME", defaultMQTTUsername),
		MQTTPassword:  reader.text("MQTT_PASSWORD", ""),
		MQTTKeepAlive: reader.duration("MQTT_KEEPALIVE", defaultMQTTKeepAlive),

		APNsDryRun:      reader.boolean("APNS_DRY_RUN", false),
		APNsBaseURL:     reader.text("APNS_BASE_URL", ""),
		APNsKeyFile:     reader.text("APNS_KEY_FILE", ""),
		APNsKeyID:       reader.text("APNS_KEY_ID", ""),
		APNsTeamID:      reader.text("APNS_TEAM_ID", ""),
		APNsTopic:       reader.text("APNS_TOPIC", defaultAPNsTopic),
		APNsEnvironment: strings.ToLower(reader.text("APNS_ENVIRONMENT", defaultAPNsEnvironment)),

		AlertExpiry:            reader.duration("ALERT_EXPIRY", defaultAlertExpiry),
		AlertDedupeWindow:      reader.duration("ALERT_DEDUPE_WINDOW", defaultAlertDedupeWindow),
		AlarmInterruptionLevel: reader.text("ALARM_INTERRUPTION_LEVEL", defaultAlarmInterruptionLevel),

		CommandAnswerTimeout: reader.duration("COMMAND_ANSWER_TIMEOUT", defaultCommandAnswerTimeout),
		CommandClockSkew:     reader.duration("COMMAND_CLOCK_SKEW", defaultCommandClockSkew),

		TelemetryStaleAfter: reader.duration("TELEMETRY_STALE_AFTER", defaultTelemetryStaleAfter),

		LogLevel: strings.ToLower(reader.text("LOG_LEVEL", defaultLogLevel)),
	}
	config.validate(reader)
	return config, errors.Join(reader.problems...)
}

func (c *Config) validate(reader *environmentReader) {
	if c.MQTTPassword == "" {
		reader.fail("MQTT_PASSWORD is required")
	}
	if c.APNsEnvironment != "development" && c.APNsEnvironment != "production" {
		reader.fail("APNS_ENVIRONMENT must be development or production")
	}
	if !c.APNsDryRun {
		for name, value := range map[string]string{"APNS_KEY_FILE": c.APNsKeyFile, "APNS_KEY_ID": c.APNsKeyID, "APNS_TEAM_ID": c.APNsTeamID} {
			if value == "" {
				reader.fail(name + " is required when APNS_DRY_RUN is false")
			}
		}
	}
	if !alert.IsValidInterruptionLevel(c.AlarmInterruptionLevel) {
		reader.fail("ALARM_INTERRUPTION_LEVEL must be passive, active, time-sensitive, or critical")
	}
	if c.AlertDedupeWindow < c.AlertExpiry {
		reader.fail("ALERT_DEDUPE_WINDOW must not be shorter than ALERT_EXPIRY")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		reader.fail("LOG_LEVEL must be debug, info, warn, or error")
	}
}

// APNsURL returns the endpoint for APNS_ENVIRONMENT, or APNS_BASE_URL when it is set.
func (c Config) APNsURL() string {
	if c.APNsBaseURL != "" {
		return strings.TrimRight(c.APNsBaseURL, "/")
	}
	if c.APNsEnvironment == "production" {
		return apns.ProductionURL
	}
	return apns.DevelopmentURL
}

type environmentReader struct {
	problems []error
}

func (r *environmentReader) fail(problem string) {
	r.problems = append(r.problems, errors.New(problem))
}

func (r *environmentReader) text(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func (r *environmentReader) boolean(name string, fallback bool) bool {
	value := r.text(name, "")
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		r.fail(fmt.Sprintf("%s must be true or false", name))
		return fallback
	}
	return parsed
}

func (r *environmentReader) duration(name string, fallback time.Duration) time.Duration {
	value := r.text(name, "")
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		r.fail(fmt.Sprintf("%s must be a positive duration such as 30s or 15m", name))
		return fallback
	}
	return parsed
}
