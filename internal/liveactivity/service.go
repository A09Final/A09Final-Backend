// Package liveactivity starts and ends the iPhone alarm Live Activity.
package liveactivity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/alert"
	"github.com/dericknorlan/beforefire-backend/internal/apns"
	"github.com/dericknorlan/beforefire-backend/internal/telemetry"
)

const (
	maxInstallations = 10
	maxActivities    = 8
	recoveredHistory = 32
	deliveryTimeout  = 20 * time.Second
	endExpiry        = 8 * time.Hour
)

var (
	installationID  = regexp.MustCompile(`^[a-fA-F0-9]{8}(-[a-fA-F0-9]{4}){3}-[a-fA-F0-9]{12}$`)
	eventID         = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	ErrRegistration = errors.New("invalid Live Activity registration")
)

type Registration struct {
	InstallationID string `json:"installationId"`
	StartToken     string `json:"startToken,omitempty"`
	EventID        string `json:"eventId,omitempty"`
	UpdateToken    string `json:"updateToken,omitempty"`
	Remove         bool   `json:"remove,omitempty"`
}

type installation struct {
	StartToken string            `json:"startToken,omitempty"`
	Updates    map[string]string `json:"updates,omitempty"`
}

type sensorState struct {
	Installations   map[string]installation `json:"installations"`
	EventID         string                  `json:"eventId,omitempty"`
	AlarmAt         int64                   `json:"alarmAt,omitempty"`
	StartTimestamp  int64                   `json:"startTimestamp,omitempty"`
	Recovered       bool                    `json:"recovered,omitempty"`
	RecoveredEvents []string                `json:"recoveredEvents,omitempty"`
}

type Service struct {
	mu      sync.Mutex
	file    string
	sender  alert.Sender
	logger  *slog.Logger
	sensors map[string]sensorState
	ending  map[string]bool
}

func Open(file string, sender alert.Sender, logger *slog.Logger) (*Service, error) {
	s := &Service{file: file, sender: sender, logger: logger, sensors: make(map[string]sensorState), ending: make(map[string]bool)}
	if file == "" {
		return s, nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.sensors); err != nil {
		return nil, err
	}
	if s.sensors == nil {
		return nil, errors.New("invalid Live Activity state")
	}
	return s, nil
}

