package device

import (
	"sort"
	"sync"
	"time"
)

// Connection is the last connection state that a device reported.
type Connection int

const (
	ConnectionUnknown Connection = iota
	ConnectionOnline
	ConnectionOffline
)

// Status is the last known connection state of one device.
type Status struct {
	Connection Connection
	UpdatedAt  time.Time
}

// StatusTracker keeps the last status message of each device in memory.
// The firmware publishes the status as a retained message, so the broker sends
// the current values again after each backend restart.
type StatusTracker struct {
	mu       sync.RWMutex
	statuses map[string]Status
	now      func() time.Time
}

func NewStatusTracker() *StatusTracker {
	return &StatusTracker{statuses: make(map[string]Status), now: time.Now}
}

// Record stores a status payload. It ignores payloads that it does not know.
// An empty payload clears the retained status (for example after a device is
// removed), so the device becomes unknown again.
func (t *StatusTracker) Record(deviceID string, payload []byte) {
	if len(payload) == 0 {
		t.mu.Lock()
		delete(t.statuses, deviceID)
		t.mu.Unlock()
		return
	}
	var connection Connection
	switch string(payload) {
	case StatusOnline:
		connection = ConnectionOnline
	case StatusOffline:
		connection = ConnectionOffline
	default:
		return
	}
	t.mu.Lock()
	t.statuses[deviceID] = Status{Connection: connection, UpdatedAt: t.now()}
	t.mu.Unlock()
}

// Entry is the status of one device in a list.
type Entry struct {
	DeviceID string
	Status
}

// List returns the last known status of each device, sorted by device ID.
// The app uses it to find sensors that already have Wi-Fi.
func (t *StatusTracker) List() []Entry {
	t.mu.RLock()
	entries := make([]Entry, 0, len(t.statuses))
	for deviceID, status := range t.statuses {
		entries = append(entries, Entry{DeviceID: deviceID, Status: status})
	}
	t.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].DeviceID < entries[j].DeviceID })
	return entries
}

// Get returns the last known status. Connection is ConnectionUnknown when the
// device did not publish a status since the backend started.
func (t *StatusTracker) Get(deviceID string) Status {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.statuses[deviceID]
}
