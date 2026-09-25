package password

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var ctx = context.Background()

func TestHashVerify_RoundTrip(t *testing.T) {
	h, err := Hash(ctx, "correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected hash format: %s", h)
	}

	needsRehash, err := Verify(ctx, h, "correct horse battery staple")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if needsRehash {
		t.Fatal("a hash with current parameters must not need a rehash")
	}

	if _, err := Verify(ctx, h, "wrong"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("Verify(wrong) err = %v, want ErrMismatch", err)
	}
}

func TestHash_SaltsEachHash(t *testing.T) {
	a, _ := Hash(ctx, "same")
	b, _ := Hash(ctx, "same")
	if a == b {
		t.Fatal("two hashes of the same password must differ")
	}
}

// The point of the migration: bcrypt ignored everything past byte 72.
func TestVerify_Argon2UsesBytesPast72(t *testing.T) {
	long := strings.Repeat("a", 100)
	h, err := Hash(ctx, long)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if _, err := Verify(ctx, h, long[:72]); !errors.Is(err, ErrMismatch) {
		t.Fatalf("72-byte prefix err = %v, want ErrMismatch", err)
	}
	if _, err := Verify(ctx, h, long[:99]+"b"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("last-byte change err = %v, want ErrMismatch", err)
	}
}

func TestVerify_LegacyBcrypt(t *testing.T) {
	legacy, err := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}

	needsRehash, err := Verify(ctx, string(legacy), "password123")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !needsRehash {
		t.Fatal("a matching bcrypt hash must report needsRehash")
	}

	if _, err := Verify(ctx, string(legacy), "nope"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("Verify(wrong) err = %v, want ErrMismatch", err)
	}
}

// Legacy hashes were made by bcryptjs, which silently truncated to 72
// bytes, so the full over-long password must still verify against them.
func TestVerify_LegacyBcryptOver72Bytes(t *testing.T) {
	long := strings.Repeat("x", 100)
	legacy, err := bcrypt.GenerateFromPassword([]byte(long[:72]), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if _, err := Verify(ctx, string(legacy), long); err != nil {
		t.Fatalf("Verify(100-byte password vs truncated legacy hash): %v", err)
	}
}

func TestVerify_OutdatedArgon2ParamsNeedRehash(t *testing.T) {
	// Valid Argon2id, but weaker than the current profile.
	weak := newHasher(Params{MemoryKiB: 8, Iterations: 1, Parallelism: 1, MaxConcurrent: 1})
	encoded, err := weak.hash("pw")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	needsRehash, err := Verify(ctx, encoded, "pw")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !needsRehash {
		t.Fatal("outdated parameters must report needsRehash")
	}
}

func TestVerify_Malformed(t *testing.T) {
	for _, h := range []string{
		"",
		"bcrypt-hash-placeholder",
		"$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$a2V5",
		"$argon2id$v=18$m=19456,t=2,p=1$c2FsdA$a2V5",
		"$argon2id$v=19$m=0,t=2,p=1$c2FsdA$a2V5",
		"$argon2id$v=19$m=2097152,t=2,p=1$c2FsdA$a2V5", // 2 GiB: over the per-slot bound
		"$argon2id$v=19$m=19456,t=2,p=1$!!!$a2V5",
		"$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$",
	} {
		_, err := Verify(ctx, h, "pw")
		if err == nil || errors.Is(err, ErrMismatch) {
			t.Errorf("Verify(%q) err = %v, want a malformed-hash error", h, err)
		}
	}
}

func TestParams_Validate(t *testing.T) {
	tests := []struct {
		name    string
		p       Params
		wantErr bool
	}{
		{"default", DefaultParams, false},
		{"owasp m=46MiB t=1", Params{MemoryKiB: 46 * 1024, Iterations: 1, Parallelism: 1, MaxConcurrent: 1}, false},
		{"owasp m=7MiB t=5", Params{MemoryKiB: 7 * 1024, Iterations: 5, Parallelism: 1, MaxConcurrent: 1}, false},
		{"memory below floor", Params{MemoryKiB: 7*1024 - 1, Iterations: 10, Parallelism: 1, MaxConcurrent: 1}, true},
		{"memory x iterations below floor", Params{MemoryKiB: 19 * 1024, Iterations: 1, Parallelism: 1, MaxConcurrent: 1}, true},
		{"memory given in bytes", Params{MemoryKiB: 19 * 1024 * 1024, Iterations: 2, Parallelism: 1, MaxConcurrent: 1}, true},
		{"zero iterations", Params{MemoryKiB: 64 * 1024, Iterations: 0, Parallelism: 1, MaxConcurrent: 1}, true},
		{"zero parallelism", Params{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 0, MaxConcurrent: 1}, true},
		{"zero concurrency", Params{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 1, MaxConcurrent: 0}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.p.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// configure swaps the process-wide parameters for one test. Tests that call
// it must not run in parallel with each other.
func configure(t *testing.T, p Params) {
	t.Helper()
	if err := Configure(p); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	t.Cleanup(func() { current.Store(newHasher(DefaultParams)) })
}

func TestConfigure_RejectsInvalidAndKeepsCurrent(t *testing.T) {
	before := current.Load()
	if err := Configure(Params{}); err == nil {
		t.Fatal("Configure(zero Params) must fail")
	}
	if current.Load() != before {
		t.Fatal("a rejected Configure must leave the current parameters in place")
	}
}

// Raising the configured cost migrates users: hashes made under the old
// parameters still verify, and report needsRehash.
func TestConfigure_ChangedParamsTriggerRehash(t *testing.T) {
	old, err := Hash(ctx, "pw")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	configure(t, Params{MemoryKiB: 12 * 1024, Iterations: 3, Parallelism: 1, MaxConcurrent: 4})

	needsRehash, err := Verify(ctx, old, "pw")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !needsRehash {
		t.Fatal("a hash made under the previous parameters must report needsRehash")
	}

	fresh, err := Hash(ctx, "pw")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(fresh, "$argon2id$v=19$m=12288,t=3,p=1$") {
		t.Fatalf("new hash not made under the configured parameters: %s", fresh)
	}
}

// With every slot taken, each entry point waits and gives up with the
// context's error rather than hashing anyway.
func TestSlots_BoundConcurrentHashing(t *testing.T) {
	configure(t, Params{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 1, MaxConcurrent: 1})
	encoded, err := Hash(ctx, "pw")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	legacy, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}

	h := current.Load()
	if err := h.acquire(ctx); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	calls := map[string]func(context.Context) error{
		"Hash":          func(c context.Context) error { _, err := Hash(c, "pw"); return err },
		"Verify":        func(c context.Context) error { _, err := Verify(c, encoded, "pw"); return err },
		"Verify bcrypt": func(c context.Context) error { _, err := Verify(c, string(legacy), "pw"); return err },
		"DummyVerify":   func(c context.Context) error { return DummyVerify(c, "pw") },
	}
	for name, call := range calls {
		c, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		err := call(c)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s with no free slot: err = %v, want context.DeadlineExceeded", name, err)
		}
	}

	h.release()
	for name, call := range calls {
		if err := call(ctx); err != nil {
			t.Errorf("%s after the slot is released: %v", name, err)
		}
	}
}
