// Package password hashes admin passwords with argon2id in PHC string format
// (ARCHITECTURE.md §11.1): $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// MinLength is the minimum password length in characters.
const MinLength = 12

const (
	memoryKiB = 64 * 1024
	iters     = 3
	threads   = 2
	keyLen    = 32
	saltLen   = 16
)

var b64 = base64.RawStdEncoding

// Validate checks the password policy without hashing.
func Validate(plain string) error {
	if n := utf8.RuneCountInString(plain); n < MinLength {
		return fmt.Errorf("password must be at least %d characters (got %d)", MinLength, n)
	}
	if len(plain) > 1024 {
		return errors.New("password is too long (max 1024 bytes)")
	}
	return nil
}

// Hash returns a PHC-formatted argon2id hash with a random salt.
func Hash(plain string) (string, error) {
	if err := Validate(plain); err != nil {
		return "", err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := argon2.IDKey([]byte(plain), salt, iters, memoryKiB, threads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memoryKiB, iters, threads, b64.EncodeToString(salt), b64.EncodeToString(sum)), nil
}

type params struct {
	m          uint32
	t          uint32
	p          uint8
	salt, hash []byte
}

// parse validates a PHC argon2id string. Parameter bounds stop a tampered
// config from turning every login into a memory/CPU bomb.
func parse(phc string) (params, error) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return params{}, errors.New("not an argon2id PHC string")
	}
	if parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return params{}, fmt.Errorf("unsupported argon2 version %q", parts[2])
	}
	var p params
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return params{}, errors.New("malformed argon2 parameters")
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return params{}, errors.New("malformed argon2 parameters")
		}
		switch k {
		case "m":
			p.m = uint32(n)
		case "t":
			p.t = uint32(n)
		case "p":
			if n > 255 {
				return params{}, errors.New("argon2 parallelism out of range")
			}
			p.p = uint8(n)
		default:
			return params{}, fmt.Errorf("unknown argon2 parameter %q", k)
		}
	}
	if p.m < 8*1024 || p.m > 1024*1024 || p.t < 1 || p.t > 16 || p.p < 1 || p.p > 16 {
		return params{}, errors.New("argon2 parameters out of the accepted range")
	}
	var err error
	if p.salt, err = b64.DecodeString(parts[4]); err != nil || len(p.salt) < 8 {
		return params{}, errors.New("malformed argon2 salt")
	}
	if p.hash, err = b64.DecodeString(parts[5]); err != nil || len(p.hash) < 16 || len(p.hash) > 64 {
		return params{}, errors.New("malformed argon2 hash")
	}
	return p, nil
}

// IsHash reports whether s is a well-formed, acceptable argon2id PHC string.
func IsHash(s string) bool {
	_, err := parse(s)
	return err == nil
}

// Verify compares plain against a PHC hash in constant time.
func Verify(phc, plain string) (bool, error) {
	p, err := parse(phc)
	if err != nil {
		return false, err
	}
	sum := argon2.IDKey([]byte(plain), p.salt, p.t, p.m, p.p, uint32(len(p.hash)))
	return subtle.ConstantTimeCompare(sum, p.hash) == 1, nil
}
