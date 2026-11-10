package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/command"
	"github.com/dericknorlan/beforefire-backend/internal/device"
	"github.com/dericknorlan/beforefire-backend/internal/telemetry"
)

type stubCommands struct {
	ack      command.Ack
	err      error
	deviceID string
	request  command.Request
}

func (s *stubCommands) Execute(_ context.Context, deviceID string, request command.Request) (command.Ack, error) {
	s.deviceID, s.request = deviceID, request
	return s.ack, s.err
}

type stubStatuses map[string]device.Status

func (s stubStatuses) Get(deviceID string) device.Status { return s[deviceID] }

type stubDevices []device.Entry

func (s stubDevices) List() []device.Entry { return s }

// stubReadings accepts only the key "good-key".
type stubReadings struct {
	snapshot telemetry.Snapshot
	hasData  bool
	gotKey   string
}

func (s *stubReadings) Read(_ string, readKey string) (telemetry.Snapshot, error) {
	s.gotKey = readKey
	if !s.hasData {
		return telemetry.Snapshot{}, telemetry.ErrNoReadings
	}
	if readKey != "good-key" {
		return telemetry.Snapshot{}, telemetry.ErrInvalidReadKey
	}
	return s.snapshot, nil
}

var testNow = time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

func newTestHandlerWithReadings(commands *stubCommands, readings ReadingsSource, isConnected bool) http.Handler {
	return NewHandler(Dependencies{
		Commands:            commands,
		Statuses:            stubStatuses{"DAPUR_A1B2C3": {Connection: device.ConnectionOnline, UpdatedAt: testNow}},
		Readings:            readings,
		IsMQTTConnected:     func() bool { return isConnected },
		TelemetryStaleAfter: 30 * time.Second,
		Now:                 func() time.Time { return testNow },
	})
}

func newTestHandler(commands *stubCommands, isConnected bool) http.Handler {
	return newTestHandlerWithReadings(commands, &stubReadings{}, isConnected)
}

