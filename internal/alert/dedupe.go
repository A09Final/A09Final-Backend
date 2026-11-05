package alert

import (
	"sync"
	"time"
)

// Deduplicator remembers recent keys for a fixed time. MQTT QoS 1 can deliver
// one message two times, and one alert must not send two notifications.
type Deduplicator struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	seenAt  map[string]time.Time
	inserts int
}

// Prune expired keys after this many inserts, so the map stays small without a timer.
const pruneEveryInserts = 128

func NewDeduplicator(ttl time.Duration) *Deduplicator {
	return &Deduplicator{ttl: ttl, now: time.Now, seenAt: make(map[string]time.Time)}
}

// FirstTime records key and reports whether it was not seen in the last ttl.
func (d *Deduplicator) FirstTime(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if seenAt, ok := d.seenAt[key]; ok && now.Sub(seenAt) < d.ttl {
		return false
	}
	d.seenAt[key] = now
	d.inserts++
	if d.inserts%pruneEveryInserts == 0 {
		for existing, seenAt := range d.seenAt {
			if now.Sub(seenAt) >= d.ttl {
				delete(d.seenAt, existing)
			}
		}
	}
	return true
}
