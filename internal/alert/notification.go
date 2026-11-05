package alert

import (
	"encoding/json"
	"fmt"
	"math"
)

// Interruption levels from the APNs payload reference. "time-sensitive"
// needs the Time Sensitive Notifications capability in the app. "critical"
// needs an entitlement that Apple must approve.
const (
	InterruptionPassive       = "passive"
	InterruptionActive        = "active"
	InterruptionTimeSensitive = "time-sensitive"
	InterruptionCritical      = "critical"
)

// IsValidInterruptionLevel reports whether level is a value that APNs accepts.
func IsValidInterruptionLevel(level string) bool {
	switch level {
	case InterruptionPassive, InterruptionActive, InterruptionTimeSensitive, InterruptionCritical:
		return true
	}
	return false
}

// User-facing text. "Check your kitchen now" is the same title that the app
// uses in AlarmActivityAttributes.
const (
	titleNormal   = "Kitchen readings are normal"
	bodyNormal    = "The readings are in the normal range again."
	titleWarning1 = "Check your kitchen"
	bodyWarning1  = "The stove is on. Make sure that somebody watches it."
	titleWarning2 = "Kitchen warning"
	bodyWarning2  = "The stove is hot and nobody is near it. Check the kitchen soon."
	titleAlarm    = "Check your kitchen now"
	bodyAlarm     = "Danger near the stove. Go to the kitchen now."
	stoveDetail   = " Stove: %d °C."
	soundName     = "default"
)

// content is the visible part of a notification for one condition.
type content struct {
	title             string
	body              string
	interruptionLevel string
	relevanceScore    float64
	hasSound          bool
	priority          int
}

func contentFor(condition Condition, alarmInterruptionLevel string) content {
	switch condition {
	case ConditionAlarm:
		return content{titleAlarm, bodyAlarm, alarmInterruptionLevel, 1.0, true, 10}
	case ConditionWarning2:
		return content{titleWarning2, bodyWarning2, InterruptionTimeSensitive, 0.8, true, 10}
	case ConditionWarning1:
		return content{titleWarning1, bodyWarning1, InterruptionActive, 0.5, true, 10}
	default:
		return content{titleNormal, bodyNormal, InterruptionPassive, 0.1, false, 5}
	}
}

type apsAlert struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type criticalSound struct {
	Critical int     `json:"critical"`
	Name     string  `json:"name"`
	Volume   float64 `json:"volume"`
}

type aps struct {
	Alert             apsAlert `json:"alert"`
	Sound             any      `json:"sound,omitempty"`
	ThreadID          string   `json:"thread-id"`
	InterruptionLevel string   `json:"interruption-level"`
	RelevanceScore    float64  `json:"relevance-score"`
}

// AppData is the custom part of the payload. The app reads it from
// userInfo["beforefire"] to open the alert details.
type AppData struct {
	DeviceID  string    `json:"deviceId"`
	AlertID   string    `json:"alertId"`
	Condition Condition `json:"condition"`
	Score     *float64  `json:"score,omitempty"`
	IssuedAt  int64     `json:"issuedAt"`
}

type payload struct {
	APS        aps     `json:"aps"`
	BeforeFire AppData `json:"beforefire"`
}

// buildPayload returns the APNs JSON payload and the APNs priority.
func buildPayload(deviceID string, report Report, alarmInterruptionLevel string) ([]byte, int, error) {
	visible := contentFor(report.Condition, alarmInterruptionLevel)
	body := visible.body
	if stove := report.Readings.StoveTemperatureC; stove != nil && report.Condition != ConditionNormal &&
		!math.IsNaN(*stove) && !math.IsInf(*stove, 0) {
		body += fmt.Sprintf(stoveDetail, int(math.Round(*stove)))
	}

	notification := payload{
		APS: aps{
			Alert:             apsAlert{Title: visible.title, Body: body},
			ThreadID:          deviceID,
			InterruptionLevel: visible.interruptionLevel,
			RelevanceScore:    visible.relevanceScore,
		},
		BeforeFire: AppData{
			DeviceID:  deviceID,
			AlertID:   report.AlertID,
			Condition: report.Condition,
			Score:     report.Score,
			IssuedAt:  report.IssuedAt,
		},
	}
	if visible.hasSound {
		if visible.interruptionLevel == InterruptionCritical {
			notification.APS.Sound = criticalSound{Critical: 1, Name: soundName, Volume: 1.0}
		} else {
			notification.APS.Sound = soundName
		}
	}
	encoded, err := json.Marshal(notification)
	return encoded, visible.priority, err
}
