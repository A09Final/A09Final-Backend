// Package httpapi is the HTTP interface for the BeforeFire app.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/alert"
	"github.com/dericknorlan/beforefire-backend/internal/command"
	"github.com/dericknorlan/beforefire-backend/internal/device"
	"github.com/dericknorlan/beforefire-backend/internal/telemetry"
)

// CommandExecutor is implemented by *command.Service.
type CommandExecutor interface {
	Execute(ctx context.Context, deviceID string, request command.Request) (command.Ack, error)
}

// ReadingsSource is implemented by *telemetry.Store.
type ReadingsSource interface {
	Read(deviceID, readKey string) (telemetry.Snapshot, error)
}

// Dependencies of the HTTP handlers.
type Dependencies struct {
	Commands        CommandExecutor
	Statuses        command.StatusSource
	Readings        ReadingsSource
	IsMQTTConnected func() bool
	// Readings older than this are marked isStale in the response.
	TelemetryStaleAfter time.Duration
	Logger              *slog.Logger
	Now                 func() time.Time
}

const maxRequestBodyBytes = 4 * 1024

// Error codes in JSON responses. The app can show a message for each code.
const (
	errorInvalidRequest     = "invalid_request"
	errorUnknownDevice      = "unknown_device_id"
	errorCommandRejected    = "command_rejected"
	errorDeviceOffline      = "device_offline"
	errorDeviceNoAnswer     = "device_no_answer"
	errorBrokerUnavailable  = "broker_unavailable"
	errorDuplicateRequest   = "duplicate_request"
	errorInternal           = "internal_error"
	errorRequestCanceled    = "request_canceled"
	errorInvalidReadKey     = "invalid_read_key"
	errorNoReadings         = "no_readings"
	connectionStatusOnline  = "online"
	connectionStatusOffline = "offline"
	connectionStatusUnknown = "unknown"
)

// NewHandler returns the routes:
//
//	GET  /healthz
//	GET  /v1/devices/{deviceId}/status
//	GET  /v1/devices/{deviceId}/readings   (header: Authorization: Bearer <readKey>)
//	POST /v1/devices/{deviceId}/commands
func NewHandler(dependencies Dependencies) http.Handler {
	if dependencies.Logger == nil {
		dependencies.Logger = slog.Default()
	}
	if dependencies.Now == nil {
		dependencies.Now = time.Now
	}
	routes := &routes{Dependencies: dependencies}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", routes.health)
	mux.HandleFunc("GET /v1/devices/{deviceId}/status", routes.deviceStatus)
	mux.HandleFunc("GET /v1/devices/{deviceId}/readings", routes.deviceReadings)
	mux.HandleFunc("POST /v1/devices/{deviceId}/commands", routes.sendCommand)
	return mux
}

type routes struct {
	Dependencies
}

type errorResponse struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func (r *routes) health(writer http.ResponseWriter, _ *http.Request) {
	isConnected := r.IsMQTTConnected()
	status := http.StatusOK
	if !isConnected {
		status = http.StatusServiceUnavailable
	}
	writeJSON(writer, status, map[string]bool{"mqttConnected": isConnected})
}

