package liveactivity

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/alert"
	"github.com/dericknorlan/beforefire-backend/internal/apns"
)

const (
	reminderInterval     = 30 * time.Second
	snoozeClockAllowance = time.Minute
	maxSnoozeDuration    = time.Hour
)

// registerSnooze changes only this installation's iPhone reminders. A delayed
// request must not undo a newer snooze or affect a subsequent alarm session.
func (s *Service) registerSnooze(deviceID string, request Registration) error {
	now := time.Now()
	if request.Remove || request.StartToken != "" || request.UpdateToken != "" || request.PushToken != "" ||
		request.SnoozeChangedAt <= 0 || request.SnoozeChangedAt > now.Add(snoozeClockAllowance).UnixMilli() ||
		*request.SnoozedUntil < 0 || *request.SnoozedUntil > request.SnoozeChangedAt/1000+int64(maxSnoozeDuration/time.Second) || *request.SnoozedUntil > now.Add(maxSnoozeDuration+snoozeClockAllowance).Unix() {
		return ErrRegistration
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sensors[deviceID]
	phone, exists := state.Installations[request.InstallationID]
	if !exists || phone.PushToken == "" || state.EventID == "" || state.Recovered ||
		(request.EventID != "" && request.EventID != state.EventID) || request.SnoozeChangedAt < state.StartTimestamp*1000 {
		return ErrRegistration
	}
	if request.SnoozeChangedAt <= phone.SnoozeChangedAt {
		return nil
	}
	if *request.SnoozedUntil > 0 {
		phone.NextReminderAt = *request.SnoozedUntil
	} else if phone.SnoozedUntil > 0 {
		phone.NextReminderAt = now.Unix()
	}
	phone.SnoozedUntil, phone.SnoozeChangedAt = *request.SnoozedUntil, request.SnoozeChangedAt
	state.Installations = clone(state.Installations)
	state.Installations[request.InstallationID] = phone
	return s.save(deviceID, state)
}

// SuppressesAlarmToken also prevents a new sensor alert from bypassing a
// snooze during the same unresolved Alarm session.
func (s *Service) SuppressesAlarmToken(deviceID, token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sensors[deviceID]
	if state.Recovered {
		return false
	}
	for _, phone := range state.Installations {
		if phone.PushToken == token && phone.SnoozedUntil > time.Now().Unix() {
			return true
		}
	}
	return false
}

// RunReminders runs only when Critical Alerts are enabled in the server
// configuration. State is persisted; a restart does not clear an active alarm.
func (s *Service) RunReminders(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.sendDueReminders(ctx, now)
		}
	}
}

type reminderDelivery struct {
	deviceID, installationID, eventID, token string
	report                                   alert.Report
	resumeToken                              string
	snoozedUntil                             int64
}

func (s *Service) sendDueReminders(ctx context.Context, now time.Time) {
	var jobs []reminderDelivery
	s.mu.Lock()
	for deviceID, state := range s.sensors {
		if state.Recovered || state.ReminderReport == nil {
			continue
		}
		state.Installations = clone(state.Installations)
		var pending []reminderDelivery
		for id, phone := range state.Installations {
			if phone.PushToken == "" || phone.SnoozedUntil > now.Unix() {
				continue
			}
			due := phone.NextReminderAt
			// Migrate the previous shared schedule after a restart.
			if due == 0 {
				due = state.NextReminderAt
				if phone.SnoozedUntil > 0 {
					due = phone.SnoozedUntil
				}
			}
			if due == 0 || due > now.Unix() {
				continue
			}
			resumeToken := ""
			if phone.SnoozedUntil > 0 {
				resumeToken = phone.Updates[state.EventID]
			}
			pending = append(pending, reminderDelivery{deviceID: deviceID, installationID: id, eventID: state.EventID,
				token: phone.PushToken, report: *state.ReminderReport, resumeToken: resumeToken, snoozedUntil: phone.SnoozedUntil})
			phone.NextReminderAt = now.Add(reminderInterval).Unix()
			state.Installations[id] = phone
		}
		if len(pending) == 0 {
			continue
		}
		state.NextReminderAt = now.Add(reminderInterval).Unix()
		if err := s.save(deviceID, state); err != nil {
			s.logger.Error("cannot save alarm reminder schedule", "deviceId", deviceID)
			continue
		}
		jobs = append(jobs, pending...)
	}
	s.mu.Unlock()
	var deliveries sync.WaitGroup
	for _, job := range jobs {
		deliveries.Add(1)
		go func() { defer deliveries.Done(); s.deliverReminder(ctx, now, job) }()
	}
	deliveries.Wait()
}

