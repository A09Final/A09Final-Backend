package liveactivity

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/alert"
	"github.com/dericknorlan/beforefire-backend/internal/apns"
	"github.com/dericknorlan/beforefire-backend/internal/telemetry"
)

const (
	testSensor       = "DAPUR_A1B2C3"
	testInstallation = "01234567-89ab-cdef-0123-456789abcdef"
)

type testSender chan apns.Notification

func (s testSender) Send(_ context.Context, notification apns.Notification) error {
	s <- notification
	return nil
}

func expectPush(t *testing.T, sender testSender, event, token string) activityAPS {
	t.Helper()
	select {
	case sent := <-sender:
		if sent.DeviceToken != token || sent.PushType != apns.PushTypeLiveActivity || sent.Priority != apns.PriorityImmediate {
			t.Fatal("incorrect ActivityKit request")
		}
		var payload struct{ APS activityAPS }
		if err := json.Unmarshal(sent.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.APS.Event != event || payload.APS.ContentState.PushEventID != "alarm-1" {
			t.Fatalf("incorrect ActivityKit payload: %s", sent.Payload)
		}
		return payload.APS
	case <-time.After(3 * time.Second):
		t.Fatalf("no %s push was sent", event)
		return activityAPS{}
	}
}

func TestActivityWaitsForConfirmedNormalAndSurvivesRestart(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	file := filepath.Join(t.TempDir(), "activity.json")
	sender := make(testSender, 8)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := Open(file, sender, logger)
	if err != nil {
		t.Fatal(err)
	}
	startToken, updateToken := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if err := service.Register(testSensor, Registration{InstallationID: testInstallation, StartToken: startToken}); err != nil {
		t.Fatal(err)
	}

	report := alert.Report{AlertID: "alarm-1", IssuedAt: now.Add(-2 * time.Second).Unix(), Condition: alert.ConditionAlarm}
	service.Alarm(context.Background(), testSensor, report)
	start := expectPush(t, sender, "start", startToken)
	if start.AttributesType != "AlarmActivityAttributes" || start.Attributes == nil || start.Attributes.SensorDeviceID != testSensor ||
		start.Attributes.RemoteEventID != report.AlertID || start.InputPushToken != 1 {
		t.Fatalf("incorrect start attributes: %+v", start)
	}
	service, err = Open(file, sender, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Register(testSensor, Registration{InstallationID: testInstallation, EventID: report.AlertID, UpdateToken: updateToken}); err != nil {
		t.Fatal(err)
	}

	stove, iaq, presence := 60.0, 30.0, false
	normal := telemetry.Report{Condition: alert.ConditionNormal, MeasuredAt: now.Unix(),
		Readings: telemetry.Readings{StoveTemperatureC: &stove, IAQ: &iaq, Presence: &presence}}
	observations := []struct {
		name     string
		adjust   func(*telemetry.Report)
		retained bool
	}{
		{name: "Warning 1", adjust: func(report *telemetry.Report) { report.Condition = alert.ConditionWarning1 }},
		{name: "Warning 2", adjust: func(report *telemetry.Report) { report.Condition = alert.ConditionWarning2 }},
		{name: "sensor failure", adjust: func(report *telemetry.Report) { report.UnavailableSensors = []string{"thermal"} }},
		{name: "missing value", adjust: func(report *telemetry.Report) { report.Readings.Presence = nil }},
		{name: "invalid result", adjust: func(report *telemetry.Report) {
			report.Assessment = &telemetry.Assessment{Condition: alert.ConditionNormal, Valid: false}
		}},
		{name: "stale data", adjust: func(report *telemetry.Report) { report.MeasuredAt = now.Add(-15 * time.Second).Unix() }},
		{name: "before the alarm", adjust: func(report *telemetry.Report) { report.MeasuredAt = now.Add(-3 * time.Second).Unix() }},
		{name: "retained data with no time", retained: true, adjust: func(report *telemetry.Report) { report.MeasuredAt = 0 }},
	}
	for _, observation := range observations {
		t.Run(observation.name, func(t *testing.T) {
			report := normal
			observation.adjust(&report)
			service.ObserveTelemetry(testSensor, telemetry.Snapshot{Report: report, ReceivedAt: now}, observation.retained, now, 10*time.Second)
			service.mu.Lock()
			recovered := service.sensors[testSensor].Recovered
			service.mu.Unlock()
			if recovered {
				t.Fatal("an unconfirmed reading ended the alarm")
			}
		})
	}
	service.Alarm(context.Background(), testSensor, alert.Report{AlertID: "alarm-after-warning", IssuedAt: now.Unix(), Condition: alert.ConditionAlarm})
	if len(sender) != 0 {
		t.Fatal("an ongoing alarm started again")
	}
	service.ObserveTelemetry(testSensor, telemetry.Snapshot{Report: normal}, false, now, 10*time.Second)
	end := expectPush(t, sender, "end", updateToken)
	if end.Timestamp <= start.Timestamp || end.DismissalDate >= end.Timestamp {
		t.Fatal("the ended activity should be dismissed immediately")
	}
}

func TestLateUpdateTokenStillEndsTheRecoveredActivity(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	sender := make(testSender, 8)
	service, err := Open("", sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	startToken, updateToken := strings.Repeat("c", 64), strings.Repeat("d", 64)
	if err := service.Register(testSensor, Registration{InstallationID: testInstallation, StartToken: startToken}); err != nil {
		t.Fatal(err)
	}
	service.Alarm(context.Background(), testSensor, alert.Report{AlertID: "alarm-1", IssuedAt: now.Add(-2 * time.Second).Unix(), Condition: alert.ConditionAlarm})
	expectPush(t, sender, "start", startToken)
	stove, iaq, presence := 60.0, 30.0, true
	report := telemetry.Report{Condition: alert.ConditionNormal, MeasuredAt: now.Unix(),
		Readings: telemetry.Readings{StoveTemperatureC: &stove, IAQ: &iaq, Presence: &presence}}
	service.ObserveTelemetry(testSensor, telemetry.Snapshot{Report: report}, false, now, 10*time.Second)
	if err := service.Register(testSensor, Registration{InstallationID: testInstallation, EventID: "alarm-1", UpdateToken: updateToken}); err != nil {
		t.Fatal(err)
	}
	expectPush(t, sender, "end", updateToken)
}
