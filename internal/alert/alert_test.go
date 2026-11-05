package alert

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/apns"
)

const (
	testDeviceID = "DAPUR_A1B2C3"
	tokenA       = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tokenB       = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var testNow = time.Unix(1_730_700_000, 0)

func TestParseReportNormalizesTokensAndTime(t *testing.T) {
	payload := `{"alertId":"a-1","condition":"alarm","pushTokens":["` + strings.ToUpper(tokenA) + `","` + tokenA + `","` + tokenB + `"]}`
	report, err := ParseReport([]byte(payload), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.PushTokens) != 2 || report.PushTokens[0] != tokenA || report.PushTokens[1] != tokenB {
		t.Fatalf("tokens = %v", report.PushTokens)
	}
	if report.IssuedAt != testNow.Unix() {
		t.Fatalf("issuedAt = %d, want the receive time", report.IssuedAt)
	}
}

func TestParseReportRejectsBadInput(t *testing.T) {
	tooManyTokens := make([]string, MaxPushTokens+1)
	for index := range tooManyTokens {
		tooManyTokens[index] = strings.Repeat(string(rune('a'+index%6)), 64)
	}
	tooMany, _ := json.Marshal(map[string]any{"alertId": "a", "condition": "alarm", "pushTokens": tooManyTokens})

	cases := map[string]string{
		"invalid JSON":      `{`,
		"empty alert ID":    `{"alertId":"","condition":"alarm","pushTokens":["` + tokenA + `"]}`,
		"long alert ID":     `{"alertId":"` + strings.Repeat("a", 65) + `","condition":"alarm","pushTokens":["` + tokenA + `"]}`,
		"unknown condition": `{"alertId":"a","condition":"fire","pushTokens":["` + tokenA + `"]}`,
		"no tokens":         `{"alertId":"a","condition":"alarm","pushTokens":[]}`,
		"short token":       `{"alertId":"a","condition":"alarm","pushTokens":["abcd"]}`,
		"non-hex token":     `{"alertId":"a","condition":"alarm","pushTokens":["` + strings.Repeat("z", 64) + `"]}`,
		"future time":       `{"alertId":"a","condition":"alarm","issuedAt":1830700000,"pushTokens":["` + tokenA + `"]}`,
		"too many tokens":   string(tooMany),
		"too large":         `{"alertId":"` + strings.Repeat("a", MaxReportBytes) + `"}`,
	}
	for name, payload := range cases {
		var validationErr *ValidationError
		if _, err := ParseReport([]byte(payload), testNow); !errors.As(err, &validationErr) {
			t.Errorf("%s: err = %v, want ValidationError", name, err)
		}
	}
}

func TestBuildPayloadForAlarm(t *testing.T) {
	stove := 231.6
	score := 87.5
	report := Report{AlertID: "a-1", Condition: ConditionAlarm, Score: &score, IssuedAt: testNow.Unix(),
		Readings: Readings{StoveTemperatureC: &stove}}
	encoded, priority, err := buildPayload(testDeviceID, report, InterruptionTimeSensitive)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	aps, app := decoded["aps"], decoded["beforefire"]
	alertBody := aps["alert"].(map[string]any)
	if alertBody["title"] != titleAlarm || alertBody["body"] != bodyAlarm+" Stove: 232 °C." {
		t.Errorf("alert = %v", alertBody)
	}
	if aps["sound"] != "default" || aps["interruption-level"] != "time-sensitive" || aps["thread-id"] != testDeviceID {
		t.Errorf("aps = %v", aps)
	}
	if app["deviceId"] != testDeviceID || app["alertId"] != "a-1" || app["condition"] != "alarm" || app["score"] != 87.5 {
		t.Errorf("beforefire = %v", app)
	}
	if priority != apns.PriorityImmediate {
		t.Errorf("priority = %d", priority)
	}
}

func TestBuildPayloadCriticalUsesCriticalSound(t *testing.T) {
	encoded, _, err := buildPayload(testDeviceID, Report{AlertID: "a", Condition: ConditionAlarm}, InterruptionCritical)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"sound":{"critical":1,"name":"default","volume":1}`) {
		t.Fatalf("payload = %s", encoded)
	}
}

func TestBuildPayloadForNormalIsQuiet(t *testing.T) {
	stove := 40.0
	encoded, priority, err := buildPayload(testDeviceID, Report{AlertID: "a", Condition: ConditionNormal,
		Readings: Readings{StoveTemperatureC: &stove}}, InterruptionTimeSensitive)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, `"sound"`) || !strings.Contains(text, `"interruption-level":"passive"`) || strings.Contains(text, "Stove:") {
		t.Fatalf("payload = %s", text)
	}
	if priority != apns.PriorityPowerSaving {
		t.Fatalf("priority = %d", priority)
	}
}

func TestDeduplicator(t *testing.T) {
	deduplicator := NewDeduplicator(time.Minute)
	now := testNow
	deduplicator.now = func() time.Time { return now }
	if !deduplicator.FirstTime("k") || deduplicator.FirstTime("k") {
		t.Fatal("second call with the same key must return false")
	}
	now = now.Add(time.Minute)
	if !deduplicator.FirstTime("k") {
		t.Fatal("key must be accepted again after the TTL")
	}
}

type fakeSender struct {
	mu            sync.Mutex
	notifications []apns.Notification
	failures      map[string]error
}

func (s *fakeSender) Send(_ context.Context, notification apns.Notification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifications = append(s.notifications, notification)
	return s.failures[notification.DeviceToken]
}

func newTestRelay(sender Sender, unregistered chan string) *Relay {
	return NewRelay(RelayConfig{
		Sender:       sender,
		Deduplicator: NewDeduplicator(time.Hour),
		Expiry:       15 * time.Minute,
		OnUnregisteredToken: func(_ context.Context, deviceID, token string) {
			unregistered <- deviceID + " " + token
		},
		Now: func() time.Time { return testNow },
	})
}

func TestRelaySendsOneNotificationPerToken(t *testing.T) {
	sender := &fakeSender{failures: map[string]error{
		tokenB: &apns.Error{StatusCode: 410, Reason: "Unregistered"},
	}}
	unregistered := make(chan string, 1)
	relay := newTestRelay(sender, unregistered)
	payload := `{"alertId":"a-1","condition":"alarm","issuedAt":` + jsonNumber(testNow.Unix()-60) +
		`,"pushTokens":["` + tokenA + `","` + tokenB + `"]}`

	result, err := relay.Handle(context.Background(), testDeviceID, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if result.Sent != 1 || result.Unregistered != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v", result)
	}
	if got := <-unregistered; got != testDeviceID+" "+tokenB {
		t.Fatalf("unregistered = %q", got)
	}
	notification := sender.notifications[0]
	if notification.CollapseID != "a-1" || !notification.Expiration.Equal(testNow.Add(14*time.Minute)) {
		t.Fatalf("notification = %+v", notification)
	}

	again, err := relay.Handle(context.Background(), testDeviceID, []byte(payload))
	if err != nil || !again.Skipped || len(sender.notifications) != 2 {
		t.Fatalf("duplicate: result %+v, err %v, notifications %d", again, err, len(sender.notifications))
	}
}

func TestRelaySkipsExpiredReport(t *testing.T) {
	sender := &fakeSender{}
	relay := newTestRelay(sender, make(chan string, 1))
	payload := `{"alertId":"old","condition":"alarm","issuedAt":` + jsonNumber(testNow.Add(-16*time.Minute).Unix()) +
		`,"pushTokens":["` + tokenA + `"]}`
	result, err := relay.Handle(context.Background(), testDeviceID, []byte(payload))
	if err != nil || !result.Skipped || len(sender.notifications) != 0 {
		t.Fatalf("result %+v, err %v, notifications %d", result, err, len(sender.notifications))
	}
}

func TestRelayCountsOtherFailures(t *testing.T) {
	sender := &fakeSender{failures: map[string]error{tokenA: errors.New("network down")}}
	relay := newTestRelay(sender, make(chan string, 1))
	payload := `{"alertId":"a-2","condition":"warning1","pushTokens":["` + tokenA + `"]}`
	result, err := relay.Handle(context.Background(), testDeviceID, []byte(payload))
	if err != nil || result.Failed != 1 || result.Sent != 0 {
		t.Fatalf("result %+v, err %v", result, err)
	}
}

func jsonNumber(value int64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
