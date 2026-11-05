// Package device holds the device identity rules and the MQTT topic layout.
// The rules must agree with the firmware in A09Final-IoT/Bersama/SetupPerangkat.h.
package device

import (
	"regexp"
	"strings"
)

// TopicRoot is the first level of every BeforeFire MQTT topic.
const TopicRoot = "beforefire"

// Topic kinds. A topic has the form beforefire/<deviceID>/<kind>.
const (
	KindAlert     = "alert"
	KindCommand   = "cmd"
	KindAck       = "ack"
	KindStatus    = "status"
	KindTelemetry = "telemetry"
)

// Status payloads that the firmware publishes (online) and sets as its MQTT will (offline).
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
)

// The firmware builds the device ID from the Bluetooth prefix and the last
// 6 hexadecimal digits of the MAC address, for example DAPUR_A1B2C3 for the
// sensor unit and DAPUR_ALM_A1B2C3 for the alarm unit. See susunNamaBLE().
var idPattern = regexp.MustCompile(`^DAPUR_(ALM_)?[0-9A-F]{6}$`)

// IsValidID reports whether id has the format that the firmware generates.
func IsValidID(id string) bool {
	return idPattern.MatchString(id)
}

// Topic returns the topic of one kind for one device.
func Topic(deviceID, kind string) string {
	return TopicRoot + "/" + deviceID + "/" + kind
}

// Filter returns the subscription filter of one kind for all devices.
func Filter(kind string) string {
	return TopicRoot + "/+/" + kind
}

// ParseTopic splits beforefire/<deviceID>/<kind>. It returns ok=false when the
// topic has a different shape or the device ID is not valid.
func ParseTopic(topic string) (deviceID, kind string, ok bool) {
	parts := strings.Split(topic, "/")
	if len(parts) != 3 || parts[0] != TopicRoot || !IsValidID(parts[1]) {
		return "", "", false
	}
	return parts[1], parts[2], true
}
