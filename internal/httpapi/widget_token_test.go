package httpapi

import (
	"github.com/dericknorlan/beforefire-backend/internal/widgetpush"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWidgetRegistrationRequiresReadKeyAndValidSingleObject(t *testing.T) {
	widgets, _ := widgetpush.Open("", nil, nil)
	handler := NewHandler(Dependencies{WidgetPush: widgets, Readings: &stubReadings{hasData: true}})
	body := `{"installationId":"01234567-89ab-cdef-0123-456789abcdef","pushToken":"` + strings.Repeat("a", 64) + `"}`
	for _, tt := range []struct {
		key, body string
		status    int
	}{
		{"", body, 401}, {"bad-key", body, 401}, {"good-key", body + `{}`, 400}, {"good-key", `{"unexpected":true}`, 400},
		{"good-key", body, 204}, {"good-key", `{"installationId":"01234567-89ab-cdef-0123-456789abcdef","remove":true}`, 204},
	} {
		r := httptest.NewRequest(http.MethodPut, "/v1/devices/DAPUR_A1B2C3/widget-token", strings.NewReader(tt.body))
		r.Header.Set("Authorization", "Bearer "+tt.key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tt.status {
			t.Errorf("status %d, want %d", w.Code, tt.status)
		}
	}
}
