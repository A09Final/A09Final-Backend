package widgetpush

import (
	"context"
	"errors"
	"github.com/dericknorlan/beforefire-backend/internal/alert"
	"github.com/dericknorlan/beforefire-backend/internal/apns"
	"github.com/dericknorlan/beforefire-backend/internal/device"
	"github.com/dericknorlan/beforefire-backend/internal/telemetry"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const sensor = "DAPUR_A1B2C3"
const installation = "01234567-89ab-cdef-0123-456789abcdef"

type sender struct {
	sync.Mutex
	sent []apns.Notification
	err  error
}

func (s *sender) Send(_ context.Context, n apns.Notification) error {
	s.Lock()
	defer s.Unlock()
	s.sent = append(s.sent, n)
	return s.err
}
func observe(s *Service, condition alert.Condition, now time.Time) {
	s.Observe(sensor, &telemetry.Snapshot{Report: telemetry.Report{Condition: condition, MeasuredAt: now.Unix()}, ReceivedAt: now}, device.ConnectionOnline, now, time.Minute)
}
func TestChangesCoalesceAndRegistrationsSurviveRestart(t *testing.T) {
	out := &sender{}
	file := filepath.Join(t.TempDir(), "widget.json")
	s, err := Open(file, out, nil)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 64)
	if err = s.Register(sensor, Registration{InstallationID: installation, PushToken: token}); err != nil {
		t.Fatal(err)
	}
	s, err = Open(file, out, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	observe(s, alert.ConditionNormal, now)
	s.Flush(context.Background(), now)
	if len(out.sent) != 1 {
		t.Fatal("initial status not sent")
	}
	push := out.sent[0]
	if push.PushType != apns.PushTypeWidgets || push.Priority != 5 || string(push.Payload) != `{"aps":{"content-changed":true}}` {
		t.Fatal("wrong WidgetKit payload")
	}
	observe(s, alert.ConditionNormal, now.Add(time.Second))
	s.Flush(context.Background(), now.Add(time.Second))
	if len(out.sent) != 1 {
		t.Fatal("unchanged readings caused a push")
	}
	observe(s, alert.ConditionWarning1, now.Add(2*time.Second))
	observe(s, alert.ConditionWarning2, now.Add(3*time.Second))
	s.Flush(context.Background(), now.Add(3*time.Second))
	if len(out.sent) != 1 {
		t.Fatal("changes were not coalesced")
	}
	s.Flush(context.Background(), now.Add(RetryInterval))
	if len(out.sent) != 2 {
		t.Fatal("latest change not sent")
	}
	if err = s.Register(sensor, Registration{InstallationID: installation, Remove: true}); err != nil {
		t.Fatal(err)
	}
	observe(s, alert.ConditionAlarm, now.Add(time.Minute))
	s.Flush(context.Background(), now.Add(time.Minute))
	if len(out.sent) != 2 {
		t.Fatal("removed installation received a push")
	}
}
func TestTokenReplacementRetryAndAPNsRejection(t *testing.T) {
	out := &sender{err: errors.New("offline")}
	s, _ := Open("", out, nil)
	now := time.Now()
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	_ = s.Register(sensor, Registration{InstallationID: installation, PushToken: a})
	observe(s, alert.ConditionWarning1, now)
	s.Flush(context.Background(), now)
	out.err = nil
	s.Flush(context.Background(), now.Add(RetryInterval))
	if len(out.sent) != 2 {
		t.Fatal("failed delivery was not retried")
	}
	_ = s.Register(sensor, Registration{InstallationID: installation, PushToken: b})
	s.Flush(context.Background(), now.Add(2*RetryInterval))
	if len(out.sent) != 3 || out.sent[2].DeviceToken != b {
		t.Fatal("replacement token not used")
	}
	out.err = &apns.Error{StatusCode: 400, Reason: "BadDeviceToken"}
	observe(s, alert.ConditionAlarm, now)
	s.Flush(context.Background(), now.Add(3*RetryInterval))
	out.err = nil
	s.Flush(context.Background(), now.Add(4*RetryInterval))
	if len(out.sent) != 5 {
		t.Fatal("configuration errors must not remove tokens")
	}
	out.err = &apns.Error{StatusCode: 410, Reason: "Unregistered"}
	observe(s, alert.ConditionNormal, now)
	s.Flush(context.Background(), now.Add(5*RetryInterval))
	s.Flush(context.Background(), now.Add(6*RetryInterval))
	if len(out.sent) != 6 {
		t.Fatal("unregistered token must be removed")
	}
}
func TestStaleAndOfflineTransitions(t *testing.T) {
	out := &sender{}
	s, _ := Open("", out, nil)
	_ = s.Register(sensor, Registration{InstallationID: installation, PushToken: strings.Repeat("a", 64)})
	now := time.Now().Truncate(time.Second)
	snapshot := &telemetry.Snapshot{Report: telemetry.Report{Condition: alert.ConditionNormal, MeasuredAt: now.Unix()}}
	s.Observe(sensor, snapshot, device.ConnectionOnline, now, 10*time.Second)
	s.Flush(context.Background(), now)
	s.Observe(sensor, snapshot, device.ConnectionOnline, now.Add(11*time.Second), 10*time.Second)
	s.Flush(context.Background(), now.Add(11*time.Second))
	s.Observe(sensor, snapshot, device.ConnectionOffline, now.Add(22*time.Second), 10*time.Second)
	s.Flush(context.Background(), now.Add(22*time.Second))
	if len(out.sent) != 3 {
		t.Fatal("freshness/connection changes were lost")
	}
}
func TestRegistrationValidationAndPersistenceFailure(t *testing.T) {
	s, _ := Open("", &sender{}, nil)
	for _, r := range []Registration{{InstallationID: "bad", PushToken: strings.Repeat("a", 64)}, {InstallationID: installation, PushToken: "bad"}, {InstallationID: installation, Remove: true, PushToken: strings.Repeat("a", 64)}} {
		if !errors.Is(s.Register(sensor, r), ErrRegistration) {
			t.Fatal("invalid registration accepted")
		}
	}
	s.file = filepath.Join(t.TempDir(), "missing", "state.json")
	if err := s.Register(sensor, Registration{InstallationID: installation, PushToken: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	// A directory cannot replace a state file. The rejected change must not replace the old token.
	s.file = t.TempDir()
	if err := s.Register(sensor, Registration{InstallationID: installation, Remove: true}); err == nil {
		t.Fatal("expected persistence failure")
	}
	if len(s.Devices()) != 1 {
		t.Fatal("failed write removed the subscription")
	}
}
