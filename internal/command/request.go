// Package command forwards user commands from the app to the sensor unit and
// waits for the answer of the unit.
//
// The backend does not store the PoP of a board, so it cannot check who sends
// a command. The app signs each command with the PoP from the device sticker,
// and the sensor unit checks the signature with the PoP in its NVS. The
// backend checks only the format and the time window.
package command

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/alert"
)

// Action is a command for the sensor unit.
type Action string

const (
	// ActionSilence asks the sensor unit to stop the alarm unit.
	ActionSilence Action = "silence"
	// ActionRegisterPushToken asks the sensor unit to keep a phone token in NVS.
	ActionRegisterPushToken Action = "register_push_token"
	// ActionUnregisterPushToken asks the sensor unit to remove a phone token from NVS.
	ActionUnregisterPushToken Action = "unregister_push_token"
)

func (a Action) needsPushToken() bool {
	return a == ActionRegisterPushToken || a == ActionUnregisterPushToken
}

func (a Action) isKnown() bool {
	return a == ActionSilence || a.needsPushToken()
}

// Origin tells the sensor unit how to check a command.
type Origin string

const (
	// OriginApp commands carry a signature. The sensor unit must check it.
	OriginApp Origin = "app"
	// OriginBackend commands have no signature. Only the backend MQTT user can
	// publish to the cmd topic (see the ACL), and the backend uses this origin
	// only for unregister_push_token after APNs rejects a token.
	OriginBackend Origin = "backend"
)

// signingVersion is the first line of the signed text. Change it when the format changes.
const signingVersion = "beforefire-cmd-v1"

var (
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)
	signaturePattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Request is the JSON body of POST /v1/devices/{deviceId}/commands.
type Request struct {
	Action    Action `json:"action"`
	RequestID string `json:"requestId"`
	IssuedAt  int64  `json:"issuedAt"` // Unix seconds on the phone
	PushToken string `json:"pushToken,omitempty"`
	Signature string `json:"signature"` // lower-case hex HMAC-SHA256, see SigningString
}

// ValidationError tells which field of a request is not correct.
type ValidationError struct {
	Field   string
	Problem string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("command: %s: %s", e.Field, e.Problem)
}

// Validate checks the format of the request and the time window. It changes
// the push token to lower case. It cannot check the signature.
func (r *Request) Validate(now time.Time, allowedClockSkew time.Duration) error {
	if !r.Action.isKnown() {
		return &ValidationError{Field: "action", Problem: "must be silence, register_push_token, or unregister_push_token"}
	}
	if !requestIDPattern.MatchString(r.RequestID) {
		return &ValidationError{Field: "requestId", Problem: "must be 8 to 64 letters, digits, or '-'"}
	}
	issuedAt := time.Unix(r.IssuedAt, 0)
	if r.IssuedAt <= 0 || issuedAt.Before(now.Add(-allowedClockSkew)) || issuedAt.After(now.Add(allowedClockSkew)) {
		return &ValidationError{Field: "issuedAt", Problem: "is outside the allowed time window; check the phone clock"}
	}
	r.PushToken = strings.ToLower(r.PushToken)
	if r.Action.needsPushToken() && !alert.IsValidPushToken(r.PushToken) {
		return &ValidationError{Field: "pushToken", Problem: "must be an APNs device token in hexadecimal"}
	}
	if !r.Action.needsPushToken() && r.PushToken != "" {
		return &ValidationError{Field: "pushToken", Problem: "is not used by this action"}
	}
	if !signaturePattern.MatchString(r.Signature) {
		return &ValidationError{Field: "signature", Problem: "must be 64 lower-case hexadecimal characters"}
	}
	return nil
}

// SigningString is the text that the app signs and the sensor unit checks:
//
//	beforefire-cmd-v1\n<deviceId>\n<action>\n<requestId>\n<issuedAt>\n<pushToken>
//
// pushToken is an empty string for silence. There is no newline at the end.
func SigningString(deviceID string, request Request) string {
	return strings.Join([]string{
		signingVersion,
		deviceID,
		string(request.Action),
		request.RequestID,
		strconv.FormatInt(request.IssuedAt, 10),
		request.PushToken,
	}, "\n")
}

// Sign returns hex(HMAC-SHA256(key = PoP, message = SigningString)). The
// backend does not call it. It is the reference for the app, the firmware, and the tests.
func Sign(pop, deviceID string, request Request) string {
	mac := hmac.New(sha256.New, []byte(pop))
	mac.Write([]byte(SigningString(deviceID, request)))
	return hex.EncodeToString(mac.Sum(nil))
}

// Message is the JSON body that the backend publishes to beforefire/<deviceID>/cmd.
type Message struct {
	Action    Action `json:"action"`
	RequestID string `json:"requestId"`
	IssuedAt  int64  `json:"issuedAt"`
	PushToken string `json:"pushToken,omitempty"`
	Signature string `json:"signature,omitempty"`
	Origin    Origin `json:"origin"`
}

// Ack results that the sensor unit publishes to beforefire/<deviceID>/ack.
const (
	ResultDone     = "done"
	ResultRejected = "rejected"
)

// Ack is the answer of the sensor unit to one command.
type Ack struct {
	RequestID string `json:"requestId"`
	Result    string `json:"result"`
	Reason    string `json:"reason,omitempty"` // for example bad_signature or expired
}
