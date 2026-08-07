package envelope

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func testKey(b byte) []byte {
	return bytes.Repeat([]byte{b}, MasterKeyLen)
}

func newTestEncryptor(t *testing.T, keyByte byte) *Encryptor {
	t.Helper()
	provider, err := NewEnvKeyProvider(testKey(keyByte))
	if err != nil {
		t.Fatalf("NewEnvKeyProvider: %v", err)
	}
	return New(provider)
}

func TestEncryptor_SealOpen_RoundTrip(t *testing.T) {
	e := newTestEncryptor(t, 1)
	ctx := context.Background()
	aad := []byte("org-aaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	plaintext := []byte(`{"host":"db.example.com","password":"hunter2"}`)

	raw, err := e.Seal(ctx, plaintext, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	got, err := e.Open(ctx, raw, aad)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("Open() = %q, want %q", got, plaintext)
	}
}

func TestEncryptor_Seal_DoesNotLeakPlaintext(t *testing.T) {
	e := newTestEncryptor(t, 2)
	ctx := context.Background()
	aad := []byte("org")
	secret := []byte("hunter2-super-secret")
	plaintext := []byte(`{"password":"hunter2-super-secret"}`)

	raw, err := e.Seal(ctx, plaintext, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if bytes.Contains(raw, secret) {
		t.Fatalf("sealed envelope contains the plaintext secret: %s", raw)
	}
}

func TestEncryptor_Seal_FreshDataKeyPerCall(t *testing.T) {
	e := newTestEncryptor(t, 3)
	ctx := context.Background()
	aad := []byte("org")
	plaintext := []byte(`{"key":"value"}`)

	raw1, err := e.Seal(ctx, plaintext, aad)
	if err != nil {
		t.Fatalf("Seal 1: %v", err)
	}
	raw2, err := e.Seal(ctx, plaintext, aad)
	if err != nil {
		t.Fatalf("Seal 2: %v", err)
	}

	var s1, s2 sealed
	if err := json.Unmarshal(raw1, &s1); err != nil {
		t.Fatalf("unmarshal raw1: %v", err)
	}
	if err := json.Unmarshal(raw2, &s2); err != nil {
		t.Fatalf("unmarshal raw2: %v", err)
	}

	if bytes.Equal(s1.Payload, s2.Payload) {
		t.Fatal("two seals of the same plaintext produced identical ciphertext")
	}
	if bytes.Equal(s1.DataKey, s2.DataKey) {
		t.Fatal("two seals of the same plaintext produced identical wrapped data keys")
	}
}

func TestEncryptor_Seal_EnvelopeShape(t *testing.T) {
	e := newTestEncryptor(t, 4)
	ctx := context.Background()

	raw, err := e.Seal(ctx, []byte("payload"), []byte("aad"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}

	v, ok := m["v"].(float64)
	if !ok || v != Version {
		t.Fatalf("envelope v = %v, want %d", m["v"], Version)
	}
	kid, ok := m["kid"].(string)
	if !ok || kid == "" {
		t.Fatalf("envelope kid = %v, want non-empty string", m["kid"])
	}
	if len(kid) < 4 || kid[:4] != "env:" {
		t.Fatalf("envelope kid = %q, want env: prefix", kid)
	}
	if _, ok := m["dek"].(string); !ok {
		t.Fatalf("envelope dek = %v, want base64 string", m["dek"])
	}
	if _, ok := m["ct"].(string); !ok {
		t.Fatalf("envelope ct = %v, want base64 string", m["ct"])
	}
}

func TestEncryptor_Open_TamperedCiphertextFails(t *testing.T) {
	e := newTestEncryptor(t, 5)
	ctx := context.Background()
	aad := []byte("org")

	raw, err := e.Seal(ctx, []byte("secret-value"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	var s sealed
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(s.Payload) == 0 {
		t.Fatal("empty payload, cannot tamper")
	}
	s.Payload[len(s.Payload)-1] ^= 0xFF // flip last byte of the ciphertext

	tampered, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal tampered: %v", err)
	}

	if _, err := e.Open(ctx, tampered, aad); err != ErrOpen {
		t.Fatalf("Open(tampered) error = %v, want ErrOpen", err)
	}
}

func TestEncryptor_Open_WrongAADFails(t *testing.T) {
	e := newTestEncryptor(t, 6)
	ctx := context.Background()

	raw, err := e.Seal(ctx, []byte("secret-value"), []byte("org-a"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// This is the tenant-binding guarantee: a row sealed for org-a must not
	// open under org-b's AAD, even with the correct master key.
	if _, err := e.Open(ctx, raw, []byte("org-b")); err != ErrOpen {
		t.Fatalf("Open(wrong aad) error = %v, want ErrOpen", err)
	}
}

func TestEncryptor_Open_WrongMasterKeyFails(t *testing.T) {
	sealer := newTestEncryptor(t, 7)
	opener := newTestEncryptor(t, 8)
	ctx := context.Background()
	aad := []byte("org")

	raw, err := sealer.Seal(ctx, []byte("secret-value"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if _, err := opener.Open(ctx, raw, aad); err != ErrOpen {
		t.Fatalf("Open(wrong master key) error = %v, want ErrOpen", err)
	}
}

func TestEncryptor_Open_MalformedJSONFails(t *testing.T) {
	e := newTestEncryptor(t, 9)
	ctx := context.Background()

	if _, err := e.Open(ctx, []byte("not json"), []byte("aad")); err != ErrOpen {
		t.Fatalf("Open(malformed) error = %v, want ErrOpen", err)
	}
}

func TestEncryptor_Open_UnknownVersionFails(t *testing.T) {
	e := newTestEncryptor(t, 10)
	ctx := context.Background()

	raw := []byte(`{"v":999,"kid":"env:deadbeef","dek":"AAAA","ct":"AAAA"}`)
	if _, err := e.Open(ctx, raw, []byte("aad")); err != ErrOpen {
		t.Fatalf("Open(unknown version) error = %v, want ErrOpen", err)
	}
}

func TestDecodeMasterKey(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString(testKey(1))

	tests := []struct {
		name    string
		encoded string
		wantErr error // nil means "any non-nil error is fine"
	}{
		{"valid 32 bytes", valid, nil},
		{"not base64", "not-valid-base64!!!", nil},
		{"wrong length (16 bytes)", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16)), ErrMasterKeyLength},
		{"empty string", "", ErrMasterKeyLength},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := DecodeMasterKey(tt.encoded)
			if tt.name == "valid 32 bytes" {
				if err != nil {
					t.Fatalf("DecodeMasterKey(%q) error = %v, want nil", tt.encoded, err)
				}
				if len(key) != MasterKeyLen {
					t.Fatalf("DecodeMasterKey(%q) len = %d, want %d", tt.encoded, len(key), MasterKeyLen)
				}
				return
			}
			if err == nil {
				t.Fatalf("DecodeMasterKey(%q) error = nil, want error", tt.encoded)
			}
			if tt.wantErr != nil && err != tt.wantErr {
				t.Fatalf("DecodeMasterKey(%q) error = %v, want %v", tt.encoded, err, tt.wantErr)
			}
		})
	}
}

func TestEnvKeyProvider_KeyID_Stability(t *testing.T) {
	p1a, err := NewEnvKeyProvider(testKey(11))
	if err != nil {
		t.Fatalf("NewEnvKeyProvider: %v", err)
	}
	p1b, err := NewEnvKeyProvider(testKey(11))
	if err != nil {
		t.Fatalf("NewEnvKeyProvider: %v", err)
	}
	p2, err := NewEnvKeyProvider(testKey(12))
	if err != nil {
		t.Fatalf("NewEnvKeyProvider: %v", err)
	}

	if p1a.KeyID() != p1b.KeyID() {
		t.Fatalf("same key produced different ids: %q vs %q", p1a.KeyID(), p1b.KeyID())
	}
	if p1a.KeyID() == p2.KeyID() {
		t.Fatalf("different keys produced the same id: %q", p1a.KeyID())
	}

	rawKey11 := testKey(11)
	if bytes.Contains([]byte(p1a.KeyID()), rawKey11) {
		t.Fatalf("KeyID leaks raw key material: %q", p1a.KeyID())
	}
}

func TestNewEnvKeyProvider_RejectsWrongLength(t *testing.T) {
	if _, err := NewEnvKeyProvider(bytes.Repeat([]byte{1}, 16)); err != ErrMasterKeyLength {
		t.Fatalf("NewEnvKeyProvider(16 bytes) error = %v, want ErrMasterKeyLength", err)
	}
}

func TestNewEnvKeyProvider_RejectsWrongLengthRetiredKey(t *testing.T) {
	if _, err := NewEnvKeyProvider(testKey(1), bytes.Repeat([]byte{2}, 16)); err != ErrMasterKeyLength {
		t.Fatalf("NewEnvKeyProvider(bad retired key) error = %v, want ErrMasterKeyLength", err)
	}
}

func TestEnvKeyProvider_Unwrap_RetiredKey(t *testing.T) {
	ctx := context.Background()

	sealerProvider, err := NewEnvKeyProvider(testKey(1))
	if err != nil {
		t.Fatalf("NewEnvKeyProvider(sealer): %v", err)
	}
	wrapped, err := sealerProvider.Wrap(ctx, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	openerProvider, err := NewEnvKeyProvider(testKey(2), testKey(1))
	if err != nil {
		t.Fatalf("NewEnvKeyProvider(opener): %v", err)
	}
	dataKey, err := openerProvider.Unwrap(ctx, sealerProvider.KeyID(), wrapped)
	if err != nil {
		t.Fatalf("Unwrap(retired key) error = %v, want nil", err)
	}
	if len(dataKey) != 32 {
		t.Fatalf("Unwrap(retired key) len = %d, want 32", len(dataKey))
	}
}

func TestEnvKeyProvider_Unwrap_UnknownKeyIDFails(t *testing.T) {
	ctx := context.Background()

	provider, err := NewEnvKeyProvider(testKey(1))
	if err != nil {
		t.Fatalf("NewEnvKeyProvider: %v", err)
	}
	if _, err := provider.Unwrap(ctx, "env:deadbeef", []byte("irrelevant")); err != ErrUnknownKeyID {
		t.Fatalf("Unwrap(unknown key id) error = %v, want ErrUnknownKeyID", err)
	}
}

func TestEnvKeyProvider_Wrap_AlwaysUsesPrimary(t *testing.T) {
	ctx := context.Background()

	provider, err := NewEnvKeyProvider(testKey(2), testKey(1))
	if err != nil {
		t.Fatalf("NewEnvKeyProvider: %v", err)
	}
	if provider.KeyID() != "env:"+hexPrefix(testKey(2)) {
		t.Fatalf("KeyID() = %q, want primary key's id", provider.KeyID())
	}

	e := New(provider)
	aad := []byte("org")
	raw, err := e.Seal(ctx, []byte("secret"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	var s sealed
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.KeyID != provider.KeyID() {
		t.Fatalf("sealed kid = %q, want primary key id %q", s.KeyID, provider.KeyID())
	}

	// A provider that only holds the retired key cannot open a fresh seal.
	retiredOnly, err := NewEnvKeyProvider(testKey(1))
	if err != nil {
		t.Fatalf("NewEnvKeyProvider(retired-only): %v", err)
	}
	if _, err := New(retiredOnly).Open(ctx, raw, aad); err != ErrOpen {
		t.Fatalf("Open with retired-only provider error = %v, want ErrOpen", err)
	}
}

func TestEncryptor_OpenAndRotate_AfterKeyRotation(t *testing.T) {
	ctx := context.Background()
	aad := []byte("org")
	plaintext := []byte("secret-value")

	oldProvider, err := NewEnvKeyProvider(testKey(1))
	if err != nil {
		t.Fatalf("NewEnvKeyProvider(old): %v", err)
	}
	raw, err := New(oldProvider).Seal(ctx, plaintext, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	newProvider, err := NewEnvKeyProvider(testKey(2), testKey(1))
	if err != nil {
		t.Fatalf("NewEnvKeyProvider(new): %v", err)
	}
	e := New(newProvider)

	got, rotated, err := e.OpenAndRotate(ctx, raw, aad)
	if err != nil {
		t.Fatalf("OpenAndRotate: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("OpenAndRotate() plaintext = %q, want %q", got, plaintext)
	}
	if rotated == nil {
		t.Fatal("OpenAndRotate() rotated = nil, want a re-sealed envelope")
	}

	var s sealed
	if err := json.Unmarshal(rotated, &s); err != nil {
		t.Fatalf("unmarshal rotated: %v", err)
	}
	if s.KeyID != newProvider.KeyID() {
		t.Fatalf("rotated kid = %q, want new primary key id %q", s.KeyID, newProvider.KeyID())
	}

	// The rotated envelope opens under a provider that only knows the new
	// key, and no longer opens under a provider that only knows the old one.
	newOnly, err := NewEnvKeyProvider(testKey(2))
	if err != nil {
		t.Fatalf("NewEnvKeyProvider(new-only): %v", err)
	}
	if _, err := New(newOnly).Open(ctx, rotated, aad); err != nil {
		t.Fatalf("Open(rotated) with new-only provider error = %v, want nil", err)
	}
	if _, err := New(oldProvider).Open(ctx, rotated, aad); err != ErrOpen {
		t.Fatalf("Open(rotated) with old-only provider error = %v, want ErrOpen", err)
	}
}

func TestEncryptor_OpenAndRotate_NoRotationWhenCurrent(t *testing.T) {
	e := newTestEncryptor(t, 1)
	ctx := context.Background()
	aad := []byte("org")

	raw, err := e.Seal(ctx, []byte("secret-value"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	_, rotated, err := e.OpenAndRotate(ctx, raw, aad)
	if err != nil {
		t.Fatalf("OpenAndRotate: %v", err)
	}
	if rotated != nil {
		t.Fatalf("OpenAndRotate() rotated = %v, want nil (already sealed under current key)", rotated)
	}
}

func TestEncryptor_OpenAndRotate_RetiredKeyDroppedFails(t *testing.T) {
	ctx := context.Background()
	aad := []byte("org")

	oldProvider, err := NewEnvKeyProvider(testKey(1))
	if err != nil {
		t.Fatalf("NewEnvKeyProvider(old): %v", err)
	}
	raw, err := New(oldProvider).Seal(ctx, []byte("secret-value"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The old key was dropped instead of retired — rotation happened too
	// early relative to this row being read.
	newOnly, err := NewEnvKeyProvider(testKey(2))
	if err != nil {
		t.Fatalf("NewEnvKeyProvider(new-only): %v", err)
	}
	if _, _, err := New(newOnly).OpenAndRotate(ctx, raw, aad); err != ErrOpen {
		t.Fatalf("OpenAndRotate(dropped key) error = %v, want ErrOpen", err)
	}
}

func TestEncryptor_Open_TamperedWrappedDataKeyFails(t *testing.T) {
	e := newTestEncryptor(t, 1)
	ctx := context.Background()
	aad := []byte("org")

	raw, err := e.Seal(ctx, []byte("secret-value"), aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	var s sealed
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(s.DataKey) == 0 {
		t.Fatal("empty wrapped data key, cannot tamper")
	}
	s.DataKey[len(s.DataKey)-1] ^= 0xFF

	tampered, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal tampered: %v", err)
	}

	if _, err := e.Open(ctx, tampered, aad); err != ErrOpen {
		t.Fatalf("Open(tampered dek) error = %v, want ErrOpen", err)
	}
}

func TestDecodeMasterKeys(t *testing.T) {
	valid1 := base64.StdEncoding.EncodeToString(testKey(1))
	valid2 := base64.StdEncoding.EncodeToString(testKey(2))

	tests := []struct {
		name    string
		encoded string
		wantLen int
		wantErr bool
	}{
		{"empty string", "", 0, false},
		{"whitespace only", "   ", 0, false},
		{"one valid key", valid1, 1, false},
		{"two valid keys", valid1 + "," + valid2, 2, false},
		{"whitespace and trailing comma tolerated", " " + valid1 + " , " + valid2 + " ,", 2, false},
		{"one bad entry", valid1 + ",not-valid-base64!!!", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys, err := DecodeMasterKeys(tt.encoded)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("DecodeMasterKeys(%q) error = nil, want error", tt.encoded)
				}
				if !strings.Contains(err.Error(), "entry 2") {
					t.Fatalf("DecodeMasterKeys(%q) error = %v, want it to name entry 2", tt.encoded, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeMasterKeys(%q) error = %v, want nil", tt.encoded, err)
			}
			if len(keys) != tt.wantLen {
				t.Fatalf("DecodeMasterKeys(%q) len = %d, want %d", tt.encoded, len(keys), tt.wantLen)
			}
		})
	}
}

func hexPrefix(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:4])
}