func validToken(token string) bool {
	if len(token) < 64 || len(token) > 512 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

func (s *Service) Register(deviceID string, request Registration) error {
	if !installationID.MatchString(request.InstallationID) {
		return ErrRegistration
	}
	if request.Remove {
		if request.StartToken != "" || request.EventID != "" || request.UpdateToken != "" {
			return ErrRegistration
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		state := s.sensors[deviceID]
		phone, exists := state.Installations[request.InstallationID]
		if !exists || phone.StartToken == "" {
			return nil
		}
		phone.StartToken = ""
		state.Installations = clone(state.Installations)
		if len(phone.Updates) == 0 {
			delete(state.Installations, request.InstallationID)
		} else {
			state.Installations[request.InstallationID] = phone
		}
		return s.save(deviceID, state)
	}
	start := request.StartToken != "" && validToken(request.StartToken) && request.EventID == "" && request.UpdateToken == ""
	update := request.StartToken == "" && eventID.MatchString(request.EventID) && validToken(request.UpdateToken)
	if !start && !update {
		return ErrRegistration
	}

	s.mu.Lock()
	state := s.sensors[deviceID]
	current, exists := state.Installations[request.InstallationID]
	if (!exists && len(state.Installations) >= maxInstallations) ||
		(update && (!exists || (request.EventID != state.EventID && !slices.Contains(state.RecoveredEvents, request.EventID)) ||
			(current.Updates[request.EventID] == "" && len(current.Updates) >= maxActivities))) {
		s.mu.Unlock()
		return ErrRegistration
	}
	if start {
		if current.StartToken == request.StartToken {
			s.mu.Unlock()
			return nil
		}
		current.StartToken = request.StartToken
	} else {
		current.Updates = clone(current.Updates)
		current.Updates[request.EventID] = request.UpdateToken
	}
	state.Installations = clone(state.Installations)
	state.Installations[request.InstallationID] = current
	err := s.save(deviceID, state)
	s.mu.Unlock()
	if err == nil && update {
		s.endRecovered(deviceID)
	}
	return err
}

// Alarm runs alongside the ordinary alert relay. A warning between two Alarm
// reports belongs to the same session until valid Normal telemetry arrives.
func (s *Service) Alarm(ctx context.Context, deviceID string, report alert.Report) {
	s.mu.Lock()
	state := s.sensors[deviceID]
	if len(state.Installations) == 0 || (state.EventID != "" && (!state.Recovered || state.EventID == report.AlertID)) {
		s.mu.Unlock()
		return
	}
	if state.EventID != "" {
		history := append(slices.Clone(state.RecoveredEvents), state.EventID)
		state.RecoveredEvents = history[max(len(history)-recoveredHistory, 0):]
	}
	state.EventID, state.AlarmAt, state.Recovered = report.AlertID, report.IssuedAt, false
	state.StartTimestamp = time.Now().Unix()
	err := s.save(deviceID, state)
	s.mu.Unlock()
	if err != nil {
		s.logger.Error("cannot save Live Activity alarm", "deviceId", deviceID, "error", err)
		return
	}
	payload, err := startPayload(deviceID, report.AlertID, report.IssuedAt, state.StartTimestamp)
	if err != nil {
		s.logger.Error("cannot encode Live Activity alarm", "deviceId", deviceID, "error", err)
		return
	}
	var deliveries sync.WaitGroup
	for _, phone := range state.Installations {
		if phone.StartToken == "" {
			continue
		}
		deliveries.Add(1)
		go func() {
			defer deliveries.Done()
			if err := s.send(ctx, phone.StartToken, report.AlertID, payload, time.Time{}); err != nil {
				s.logDeliveryError("start", deviceID, err)
			}
		}()
	}
	deliveries.Wait()
}

// ObserveTelemetry accepts only a recent, complete Normal measurement made
// after the alarm started. A missing sensor or a lost connection holds Alarm.
func (s *Service) ObserveTelemetry(deviceID string, snapshot telemetry.Snapshot, retained bool, now time.Time, staleAfter time.Duration) {
	report := snapshot.Report
	measuredAt := snapshot.MeasuredTime()
	if !snapshot.ConfirmsNormal() || (retained && report.MeasuredAt == 0) ||
		measuredAt.After(now) || now.Sub(measuredAt) > staleAfter {
		return
	}
	s.mu.Lock()
	state := s.sensors[deviceID]
	if state.EventID == "" || measuredAt.Unix() <= state.AlarmAt {
		s.mu.Unlock()
		return
	}
	if !state.Recovered {
		state.Recovered = true
		if err := s.save(deviceID, state); err != nil {
			s.mu.Unlock()
			s.logger.Error("cannot save Live Activity recovery", "deviceId", deviceID, "error", err)
			return
		}
	}
	s.mu.Unlock()
	s.endRecovered(deviceID)
}

func (s *Service) endRecovered(deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sensors[deviceID]
	for id, phone := range state.Installations {
		for event, token := range phone.Updates {
			confirmed := (event == state.EventID && state.Recovered) || slices.Contains(state.RecoveredEvents, event)
			if !confirmed || s.ending[token] {
				continue
			}
			s.ending[token] = true
			startTimestamp := int64(0)
			if event == state.EventID {
				startTimestamp = state.StartTimestamp
			}
			go s.deliverEnd(deviceID, id, event, token, startTimestamp)
		}
	}
}

func (s *Service) deliverEnd(deviceID, installationID, eventID, token string, startTimestamp int64) {
	ctx, cancel := context.WithTimeout(context.Background(), deliveryTimeout)
	defer cancel()
	// ActivityKit uses the timestamp to order pushes. An immediate recovery
	// can otherwise have the same timestamp as its start.
	if delay := time.Until(time.Unix(startTimestamp+1, 0)); delay > 0 {
		time.Sleep(delay)
	}
	now := time.Now()
	payload, err := endPayload(eventID, now.Unix())
	if err == nil {
		err = s.send(ctx, token, eventID, payload, now.Add(endExpiry))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ending, token)
	var apnsErr *apns.Error
	if err != nil && !(errors.As(err, &apnsErr) && apnsErr.IsUnregistered()) {
		s.logDeliveryError("end", deviceID, err)
		return
	}
	state := s.sensors[deviceID]
	phone := state.Installations[installationID]
	if phone.Updates[eventID] != token {
		return
	}
	phone.Updates = clone(phone.Updates)
	delete(phone.Updates, eventID)
	state.Installations = clone(state.Installations)
	if phone.StartToken == "" && len(phone.Updates) == 0 {
		delete(state.Installations, installationID)
	} else {
		state.Installations[installationID] = phone
	}
	if err := s.save(deviceID, state); err != nil {
		s.logger.Error("cannot save Live Activity delivery", "deviceId", deviceID, "error", err)
	}
}

func (s *Service) send(ctx context.Context, token, eventID string, payload []byte, expiration time.Time) error {
	return s.sender.Send(ctx, apns.Notification{
		DeviceToken: token, Payload: payload, PushType: apns.PushTypeLiveActivity,
		Priority: apns.PriorityImmediate, CollapseID: eventID, Expiration: expiration,
	})
}

func (s *Service) logDeliveryError(event, deviceID string, err error) {
	// A transport error can contain the APNs URL, which contains the token.
	var rejected *apns.Error
	if errors.As(err, &rejected) {
		s.logger.Warn("Live Activity rejected", "event", event, "deviceId", deviceID, "status", rejected.StatusCode, "reason", rejected.Reason)
		return
	}
	s.logger.Warn("Live Activity delivery failed", "event", event, "deviceId", deviceID)
}

// save runs under mu. Only publish the new state after the disk write succeeds.
func (s *Service) save(deviceID string, state sensorState) error {
	next := clone(s.sensors)
	next[deviceID] = state
	if s.file != "" {
		data, err := json.Marshal(next)
		if err != nil {
			return err
		}
		temp, err := os.CreateTemp(filepath.Dir(s.file), ".live-activity-*")
		if err != nil {
			return err
		}
		defer os.Remove(temp.Name())
		_, writeErr := temp.Write(data)
		if err := errors.Join(writeErr, temp.Sync(), temp.Close()); err != nil {
			return err
		}
		if err := os.Rename(temp.Name(), s.file); err != nil {
			return err
		}
	}
	s.sensors = next
	return nil
}

func clone[K comparable, V any](source map[K]V) map[K]V {
	result := make(map[K]V, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}

type activityState struct {
	IsSnoozed   bool   `json:"isSnoozed"`
	PushEventID string `json:"pushEventID"`
}

type activityAPS struct {
	Timestamp      int64               `json:"timestamp"`
	Event          string              `json:"event"`
	ContentState   activityState       `json:"content-state"`
	AttributesType string              `json:"attributes-type,omitempty"`
	Attributes     *activityAttributes `json:"attributes,omitempty"`
	InputPushToken int                 `json:"input-push-token,omitempty"`
	DismissalDate  int64               `json:"dismissal-date,omitempty"`
	Alert          map[string]string   `json:"alert,omitempty"`
}

type activityAttributes struct {
	PreviewID      string `json:"previewID"`
	SensorDeviceID string `json:"sensorDeviceID"`
	RemoteEventID  string `json:"remoteEventID"`
	AlarmIssuedAt  int64  `json:"alarmIssuedAt"`
}

func startPayload(deviceID, eventID string, alarmAt, now int64) ([]byte, error) {
	id := sha256.Sum256([]byte(deviceID + "/" + eventID))
	uuid := fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:16])
	return json.Marshal(map[string]activityAPS{"aps": {
		Timestamp: now, Event: "start", ContentState: activityState{PushEventID: eventID},
		AttributesType: "AlarmActivityAttributes", Attributes: &activityAttributes{PreviewID: uuid, SensorDeviceID: deviceID, RemoteEventID: eventID, AlarmIssuedAt: alarmAt},
		InputPushToken: 1, Alert: map[string]string{"title": "Stove alarm", "body": "Turn off the stove now if you can safely reach it."},
	}})
}

func endPayload(eventID string, now int64) ([]byte, error) {
	return json.Marshal(map[string]activityAPS{"aps": {
		Timestamp: now, Event: "end", ContentState: activityState{PushEventID: eventID},
		DismissalDate: now - 1,
	}})
}
