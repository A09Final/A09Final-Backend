package apns

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// APNs endpoints. Use DevelopmentURL for apps that Xcode installs and
// ProductionURL for TestFlight and App Store builds.
const (
	DevelopmentURL = "https://api.sandbox.push.apple.com"
	ProductionURL  = "https://api.push.apple.com"
)

// Push types and priorities from the APNs request headers documentation.
const (
	PushTypeAlert       = "alert"
	PriorityImmediate   = 10
	PriorityPowerSaving = 5
)

const (
	defaultMaxAttempts = 3
	defaultRetryDelay  = time.Second
	requestTimeout     = 15 * time.Second
	maxErrorBodyBytes  = 4096
)

// Notification is one request to APNs.
type Notification struct {
	DeviceToken string
	Payload     []byte
	PushType    string
	Priority    int
	Expiration  time.Time // zero value: APNs tries to deliver one time only
	CollapseID  string
}

// Error is a rejected APNs request.
type Error struct {
	StatusCode int
	Reason     string
}

func (e *Error) Error() string {
	return fmt.Sprintf("apns: status %d: %s", e.StatusCode, e.Reason)
}

// IsUnregistered reports that the device token is no longer valid for the app.
// Only HTTP 410 is used. A 400 BadDeviceToken can also come from a wrong
// APNS_ENVIRONMENT, and tokens must not be removed because of a configuration error.
func (e *Error) IsUnregistered() bool {
	return e.StatusCode == http.StatusGone
}

// Config holds the values from the Apple Developer account.
type Config struct {
	BaseURL     string
	Topic       string // the app bundle ID
	KeyID       string
	TeamID      string
	Key         *ecdsa.PrivateKey
	HTTPClient  *http.Client
	MaxAttempts int
	RetryDelay  time.Duration
}

// Client sends notifications over HTTP/2. It is safe for concurrent use.
type Client struct {
	config Config
	token  *providerToken
}

func NewClient(config Config) (*Client, error) {
	if config.BaseURL == "" || config.Topic == "" || config.KeyID == "" || config.TeamID == "" || config.Key == nil {
		return nil, errors.New("apns: base URL, topic, key ID, team ID, and key are required")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				ForceAttemptHTTP2:   true, // APNs accepts HTTP/2 only
				TLSHandshakeTimeout: 10 * time.Second,
				IdleConnTimeout:     5 * time.Minute,
			},
		}
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = defaultMaxAttempts
	}
	if config.RetryDelay <= 0 {
		config.RetryDelay = defaultRetryDelay
	}
	return &Client{config: config, token: newProviderToken(config.Key, config.KeyID, config.TeamID)}, nil
}

// Send delivers one notification. It retries on rate limits and server errors.
func (c *Client) Send(ctx context.Context, notification Notification) error {
	var lastErr error
	for attempt := 1; attempt <= c.config.MaxAttempts; attempt++ {
		err := c.sendOnce(ctx, notification)
		if err == nil {
			return nil
		}
		lastErr = err
		var apnsErr *Error
		if !errors.As(err, &apnsErr) {
			// Network errors can be temporary. Retry them.
		} else if apnsErr.Reason == "ExpiredProviderToken" {
			if _, renewErr := c.token.renew(); renewErr != nil {
				return renewErr
			}
		} else if !isRetryable(apnsErr.StatusCode) {
			return err
		}
		if attempt == c.config.MaxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.config.RetryDelay * time.Duration(attempt)):
		}
	}
	return lastErr
}

func isRetryable(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError
}

func (c *Client) sendOnce(ctx context.Context, notification Notification) error {
	providerToken, err := c.token.current()
	if err != nil {
		return err
	}
	url := c.config.BaseURL + "/3/device/" + notification.DeviceToken
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(notification.Payload))
	if err != nil {
		return err
	}
	request.Header.Set("authorization", "bearer "+providerToken)
	request.Header.Set("apns-topic", c.config.Topic)
	request.Header.Set("apns-push-type", notification.PushType)
	request.Header.Set("apns-priority", strconv.Itoa(notification.Priority))
	request.Header.Set("apns-expiration", expirationHeader(notification.Expiration))
	if notification.CollapseID != "" {
		request.Header.Set("apns-collapse-id", notification.CollapseID)
	}

	response, err := c.config.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, maxErrorBodyBytes)).Decode(&body)
	return &Error{StatusCode: response.StatusCode, Reason: body.Reason}
}

func expirationHeader(expiration time.Time) string {
	if expiration.IsZero() {
		return "0"
	}
	return strconv.FormatInt(expiration.Unix(), 10)
}
