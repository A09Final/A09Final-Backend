package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dericknorlan/beforefire-backend/internal/liveactivity"
)

func TestLiveActivityRegistrationRequiresTheSensorReadKey(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	activities, err := liveactivity.Open("", nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Dependencies{LiveActivities: activities, Readings: &stubReadings{hasData: true}})
	path := "/v1/devices/DAPUR_A1B2C3/live-activity-token"
	registration := `{"installationId":"01234567-89ab-cdef-0123-456789abcdef","startToken":"` + strings.Repeat("a", 64) + `"}`
	for _, test := range []struct {
		key, body string
		status    int
	}{
		{"", registration, http.StatusUnauthorized},
		{"bad-key", registration, http.StatusUnauthorized},
		{"good-key", registration + "{}", http.StatusBadRequest},
		{"good-key", registration, http.StatusNoContent},
		{"good-key", `{"installationId":"01234567-89ab-cdef-0123-456789abcdef","remove":true}`, http.StatusNoContent},
	} {
		request := httptest.NewRequest(http.MethodPut, path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer "+test.key)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != test.status {
			t.Errorf("status %d, want %d", recorder.Code, test.status)
		}
	}
}
