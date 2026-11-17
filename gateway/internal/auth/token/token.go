// Package token handles downstream access tokens (ARCHITECTURE.md §11.2).
//
// Generated format: "sw_" followed by 32 lowercase letters or digits, e.g.
// sw_9f2c4a7b1e5d8c3a6f0b4d2e7a1c5b98. Lowercase keeps tokens easy to read
// aloud or paste from a terminal, and the "sw_" prefix makes leaked tokens
// recognizable by secret scanners. Only SHA-256 hashes are stored.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"unicode"
)

// Prefix marks generated tokens.
const Prefix = "sw_"

// MinCustomLength is the minimum length of a hand-written token in tokens.yaml.
const MinCustomLength = 20

const (
	alphabet  = "0123456789abcdefghijklmnopqrstuvwxyz" // lowercase only
	bodyLen   = 32
	displayN  = 12
	maxLength = 256
)

// Generate returns a new random token: "sw_" + 32 lowercase alphanumeric chars.
func Generate() (string, error) {
	body := make([]byte, bodyLen)
	max := big.NewInt(int64(len(alphabet)))
	for i := range body {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		body[i] = alphabet[n.Int64()]
	}
	return Prefix + string(body), nil
}

// isLowerAlnum reports whether s consists of lowercase letters and digits only.
func isLowerAlnum(s string) bool {
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return s != ""
}

// Hash returns the hex SHA-256 used to store and look up tokens.
func Hash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// IsHash reports whether s looks like a value produced by Hash.
func IsHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// DisplayPrefix is the non-secret part shown in the panel (e.g. "sw_9f2c4a7b").
func DisplayPrefix(tok string) string {
	n := displayN
	if !strings.HasPrefix(tok, Prefix) {
		n = 6
	}
	if len(tok) <= n {
		return tok[:len(tok)/2]
	}
	return tok[:n]
}

// Validate checks a token supplied by a client or written in tokens.yaml.
// A generated token is "sw_" plus exactly 32 lowercase letters or digits;
// a custom token must be long enough and free of whitespace/control characters.
func Validate(tok string) error {
	if len(tok) > maxLength {
		return fmt.Errorf("token is too long (max %d bytes)", maxLength)
	}
	for _, r := range tok {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("token must not contain whitespace or control characters")
		}
	}
	if strings.HasPrefix(tok, Prefix) {
		rest := tok[len(Prefix):]
		if len(rest) != bodyLen || !isLowerAlnum(rest) {
			// A "sw_" prefix marks a generated token, so a malformed one is an error
			// rather than a custom token: it is almost certainly a typo or a truncation.
			return errors.New("generated tokens are \"sw_\" plus 32 lowercase letters or digits")
		}
		return nil
	}
	if len([]rune(tok)) < MinCustomLength {
		return fmt.Errorf("token must be at least %d characters", MinCustomLength)
	}
	return nil
}
