// Package auth authenticates the operators of the dashboard and the operational API.
//
// Milestone 3 starts here (handoff §13): the API is bound to localhost and left open by
// default, and once `server.auth.users` is configured every endpoint that answers
// operational state requires a session. Passwords are stored as PBKDF2-HMAC-SHA256
// hashes, never as plaintext, and sessions are stateless signed cookies so a service
// restart does not sign everyone out.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PBKDF2 parameters. The iteration count follows the OWASP recommendation for
// PBKDF2-HMAC-SHA256 (600k was for SHA-1; 210k is the SHA-256 figure) and is stored in
// each hash, so raising it later does not invalidate the hashes already on disk.
const (
	hashScheme     = "pbkdf2-sha256"
	hashIterations = 210_000
	saltBytes      = 16
	keyBytes       = 32
)

// HashPassword returns a self-describing hash: scheme, iterations, salt and key, in the
// form `pbkdf2-sha256$210000$<salt>$<key>`. Nothing else about the password is stored.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("password must not be empty")
	}
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, hashIterations, keyBytes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", hashScheme, hashIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the encoded hash. A malformed hash
// verifies nothing rather than panicking on a configuration mistake.
func VerifyPassword(encoded, password string) bool {
	salt, want, iterations, err := parseHash(encoded)
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ValidateHash checks the encoding of a configured hash without needing the password,
// so `gohorizon validate` catches a truncated paste instead of leaving an operator with
// an account that can never sign in.
func ValidateHash(encoded string) error {
	_, _, _, err := parseHash(encoded)
	return err
}

func parseHash(encoded string) (salt, key []byte, iterations int, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 {
		return nil, nil, 0, errors.New("password_hash must be pbkdf2-sha256$<iterations>$<salt>$<key>")
	}
	if parts[0] != hashScheme {
		return nil, nil, 0, fmt.Errorf("password_hash scheme %q is not supported (supported: %s)", parts[0], hashScheme)
	}
	iterations, err = strconv.Atoi(parts[1])
	if err != nil || iterations < 1 || iterations > 10_000_000 {
		return nil, nil, 0, fmt.Errorf("password_hash iterations %q are not a usable count", parts[1])
	}
	salt, err = base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) < saltBytes {
		return nil, nil, 0, errors.New("password_hash salt is not base64 of at least 16 bytes")
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(key) < keyBytes {
		return nil, nil, 0, errors.New("password_hash key is not base64 of at least 32 bytes")
	}
	return salt, key, iterations, nil
}
