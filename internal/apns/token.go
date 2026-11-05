// Package apns sends notifications to the Apple Push Notification service
// (APNs) with token-based authentication (a .p8 key from the Apple Developer account).
package apns

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Apple accepts a provider token for one hour and rejects new tokens that
// arrive more often than every 20 minutes. Renew between those limits.
const providerTokenLifetime = 40 * time.Minute

// ParsePrivateKey reads the PEM content of an APNs .p8 key (PKCS #8, P-256).
func ParsePrivateKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("apns: key file has no PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apns: key is not PKCS #8: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("apns: key is not an ECDSA P-256 key")
	}
	return key, nil
}

// providerToken creates and caches the ES256 JSON Web Token for APNs.
type providerToken struct {
	key    *ecdsa.PrivateKey
	keyID  string
	teamID string
	now    func() time.Time

	mu        sync.Mutex
	value     string
	createdAt time.Time
}

func newProviderToken(key *ecdsa.PrivateKey, keyID, teamID string) *providerToken {
	return &providerToken{key: key, keyID: keyID, teamID: teamID, now: time.Now}
}

// current returns the cached token, or a new token when the cached token is old.
func (t *providerToken) current() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.value != "" && t.now().Sub(t.createdAt) < providerTokenLifetime {
		return t.value, nil
	}
	return t.renewLocked()
}

// renew discards the cached token. APNs asks for this with ExpiredProviderToken.
func (t *providerToken) renew() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.renewLocked()
}

func (t *providerToken) renewLocked() (string, error) {
	issuedAt := t.now()
	header, err := json.Marshal(map[string]string{"alg": "ES256", "kid": t.keyID})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{"iss": t.teamID, "iat": issuedAt.Unix()})
	if err != nil {
		return "", err
	}
	encoding := base64.RawURLEncoding
	signingInput := encoding.EncodeToString(header) + "." + encoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, t.key, digest[:])
	if err != nil {
		return "", err
	}
	// JWS ES256 uses the fixed 64-byte R || S form, not ASN.1 DER (RFC 7518 section 3.4).
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])

	t.value = signingInput + "." + encoding.EncodeToString(signature)
	t.createdAt = issuedAt
	return t.value, nil
}
