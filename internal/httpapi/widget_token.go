package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/dericknorlan/beforefire-backend/internal/telemetry"
	"github.com/dericknorlan/beforefire-backend/internal/widgetpush"
)

func (r *routes) registerWidgetToken(writer http.ResponseWriter, request *http.Request) {
	deviceID, ok := deviceIDFrom(writer, request)
	if !ok {
		return
	}
	if r.WidgetPush == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: "widget_push_unavailable"})
		return
	}
	if _, err := r.Readings.Read(deviceID, bearerToken(request)); err != nil {
		switch {
		case errors.Is(err, telemetry.ErrNoReadings):
			writeJSON(writer, http.StatusNotFound, errorResponse{Error: errorNoReadings})
		case errors.Is(err, telemetry.ErrInvalidReadKey):
			writer.Header().Set("WWW-Authenticate", `Bearer realm="beforefire"`)
			writeJSON(writer, http.StatusUnauthorized, errorResponse{Error: errorInvalidReadKey})
		default:
			writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: errorInternal})
		}
		return
	}
	var registration widgetpush.Registration
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxRequestBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&registration); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: errorInvalidRequest})
		return
	}
	if err := r.WidgetPush.Register(deviceID, registration); err != nil {
		if errors.Is(err, widgetpush.ErrRegistration) {
			writeJSON(writer, http.StatusBadRequest, errorResponse{Error: errorInvalidRequest})
		} else {
			r.Logger.Error("cannot save widget token", "deviceId", deviceID, "error", err)
			writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: errorInternal})
		}
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}
