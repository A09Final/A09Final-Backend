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
	OnAlarm                func(context.Context, string, Report)
	SuppressAlarmToken     func(string, string) bool
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
		mu        sync.Mutex
		result    DeliveryResult
		group     sync.WaitGroup
		badTokens []string // rejected with BadDeviceToken, decided after all sends
	)
	if report.Condition == ConditionAlarm && r.config.OnAlarm != nil {
		group.Add(1)
		go func() {
			defer group.Done()
			r.config.OnAlarm(ctx, deviceID, report)
		}()
	}
	for _, token := range report.PushTokens {
		if report.Condition == ConditionAlarm && r.config.SuppressAlarmToken != nil && r.config.SuppressAlarmToken(deviceID, token) {
			continue
		}
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
				r.removeToken(ctx, deviceID, token)
			case errors.As(err, &apnsErr) && apnsErr.IsBadDeviceToken():
				badTokens = append(badTokens, token)
			default:
				result.Failed++
				logger.Error("notification failed", "token", MaskToken(token), "error", err)
			}
		}()
	}
	group.Wait()

	// APNs accepted another token of this report, so APNS_ENVIRONMENT is
	// correct and the rejected tokens are old or from the other environment,
	// for example from an Xcode build when the phones use TestFlight. With a
	// wrong APNS_ENVIRONMENT every token fails, and then no token is removed.
	for _, token := range badTokens {
		if result.Sent > 0 {
			result.Unregistered++
			logger.Info("push token is not valid in this APNs environment, removed from the sensor", "token", MaskToken(token))
			r.removeToken(ctx, deviceID, token)
		} else {
			result.Failed++
			logger.Error("notification failed", "token", MaskToken(token),
				"error", "BadDeviceToken: check that APNS_ENVIRONMENT matches the app build (development for Xcode, production for TestFlight)")
		}
	}
	logger.Info("alert relayed", "sent", result.Sent, "failed", result.Failed, "unregistered", result.Unregistered)
	return result, nil
}

// removeToken asks the sensor unit to forget a push token. It does not wait for
// the answer, so a slow sensor does not delay the other notifications.
func (r *Relay) removeToken(ctx context.Context, deviceID, token string) {
	if r.config.OnUnregisteredToken != nil {
		go r.config.OnUnregisteredToken(context.WithoutCancel(ctx), deviceID, token)
	}
}
