// Package mqtt connects the backend to the MQTT broker. It is a small wrapper
// around github.com/eclipse/paho.mqtt.golang, so the other packages do not
// depend on the library directly.
package mqtt

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// ErrNotConnected is returned by Publish when no broker connection is open.
var ErrNotConnected = errors.New("mqtt: not connected to the broker")

// Subscription is one topic filter and its maximum QoS (0 or 1).
type Subscription struct {
	Filter string
	QoS    byte
}

// Will is the message that the broker publishes when the client disconnects unexpectedly.
type Will struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
}

// Message is one application message from the broker.
type Message struct {
	Topic    string
	Payload  []byte
	Retained bool
}

// Options configure a Client. Address, ClientID, and OnMessage are required.
type Options struct {
	Address   string // host:port
	TLSConfig *tls.Config
	ClientID  string
	Username  string
	Password  string
	Will      *Will

	KeepAlive     time.Duration
	Subscriptions []Subscription

	// OnMessage runs in its own goroutine for each message.
	OnMessage          func(Message)
	OnConnectionChange func(isConnected bool)
	Logger             *slog.Logger
}

const (
	defaultKeepAlive = 30 * time.Second
	connectTimeout   = 10 * time.Second
	// Wait between attempts while the broker is not reachable yet (first connection).
	connectRetryInterval = 5 * time.Second
	minReconnectInterval = time.Second
	maxReconnectInterval = 30 * time.Second
	subscribeTimeout     = 10 * time.Second
	disconnectQuiesceMs  = 250
	// paho reports a refused subscription with this return code (MQTT 3.1.1 section 3.9.3).
	subscriptionRefused byte = 0x80
)

// Client keeps one broker connection open and reconnects when it fails.
type Client struct {
	options Options
	paho    paho.Client
}

// paho writes connection problems (for example "Not Authorised" from the
// broker) to package-level loggers that discard everything by default.
var connectPahoLogs sync.Once

func NewClient(options Options) *Client {
	if options.KeepAlive <= 0 {
		options.KeepAlive = defaultKeepAlive
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	connectPahoLogs.Do(func() {
		handler := options.Logger.With("component", "paho").Handler()
		paho.CRITICAL = slog.NewLogLogger(handler, slog.LevelError)
		paho.ERROR = slog.NewLogLogger(handler, slog.LevelError)
	})
	client := &Client{options: options}

	pahoOptions := paho.NewClientOptions().
		AddBroker(brokerURL(options.Address, options.TLSConfig != nil)).
		SetClientID(options.ClientID).
		SetUsername(options.Username).
		SetPassword(options.Password).
		SetKeepAlive(options.KeepAlive).
		SetCleanSession(true).
		SetConnectTimeout(connectTimeout).
		SetAutoReconnect(true).
		// Run retries the first connection itself, so it can log the reason of each failure.
		SetConnectRetry(false).
		SetMaxReconnectInterval(maxReconnectInterval).
		// Handlers can wait for APNs. Do not let one slow message stop the others.
		SetOrderMatters(false).
		SetOnConnectHandler(client.subscribe).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			client.options.Logger.Warn("mqtt connection lost, reconnecting", "error", err)
			client.notifyConnection(false)
		})
	if options.TLSConfig != nil {
		pahoOptions.SetTLSConfig(options.TLSConfig)
	}
	if will := options.Will; will != nil {
		pahoOptions.SetBinaryWill(will.Topic, will.Payload, will.QoS, will.Retain)
	}
	client.paho = paho.NewClient(pahoOptions)
	return client
}

// brokerURL turns host:port into the URL form that paho expects.
func brokerURL(address string, useTLS bool) string {
	if useTLS {
		return "ssl://" + address
	}
	return "tcp://" + address
}

// Connected reports whether the broker connection is open now.
func (c *Client) Connected() bool {
	return c.paho.IsConnectionOpen()
}

// Run connects and keeps the connection until ctx ends. It retries the first
// connection and logs why it failed (for example a wrong MQTT_PASSWORD).
// After the first connection, paho reconnects by itself.
func (c *Client) Run(ctx context.Context) {
	defer c.notifyConnection(false)
	for {
		token := c.paho.Connect()
		select {
		case <-token.Done():
		case <-ctx.Done():
			c.paho.Disconnect(disconnectQuiesceMs)
			return
		}
		err := token.Error()
		if err == nil {
			break
		}
		c.options.Logger.Warn("mqtt connect failed, retrying",
			"address", c.options.Address, "error", err, "delay", connectRetryInterval.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(connectRetryInterval):
		}
	}
	<-ctx.Done()
	c.paho.Disconnect(disconnectQuiesceMs)
}

// Publish sends one message. With QoS 1 it waits for the broker PUBACK.
// It fails at once when the connection is not open, so commands do not wait
// in a queue while the broker is unavailable.
func (c *Client) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	if !c.Connected() {
		return ErrNotConnected
	}
	token := c.paho.Publish(topic, qos, retain, payload)
	select {
	case <-token.Done():
		return token.Error()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// subscribe runs after each successful connection. CleanSession is true, so
// the broker forgets the subscriptions when the connection ends.
func (c *Client) subscribe(client paho.Client) {
	c.options.Logger.Info("mqtt connected", "address", c.options.Address)
	if len(c.options.Subscriptions) == 0 {
		c.notifyConnection(true)
		return
	}
	filters := make(map[string]byte, len(c.options.Subscriptions))
	for _, subscription := range c.options.Subscriptions {
		filters[subscription.Filter] = subscription.QoS
	}
	token := client.SubscribeMultiple(filters, c.deliver)
	if !token.WaitTimeout(subscribeTimeout) {
		c.options.Logger.Error("mqtt subscribe timed out")
		return
	}
	if err := checkSubscription(token); err != nil {
		c.options.Logger.Error("mqtt subscribe failed", "error", err)
		return
	}
	c.notifyConnection(true)
}

func checkSubscription(token paho.Token) error {
	if err := token.Error(); err != nil {
		return err
	}
	subscribeToken, ok := token.(*paho.SubscribeToken)
	if !ok {
		return nil
	}
	for filter, code := range subscribeToken.Result() {
		if code == subscriptionRefused {
			return fmt.Errorf("broker refused subscription %q (check the ACL)", filter)
		}
	}
	return nil
}

func (c *Client) deliver(_ paho.Client, message paho.Message) {
	c.options.OnMessage(Message{Topic: message.Topic(), Payload: message.Payload(), Retained: message.Retained()})
}

func (c *Client) notifyConnection(isConnected bool) {
	if c.options.OnConnectionChange != nil {
		c.options.OnConnectionChange(isConnected)
	}
}