type statusResponse struct {
	DeviceID   string     `json:"deviceId"`
	Connection string     `json:"connection"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
}

func (r *routes) deviceStatus(writer http.ResponseWriter, request *http.Request) {
	deviceID, ok := deviceIDFrom(writer, request)
	if !ok {
		return
	}
	status := r.Statuses.Get(deviceID)
	response := statusResponse{DeviceID: deviceID, Connection: connectionText(status.Connection)}
	if !status.UpdatedAt.IsZero() {
		updatedAt := status.UpdatedAt.UTC()
		response.UpdatedAt = &updatedAt
	}
	writeJSON(writer, http.StatusOK, response)
}

func connectionText(connection device.Connection) string {
	switch connection {
	case device.ConnectionOnline:
		return connectionStatusOnline
	case device.ConnectionOffline:
		return connectionStatusOffline
	default:
		return connectionStatusUnknown
	}
}

type readingsResponse struct {
	DeviceID           string             `json:"deviceId"`
	Condition          alert.Condition    `json:"condition,omitempty"`
	Score              *float64           `json:"score,omitempty"`
	Readings           telemetry.Readings `json:"readings"`
	UnavailableSensors []string           `json:"unavailableSensors"`
	MeasuredAt         time.Time          `json:"measuredAt"`
	AgeSeconds         int64              `json:"ageSeconds"`
	IsStale            bool               `json:"isStale"`
	Connection         string             `json:"connection"`
}

func (r *routes) deviceReadings(writer http.ResponseWriter, request *http.Request) {
	deviceID, ok := deviceIDFrom(writer, request)
	if !ok {
		return
	}
	snapshot, err := r.Readings.Read(deviceID, bearerToken(request))
	switch {
	case errors.Is(err, telemetry.ErrNoReadings):
		writeJSON(writer, http.StatusNotFound, errorResponse{Error: errorNoReadings, Detail: "the device has not sent readings yet"})
		return
	case errors.Is(err, telemetry.ErrInvalidReadKey):
		writer.Header().Set("WWW-Authenticate", `Bearer realm="beforefire"`)
		writeJSON(writer, http.StatusUnauthorized, errorResponse{Error: errorInvalidReadKey})
		return
	case err != nil:
		r.Logger.Error("readings failed", "deviceId", deviceID, "error", err)
		writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: errorInternal})
		return
	}

	measuredAt := snapshot.MeasuredTime()
	age := max(r.Now().Sub(measuredAt), 0)
	unavailable := snapshot.Report.UnavailableSensors
	if unavailable == nil {
		unavailable = []string{}
	}
	writeJSON(writer, http.StatusOK, readingsResponse{
		DeviceID:           deviceID,
		Condition:          snapshot.Report.Condition,
		Score:              snapshot.Report.Score,
		Readings:           snapshot.Report.Readings,
		UnavailableSensors: unavailable,
		MeasuredAt:         measuredAt.UTC(),
		AgeSeconds:         int64(age / time.Second),
		IsStale:            age > r.TelemetryStaleAfter,
		Connection:         connectionText(r.Statuses.Get(deviceID).Connection),
	})
}

// bearerToken returns the value of "Authorization: Bearer <token>", or "".
func bearerToken(request *http.Request) string {
	const prefix = "bearer "
	header := request.Header.Get("Authorization")
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

func (r *routes) sendCommand(writer http.ResponseWriter, request *http.Request) {
	deviceID, ok := deviceIDFrom(writer, request)
	if !ok {
		return
	}
	var commandRequest command.Request
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxRequestBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&commandRequest); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: errorInvalidRequest, Detail: "body must be one JSON command object"})
		return
	}

	ack, err := r.Commands.Execute(request.Context(), deviceID, commandRequest)
	var validationErr *command.ValidationError
	switch {
	case err == nil && ack.Result == command.ResultDone:
		writeJSON(writer, http.StatusOK, ack)
	case err == nil:
		writeJSON(writer, http.StatusUnprocessableEntity, errorResponse{Error: errorCommandRejected, Detail: ack.Reason})
	case errors.As(err, &validationErr):
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: errorInvalidRequest, Detail: validationErr.Error()})
	case errors.Is(err, command.ErrDuplicateRequest):
		writeJSON(writer, http.StatusConflict, errorResponse{Error: errorDuplicateRequest})
	case errors.Is(err, command.ErrDeviceOffline):
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: errorDeviceOffline})
	case errors.Is(err, command.ErrBrokerUnavailable):
		r.Logger.Error("command not sent", "deviceId", deviceID, "error", err)
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: errorBrokerUnavailable})
	case errors.Is(err, command.ErrNoAnswer):
		writeJSON(writer, http.StatusGatewayTimeout, errorResponse{Error: errorDeviceNoAnswer})
	case errors.Is(err, context.Canceled):
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Error: errorRequestCanceled})
	default:
		r.Logger.Error("command failed", "deviceId", deviceID, "error", err)
		writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: errorInternal})
	}
}

func deviceIDFrom(writer http.ResponseWriter, request *http.Request) (string, bool) {
	deviceID := request.PathValue("deviceId")
	if !device.IsValidID(deviceID) {
		writeJSON(writer, http.StatusNotFound, errorResponse{Error: errorUnknownDevice, Detail: "expected a sticker serial such as DAPUR_A1B2C3"})
		return "", false
	}
	return deviceID, true
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
