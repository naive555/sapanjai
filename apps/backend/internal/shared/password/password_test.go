package password

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHashVerify_RoundTrip(t *testing.T) {
	h, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected hash format: %s", h)
	}

	needsRehash, err := Verify(h, "correct horse battery staple")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if needsRehash {
		t.Fatal("a hash with current parameters must not need a rehash")
	}

	if _, err := Verify(h, "wrong"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("Verify(wrong) err = %v, want ErrMismatch", err)
	}
}

func TestHash_SaltsEachHash(t *testing.T) {
	a, _ := Hash("same")
	b, _ := Hash("same")
	if a == b {
		t.Fatal("two hashes of the same password must differ")
	}
}

// The point of the migration: bcrypt ignored everything past byte 72.
func TestVerify_Argon2UsesBytesPast72(t *testing.T) {
	long := strings.Repeat("a", 100)
	h, err := Hash(long)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if _, err := Verify(h, long[:72]); !errors.Is(err, ErrMismatch) {
		t.Fatalf("72-byte prefix err = %v, want ErrMismatch", err)
	}
	if _, err := Verify(h, long[:99]+"b"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("last-byte change err = %v, want ErrMismatch", err)
	}
}

func TestVerify_LegacyBcrypt(t *testing.T) {
	legacy, err := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}

	needsRehash, err := Verify(string(legacy), "password123")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !needsRehash {
		t.Fatal("a matching bcrypt hash must report needsRehash")
	}

	if _, err := Verify(string(legacy), "nope"); !errors.Is(err, ErrMismatch) {
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
	if _, err := Verify(string(legacy), long); err != nil {
		t.Fatalf("Verify(100-byte password vs truncated legacy hash): %v", err)
	}
}

func TestVerify_OutdatedArgon2ParamsNeedRehash(t *testing.T) {
	// Valid Argon2id, but weaker than the current profile.
	encoded, err := hashWith("pw", 8, 1, 1)
	if err != nil {
		t.Fatalf("hashWith: %v", err)
	}
	needsRehash, err := Verify(encoded, "pw")
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
		"$argon2id$v=19$m=19456,t=2,p=1$!!!$a2V5",
		"$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$",
	} {
		_, err := Verify(h, "pw")
		if err == nil || errors.Is(err, ErrMismatch) {
			t.Errorf("Verify(%q) err = %v, want a malformed-hash error", h, err)
		}
	}
}
