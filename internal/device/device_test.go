package device

import (
	"testing"
	"time"
)

func TestIsValidID(t *testing.T) {
	cases := map[string]bool{
		"DAPUR_A1B2C3":     true,
		"DAPUR_ALM_A1B2C3": true,
		"DAPUR_a1b2c3":     false, // firmware prints %06X, upper case
		"DAPUR_A1B2C":      false,
		"DAPUR_A1B2C3D":    false,
		"DAPUR_ALM_":       false,
		"":                 false,
		"DAPUR_A1B2C3/x":   false,
		"DAPUR_+":          false,
	}
	for id, want := range cases {
		if got := IsValidID(id); got != want {
			t.Errorf("IsValidID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestParseTopic(t *testing.T) {
	id, kind, ok := ParseTopic("beforefire/DAPUR_A1B2C3/alert")
	if !ok || id != "DAPUR_A1B2C3" || kind != KindAlert {
		t.Fatalf("got %q %q %v", id, kind, ok)
	}
	for _, topic := range []string{
		"beforefire/DAPUR_A1B2C3",
		"beforefire/DAPUR_A1B2C3/alert/extra",
		"other/DAPUR_A1B2C3/alert",
		"beforefire/bad/alert",
	} {
		if _, _, ok := ParseTopic(topic); ok {
			t.Errorf("ParseTopic(%q) accepted a bad topic", topic)
		}
	}
	if got := Topic("DAPUR_A1B2C3", KindCommand); got != "beforefire/DAPUR_A1B2C3/cmd" {
		t.Errorf("Topic = %q", got)
	}
	if got := Filter(KindAck); got != "beforefire/+/ack" {
		t.Errorf("Filter = %q", got)
	}
}

func TestStatusTracker(t *testing.T) {
	tracker := NewStatusTracker()
	fixed := time.Unix(1_700_000_000, 0)
	tracker.now = func() time.Time { return fixed }

	if got := tracker.Get("DAPUR_A1B2C3").Connection; got != ConnectionUnknown {
		t.Fatalf("new device connection = %v, want unknown", got)
	}
	tracker.Record("DAPUR_A1B2C3", []byte("online"))
	if got := tracker.Get("DAPUR_A1B2C3"); got.Connection != ConnectionOnline || !got.UpdatedAt.Equal(fixed) {
		t.Fatalf("after online: %+v", got)
	}
	tracker.Record("DAPUR_A1B2C3", []byte("garbage"))
	if got := tracker.Get("DAPUR_A1B2C3").Connection; got != ConnectionOnline {
		t.Fatalf("unknown payload changed the status to %v", got)
	}
	tracker.Record("DAPUR_A1B2C3", []byte("offline"))
	if got := tracker.Get("DAPUR_A1B2C3").Connection; got != ConnectionOffline {
		t.Fatalf("after offline: %v", got)
	}
	tracker.Record("DAPUR_A1B2C3", nil)
	if got := tracker.Get("DAPUR_A1B2C3"); got.Connection != ConnectionUnknown || !got.UpdatedAt.IsZero() {
		t.Fatalf("after empty payload: %+v", got)
	}
}
