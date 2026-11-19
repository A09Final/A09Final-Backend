package liveactivity

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/dericknorlan/beforefire-backend/internal/alert"
	"github.com/dericknorlan/beforefire-backend/internal/apns"
	"github.com/dericknorlan/beforefire-backend/internal/telemetry"
	"strings"
	"sync"
	"testing"
	"time"
)

type recoverySender struct {
	mu     sync.Mutex
	failed bool
	ends   chan bool
}

func (s *recoverySender) Send(_ context.Context, n apns.Notification) error {
	var body struct{ APS activityAPS }
	_ = json.Unmarshal(n.Payload, &body)
	if body.APS.Event != "end" {
		return nil
	}
	s.mu.Lock()
	wasFailed := s.failed
	s.failed = true
	s.mu.Unlock()
	s.ends <- wasFailed
	if !wasFailed {
		return errors.New("temporary network failure")
	}
	return nil
}
func TestRecoveredActivityRetriesFailedEndWithoutAnotherReading(t *testing.T) {
	output := &recoverySender{ends: make(chan bool, 4)}
	s, _ := Open("", output, nil)
	_ = s.Register(testSensor, Registration{InstallationID: testInstallation, StartToken: strings.Repeat("a", 64)})
	now := time.Now().Truncate(time.Second)
	s.Alarm(context.Background(), testSensor, alert.Report{AlertID: "recover-1", Condition: alert.ConditionAlarm, IssuedAt: now.Add(-3 * time.Second).Unix()})
	_ = s.Register(testSensor, Registration{InstallationID: testInstallation, EventID: "recover-1", UpdateToken: strings.Repeat("b", 64)})
	stove, iaq, presence := 40.0, 65.0, true
	normal := telemetry.Snapshot{Report: telemetry.Report{Condition: alert.ConditionNormal, MeasuredAt: now.Unix(), Readings: telemetry.Readings{StoveTemperatureC: &stove, IAQ: &iaq, Presence: &presence}}}
	s.ObserveTelemetry(testSensor, normal, false, now, time.Minute)
	select {
	case <-output.ends:
	case <-time.After(3 * time.Second):
		t.Fatal("no initial end")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.RetryRecovery()
		select {
		case succeeded := <-output.ends:
			if !succeeded {
				t.Fatal("retry did not recover")
			}
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("end was not retried without a new reading")
}
func TestRecoveryWorkerStopsWhenCanceled(t *testing.T) {
	s, _ := Open("", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunRecovery(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recovery worker did not stop")
	}
}
