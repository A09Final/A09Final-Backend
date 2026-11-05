package telemetry

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	testDeviceID = "DAPUR_A1B2C3"
	testPoP      = "abcd2345"
	// Checked with:
	//   printf 'beforefire-read-v1\nDAPUR_A1B2C3' | openssl dgst -sha256 -hmac abcd2345
	//   printf '%s' <readKey> | openssl dgst -sha256
	wantReadKey     = "6055c941c040745007ad78c6cad28078f0e92dd4aaf06598a5e8a7446fb2b876"
	wantReadKeyHash = "6b3f6671d12855f3048da9f9f26b67077a7e6ef568512789204146eb902e1b6b"
)

var testNow = time.Unix(1_730_700_000, 0)

func validPayload() string {
	return `{"condition":"normal","score":12.5,"measuredAt":1730699990,"readKeyHash":"` + wantReadKeyHash + `",` +
		`"readings":{"stoveTemperatureC":80,"iaq":45,"iaqAccuracy":3,"presence":true,"roomTemperatureC":30,` +
		`"roomHumidityRH":60,"pressureHPa":1013.2},"unavailableSensors":["thermal"]}`
}

func TestReadKeyMatchesReferenceVector(t *testing.T) {
	if got := ReadKey(testPoP, testDeviceID); got != wantReadKey {
		t.Fatalf("ReadKey = %s", got)
	}
	if got := ReadKeyHash(wantReadKey); got != wantReadKeyHash {
		t.Fatalf("ReadKeyHash = %s", got)
	}
}

func TestParseReport(t *testing.T) {
	report, err := ParseReport([]byte(validPayload()), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.Condition != "normal" || *report.Readings.PressureHPa != 1013.2 || !*report.Readings.Presence ||
		report.UnavailableSensors[0] != "thermal" {
		t.Fatalf("report = %+v", report)
	}
	minimal := `{"readKeyHash":"` + wantReadKeyHash + `","readings":{}}`
	if _, err := ParseReport([]byte(minimal), testNow); err != nil {
		t.Fatalf("minimal report: %v", err)
	}
}

func TestParseReportRejectsBadInput(t *testing.T) {
	hash := `"readKeyHash":"` + wantReadKeyHash + `"`
	cases := map[string]string{
		"invalid JSON":       `{`,
		"missing hash":       `{"readings":{}}`,
		"upper-case hash":    `{"readKeyHash":"` + strings.ToUpper(wantReadKeyHash) + `"}`,
		"unknown condition":  `{` + hash + `,"condition":"fire"}`,
		"future time":        `{` + hash + `,"measuredAt":1830700000}`,
		"bad sensor name":    `{` + hash + `,"unavailableSensors":["Thermal Camera"]}`,
		"too many sensors":   `{` + hash + `,"unavailableSensors":["a","b","c","d","e","f","g","h","i"]}`,
		"too large":          `{` + hash + `,"x":"` + strings.Repeat("a", MaxReportBytes) + `"}`,
		"string temperature": `{` + hash + `,"readings":{"roomTemperatureC":"hot"}}`,
	}
	for name, payload := range cases {
		var validationErr *ValidationError
		if _, err := ParseReport([]byte(payload), testNow); !errors.As(err, &validationErr) {
			t.Errorf("%s: err = %v, want ValidationError", name, err)
		}
	}
}

func TestStoreReadChecksKey(t *testing.T) {
	store := NewStore()
	store.now = func() time.Time { return testNow }

	if _, err := store.Read(testDeviceID, wantReadKey); !errors.Is(err, ErrNoReadings) {
		t.Fatalf("before first message: err = %v", err)
	}
	if err := store.Record(testDeviceID, []byte(validPayload())); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Read(testDeviceID, wantReadKey)
	if err != nil || !snapshot.ReceivedAt.Equal(testNow) || *snapshot.Report.Readings.IAQ != 45 {
		t.Fatalf("snapshot %+v, err %v", snapshot, err)
	}
	if !snapshot.MeasuredTime().Equal(time.Unix(1730699990, 0)) {
		t.Fatalf("measured time = %v", snapshot.MeasuredTime())
	}
	for _, key := range []string{"", ReadKey("wrongpop", testDeviceID), wantReadKeyHash} {
		if _, err := store.Read(testDeviceID, key); !errors.Is(err, ErrInvalidReadKey) {
			t.Errorf("key %q: err = %v", key, err)
		}
	}

	if err := store.Record(testDeviceID, []byte(`{`)); err == nil {
		t.Fatal("invalid payload was accepted")
	}
	if _, err := store.Read(testDeviceID, wantReadKey); err != nil {
		t.Fatal("invalid payload removed the last good snapshot")
	}
	if err := store.Record(testDeviceID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(testDeviceID, wantReadKey); !errors.Is(err, ErrNoReadings) {
		t.Fatalf("after empty payload: err = %v", err)
	}
}

func TestMeasuredTimeFallsBackToReceiveTime(t *testing.T) {
	snapshot := Snapshot{ReceivedAt: testNow}
	if !snapshot.MeasuredTime().Equal(testNow) {
		t.Fatalf("measured time = %v", snapshot.MeasuredTime())
	}
}
