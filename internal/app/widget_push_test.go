package app

import (
	"context"
	"github.com/dericknorlan/beforefire-backend/internal/apns"
	"github.com/dericknorlan/beforefire-backend/internal/config"
	"github.com/dericknorlan/beforefire-backend/internal/widgetpush"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type widgetTestSender chan apns.Notification

func (s widgetTestSender) Send(_ context.Context, n apns.Notification) error { s <- n; return nil }
func TestWidgetWorkerObservesLatestTelemetryWithoutOpeningTheApp(t *testing.T) {
	output := make(widgetTestSender, 8)
	application := New(config.Config{TelemetryStaleAfter: time.Minute}, output, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := application.widgetPush.Register("DAPUR_A1B2C3", widgetpush.Registration{InstallationID: "01234567-89ab-cdef-0123-456789abcdef", PushToken: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	application.statuses.Record("DAPUR_A1B2C3", []byte("online"))
	body := []byte(`{"condition":"warning1","readKeyHash":"` + strings.Repeat("a", 64) + `","readings":{}}`)
	if err := application.readings.Record("DAPUR_A1B2C3", body); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { application.runWidgetRefreshes(ctx); close(done) }()
	select {
	case push := <-output:
		if push.PushType != apns.PushTypeWidgets {
			t.Fatal("wrong push type")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no widget push")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}
