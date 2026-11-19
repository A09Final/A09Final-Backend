package alert

import (
	"encoding/json"
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

// User-facing push text for each condition.
const (
	titleNormal   = "Kitchen back to normal"
	bodyNormal    = "Readings are back to normal."
	titleWarning1 = "Check your kitchen"
	bodyWarning1  = "Check your kitchen.\nCheck the stove and nearby area."
	titleWarning2 = "Check the stove now"
	bodyWarning2  = "Check the stove NOW.\nTurn it off if safe."
	titleAlarm    = "Stove alarm"
	bodyAlarm     = AlarmNotificationBody
	soundName     = "default"
)

// AlarmNotificationBody is shared with the ActivityKit start alert.
const AlarmNotificationBody = "Turn stove off NOW if safe.\nIf not, call someone nearby."

// BeforeFire includes this file in its app bundle for Critical Alerts.
const criticalAlarmSoundName = "BeforeFireCriticalSiren.caf"

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
		return content{titleWarning2, bodyWarning2, InterruptionCritical, 0.8, true, 10}
	case ConditionWarning1:
		return content{titleWarning1, bodyWarning1, InterruptionCritical, 0.5, true, 10}
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
			name := soundName
			if report.Condition == ConditionAlarm {
				name = criticalAlarmSoundName
			}
			notification.APS.Sound = criticalSound{Critical: 1, Name: name, Volume: 1.0}
		} else {
			notification.APS.Sound = soundName
		}
	}
	encoded, err := json.Marshal(notification)
	return encoded, visible.priority, err
}

// AlarmReminderPayload reuses the alarm text and the full-volume critical siren.
func AlarmReminderPayload(deviceID string, report Report) ([]byte, error) {
	// The original readings may no longer describe the kitchen.
	report.Readings = Readings{}
	report.Condition = ConditionAlarm
	encoded, _, err := buildPayload(deviceID, report, InterruptionCritical)
	return encoded, err
}
