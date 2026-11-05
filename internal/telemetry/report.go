// Package telemetry keeps the latest sensor readings of each device in memory
// and lets the app read them. The sensor unit publishes its readings as a
// retained MQTT message, so the broker gives the latest values again after a
// backend restart. No database is necessary.
package telemetry

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/alert"
)

// Limits for one telemetry message.
const (
	MaxReportBytes        = 4 * 1024
	maxUnavailableSensors = 8
	// A report that claims a time this far in the future has a wrong clock.
	maxClockAhead = 10 * time.Minute
)

var (
	hexHashPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	sensorNamePattern = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
)

// Readings are the sensor values. All fields are optional, because a sensor
// can be unavailable. The names follow FuzzyInput in FuzzyKompor.h where possible.
type Readings struct {
	StoveTemperatureC *float64 `json:"stoveTemperatureC,omitempty"` // MLX90640 hot spot
	IAQ               *float64 `json:"iaq,omitempty"`               // BME688 static IAQ, 0..500
	IAQAccuracy       *int     `json:"iaqAccuracy,omitempty"`       // BSEC accuracy, 0..3
	ECO2PPM           *float64 `json:"eco2Ppm,omitempty"`           // BME688 estimated CO2
	Presence          *bool    `json:"presence,omitempty"`          // LD2410B
	AbsenceDurationS  *int64   `json:"absenceDurationS,omitempty"`
	RoomTemperatureC  *float64 `json:"roomTemperatureC,omitempty"` // BME688
	RoomHumidityRH    *float64 `json:"roomHumidityRH,omitempty"`   // BME688
	PressureHPa       *float64 `json:"pressureHPa,omitempty"`      // BME688
}

// Report is the JSON body that the sensor unit publishes to
// beforefire/<deviceID>/telemetry as a retained message.
type Report struct {
	// Condition is the result of evaluateFuzzy(). It is optional until the firmware calls it.
	Condition alert.Condition `json:"condition,omitempty"`
	Score     *float64        `json:"score,omitempty"`
	// MeasuredAt is Unix seconds. 0 when the board has no clock yet.
	MeasuredAt int64    `json:"measuredAt,omitempty"`
	Readings   Readings `json:"readings"`
	// UnavailableSensors names sensors with a fault, for example ["thermal"].
	UnavailableSensors []string `json:"unavailableSensors,omitempty"`
	// ReadKeyHash is hex(SHA-256(read key)). The backend compares the read key
	// of the app with it. See ReadKey.
	ReadKeyHash string `json:"readKeyHash"`
}

// ValidationError tells which field of a report is not correct.
type ValidationError struct {
	Field   string
	Problem string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("telemetry: %s: %s", e.Field, e.Problem)
}

// ParseReport decodes and checks one telemetry message.
func ParseReport(payload []byte, now time.Time) (Report, error) {
	if len(payload) > MaxReportBytes {
		return Report{}, &ValidationError{Field: "payload", Problem: "too large"}
	}
	var report Report
	if err := json.Unmarshal(payload, &report); err != nil {
		return Report{}, &ValidationError{Field: "payload", Problem: "not valid JSON"}
	}
	if !hexHashPattern.MatchString(report.ReadKeyHash) {
		return Report{}, &ValidationError{Field: "readKeyHash", Problem: "must be 64 lower-case hexadecimal characters"}
	}
	if report.Condition != "" && !isKnownCondition(report.Condition) {
		return Report{}, &ValidationError{Field: "condition", Problem: "must be normal, warning1, warning2, or alarm"}
	}
	if report.MeasuredAt < 0 || time.Unix(report.MeasuredAt, 0).After(now.Add(maxClockAhead)) {
		return Report{}, &ValidationError{Field: "measuredAt", Problem: "is in the future or negative"}
	}
	if len(report.UnavailableSensors) > maxUnavailableSensors {
		return Report{}, &ValidationError{Field: "unavailableSensors", Problem: "has too many entries"}
	}
	for _, name := range report.UnavailableSensors {
		if !sensorNamePattern.MatchString(name) {
			return Report{}, &ValidationError{Field: "unavailableSensors", Problem: "names must be lower-case letters, digits, or '_'"}
		}
	}
	if !report.Readings.areFinite() || (report.Score != nil && !isFinite(*report.Score)) {
		return Report{}, &ValidationError{Field: "readings", Problem: "numbers must be finite"}
	}
	return report, nil
}

func isKnownCondition(condition alert.Condition) bool {
	switch condition {
	case alert.ConditionNormal, alert.ConditionWarning1, alert.ConditionWarning2, alert.ConditionAlarm:
		return true
	}
	return false
}

func (r Readings) areFinite() bool {
	for _, value := range []*float64{r.StoveTemperatureC, r.IAQ, r.ECO2PPM, r.RoomTemperatureC, r.RoomHumidityRH, r.PressureHPa} {
		if value != nil && !isFinite(*value) {
			return false
		}
	}
	return true
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
