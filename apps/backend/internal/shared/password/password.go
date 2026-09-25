// Package password hashes user passwords with Argon2id and verifies both
// Argon2id and legacy bcrypt hashes, so accounts created before the switch
// keep working and are upgraded on their next successful login.
//
// Hashing is bounded process-wide: at most Params.MaxConcurrent hashes run
// at once, and callers beyond that wait for a slot or for their context to
// end. Argon2id holds Params.MemoryKiB for every hash in flight and login is
// unauthenticated, so without the bound a burst of logins is a burst of
// allocations the container's memory limit has to absorb — peak hashing
// memory is MaxConcurrent × MemoryKiB. The bound lives here rather than on
// an injected value because it has to be one budget shared by every caller
// in the process (login, register, reset, admin reauth).
package password

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

// ErrMismatch is returned by Verify when the password does not match.
var ErrMismatch = errors.New("password: mismatch")

// Params configures Argon2id and the process-wide hashing concurrency.
type Params struct {
	MemoryKiB     uint32
	Iterations    uint32
	Parallelism   uint8
	MaxConcurrent int
}

// DefaultParams is OWASP's m=19 MiB, t=2, p=1 profile, four hashes at a
// time: ~76 MiB peak, well inside the API pod's 512 MiB limit.
var DefaultParams = Params{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 1, MaxConcurrent: 4}

// The weakest profile OWASP's Password Storage Cheat Sheet lists is
// m=7 MiB, t=5; every listed profile has memory×iterations ≥ 35840 KiB.
// Anything below both floors is weaker than all of them.
const (
	minMemoryKiB            = 7 * 1024
	minMemoryPassKiB        = 35840
	maxMemoryKiB            = 1024 * 1024 // 1 GiB: catches a value given in bytes, not KiB
	saltLen                 = 16
	keyLen           uint32 = 32
)

// Validate reports whether p is at least as strong as the weakest
// OWASP-listed Argon2id profile and otherwise usable.
func (p Params) Validate() error {
	switch {
	case p.Iterations < 1 || p.Parallelism < 1 || p.MaxConcurrent < 1:
		return errors.New("iterations, parallelism and max concurrency must all be at least 1")
	case p.MemoryKiB < minMemoryKiB:
		return fmt.Errorf("memory must be at least %d KiB", minMemoryKiB)
	case p.MemoryKiB > maxMemoryKiB:
		return fmt.Errorf("memory must be at most %d KiB (is it in bytes?)", maxMemoryKiB)
	case uint64(p.MemoryKiB)*uint64(p.Iterations) < minMemoryPassKiB:
		return fmt.Errorf("memory × iterations must be at least %d KiB (e.g. 19456 KiB × 2)", minMemoryPassKiB)
	}
	return nil
}

type hasher struct {
	params Params
	slots  chan struct{}
}

var current atomic.Pointer[hasher]

func init() {
	current.Store(newHasher(DefaultParams))
}

func newHasher(p Params) *hasher {
	return &hasher{params: p, slots: make(chan struct{}, p.MaxConcurrent)}
}

// Configure replaces the process-wide parameters. Call it once at startup,
// before serving. Hashes already in flight finish under the old parameters
// and release their slot to the old budget. Existing hashes made under
// other Argon2id parameters keep verifying and report needsRehash, so a
// change migrates users on their next login.
func Configure(p Params) error {
	if err := p.Validate(); err != nil {
		return err
	}
	current.Store(newHasher(p))
	return nil
}

func (h *hasher) acquire(ctx context.Context) error {
	// select picks at random when both cases are ready, so without this a
	// request whose client is already gone could still start a hash.
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case h.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *hasher) release() { <-h.slots }

// bcryptMaxLen is bcrypt's input limit. Go's bcrypt errors past it where
// bcryptjs (which produced the legacy hashes) silently truncated, so legacy
// verification must truncate identically or a long password never matches.
const bcryptMaxLen = 72

var b64 = base64.RawStdEncoding

// Hash returns an Argon2id hash of password in PHC string format. It
// returns ctx's error if ctx ends while waiting for a hashing slot.
func Hash(ctx context.Context, password string) (string, error) {
	h := current.Load()
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	return h.hash(password)
}

func (h *hasher) hash(password string) (string, error) {
	p := h.params
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify reports whether password matches encoded. It returns ErrMismatch
// on a wrong password, ctx's error if ctx ends while waiting for a hashing
// slot, and a different error for a malformed hash. needsRehash is true on
// a match whose hash is bcrypt or uses Argon2id parameters other than the
// configured ones — the caller should then store a fresh Hash(password).
func Verify(ctx context.Context, encoded, password string) (needsRehash bool, err error) {
	h := current.Load()
	if isBcrypt(encoded) {
		if err := h.acquire(ctx); err != nil {
			return false, err
		}
		defer h.release()
		return true, verifyBcrypt(encoded, password)
	}

	stored, salt, want, err := decodeArgon2id(encoded)
	if err != nil {
		return false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()

	got := argon2.IDKey([]byte(password), salt, stored.Iterations, stored.MemoryKiB, stored.Parallelism, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, ErrMismatch
	}
	p := h.params
	upToDate := stored.MemoryKiB == p.MemoryKiB && stored.Iterations == p.Iterations &&
		stored.Parallelism == p.Parallelism && len(salt) == saltLen && uint32(len(want)) == keyLen
	return !upToDate, nil
}

// dummySalt only has to be the right length: DummyVerify's output is
// discarded, so nothing about it needs to be secret or unique.
var dummySalt = make([]byte, saltLen)

// DummyVerify spends the same work as Verify against a current-parameter
// hash, for the path where there is no hash to check against (an unknown
// email). Without it, "no such user" answers measurably faster than "wrong
// password", and login becomes an account-enumeration oracle. It takes a
// hashing slot like any real verify — skipping the queue would itself be a
// timing difference under load.
func DummyVerify(ctx context.Context, password string) error {
	h := current.Load()
	if err := h.acquire(ctx); err != nil {
		return err
	}
	defer h.release()
	p := h.params
	argon2.IDKey([]byte(password), dummySalt, p.Iterations, p.MemoryKiB, p.Parallelism, keyLen)
	return nil
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

func decodeArgon2id(encoded string) (Params, []byte, []byte, error) {
	var p Params
	// "", "argon2id", "v=19", "m=…,t=…,p=…", salt, key
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, errors.New("password: unrecognized hash format")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, errors.New("password: unsupported argon2 version")
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Iterations, &p.Parallelism); err != nil {
		return p, nil, nil, errors.New("password: malformed argon2 parameters")
	}
	// The upper bound keeps a stored hash from costing more memory per slot
	// than any configuration is allowed to, which is what the concurrency
	// budget assumes.
	if p.MemoryKiB == 0 || p.MemoryKiB > maxMemoryKiB || p.Iterations == 0 || p.Parallelism == 0 {
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
