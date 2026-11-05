package apns

import (
	"context"
	"log/slog"
)

// DryRunSender writes notifications to the log and sends nothing. Use it
// (APNS_DRY_RUN=true) to test the MQTT path before the APNs key is ready.
type DryRunSender struct {
	Logger *slog.Logger
}

func (s DryRunSender) Send(_ context.Context, notification Notification) error {
	tokenPrefix := notification.DeviceToken
	if len(tokenPrefix) > 6 {
		tokenPrefix = tokenPrefix[:6] + "…"
	}
	s.Logger.Info("APNs dry run: notification not sent",
		"token", tokenPrefix, "priority", notification.Priority, "payload", string(notification.Payload))
	return nil
}
