// Package alert receives detection reports from the sensor unit and sends
// them to the registered phones through APNs.
package alert

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Condition is the output of evaluateFuzzy() in A09Final-IoT/Sensor/FuzzyKompor.cpp.
type Condition string

const (
	ConditionNormal   Condition = "normal"
	ConditionWarning1 Condition = "warning1"
	ConditionWarning2 Condition = "warning2"
	ConditionAlarm    Condition = "alarm"
)

func (c Condition) isKnown() bool {
	switch c {
	case ConditionNormal, ConditionWarning1, ConditionWarning2, ConditionAlarm:
		return true
	}
	return false
}

// Limits for one report. MaxPushTokens must not be lower than the number of
// phone tokens that the firmware keeps in NVS.
const (
	MaxReportBytes     = 8 * 1024
	MaxPushTokens      = 10
	minPushTokenLength = 64
	maxPushTokenLength = 200
	// A report that claims a time this far in the future has a wrong clock.
	maxClockAhead = 10 * time.Minute
)

// APNs uses the alert ID as apns-collapse-id, which has a 64-byte limit.
var alertIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var hexPattern = regexp.MustCompile(`^[0-9a-f]+$`)

// Readings are the optional sensor values at the time of the report.
// The names follow FuzzyInput in FuzzyKompor.h.
type Readings struct {
	StoveTemperatureC *float64 `json:"stoveTemperatureC,omitempty"`
	Presence          *bool    `json:"presence,omitempty"`
	AbsenceDurationS  *int64   `json:"absenceDurationS,omitempty"`
	IAQ               *float64 `json:"iaq,omitempty"`
	RoomTemperatureC  *float64 `json:"roomTemperatureC,omitempty"`
	RoomHumidityRH    *float64 `json:"roomHumidityRH,omitempty"`
}

// Report is the JSON body that the sensor unit publishes to beforefire/<deviceID>/alert.
type Report struct {
	AlertID    string    `json:"alertId"`
	Condition  Condition `json:"condition"`
	Score      *float64  `json:"score,omitempty"`
	IssuedAt   int64     `json:"issuedAt,omitempty"` // Unix seconds. 0 when the board has no clock yet.
	Readings   Readings  `json:"readings"`
	PushTokens []string  `json:"pushTokens"`
}

// ValidationError tells which field of a report is not correct.
type ValidationError struct {
	Field   string
	Problem string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("alert report: %s: %s", e.Field, e.Problem)
}

// ParseReport decodes and checks a report. It changes push tokens to lower
// case, removes duplicate tokens, and sets IssuedAt to now when it is 0.
func ParseReport(payload []byte, now time.Time) (Report, error) {
	if len(payload) > MaxReportBytes {
		return Report{}, &ValidationError{Field: "payload", Problem: "too large"}
	}
	var report Report
	if err := json.Unmarshal(payload, &report); err != nil {
		return Report{}, &ValidationError{Field: "payload", Problem: "not valid JSON"}
	}
	if !alertIDPattern.MatchString(report.AlertID) {
		return Report{}, &ValidationError{Field: "alertId", Problem: "must be 1 to 64 letters, digits, '_' or '-'"}
	}
	if !report.Condition.isKnown() {
		return Report{}, &ValidationError{Field: "condition", Problem: "must be normal, warning1, warning2, or alarm"}
	}
	if report.IssuedAt == 0 {
		report.IssuedAt = now.Unix()
	} else if time.Unix(report.IssuedAt, 0).After(now.Add(maxClockAhead)) {
		return Report{}, &ValidationError{Field: "issuedAt", Problem: "is in the future"}
	}
	tokens, err := normalizePushTokens(report.PushTokens)
	if err != nil {
		return Report{}, err
	}
	report.PushTokens = tokens
	return report, nil
}

func normalizePushTokens(tokens []string) ([]string, error) {
	if len(tokens) == 0 {
		return nil, &ValidationError{Field: "pushTokens", Problem: "must contain at least one token"}
	}
	if len(tokens) > MaxPushTokens {
		return nil, &ValidationError{Field: "pushTokens", Problem: fmt.Sprintf("must contain at most %d tokens", MaxPushTokens)}
	}
	seen := make(map[string]bool, len(tokens))
	normalized := make([]string, 0, len(tokens))
	for _, token := range tokens {
		token = strings.ToLower(token)
		if !IsValidPushToken(token) {
			return nil, &ValidationError{Field: "pushTokens", Problem: "each token must be an APNs device token in hexadecimal"}
		}
		if !seen[token] {
			seen[token] = true
			normalized = append(normalized, token)
		}
	}
	return normalized, nil
}

// IsValidPushToken reports whether token looks like an APNs device token in lower-case hexadecimal.
func IsValidPushToken(token string) bool {
	return len(token) >= minPushTokenLength && len(token) <= maxPushTokenLength &&
		len(token)%2 == 0 && hexPattern.MatchString(token)
}

// MaskToken returns a short form of a push token that is safe for logs.
func MaskToken(token string) string {
	const visibleCharacters = 6
	if len(token) <= visibleCharacters {
		return "***"
	}
	return token[:visibleCharacters] + "…"
}
