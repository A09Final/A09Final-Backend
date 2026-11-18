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

func reminderFixture(t *testing.T) (*Service, testSender, time.Time, string) {
	t.Helper()
	sender := make(testSender, 32)
	service, err := Open(filepath.Join(t.TempDir(), "state.json"), sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 64)
	if err := service.Register(testSensor, Registration{InstallationID: testInstallation, PushToken: token}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	service.Alarm(context.Background(), testSensor, alert.Report{AlertID: "alarm-1", Condition: alert.ConditionAlarm, IssuedAt: now.Add(-2 * time.Second).Unix(), PushTokens: []string{token}})
	return service, sender, now, token
}

func checkReminder(t *testing.T, sender testSender, token string) {
	t.Helper()
	select {
	case sent := <-sender:
		if sent.DeviceToken != token || sent.PushType != apns.PushTypeAlert || sent.Priority != apns.PriorityImmediate || !sent.Expiration.IsZero() || sent.CollapseID != "alarm-1" {
			t.Fatal("incorrect reminder request")
		}
		var payload struct {
			APS struct {
				Level string `json:"interruption-level"`
				Sound struct {
					Critical int
					Name     string
					Volume   float64
				}
			}
			BeforeFire struct {
				AlertID   string
				Condition string
			}
		}
		if err := json.Unmarshal(sent.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.APS.Level != "critical" || payload.APS.Sound.Critical != 1 || payload.APS.Sound.Volume != 1 || payload.APS.Sound.Name != "BeforeFireCriticalSiren.caf" || payload.BeforeFire.AlertID != "alarm-1" || payload.BeforeFire.Condition != "alarm" {
			t.Fatalf("incorrect payload: %s", sent.Payload)
		}
	default:
		t.Fatal("no alarm reminder")
	}
}

func TestRemindersEveryThirtySecondsAndAfterRestart(t *testing.T) {
	service, sender, now, token := reminderFixture(t)
	service.sendDueReminders(context.Background(), now.Add(29*time.Second))
	if len(sender) != 0 {
		t.Fatal("early reminder")
	}
	service.sendDueReminders(context.Background(), now.Add(30*time.Second))
	checkReminder(t, sender, token)
	service.sendDueReminders(context.Background(), now.Add(59*time.Second))
	if len(sender) != 0 {
		t.Fatal("early second reminder")
	}
	restarted, err := Open(service.file, sender, service.logger)
	if err != nil {
		t.Fatal(err)
	}
	restarted.sendDueReminders(context.Background(), now.Add(60*time.Second))
	checkReminder(t, sender, token)
	// Missed intervals do not create a burst after a restart.
	restarted.sendDueReminders(context.Background(), now.Add(240*time.Second))
	checkReminder(t, sender, token)
	if len(sender) != 0 {
		t.Fatal("catch-up burst")
	}
}

func TestSnoozeIsPerPhoneAndSurvivesRestart(t *testing.T) {
	service, sender, now, token := reminderFixture(t)
	secondID := "11234567-89ab-cdef-0123-456789abcdef"
	secondToken := strings.Repeat("b", 64)
	if err := service.Register(testSensor, Registration{InstallationID: secondID, PushToken: secondToken}); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	state := service.sensors[testSensor]
	copied := *state.ReminderReport
	copied.PushTokens = append([]string{token}, secondToken)
	state.ReminderReport = &copied
	if err := service.save(testSensor, state); err != nil {
		t.Fatal(err)
	}
	service.mu.Unlock()
	deadline := now.Add(time.Minute).Unix()
	changed := time.Now().UnixMilli()
	if err := service.Register(testSensor, Registration{InstallationID: testInstallation, EventID: "alarm-1", SnoozedUntil: &deadline, SnoozeChangedAt: changed}); err != nil {
		t.Fatal(err)
	}
	if !service.SuppressesAlarmToken(testSensor, token) || service.SuppressesAlarmToken(testSensor, secondToken) {
		t.Fatal("snooze leaked to another phone")
	}
	// A delayed resume must not overwrite the newer snooze.
	zero := int64(0)
	if err := service.Register(testSensor, Registration{InstallationID: testInstallation, EventID: "alarm-1", SnoozedUntil: &zero, SnoozeChangedAt: changed - 1}); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(service.file, sender, service.logger)
	if err != nil {
		t.Fatal(err)
	}
	restarted.sendDueReminders(context.Background(), now.Add(30*time.Second))
	checkReminder(t, sender, secondToken)
	if len(sender) != 0 {
		t.Fatal("snoozed phone received a reminder")
	}
	restarted.sendDueReminders(context.Background(), now.Add(60*time.Second))
	// Send order is concurrent. Both phones receive the next reminder.
	received := map[string]bool{}
	for range 2 {
		sent := <-sender
		received[sent.DeviceToken] = true
	}
	if !received[token] || !received[secondToken] {
		t.Fatal("snooze expiry did not resume reminders")
	}
}

func TestResumeAndUnlinkCancelPendingReminders(t *testing.T) {
	service, sender, now, token := reminderFixture(t)
	deadline := now.Add(time.Minute).Unix()
	changed := time.Now().UnixMilli()
	request := Registration{InstallationID: testInstallation, SnoozedUntil: &deadline, SnoozeChangedAt: changed}
	if err := service.Register(testSensor, request); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	request.SnoozedUntil = &zero
	request.SnoozeChangedAt++
	if err := service.Register(testSensor, request); err != nil {
		t.Fatal(err)
	}
	service.sendDueReminders(context.Background(), now.Add(30*time.Second))
	checkReminder(t, sender, token)
	if err := service.Register(testSensor, Registration{InstallationID: testInstallation, Remove: true}); err != nil {
		t.Fatal(err)
	}
	service.sendDueReminders(context.Background(), now.Add(time.Minute))
	if len(sender) != 0 {
		t.Fatal("unlinked phone received a reminder")
	}
	service.deliverReminder(context.Background(), now, reminderDelivery{deviceID: testSensor, installationID: testInstallation, eventID: "alarm-1", token: token, report: alert.Report{AlertID: "alarm-1"}})
	if len(sender) != 0 {
		t.Fatal("queued delivery survived unlink")
	}
}

func TestOnlyFreshCompleteNormalStopsReminders(t *testing.T) {
	service, sender, now, token := reminderFixture(t)
	stove, iaq, presence := 60.0, 30.0, true
	report := telemetry.Report{Condition: alert.ConditionNormal, MeasuredAt: now.Unix(), Readings: telemetry.Readings{StoveTemperatureC: &stove, IAQ: &iaq, Presence: &presence}}
	for _, condition := range []alert.Condition{alert.ConditionWarning1, alert.ConditionWarning2} {
		warning := report
		warning.Condition = condition
		service.ObserveTelemetry(testSensor, telemetry.Snapshot{Report: warning}, false, now, 10*time.Second)
	}
	incomplete := report
	incomplete.Readings = telemetry.Readings{}
	service.ObserveTelemetry(testSensor, telemetry.Snapshot{Report: incomplete}, false, now, 10*time.Second)
	stale := report
	stale.MeasuredAt = now.Add(-time.Minute).Unix()
	service.ObserveTelemetry(testSensor, telemetry.Snapshot{Report: stale}, false, now, 10*time.Second)
	service.sendDueReminders(context.Background(), now.Add(30*time.Second))
	checkReminder(t, sender, token)
	service.ObserveTelemetry(testSensor, telemetry.Snapshot{Report: report}, false, now, 10*time.Second)
	service.sendDueReminders(context.Background(), now.Add(time.Minute))
	if len(sender) != 0 {
		t.Fatal("confirmed Normal did not stop reminders")
	}
}

func TestSnoozeRejectsWrongSessionAndInvalidRequests(t *testing.T) {
	service, _, now, _ := reminderFixture(t)
	deadline := now.Add(time.Minute).Unix()
	negative := int64(-1)
	tooLong := now.Add(2 * time.Hour).Unix()
	changed := time.Now().UnixMilli()
	for _, request := range []Registration{
		{InstallationID: testInstallation, EventID: "other", SnoozedUntil: &deadline, SnoozeChangedAt: changed},
		{InstallationID: testInstallation, SnoozedUntil: &negative, SnoozeChangedAt: changed},
		{InstallationID: testInstallation, SnoozedUntil: &tooLong, SnoozeChangedAt: changed},
		{InstallationID: testInstallation, SnoozedUntil: &deadline},
		{InstallationID: testInstallation, SnoozedUntil: &deadline, SnoozeChangedAt: changed, Remove: true},
		{InstallationID: testInstallation, SnoozedUntil: &deadline, SnoozeChangedAt: changed + 120_000},
	} {
		if err := service.Register(testSensor, request); err != ErrRegistration {
			t.Fatalf("invalid request accepted: %v", err)
		}
	}
}

func TestCancelledReminderDoesNotSend(t *testing.T) {
	service, sender, now, _ := reminderFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service.sendDueReminders(ctx, now.Add(30*time.Second))
	if len(sender) != 0 {
		t.Fatal("cancelled reminder was sent")
	}
}

func TestSnoozeExpiryUpdatesExistingActivityOnlyOnce(t *testing.T) {
	service, sender, now, token := reminderFixture(t)
	updateToken := strings.Repeat("c", 64)
	if err := service.Register(testSensor, Registration{InstallationID: testInstallation, EventID: "alarm-1", UpdateToken: updateToken}); err != nil {
		t.Fatal(err)
	}
	deadline := now.Add(30 * time.Second).Unix()
	if err := service.Register(testSensor, Registration{InstallationID: testInstallation, EventID: "alarm-1", SnoozedUntil: &deadline, SnoozeChangedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	service.sendDueReminders(context.Background(), now.Add(30*time.Second))
	alerts, updates := make(testSender, 2), make(testSender, 2)
	for range 2 {
		select {
		case sent := <-sender:
			if sent.PushType == apns.PushTypeAlert {
				alerts <- sent
			} else {
				updates <- sent
			}
		case <-time.After(time.Second):
			t.Fatal("missing reminder or ActivityKit update")
		}
	}
	checkReminder(t, alerts, token)
	update := expectPush(t, updates, "update", updateToken)
	if update.ContentState.IsSnoozed {
		t.Fatal("expired snooze remained active")
	}
	service.sendDueReminders(context.Background(), now.Add(60*time.Second))
	checkReminder(t, sender, token)
	if len(sender) != 0 {
		t.Fatal("unneeded ActivityKit update")
	}
}

func TestSnoozeDeadlineDoesNotWaitForTheSharedThirtySecondCycle(t *testing.T) {
	service, sender, now, token := reminderFixture(t)
	deadline := now.Add(37 * time.Second).Unix()
	if err := service.Register(testSensor, Registration{InstallationID: testInstallation, EventID: "alarm-1", SnoozedUntil: &deadline, SnoozeChangedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	// Reload to check that the individual deadline survives a restart.
	service, err := Open(service.file, sender, service.logger)
	if err != nil {
		t.Fatal(err)
	}
	for _, seconds := range []int{30, 36} {
		service.sendDueReminders(context.Background(), now.Add(time.Duration(seconds)*time.Second))
		if len(sender) != 0 {
			t.Fatal("snooze ended too early")
		}
	}
	service.sendDueReminders(context.Background(), now.Add(37*time.Second))
	checkReminder(t, sender, token)
	service.sendDueReminders(context.Background(), now.Add(60*time.Second))
	if len(sender) != 0 {
		t.Fatal("old shared cycle sent an extra alert")
	}
	service.sendDueReminders(context.Background(), now.Add(67*time.Second))
	checkReminder(t, sender, token)
}

func TestPhonesHaveIndependentSnoozeExpirySchedules(t *testing.T) {
	service, sender, now, firstToken := reminderFixture(t)
	secondID := "11234567-89ab-cdef-0123-456789abcdef"
	secondToken := strings.Repeat("b", 64)
	if err := service.Register(testSensor, Registration{InstallationID: secondID, PushToken: secondToken}); err != nil {
		t.Fatal(err)
	}
	deadline := now.Add(37 * time.Second).Unix()
	if err := service.Register(testSensor, Registration{InstallationID: testInstallation, EventID: "alarm-1", SnoozedUntil: &deadline, SnoozeChangedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		seconds int
		token   string
	}{{30, secondToken}, {37, firstToken}, {60, secondToken}, {67, firstToken}} {
		service.sendDueReminders(context.Background(), now.Add(time.Duration(step.seconds)*time.Second))
		checkReminder(t, sender, step.token)
		if len(sender) != 0 {
			t.Fatal("snooze changed another phone's schedule")
		}
	}
}

func TestLegacySnoozeDeadlineMigratesToThePhoneSchedule(t *testing.T) {
	service, sender, now, token := reminderFixture(t)
	service.mu.Lock()
	state := service.sensors[testSensor]
	phone := state.Installations[testInstallation]
	phone.NextReminderAt = 0
	phone.SnoozedUntil = now.Add(37 * time.Second).Unix()
	state.Installations = clone(state.Installations)
	state.Installations[testInstallation] = phone
	state.NextReminderAt = now.Add(60 * time.Second).Unix()
	if err := service.save(testSensor, state); err != nil {
		t.Fatal(err)
	}
	service.mu.Unlock()
	service.sendDueReminders(context.Background(), now.Add(37*time.Second))
	checkReminder(t, sender, token)
}
