package command

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dericknorlan/beforefire-backend/internal/device"
)

const (
	testDeviceID = "DAPUR_A1B2C3"
	testPoP      = "abcd2345"
	testToken    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

var testNow = time.Unix(1_730_700_000, 0)

func signedRequest(action Action, requestID, pushToken string) Request {
	request := Request{Action: action, RequestID: requestID, IssuedAt: testNow.Unix(), PushToken: pushToken}
	request.Signature = Sign(testPoP, testDeviceID, request)
	return request
}

// The app and the firmware must produce this exact value. README.md shows the same
// vector. It was checked with: openssl dgst -sha256 -hmac abcd2345.
func TestSignMatchesReferenceVector(t *testing.T) {
	request := Request{Action: ActionSilence, RequestID: "3f1c2b7e-0000-4000-8000-000000000001", IssuedAt: 1730700000}
	wantText := "beforefire-cmd-v1\nDAPUR_A1B2C3\nsilence\n3f1c2b7e-0000-4000-8000-000000000001\n1730700000\n"
	if got := SigningString(testDeviceID, request); got != wantText {
		t.Fatalf("signing string = %q", got)
	}
	const wantSignature = "c3543e3f62513b35540d7f523867b05129d7281707750f6f043e74728d9f499c"
	if got := Sign(testPoP, testDeviceID, request); got != wantSignature {
		t.Fatalf("signature = %s", got)
	}
}

func TestValidate(t *testing.T) {
	skew := 5 * time.Minute
	valid := signedRequest(ActionSilence, "request-0001", "")
	if err := valid.Validate(testNow, skew); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	register := signedRequest(ActionRegisterPushToken, "request-0002", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if err := register.Validate(testNow, skew); err != nil || register.PushToken != testToken {
		t.Fatalf("register: %v, token %q", err, register.PushToken)
	}

	cases := map[string]func(*Request){
		"unknown action":       func(r *Request) { r.Action = "explode" },
		"short request ID":     func(r *Request) { r.RequestID = "abc" },
		"bad request ID chars": func(r *Request) { r.RequestID = "request/0001" },
		"old time":             func(r *Request) { r.IssuedAt = testNow.Add(-6 * time.Minute).Unix() },
		"future time":          func(r *Request) { r.IssuedAt = testNow.Add(6 * time.Minute).Unix() },
		"missing time":         func(r *Request) { r.IssuedAt = 0 },
		"token with silence":   func(r *Request) { r.PushToken = testToken },
		"short signature":      func(r *Request) { r.Signature = "abc" },
		"upper-case signature": func(r *Request) { r.Signature = "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789" },
	}
	for name, change := range cases {
		request := valid
		change(&request)
		var validationErr *ValidationError
		if err := request.Validate(testNow, skew); !errors.As(err, &validationErr) {
			t.Errorf("%s: err = %v, want ValidationError", name, err)
		}
	}
	missingToken := signedRequest(ActionRegisterPushToken, "request-0003", "")
	if err := missingToken.Validate(testNow, skew); err == nil {
		t.Error("register without token: expected an error")
	}
}

// fakeDevice answers each published command like the sensor unit would.
type fakeDevice struct {
	mu         sync.Mutex
	published  []Message
	topics     []string
	service    *Service
	answer     func(Message) *Ack
	publishErr error
}

func (d *fakeDevice) Publish(_ context.Context, topic string, payload []byte, qos byte, retain bool) error {
	if d.publishErr != nil {
		return d.publishErr
	}
	var message Message
	if err := json.Unmarshal(payload, &message); err != nil {
		return err
	}
	d.mu.Lock()
	d.published = append(d.published, message)
	d.topics = append(d.topics, topic)
	d.mu.Unlock()
	if d.answer != nil {
		if ack := d.answer(message); ack != nil {
			encoded, _ := json.Marshal(ack)
			go d.service.HandleAck(testDeviceID, encoded)
		}
	}
	return nil
}

type fixedStatus device.Status

func (s fixedStatus) Get(string) device.Status { return device.Status(s) }

func newTestService(publisher *fakeDevice, status device.Connection) *Service {
	service := NewService(ServiceConfig{
		Publisher:        publisher,
		Statuses:         fixedStatus{Connection: status},
		AnswerTimeout:    100 * time.Millisecond,
		AllowedClockSkew: 5 * time.Minute,
		Now:              func() time.Time { return testNow },
	})
	publisher.service = service
	return service
}

func TestExecuteReturnsDeviceAck(t *testing.T) {
	publisher := &fakeDevice{answer: func(message Message) *Ack {
		return &Ack{RequestID: message.RequestID, Result: ResultDone}
	}}
	service := newTestService(publisher, device.ConnectionOnline)
	ack, err := service.Execute(context.Background(), testDeviceID, signedRequest(ActionSilence, "request-0001", ""))
	if err != nil || ack.Result != ResultDone {
		t.Fatalf("ack %+v, err %v", ack, err)
	}
	message := publisher.published[0]
	if publisher.topics[0] != "beforefire/DAPUR_A1B2C3/cmd" || message.Origin != OriginApp || message.Signature == "" {
		t.Fatalf("published %s %+v", publisher.topics[0], message)
	}
}

func TestExecuteReturnsRejection(t *testing.T) {
	publisher := &fakeDevice{answer: func(message Message) *Ack {
		return &Ack{RequestID: message.RequestID, Result: ResultRejected, Reason: "bad_signature"}
	}}
	service := newTestService(publisher, device.ConnectionUnknown)
	ack, err := service.Execute(context.Background(), testDeviceID, signedRequest(ActionSilence, "request-0001", ""))
	if err != nil || ack.Result != ResultRejected || ack.Reason != "bad_signature" {
		t.Fatalf("ack %+v, err %v", ack, err)
	}
}

func TestExecuteTimesOutWithoutAck(t *testing.T) {
	service := newTestService(&fakeDevice{}, device.ConnectionOnline)
	_, err := service.Execute(context.Background(), testDeviceID, signedRequest(ActionSilence, "request-0001", ""))
	if !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("err = %v", err)
	}
}

func TestExecuteIgnoresAckForOtherRequest(t *testing.T) {
	publisher := &fakeDevice{answer: func(Message) *Ack {
		return &Ack{RequestID: "other-request", Result: ResultDone}
	}}
	service := newTestService(publisher, device.ConnectionOnline)
	if _, err := service.Execute(context.Background(), testDeviceID, signedRequest(ActionSilence, "request-0001", "")); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("err = %v", err)
	}
}

