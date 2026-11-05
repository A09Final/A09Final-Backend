package telemetry

import (
	"errors"
	"sync"
	"time"
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
	s.mu.RLock()
	snapshot, ok := s.snapshots[deviceID]
	s.mu.RUnlock()
	if !ok {
		return Snapshot{}, ErrNoReadings
	}
	if !matchesReadKey(readKey, snapshot.Report.ReadKeyHash) {
		return Snapshot{}, ErrInvalidReadKey
	}
	return snapshot, nil
}
