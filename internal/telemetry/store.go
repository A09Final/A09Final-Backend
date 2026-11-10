package telemetry

import (
	"errors"
	"sync"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/alert"
)

// Errors that Read returns.
var (
	ErrNoReadings     = errors.New("telemetry: the device has not sent readings yet")
	ErrInvalidReadKey = errors.New("telemetry: the read key is not correct")
)

// Snapshot is the latest telemetry of one device.
type Snapshot struct {
	Report     Report
	ReceivedAt time.Time
}

// MeasuredTime returns the time of the measurement: MeasuredAt when the board
// has a clock, otherwise the time when the backend received the message.
func (s Snapshot) MeasuredTime() time.Time {
	if s.Report.MeasuredAt > 0 {
		return time.Unix(s.Report.MeasuredAt, 0)
	}
	return s.ReceivedAt
}

// ConfirmsNormal requires a Normal label and readings from all three sensors.
func (s Snapshot) ConfirmsNormal() bool {
	report := s.Report
	return report.Condition == alert.ConditionNormal && !report.AssessmentMalformed && len(report.UnavailableSensors) == 0 &&
		report.Readings.StoveTemperatureC != nil && report.Readings.IAQ != nil && report.Readings.Presence != nil &&
		(report.Assessment == nil || (report.Assessment.Valid && report.Assessment.Condition == alert.ConditionNormal))
}

// Store keeps the latest telemetry of each device in memory.
type Store struct {
	mu        sync.RWMutex
	snapshots map[string]Snapshot
	now       func() time.Time
}

func NewStore() *Store {
	return &Store{snapshots: make(map[string]Snapshot), now: time.Now}
}

// Record checks and stores one MQTT payload from beforefire/<deviceID>/telemetry.
// An empty payload clears the retained message, so the device has no readings again.
func (s *Store) Record(deviceID string, payload []byte) error {
	if len(payload) == 0 {
		s.mu.Lock()
		delete(s.snapshots, deviceID)
		s.mu.Unlock()
		return nil
	}
	now := s.now()
	report, err := ParseReport(payload, now)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.snapshots[deviceID] = Snapshot{Report: report, ReceivedAt: now}
	s.mu.Unlock()
	return nil
}

// Read returns the latest snapshot when readKey matches the hash that the device sent.
// It returns ErrNoReadings before the first message of the device.
func (s *Store) Read(deviceID, readKey string) (Snapshot, error) {
	snapshot, ok := s.Latest(deviceID)
	if !ok {
		return Snapshot{}, ErrNoReadings
	}
	if !matchesReadKey(readKey, snapshot.Report.ReadKeyHash) {
		return Snapshot{}, ErrInvalidReadKey
	}
	return snapshot, nil
}

// Latest returns the server's copy of the most recent reading.
func (s *Store) Latest(deviceID string) (Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot, ok := s.snapshots[deviceID]
	return snapshot, ok
}
