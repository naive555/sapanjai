// Package password hashes user passwords with Argon2id and verifies both
// Argon2id and legacy bcrypt hashes, so accounts created before the switch
// keep working and are upgraded on their next successful login.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

// ErrMismatch is returned by Verify when the password does not match.
var ErrMismatch = errors.New("password: mismatch")

// OWASP's minimum Argon2id profile (m=19 MiB, t=2, p=1). Memory is the
// binding constraint: every concurrent hash holds `memoryKiB` for its
// duration, and login is unauthenticated.
const (
	memoryKiB   uint32 = 19 * 1024
	iterations  uint32 = 2
	parallelism uint8  = 1
	saltLen            = 16
	keyLen      uint32 = 32
)

// bcryptMaxLen is bcrypt's input limit. Go's bcrypt errors past it where
// bcryptjs (which produced the legacy hashes) silently truncated, so legacy
// verification must truncate identically or a long password never matches.
const bcryptMaxLen = 72

var b64 = base64.RawStdEncoding

// Hash returns an Argon2id hash of password in PHC string format.
func Hash(password string) (string, error) {
	return hashWith(password, memoryKiB, iterations, parallelism)
}

func hashWith(password string, m, t uint32, p uint8) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, t, m, p, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, m, t, p, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify reports whether password matches encoded. It returns ErrMismatch
// on a wrong password and a different error for a malformed hash.
// needsRehash is true on a match whose hash is bcrypt or uses Argon2id
// parameters other than the current ones — the caller should then store
// a fresh Hash(password).
func Verify(encoded, password string) (needsRehash bool, err error) {
	if isBcrypt(encoded) {
		return true, verifyBcrypt(encoded, password)
	}

	p, salt, want, err := decodeArgon2id(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.iterations, p.memoryKiB, p.parallelism, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, ErrMismatch
	}
	current := p.memoryKiB == memoryKiB && p.iterations == iterations &&
		p.parallelism == parallelism && len(salt) == saltLen && uint32(len(want)) == keyLen
	return !current, nil
}

func isBcrypt(encoded string) bool {
	return strings.HasPrefix(encoded, "$2a$") ||
		strings.HasPrefix(encoded, "$2b$") ||
		strings.HasPrefix(encoded, "$2y$")
}

func verifyBcrypt(encoded, password string) error {
	b := []byte(password)
	if len(b) > bcryptMaxLen {
		b = b[:bcryptMaxLen]
	}
	err := bcrypt.CompareHashAndPassword([]byte(encoded), b)
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return ErrMismatch
	}
	return err
}

type argon2Params struct {
	memoryKiB   uint32
	iterations  uint32
	parallelism uint8
}

func decodeArgon2id(encoded string) (argon2Params, []byte, []byte, error) {
	var p argon2Params
	// "", "argon2id", "v=19", "m=…,t=…,p=…", salt, key
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, errors.New("password: unrecognized hash format")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, errors.New("password: unsupported argon2 version")
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memoryKiB, &p.iterations, &p.parallelism); err != nil {
		return p, nil, nil, errors.New("password: malformed argon2 parameters")
	}
	if p.memoryKiB == 0 || p.iterations == 0 || p.parallelism == 0 {
		return p, nil, nil, errors.New("password: malformed argon2 parameters")
	}

	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return p, nil, nil, errors.New("password: malformed argon2 salt")
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return p, nil, nil, errors.New("password: malformed argon2 key")
	}
	return p, salt, key, nil
}