func (s *Service) deliverReminder(ctx context.Context, now time.Time, job reminderDelivery) {
	if ctx.Err() != nil {
		return
	}
	payload, err := alert.AlarmReminderPayload(job.deviceID, job.report)
	if err != nil {
		return
	}
	// Recheck after queuing, so recovery, unlinking and snooze cancel pending work.
	s.mu.Lock()
	state := s.sensors[job.deviceID]
	phone := state.Installations[job.installationID]
	active := !state.Recovered && state.EventID == job.eventID && phone.PushToken == job.token && phone.SnoozedUntil <= now.Unix()
	s.mu.Unlock()
	if !active {
		return
	}
	var resume sync.WaitGroup
	if job.resumeToken != "" {
		resume.Add(1)
		go func() {
			defer resume.Done()
			updateCtx, stop := context.WithTimeout(ctx, deliveryTimeout)
			defer stop()
			s.resumeActivity(updateCtx, now, job)
		}()
	}
	defer resume.Wait()
	deliveryCtx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()
	err = s.sender.Send(deliveryCtx, apns.Notification{
		DeviceToken: job.token, Payload: payload, PushType: apns.PushTypeAlert,
		Priority: apns.PriorityImmediate, CollapseID: job.eventID,
		// Do not queue an old siren for delivery when the phone reconnects.
		Expiration: time.Time{},
	})
	if err == nil {
		return
	}
	var rejected *apns.Error
	if errors.As(err, &rejected) && rejected.IsUnregistered() {
		s.mu.Lock()
		defer s.mu.Unlock()
		state = s.sensors[job.deviceID]
		phone = state.Installations[job.installationID]
		if phone.PushToken == job.token {
			phone.PushToken = ""
			state.Installations = clone(state.Installations)
			state.Installations[job.installationID] = phone
			if err := s.save(job.deviceID, state); err != nil {
				s.logger.Error("cannot remove alarm reminder token", "deviceId", job.deviceID)
			}
		}
		return
	}
	s.logDeliveryError("reminder", job.deviceID, err)
}

// The Lock Screen countdown can reach zero while the app is closed.
// Update that card once when its snooze expires. Do not start a new card.
func (s *Service) resumeActivity(ctx context.Context, now time.Time, job reminderDelivery) {
	s.mu.Lock()
	state := s.sensors[job.deviceID]
	phone := state.Installations[job.installationID]
	active := !state.Recovered && state.EventID == job.eventID && phone.SnoozedUntil == job.snoozedUntil && phone.Updates[job.eventID] == job.resumeToken
	s.mu.Unlock()
	if !active {
		return
	}
	payload, err := json.Marshal(map[string]activityAPS{"aps": {Timestamp: now.Unix(), Event: "update", ContentState: activityState{PushEventID: job.eventID}}})
	if err == nil {
		err = s.send(ctx, job.resumeToken, job.eventID, payload, time.Time{})
	}
	if err != nil {
		s.logDeliveryError("resume", job.deviceID, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state = s.sensors[job.deviceID]
	phone = state.Installations[job.installationID]
	if state.EventID != job.eventID || phone.SnoozedUntil != job.snoozedUntil {
		return
	}
	phone.SnoozedUntil = 0
	state.Installations = clone(state.Installations)
	state.Installations[job.installationID] = phone
	if err := s.save(job.deviceID, state); err != nil {
		s.logger.Error("cannot save alarm resume", "deviceId", job.deviceID)
	}
}
