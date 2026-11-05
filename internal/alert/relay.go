package alert

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/apns"
)

// Sender delivers one APNs notification. *apns.Client and the dry-run sender implement it.
type Sender interface {
	Send(ctx context.Context, notification apns.Notification) error
}

// UnregisteredTokenHandler is called when APNs reports that a token is no
// longer valid, so the sensor unit can remove it from its NVS.
type UnregisteredTokenHandler func(ctx context.Context, deviceID, token string)

// RelayConfig configures a Relay.
type RelayConfig struct {
	Sender                 Sender
	Deduplicator           *Deduplicator
	Expiry                 time.Duration // after this time APNs stops trying to deliver
	AlarmInterruptionLevel string
	OnUnregisteredToken    UnregisteredTokenHandler
	Logger                 *slog.Logger
	Now                    func() time.Time
}

// Relay turns one alert report into one APNs notification for each push token.
// It stores nothing except recent alert IDs in memory.
type Relay struct {
	config RelayConfig
}

func NewRelay(config RelayConfig) *Relay {
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.AlarmInterruptionLevel == "" {
		config.AlarmInterruptionLevel = InterruptionTimeSensitive
	}
	return &Relay{config: config}
}

// DeliveryResult counts the outcome of one report.
type DeliveryResult struct {
	Sent         int
	Failed       int
	Unregistered int
	Skipped      bool // duplicate or expired report
}

// Handle processes one MQTT payload from beforefire/<deviceID>/alert.
func (r *Relay) Handle(ctx context.Context, deviceID string, payload []byte) (DeliveryResult, error) {
	now := r.config.Now()
	report, err := ParseReport(payload, now)
	if err != nil {
		return DeliveryResult{}, err
	}
	logger := r.config.Logger.With("deviceId", deviceID, "alertId", report.AlertID, "condition", report.Condition)

	expiresAt := time.Unix(report.IssuedAt, 0).Add(r.config.Expiry)
	if !now.Before(expiresAt) {
		logger.Warn("alert report is too old, notification skipped", "issuedAt", report.IssuedAt)
		return DeliveryResult{Skipped: true}, nil
	}
	if !r.config.Deduplicator.FirstTime(deviceID + "/" + report.AlertID) {
		logger.Info("duplicate alert report ignored")
		return DeliveryResult{Skipped: true}, nil
	}

	encoded, priority, err := buildPayload(deviceID, report, r.config.AlarmInterruptionLevel)
	if err != nil {
		return DeliveryResult{}, err
	}

	var (
		mu     sync.Mutex
		result DeliveryResult
		group  sync.WaitGroup
	)
	for _, token := range report.PushTokens {
		group.Add(1)
		go func() {
			defer group.Done()
			err := r.config.Sender.Send(ctx, apns.Notification{
				DeviceToken: token,
				Payload:     encoded,
				PushType:    apns.PushTypeAlert,
				Priority:    priority,
				Expiration:  expiresAt,
				CollapseID:  report.AlertID,
			})
			mu.Lock()
			defer mu.Unlock()
			var apnsErr *apns.Error
			switch {
			case err == nil:
				result.Sent++
			case errors.As(err, &apnsErr) && apnsErr.IsUnregistered():
				result.Unregistered++
				logger.Info("push token is no longer registered", "token", MaskToken(token))
				if r.config.OnUnregisteredToken != nil {
					go r.config.OnUnregisteredToken(context.WithoutCancel(ctx), deviceID, token)
				}
			default:
				result.Failed++
				logger.Error("notification failed", "token", MaskToken(token), "error", err)
			}
		}()
	}
	group.Wait()
	logger.Info("alert relayed", "sent", result.Sent, "failed", result.Failed, "unregistered", result.Unregistered)
	return result, nil
}
