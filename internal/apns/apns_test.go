package apns

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestParsePrivateKey(t *testing.T) {
	key := newTestKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	parsed, err := ParsePrivateKey(pemBytes)
	if err != nil || !parsed.Equal(key) {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	if _, err := ParsePrivateKey([]byte("not a key")); err == nil {
		t.Fatal("expected an error for text without PEM")
	}
}

func TestProviderTokenIsValidES256AndCached(t *testing.T) {
	key := newTestKey(t)
	token := newProviderToken(key, "KEY1234567", "TEAM123456")
	now := time.Unix(1_700_000_000, 0)
	token.now = func() time.Time { return now }

	first, err := token.current()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(first, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	var header map[string]string
	var claims map[string]any
	decodeSegment(t, parts[0], &header)
	decodeSegment(t, parts[1], &claims)
	if header["alg"] != "ES256" || header["kid"] != "KEY1234567" {
		t.Fatalf("header = %v", header)
	}
	if claims["iss"] != "TEAM123456" || claims["iat"] != float64(now.Unix()) {
		t.Fatalf("claims = %v", claims)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 64 {
		t.Fatalf("signature length %d, err %v", len(signature), err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])
	if !ecdsa.Verify(&key.PublicKey, digest[:], r, s) {
		t.Fatal("signature does not verify with the public key")
	}

	now = now.Add(providerTokenLifetime - time.Second)
	if again, _ := token.current(); again != first {
		t.Fatal("token changed before its lifetime ended")
	}
	now = now.Add(2 * time.Second)
	if renewed, _ := token.current(); renewed == first {
		t.Fatal("token did not change after its lifetime ended")
	}
}

func decodeSegment(t *testing.T, segment string, target any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}

type recordedRequest struct {
	path    string
	headers http.Header
	body    string
	proto   int
}

// newTestAPNs starts an HTTP/2 TLS server. reply chooses the status and reason for each attempt.
func newTestAPNs(t *testing.T, reply func(attempt int) (int, string)) (*Client, *[]recordedRequest) {
	t.Helper()
	var requests []recordedRequest
	var attempts atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, recordedRequest{r.URL.Path, r.Header.Clone(), string(body), r.ProtoMajor})
		status, reason := reply(int(attempts.Add(1)))
		w.WriteHeader(status)
		if reason != "" {
			_ = json.NewEncoder(w).Encode(map[string]string{"reason": reason})
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	client, err := NewClient(Config{
		BaseURL:    server.URL,
		Topic:      "com.dericknorlan.BeforeFire",
		KeyID:      "KEY1234567",
		TeamID:     "TEAM123456",
		Key:        newTestKey(t),
		HTTPClient: server.Client(),
		RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, &requests
}

func TestSendSetsAPNsHeaders(t *testing.T) {
	client, requests := newTestAPNs(t, func(int) (int, string) { return http.StatusOK, "" })
	expiration := time.Unix(1_700_000_900, 0)
	err := client.Send(context.Background(), Notification{
		DeviceToken: "abc123",
		Payload:     []byte(`{"aps":{}}`),
		PushType:    PushTypeAlert,
		Priority:    PriorityImmediate,
		Expiration:  expiration,
		CollapseID:  "alert-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := (*requests)[0]
	if request.proto != 2 {
		t.Errorf("protocol HTTP/%d, want HTTP/2", request.proto)
	}
	if request.path != "/3/device/abc123" || request.body != `{"aps":{}}` {
		t.Errorf("path %q body %q", request.path, request.body)
	}
	expected := map[string]string{
		"Apns-Topic":       "com.dericknorlan.BeforeFire",
		"Apns-Push-Type":   "alert",
		"Apns-Priority":    "10",
		"Apns-Expiration":  "1700000900",
		"Apns-Collapse-Id": "alert-1",
	}
	for name, value := range expected {
		if got := request.headers.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	if !strings.HasPrefix(request.headers.Get("Authorization"), "bearer ") {
		t.Errorf("authorization header = %q", request.headers.Get("Authorization"))
	}
}

func TestSendUsesLiveActivityTopic(t *testing.T) {
	client, requests := newTestAPNs(t, func(int) (int, string) { return http.StatusOK, "" })
	err := client.Send(context.Background(), Notification{
		DeviceToken: "activity-token", PushType: PushTypeLiveActivity, Priority: PriorityImmediate,
	})
	if err != nil {
		t.Fatal(err)
	}
	headers := (*requests)[0].headers
	if headers.Get("Apns-Topic") != "com.dericknorlan.BeforeFire.push-type.liveactivity" ||
		headers.Get("Apns-Push-Type") != "liveactivity" {
		t.Errorf("wrong headers for ActivityKit")
	}
}

func TestSendReportsUnregisteredWithoutRetry(t *testing.T) {
	client, requests := newTestAPNs(t, func(int) (int, string) { return http.StatusGone, "Unregistered" })
	err := client.Send(context.Background(), Notification{DeviceToken: "abc", PushType: PushTypeAlert, Priority: 10})
	var apnsErr *Error
	if !errors.As(err, &apnsErr) || !apnsErr.IsUnregistered() || apnsErr.Reason != "Unregistered" {
		t.Fatalf("err = %v", err)
	}
	if len(*requests) != 1 {
		t.Fatalf("sent %d requests, want 1", len(*requests))
	}
}

func TestSendDoesNotTreatBadDeviceTokenAsUnregistered(t *testing.T) {
	client, _ := newTestAPNs(t, func(int) (int, string) { return http.StatusBadRequest, "BadDeviceToken" })
	err := client.Send(context.Background(), Notification{DeviceToken: "abc", PushType: PushTypeAlert, Priority: 10})
	var apnsErr *Error
	if !errors.As(err, &apnsErr) || apnsErr.IsUnregistered() {
		t.Fatalf("err = %v; BadDeviceToken must not remove tokens", err)
	}
}

func TestSendRetriesServerErrors(t *testing.T) {
	client, requests := newTestAPNs(t, func(attempt int) (int, string) {
		if attempt < 3 {
			return http.StatusServiceUnavailable, "ServiceUnavailable"
		}
		return http.StatusOK, ""
	})
	if err := client.Send(context.Background(), Notification{DeviceToken: "abc", PushType: PushTypeAlert, Priority: 10}); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 3 {
		t.Fatalf("sent %d requests, want 3", len(*requests))
	}
}

func TestSendRenewsExpiredProviderToken(t *testing.T) {
	client, requests := newTestAPNs(t, func(attempt int) (int, string) {
		if attempt == 1 {
			return http.StatusForbidden, "ExpiredProviderToken"
		}
		return http.StatusOK, ""
	})
	client.token.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	if err := client.Send(context.Background(), Notification{DeviceToken: "abc", PushType: PushTypeAlert, Priority: 10}); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 {
		t.Fatalf("sent %d requests, want 2", len(*requests))
	}
}
