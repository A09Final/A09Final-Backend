// Package widgetpush sends data-change signals to WidgetKit. It does not send alerts.
package widgetpush

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/alert"
	"github.com/dericknorlan/beforefire-backend/internal/apns"
	"github.com/dericknorlan/beforefire-backend/internal/device"
	"github.com/dericknorlan/beforefire-backend/internal/telemetry"
)

var ErrRegistration = errors.New("invalid widget registration")
var installationPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)

const RetryInterval = 10 * time.Second

type Registration struct {
	InstallationID string `json:"installationId"`
	PushToken      string `json:"pushToken,omitempty"`
	Remove         bool   `json:"remove,omitempty"`
}

type delivery struct {
	token, signature string
	attemptedAt      time.Time
}

type Service struct {
	mu            sync.Mutex
	file          string
	sender        alert.Sender
	logger        *slog.Logger
	registrations map[string]map[string]string
	current       map[string]string
	delivered     map[string]delivery
}

func Open(file string, sender alert.Sender, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{file: file, sender: sender, logger: logger, registrations: map[string]map[string]string{}, current: map[string]string{}, delivered: map[string]delivery{}}
	if file != "" {
		body, err := os.ReadFile(file)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil {
			if err := json.Unmarshal(body, &s.registrations); err != nil {
				return nil, err
			}
			if s.registrations == nil {
				return nil, errors.New("invalid widget store")
			}
		}
	}
	return s, nil
}

func (s *Service) Register(deviceID string, request Registration) error {
	if !device.IsValidID(deviceID) || !installationPattern.MatchString(request.InstallationID) ||
		(request.Remove && request.PushToken != "") || (!request.Remove && !alert.IsValidPushToken(request.PushToken)) {
		return ErrRegistration
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	updated := make(map[string]string)
	for id, token := range s.registrations[deviceID] {
		updated[id] = token
	}
	if request.Remove {
		delete(updated, request.InstallationID)
	} else {
		for id, token := range updated {
			if token == request.PushToken {
				delete(updated, id)
			}
		}
		if len(updated) >= 64 && updated[request.InstallationID] == "" {
			return ErrRegistration
		}
		updated[request.InstallationID] = request.PushToken
	}
	old := s.registrations[deviceID]
	s.registrations[deviceID] = updated
	if err := s.saveLocked(); err != nil {
		s.registrations[deviceID] = old
		return err
	}
	if request.Remove {
		delete(s.delivered, deviceID+"/"+request.InstallationID)
	}
	return nil
}

// Observe ignores numeric changes that do not change the widget's status or rule.
// This avoids a push for every sensor sample or absence-duration increment.
func (s *Service) Observe(deviceID string, snapshot *telemetry.Snapshot, connection device.Connection, now time.Time, staleAfter time.Duration) {
	state := struct {
		Connection  device.Connection
		Condition   alert.Condition
		Valid       bool
		RuleID      *int
		Unavailable []string
		Stale       bool
	}{Connection: connection}
	if snapshot != nil {
		r := snapshot.Report
		state.Condition, state.Valid, state.Unavailable = r.Condition, !r.AssessmentMalformed, r.UnavailableSensors
		if r.Assessment != nil {
			state.Valid = state.Valid && r.Assessment.Valid && r.Assessment.Condition == r.Condition
			state.RuleID = r.Assessment.RuleID
		}
		state.Stale = now.Sub(snapshot.MeasuredTime()) > staleAfter || snapshot.MeasuredTime().After(now)
	}
	encoded, _ := json.Marshal(state)
	s.mu.Lock()
	s.current[deviceID] = string(encoded)
	s.mu.Unlock()
}

// Flush coalesces changes and retries failures. iOS controls when the widget reloads.
func (s *Service) Flush(ctx context.Context, now time.Time) {
	type target struct{ deviceID, installationID, token, signature string }
	var targets []target
	s.mu.Lock()
	for deviceID, phones := range s.registrations {
		for id, token := range phones {
			key, signature := deviceID+"/"+id, s.current[deviceID]
			previous := s.delivered[key]
			if signature == "" || (previous.token == token && previous.signature == signature) ||
				(!previous.attemptedAt.IsZero() && now.Sub(previous.attemptedAt) < RetryInterval) {
				continue
			}
			previous.attemptedAt = now
			s.delivered[key] = previous
			targets = append(targets, target{deviceID, id, token, signature})
		}
	}
	s.mu.Unlock()
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		err := s.sender.Send(ctx, apns.Notification{DeviceToken: target.token, PushType: apns.PushTypeWidgets,
			Payload: []byte(`{"aps":{"content-changed":true}}`), Priority: apns.PriorityPowerSaving,
			Expiration: now.Add(15 * time.Minute), CollapseID: "kitchen-status"})
		s.mu.Lock()
		if s.registrations[target.deviceID][target.installationID] == target.token {
			key := target.deviceID + "/" + target.installationID
			var rejected *apns.Error
			switch {
			case err == nil:
				s.delivered[key] = delivery{target.token, target.signature, now}
			case errors.As(err, &rejected) && rejected.IsUnregistered():
				delete(s.registrations[target.deviceID], target.installationID)
				if saveErr := s.saveLocked(); saveErr != nil {
					s.logger.Error("cannot save removed widget token", "error", saveErr)
				}
			default:
				s.logger.Warn("widget refresh push failed", "deviceId", target.deviceID, "error", err)
			}
		}
		s.mu.Unlock()
	}
}

func (s *Service) saveLocked() error {
	if s.file == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.file), 0700); err != nil {
		return err
	}
	encoded, err := json.Marshal(s.registrations)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(s.file), ".widget-push-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(encoded); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), s.file)
}

// Devices returns the sensors that have widget subscriptions.
func (s *Service) Devices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var devices []string
	for id, subscriptions := range s.registrations {
		if len(subscriptions) > 0 {
			devices = append(devices, id)
		}
	}
	return devices
}
