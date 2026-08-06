package envelope

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
