package command

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/alert"
	"github.com/dericknorlan/beforefire-backend/internal/device"
)

// Errors that Execute returns. The HTTP layer maps them to status codes.
var (
	ErrDeviceOffline     = errors.New("command: the device reported that it is offline")
	ErrBrokerUnavailable = errors.New("command: the backend is not connected to the MQTT broker")
	ErrNoAnswer          = errors.New("command: the device did not answer in time")
	ErrDuplicateRequest  = errors.New("command: a request with this ID is already in progress")
)

// Publisher sends one MQTT message. *mqtt.Client implements it.
type Publisher interface {
	Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error
}

// StatusSource gives the last known connection state of a device.
type StatusSource interface {
	Get(deviceID string) device.Status
}

// ServiceConfig configures a Service.
type ServiceConfig struct {
	Publisher        Publisher
	Statuses         StatusSource
	AnswerTimeout    time.Duration
	AllowedClockSkew time.Duration
	Logger           *slog.Logger
	Now              func() time.Time
}

// Service publishes commands and matches the ack messages to the waiting requests.
type Service struct {
	config ServiceConfig

	mu      sync.Mutex
	waiting map[string]chan Ack
}

const commandQoS = 1

func NewService(config ServiceConfig) *Service {
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Service{config: config, waiting: make(map[string]chan Ack)}
}

// Execute validates request, publishes it, and waits for the ack of the sensor unit.
// A rejected command returns an Ack with Result "rejected" and no error.
func (s *Service) Execute(ctx context.Context, deviceID string, request Request) (Ack, error) {
	if err := request.Validate(s.config.Now(), s.config.AllowedClockSkew); err != nil {
		return Ack{}, err
	}
	if s.config.Statuses.Get(deviceID).Connection == device.ConnectionOffline {
		return Ack{}, ErrDeviceOffline
	}

	key := waitKey(deviceID, request.RequestID)
	answer, err := s.startWaiting(key)
	if err != nil {
		return Ack{}, err
	}
	defer s.stopWaiting(key)

	message := Message{
		Action:    request.Action,
		RequestID: request.RequestID,
		IssuedAt:  request.IssuedAt,
		PushToken: request.PushToken,
		Signature: request.Signature,
		Origin:    OriginApp,
	}
	if err := s.publish(ctx, deviceID, message); err != nil {
		return Ack{}, err
	}

	timer := time.NewTimer(s.config.AnswerTimeout)
	defer timer.Stop()
	select {
	case ack := <-answer:
		return ack, nil
	case <-timer.C:
		return Ack{}, ErrNoAnswer
	case <-ctx.Done():
		return Ack{}, ctx.Err()
	}
}

// HandleAck processes one payload from beforefire/<deviceID>/ack.
func (s *Service) HandleAck(deviceID string, payload []byte) {
	var ack Ack
	if err := json.Unmarshal(payload, &ack); err != nil || (ack.Result != ResultDone && ack.Result != ResultRejected) {
		s.config.Logger.Warn("ack message ignored: format is not correct", "deviceId", deviceID)
		return
	}
	s.mu.Lock()
	answer, ok := s.waiting[waitKey(deviceID, ack.RequestID)]
	s.mu.Unlock()
	if !ok {
		// The request timed out, or the ack answers a backend command.
		return
	}
	select {
	case answer <- ack:
	default:
	}
}

// RemovePushToken tells the sensor unit to remove a token that APNs no longer accepts.
// It does not wait for an ack.
func (s *Service) RemovePushToken(ctx context.Context, deviceID, token string) {
	requestID, err := newRequestID()
	if err != nil {
		s.config.Logger.Error("cannot create request ID", "error", err)
		return
	}
	message := Message{
		Action:    ActionUnregisterPushToken,
		RequestID: requestID,
		IssuedAt:  s.config.Now().Unix(),
		PushToken: token,
		Origin:    OriginBackend,
	}
	if err := s.publish(ctx, deviceID, message); err != nil {
		s.config.Logger.Warn("cannot tell the device to remove a push token",
			"deviceId", deviceID, "token", alert.MaskToken(token), "error", err)
	}
}

func (s *Service) publish(ctx context.Context, deviceID string, message Message) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if err := s.config.Publisher.Publish(ctx, device.Topic(deviceID, device.KindCommand), encoded, commandQoS, false); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.Join(ErrBrokerUnavailable, err)
	}
	return nil
}

func (s *Service) startWaiting(key string) (chan Ack, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.waiting[key]; exists {
		return nil, ErrDuplicateRequest
	}
	answer := make(chan Ack, 1)
	s.waiting[key] = answer
	return answer, nil
}

func (s *Service) stopWaiting(key string) {
	s.mu.Lock()
	delete(s.waiting, key)
	s.mu.Unlock()
}

func waitKey(deviceID, requestID string) string {
	return deviceID + "/" + requestID
}

func newRequestID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "backend-" + hex.EncodeToString(random), nil
}
