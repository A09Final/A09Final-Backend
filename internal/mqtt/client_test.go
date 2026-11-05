package mqtt

import (
	"context"
	"errors"
	"testing"
)

func TestBrokerURL(t *testing.T) {
	if got := brokerURL("mosquitto:1883", false); got != "tcp://mosquitto:1883" {
		t.Errorf("plain URL = %q", got)
	}
	if got := brokerURL("api.example.com:8883", true); got != "ssl://api.example.com:8883" {
		t.Errorf("TLS URL = %q", got)
	}
}

func TestPublishFailsFastWithoutConnection(t *testing.T) {
	client := NewClient(Options{Address: "127.0.0.1:1", ClientID: "test", OnMessage: func(Message) {}})
	err := client.Publish(context.Background(), "beforefire/DAPUR_A1B2C3/cmd", []byte("{}"), 1, false)
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
	if client.Connected() {
		t.Fatal("client reports a connection that it never opened")
	}
}
