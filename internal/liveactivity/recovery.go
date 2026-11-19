package liveactivity

import (
	"context"
	"time"
)

// RunRecovery retries an end push after a transport or APNs failure.
// A confirmed Normal measurement remains required. Missing data does not end an alarm.
func (s *Service) RunRecovery(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RetryRecovery()
		}
	}
}

func (s *Service) RetryRecovery() {
	s.mu.Lock()
	var devices []string
	for id, state := range s.sensors {
		if state.Recovered || len(state.RecoveredEvents) > 0 {
			devices = append(devices, id)
		}
	}
	s.mu.Unlock()
	for _, id := range devices {
		s.endRecovered(id)
	}
}
