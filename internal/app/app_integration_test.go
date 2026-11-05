//go:build integration

// This test needs a running Mosquitto broker that uses deploy/mosquitto/config/acl.
// scripts/test-integration.sh starts one and runs:
//
//	go test -tags integration ./internal/app
package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/apns"
	"github.com/dericknorlan/beforefire-backend/internal/command"
	"github.com/dericknorlan/beforefire-backend/internal/config"
	"github.com/dericknorlan/beforefire-backend/internal/device"
	"github.com/dericknorlan/beforefire-backend/internal/mqtt"
	"github.com/dericknorlan/beforefire-backend/internal/telemetry"
)

const (
	sensorID      = "DAPUR_A1B2C3"
	sensorPoP     = "abcd2345"
	otherSensorID = "DAPUR_B2C3D4"
	tokenGood     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tokenGone     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	waitLimit     = 5 * time.Second
)

type recordingSender struct {
	mu   sync.Mutex
	sent []apns.Notification
}

func (s *recordingSender) Send(_ context.Context, notification apns.Notification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, notification)
	if notification.DeviceToken == tokenGone {
		return &apns.Error{StatusCode: http.StatusGone, Reason: "Unregistered"}
	}
	return nil
}

func (s *recordingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

// fakeSensor acts like the ESP32-S3 firmware: it checks signatures with its PoP and answers on ack.
type fakeSensor struct {
	client   *mqtt.Client
	commands chan command.Message
}

func startFakeSensor(t *testing.T, ctx context.Context, address string) *fakeSensor {
	sensor := &fakeSensor{commands: make(chan command.Message, 8)}
	sensor.client = mqtt.NewClient(mqtt.Options{
		Address:       address,
		ClientID:      sensorID,
		Username:      sensorID,
		Password:      sensorPoP,
		KeepAlive:     10 * time.Second,
		Will:          &mqtt.Will{Topic: device.Topic(sensorID, device.KindStatus), Payload: []byte("offline"), QoS: 1, Retain: true},
		Subscriptions: []mqtt.Subscription{{Filter: device.Topic(sensorID, device.KindCommand), QoS: 1}},
		OnMessage:     func(message mqtt.Message) { sensor.handleCommand(ctx, message) },
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	go sensor.client.Run(ctx)
	waitFor(t, "fake sensor connection", sensor.client.Connected)
	return sensor
}

func (s *fakeSensor) handleCommand(ctx context.Context, message mqtt.Message) {
	var received command.Message
	if err := json.Unmarshal(message.Payload, &received); err != nil {
		return
	}
	s.commands <- received
	if received.Origin != command.OriginApp {
		return
	}
	request := command.Request{Action: received.Action, RequestID: received.RequestID, IssuedAt: received.IssuedAt, PushToken: received.PushToken}
	ack := command.Ack{RequestID: received.RequestID, Result: command.ResultDone}
	if command.Sign(sensorPoP, sensorID, request) != received.Signature {
		ack = command.Ack{RequestID: received.RequestID, Result: command.ResultRejected, Reason: "bad_signature"}
	}
	encoded, _ := json.Marshal(ack)
	_ = s.client.Publish(ctx, device.Topic(sensorID, device.KindAck), encoded, 1, false)
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func postCommand(t *testing.T, handler http.Handler, request command.Request) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(request)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/devices/"+sensorID+"/commands", strings.NewReader(string(body))))
	return recorder
}

func TestBackendWithMosquitto(t *testing.T) {
	address := os.Getenv("BEFOREFIRE_TEST_MQTT_ADDRESS")
	backendPassword := os.Getenv("BEFOREFIRE_TEST_BACKEND_PASSWORD")
	if address == "" || backendPassword == "" {
		t.Skip("set BEFOREFIRE_TEST_MQTT_ADDRESS and BEFOREFIRE_TEST_BACKEND_PASSWORD (see scripts/test-integration.sh)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sender := &recordingSender{}
	cfg := config.Config{
		TelemetryStaleAfter: 30 * time.Second,
		HTTPAddress:         "127.0.0.1:0", MQTTAddress: address, MQTTClientID: "beforefire-backend-test",
		MQTTUsername: "backend", MQTTPassword: backendPassword, MQTTKeepAlive: 10 * time.Second,
		AlertExpiry: 15 * time.Minute, AlertDedupeWindow: 30 * time.Minute,
		AlarmInterruptionLevel: "time-sensitive", CommandAnswerTimeout: 3 * time.Second, CommandClockSkew: 5 * time.Minute,
	}
	backend := New(cfg, sender, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	go func() { _ = backend.Run(ctx) }()
	waitFor(t, "backend connection", backend.mqtt.Connected)
	handler := backend.server.Handler

	sensor := startFakeSensor(t, ctx, address)
	if err := sensor.client.Publish(ctx, device.Topic(sensorID, device.KindStatus), []byte("online"), 1, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "online status", func() bool { return backend.statuses.Get(sensorID).Connection == device.ConnectionOnline })

	t.Run("alert reaches APNs sender once", func(t *testing.T) {
		alertPayload := []byte(`{"alertId":"it-alarm-1","condition":"alarm","score":90,"readings":{"stoveTemperatureC":245},"pushTokens":["` + tokenGood + `"]}`)
		for range 2 { // second publish is a duplicate
			if err := sensor.client.Publish(ctx, device.Topic(sensorID, device.KindAlert), alertPayload, 1, false); err != nil {
				t.Fatal(err)
			}
		}
		waitFor(t, "notification", func() bool { return sender.count() == 1 })
		time.Sleep(300 * time.Millisecond)
		if sender.count() != 1 {
			t.Fatalf("sent %d notifications, want 1", sender.count())
		}
		if !strings.Contains(string(sender.sent[0].Payload), `"deviceId":"DAPUR_A1B2C3"`) {
			t.Fatalf("payload = %s", sender.sent[0].Payload)
		}
	})

	t.Run("ACL blocks alerts for another device", func(t *testing.T) {
		before := sender.count()
		spoofed := []byte(`{"alertId":"it-spoof","condition":"alarm","pushTokens":["` + tokenGood + `"]}`)
		_ = sensor.client.Publish(ctx, device.Topic(otherSensorID, device.KindAlert), spoofed, 1, false)
		time.Sleep(500 * time.Millisecond)
		if sender.count() != before {
			t.Fatal("broker delivered an alert that the device is not allowed to publish")
		}
	})

	t.Run("signed command is done", func(t *testing.T) {
		request := command.Request{Action: command.ActionSilence, RequestID: "it-request-0001", IssuedAt: time.Now().Unix()}
		request.Signature = command.Sign(sensorPoP, sensorID, request)
		recorder := postCommand(t, handler, request)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"result":"done"`) {
			t.Fatalf("status %d body %s", recorder.Code, recorder.Body)
		}
	})

	t.Run("wrong signature is rejected by the device", func(t *testing.T) {
		request := command.Request{Action: command.ActionSilence, RequestID: "it-request-0002", IssuedAt: time.Now().Unix()}
		request.Signature = command.Sign("wrongpop", sensorID, request)
		recorder := postCommand(t, handler, request)
		if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "bad_signature") {
			t.Fatalf("status %d body %s", recorder.Code, recorder.Body)
		}
	})

	t.Run("unregistered token is sent back to the device", func(t *testing.T) {
		for len(sensor.commands) > 0 {
			<-sensor.commands
		}
		alertPayload := []byte(`{"alertId":"it-alarm-2","condition":"warning2","pushTokens":["` + tokenGone + `"]}`)
		if err := sensor.client.Publish(ctx, device.Topic(sensorID, device.KindAlert), alertPayload, 1, false); err != nil {
			t.Fatal(err)
		}
		select {
		case received := <-sensor.commands:
			if received.Action != command.ActionUnregisterPushToken || received.Origin != command.OriginBackend || received.PushToken != tokenGone {
				t.Fatalf("command = %+v", received)
			}
		case <-time.After(waitLimit):
			t.Fatal("device did not receive unregister_push_token")
		}
	})

	t.Run("telemetry reaches the app with the read key only", func(t *testing.T) {
		readKey := telemetry.ReadKey(sensorPoP, sensorID)
		payload := `{"condition":"normal","score":12.5,"readKeyHash":"` + telemetry.ReadKeyHash(readKey) + `",` +
			`"readings":{"stoveTemperatureC":80,"pressureHPa":1013.2,"presence":true}}`
		if err := sensor.client.Publish(ctx, device.Topic(sensorID, device.KindTelemetry), []byte(payload), 0, true); err != nil {
			t.Fatal(err)
		}
		get := func(key string) *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodGet, "/v1/devices/"+sensorID+"/readings", nil)
			request.Header.Set("Authorization", "Bearer "+key)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			return recorder
		}
		waitFor(t, "readings", func() bool { return get(readKey).Code == http.StatusOK })
		body := get(readKey).Body.String()
		if !strings.Contains(body, `"pressureHPa":1013.2`) || !strings.Contains(body, `"condition":"normal"`) {
			t.Fatalf("body = %s", body)
		}
		if code := get(telemetry.ReadKey("wrongpop", sensorID)).Code; code != http.StatusUnauthorized {
			t.Fatalf("wrong key: status %d", code)
		}
		// ACL: the sensor cannot publish readings for another device.
		_ = sensor.client.Publish(ctx, device.Topic(otherSensorID, device.KindTelemetry), []byte(payload), 1, true)
		time.Sleep(300 * time.Millisecond)
		if _, err := backend.readings.Read(otherSensorID, readKey); err == nil {
			t.Fatal("broker delivered telemetry that the device is not allowed to publish")
		}
		// Clear the retained message so the next run starts clean.
		_ = sensor.client.Publish(ctx, device.Topic(sensorID, device.KindTelemetry), nil, 1, true)
	})

	t.Run("offline device fails fast", func(t *testing.T) {
		if err := sensor.client.Publish(ctx, device.Topic(sensorID, device.KindStatus), []byte("offline"), 1, true); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "offline status", func() bool { return backend.statuses.Get(sensorID).Connection == device.ConnectionOffline })
		request := command.Request{Action: command.ActionSilence, RequestID: "it-request-0003", IssuedAt: time.Now().Unix()}
		request.Signature = command.Sign(sensorPoP, sensorID, request)
		if recorder := postCommand(t, handler, request); recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d body %s", recorder.Code, recorder.Body)
		}
		// Clear the retained status so the next run starts clean.
		_ = sensor.client.Publish(ctx, device.Topic(sensorID, device.KindStatus), []byte("online"), 1, true)
	})
}
