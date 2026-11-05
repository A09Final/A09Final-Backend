package telemetry

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
)

// readKeyVersion is the first line of the HMAC message. Change it when the format changes.
const readKeyVersion = "beforefire-read-v1"

// ReadKey returns the key that the app sends to read the sensor values:
//
//	readKey = hex(HMAC-SHA256(key = PoP, message = "beforefire-read-v1\n" + deviceId))
//
// The backend does not call it, because it does not know the PoP. It is the
// reference for the app, the firmware, and the tests.
func ReadKey(pop, deviceID string) string {
	mac := hmac.New(sha256.New, []byte(pop))
	mac.Write([]byte(readKeyVersion + "\n" + deviceID))
	return hex.EncodeToString(mac.Sum(nil))
}

// ReadKeyHash returns hex(SHA-256(readKey)). The firmware sends this value in
// each telemetry message, so the backend can check a read key without the PoP.
func ReadKeyHash(readKey string) string {
	sum := sha256.Sum256([]byte(readKey))
	return hex.EncodeToString(sum[:])
}

// matchesReadKey compares in constant time, so the response time does not tell
// how much of a guessed key is correct.
func matchesReadKey(readKey, expectedHash string) bool {
	if readKey == "" || expectedHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(ReadKeyHash(readKey)), []byte(expectedHash)) == 1
}
