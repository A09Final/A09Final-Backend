package config

import (
	"strings"
	"testing"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/apns"
)

func setEnvironment(t *testing.T, values map[string]string) {
	t.Helper()
	for _, name := range []string{"HTTP_ADDRESS", "MQTT_ADDRESS", "MQTT_TLS", "MQTT_CLIENT_ID", "MQTT_USERNAME",
		"MQTT_PASSWORD", "MQTT_KEEPALIVE", "APNS_DRY_RUN", "APNS_BASE_URL", "APNS_KEY_FILE", "APNS_KEY_ID",
		"APNS_TEAM_ID", "APNS_TOPIC", "APNS_ENVIRONMENT", "ALERT_EXPIRY", "ALERT_DEDUPE_WINDOW",
		"ALARM_INTERRUPTION_LEVEL", "COMMAND_ANSWER_TIMEOUT", "COMMAND_CLOCK_SKEW", "TELEMETRY_STALE_AFTER", "LOG_LEVEL"} {
		t.Setenv(name, values[name])
	}
}

func TestLoadDefaults(t *testing.T) {
	setEnvironment(t, map[string]string{
		"MQTT_PASSWORD": "secret", "APNS_KEY_FILE": "/run/secrets/key.p8", "APNS_KEY_ID": "K", "APNS_TEAM_ID": "T",
	})
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.MQTTAddress != "mosquitto:1883" || config.APNsTopic != "com.dericknorlan.BeforeFire" ||
		config.AlertExpiry != 15*time.Minute || config.APNsURL() != apns.DevelopmentURL ||
		config.TelemetryStaleAfter != 10*time.Second {
		t.Fatalf("defaults = %+v", config)
	}
}

func TestLoadProductionAndOverrides(t *testing.T) {
	setEnvironment(t, map[string]string{
		"MQTT_PASSWORD": "secret", "APNS_DRY_RUN": "true", "APNS_ENVIRONMENT": "Production",
		"ALERT_EXPIRY": "5m", "ALARM_INTERRUPTION_LEVEL": "critical",
	})
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.APNsURL() != apns.ProductionURL || config.AlertExpiry != 5*time.Minute || !config.APNsDryRun {
		t.Fatalf("config = %+v", config)
	}
	t.Setenv("APNS_BASE_URL", "https://localhost:9999/")
	config, _ = Load()
	if config.APNsURL() != "https://localhost:9999" {
		t.Fatalf("override URL = %q", config.APNsURL())
	}
}

func TestLoadReportsAllProblems(t *testing.T) {
	setEnvironment(t, map[string]string{
		"MQTT_TLS": "maybe", "ALERT_EXPIRY": "-1s", "APNS_ENVIRONMENT": "staging",
		"ALARM_INTERRUPTION_LEVEL": "loud", "LOG_LEVEL": "verbose",
	})
	_, err := Load()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"MQTT_PASSWORD", "MQTT_TLS", "ALERT_EXPIRY", "APNS_ENVIRONMENT",
		"APNS_KEY_FILE", "ALARM_INTERRUPTION_LEVEL", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}