func getWithKey(handler http.Handler, path, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestDeviceReadings(t *testing.T) {
	stove, pressure, presence := 80.0, 1013.2, true
	readings := &stubReadings{hasData: true, snapshot: telemetry.Snapshot{
		Report: telemetry.Report{
			Condition: "normal",
			Readings:  telemetry.Readings{StoveTemperatureC: &stove, PressureHPa: &pressure, Presence: &presence},
		},
		ReceivedAt: testNow.Add(-10 * time.Second),
	}}
	handler := newTestHandlerWithReadings(&stubCommands{}, readings, true)

	recorder := getWithKey(handler, "/v1/devices/DAPUR_A1B2C3/readings", "Bearer good-key")
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d body %s", recorder.Code, body)
	}
	for _, want := range []string{`"condition":"normal"`, `"stoveTemperatureC":80`, `"pressureHPa":1013.2`,
		`"presence":true`, `"unavailableSensors":[]`, `"ageSeconds":10`, `"isStale":false`, `"connection":"online"`,
		`"measuredAt":"2026-11-04T09:59:50Z"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not contain %s:\n%s", want, body)
		}
	}

	readings.snapshot.ReceivedAt = testNow.Add(-31 * time.Second)
	if body := getWithKey(handler, "/v1/devices/DAPUR_A1B2C3/readings", "bearer good-key").Body.String(); !strings.Contains(body, `"isStale":true`) {
		t.Errorf("old readings are not stale: %s", body)
	}
	if readings.gotKey != "good-key" {
		t.Errorf("lower-case bearer prefix: key = %q", readings.gotKey)
	}
}

func TestDeviceReadingsForwardsAssessment(t *testing.T) {
	const deviceID = "DAPUR_A1B2C3"
	const readKey = "good-key"
	const firmwareAssessment = `{"condition":"warning2","valid":true,"rule_id":10,"absence_duration_seconds":0}`
	const invalidAssessment = `{"condition":"warning2","valid":false}`
	base := `{"readKeyHash":"` + telemetry.ReadKeyHash(readKey) + `","condition":"warning2","readings":{"stoveTemperatureC":225}`
	store := telemetry.NewStore()
	handler := newTestHandlerWithReadings(&stubCommands{}, store, true)

	for _, test := range []struct {
		name     string
		metadata string
		want     string
	}{
		{"firmware rule", `,"assessment":` + firmwareAssessment, firmwareAssessment},
		{"older firmware", "", ""},
		{"invalid fuzzy result", `,"assessment":` + invalidAssessment, invalidAssessment},
		{"malformed metadata", `,"assessment":{"rule_id":"unknown"}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := store.Record(deviceID, []byte(base+test.metadata+`}`)); err != nil {
				t.Fatal(err)
			}
			recorder := getWithKey(handler, "/v1/devices/"+deviceID+"/readings", "Bearer "+readKey)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d body %s", recorder.Code, recorder.Body)
			}
			var response struct {
				Condition  string          `json:"condition"`
				Assessment json.RawMessage `json:"assessment"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Condition != "warning2" || string(response.Assessment) != test.want {
				t.Errorf("condition = %q, assessment = %s, want warning2 and %s", response.Condition, response.Assessment, test.want)
			}
		})
	}
}

func TestDeviceReadingsErrors(t *testing.T) {
	handler := newTestHandlerWithReadings(&stubCommands{}, &stubReadings{hasData: true}, true)
	for name, authorization := range map[string]string{"missing": "", "wrong": "Bearer bad-key", "not bearer": "Basic good-key"} {
		recorder := getWithKey(handler, "/v1/devices/DAPUR_A1B2C3/readings", authorization)
		if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "invalid_read_key") ||
			recorder.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s key: status %d body %s", name, recorder.Code, recorder.Body)
		}
	}
	empty := newTestHandlerWithReadings(&stubCommands{}, &stubReadings{}, true)
	if recorder := getWithKey(empty, "/v1/devices/DAPUR_A1B2C3/readings", "Bearer good-key"); recorder.Code != http.StatusNotFound ||
		!strings.Contains(recorder.Body.String(), "no_readings") {
		t.Errorf("no readings: status %d body %s", recorder.Code, recorder.Body)
	}
	if got := getWithKey(empty, "/v1/devices/bad/readings", "Bearer good-key").Code; got != http.StatusNotFound {
		t.Errorf("invalid ID: status %d", got)
	}
}

func call(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestHealth(t *testing.T) {
	if got := call(newTestHandler(&stubCommands{}, true), "GET", "/healthz", "").Code; got != http.StatusOK {
		t.Errorf("connected: status %d", got)
	}
	if got := call(newTestHandler(&stubCommands{}, false), "GET", "/healthz", "").Code; got != http.StatusServiceUnavailable {
		t.Errorf("disconnected: status %d", got)
	}
}

func TestDeviceStatus(t *testing.T) {
	handler := newTestHandler(&stubCommands{}, true)
	recorder := call(handler, "GET", "/v1/devices/DAPUR_A1B2C3/status", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"connection":"online"`) ||
		!strings.Contains(recorder.Body.String(), `"updatedAt":"2026-11-04T10:00:00Z"`) {
		t.Fatalf("status %d body %s", recorder.Code, recorder.Body)
	}
	unknown := call(handler, "GET", "/v1/devices/DAPUR_FFFFFF/status", "")
	if !strings.Contains(unknown.Body.String(), `"connection":"unknown"`) || strings.Contains(unknown.Body.String(), "updatedAt") {
		t.Fatalf("unknown device body %s", unknown.Body)
	}
	if got := call(handler, "GET", "/v1/devices/not-a-device/status", "").Code; got != http.StatusNotFound {
		t.Fatalf("invalid ID: status %d", got)
	}
}

func TestSendCommandPassesRequest(t *testing.T) {
	commands := &stubCommands{ack: command.Ack{RequestID: "request-0001", Result: command.ResultDone}}
	body := `{"action":"silence","requestId":"request-0001","issuedAt":1730700000,"signature":"` + strings.Repeat("a", 64) + `"}`
	recorder := call(newTestHandler(commands, true), "POST", "/v1/devices/DAPUR_A1B2C3/commands", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d body %s", recorder.Code, recorder.Body)
	}
	if commands.deviceID != "DAPUR_A1B2C3" || commands.request.Action != command.ActionSilence || commands.request.IssuedAt != 1730700000 {
		t.Fatalf("executor got %s %+v", commands.deviceID, commands.request)
	}
	var ack command.Ack
	if err := json.Unmarshal(recorder.Body.Bytes(), &ack); err != nil || ack.Result != "done" {
		t.Fatalf("body %s", recorder.Body)
	}
}

func TestSendCommandMapsErrors(t *testing.T) {
	body := `{"action":"silence","requestId":"request-0001","issuedAt":1,"signature":"x"}`
	cases := []struct {
		name       string
		commands   *stubCommands
		wantStatus int
		wantError  string
	}{
		{"rejected", &stubCommands{ack: command.Ack{Result: command.ResultRejected, Reason: "bad_signature"}}, 422, "command_rejected"},
		{"validation", &stubCommands{err: &command.ValidationError{Field: "issuedAt", Problem: "old"}}, 400, "invalid_request"},
		{"duplicate", &stubCommands{err: command.ErrDuplicateRequest}, 409, "duplicate_request"},
		{"offline", &stubCommands{err: command.ErrDeviceOffline}, 503, "device_offline"},
		{"broker", &stubCommands{err: errors.Join(command.ErrBrokerUnavailable, errors.New("x"))}, 503, "broker_unavailable"},
		{"timeout", &stubCommands{err: command.ErrNoAnswer}, 504, "device_no_answer"},
		{"other", &stubCommands{err: errors.New("boom")}, 500, "internal_error"},
	}
	for _, testCase := range cases {
		recorder := call(newTestHandler(testCase.commands, true), "POST", "/v1/devices/DAPUR_A1B2C3/commands", body)
		if recorder.Code != testCase.wantStatus || !strings.Contains(recorder.Body.String(), `"error":"`+testCase.wantError+`"`) {
			t.Errorf("%s: status %d body %s", testCase.name, recorder.Code, recorder.Body)
		}
	}
}

func TestSendCommandRejectsBadBodies(t *testing.T) {
	handler := newTestHandler(&stubCommands{}, true)
	for name, body := range map[string]string{
		"not JSON":      `{`,
		"unknown field": `{"action":"silence","extra":true}`,
		"too large":     `{"action":"` + strings.Repeat("a", maxRequestBodyBytes) + `"}`,
	} {
		if got := call(handler, "POST", "/v1/devices/DAPUR_A1B2C3/commands", body).Code; got != http.StatusBadRequest {
			t.Errorf("%s: status %d", name, got)
		}
	}
	if got := call(handler, "GET", "/v1/devices/DAPUR_A1B2C3/commands", "").Code; got != http.StatusMethodNotAllowed {
		t.Errorf("GET on commands: status %d", got)
	}
}

func TestDeviceList(t *testing.T) {
	handler := NewHandler(Dependencies{
		Commands: &stubCommands{},
		Statuses: stubStatuses{},
		Devices: stubDevices{
			{DeviceID: "DAPUR_020F3C", Status: device.Status{Connection: device.ConnectionOnline, UpdatedAt: testNow}},
			{DeviceID: "DAPUR_FFFFFF", Status: device.Status{Connection: device.ConnectionOffline}},
		},
		Readings:        &stubReadings{},
		IsMQTTConnected: func() bool { return true },
		Now:             func() time.Time { return testNow },
	})

	recorder := getWithKey(handler, "/v1/devices", "")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	want := `{"devices":[{"deviceId":"DAPUR_020F3C","connection":"online","updatedAt":"2026-11-04T10:00:00Z"},` +
		`{"deviceId":"DAPUR_FFFFFF","connection":"offline"}]}`
	if got := strings.TrimSpace(recorder.Body.String()); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
}

func TestDeviceListIsEmptyArrayWithNoDevices(t *testing.T) {
	recorder := getWithKey(newTestHandler(&stubCommands{}, true), "/v1/devices", "")

	if got := strings.TrimSpace(recorder.Body.String()); got != `{"devices":[]}` {
		t.Fatalf("body = %s, want an empty list", got)
	}
}