func TestExecuteFailsFastWhenOffline(t *testing.T) {
	publisher := &fakeDevice{}
	service := newTestService(publisher, device.ConnectionOffline)
	if _, err := service.Execute(context.Background(), testDeviceID, signedRequest(ActionSilence, "request-0001", "")); !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("err = %v", err)
	}
	if len(publisher.published) != 0 {
		t.Fatal("command was published to an offline device")
	}
}

func TestExecuteReportsBrokerFailure(t *testing.T) {
	service := newTestService(&fakeDevice{publishErr: errors.New("not connected")}, device.ConnectionOnline)
	if _, err := service.Execute(context.Background(), testDeviceID, signedRequest(ActionSilence, "request-0001", "")); !errors.Is(err, ErrBrokerUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestExecuteRejectsDuplicateRequestInProgress(t *testing.T) {
	service := newTestService(&fakeDevice{}, device.ConnectionOnline)
	request := signedRequest(ActionSilence, "request-0001", "")
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.Execute(context.Background(), testDeviceID, request)
		firstDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if _, err := service.Execute(context.Background(), testDeviceID, request); !errors.Is(err, ErrDuplicateRequest) {
		t.Fatalf("second call err = %v", err)
	}
	<-firstDone
}

func TestRemovePushTokenPublishesBackendCommand(t *testing.T) {
	publisher := &fakeDevice{}
	service := newTestService(publisher, device.ConnectionOnline)
	service.RemovePushToken(context.Background(), testDeviceID, testToken)
	message := publisher.published[0]
	if message.Action != ActionUnregisterPushToken || message.Origin != OriginBackend ||
		message.PushToken != testToken || message.Signature != "" || message.IssuedAt != testNow.Unix() {
		t.Fatalf("message = %+v", message)
	}
}
