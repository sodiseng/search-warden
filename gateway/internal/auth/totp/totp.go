// Package totp implements RFC 6238 time-based one-time passwords
// (SHA-1, 6 digits, 30 s period — what every authenticator app supports).
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	digits = 6
	period = 30
	skew   = 1 // accept the previous and next step for clock drift
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret returns a new 160-bit secret in base32 (32 characters).
func GenerateSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b32.EncodeToString(b), nil
}

func decode(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.ReplaceAll(strings.TrimRight(secret, "="), " ", ""))
	key, err := b32.DecodeString(s)
	if err != nil || len(key) < 10 {
		return nil, errors.New("invalid TOTP secret")
	}
	return key, nil
}

func hotp(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", digits, v%1_000_000)
}

func counterAt(t time.Time) uint64 { return uint64(t.Unix() / period) }

// Code returns the code valid at t.
func Code(secret string, t time.Time) (string, error) {
	key, err := decode(secret)
	if err != nil {
		return "", err
	}
	return hotp(key, counterAt(t)), nil
}

// Verify checks code at time t, accepting ±1 step. Codes at or below
// lastUsed are rejected so a code cannot be replayed; on success the matched
// counter is returned for the caller to store as the new lastUsed.
func Verify(secret, code string, t time.Time, lastUsed uint64) (uint64, bool) {
	key, err := decode(secret)
	if err != nil {
		return 0, false
	}
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != digits {
		return 0, false
	}
	now := counterAt(t)
	for d := -skew; d <= skew; d++ {
		c := uint64(int64(now) + int64(d))
		if c <= lastUsed {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(hotp(key, c)), []byte(code)) == 1 {
			return c, true
		}
	}
	return 0, false
}

// URI builds the otpauth:// link that authenticator apps import (usually via QR code).
func URI(secret, account, issuer string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(digits))
	q.Set("period", fmt.Sprint(period))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
