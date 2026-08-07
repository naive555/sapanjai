package envelope

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
)

// Version is the envelope format version stamped into every sealed blob.
// Open rejects versions it does not know, so a future format change is a
// bump here plus a branch in Open, never a silent misread.
const Version = 1

const dataKeyLen = 32 // AES-256

// ErrOpen is returned for every failure to open a sealed envelope — wrong
// master key, tampered ciphertext, wrong AAD. Callers must not be able to
// tell these apart.
var ErrOpen = errors.New("envelope: cannot open")

// sealed is the stored (jsonb) shape. Field names are short because this
// lives on every row; it is not a human-facing document.
type sealed struct {
	Version int    `json:"v"`
	KeyID   string `json:"kid"` // master key that wrapped DataKey
	DataKey []byte `json:"dek"` // data key, wrapped by the KeyProvider
	Payload []byte `json:"ct"`  // nonce || ciphertext, sealed with the data key
}

// Encryptor seals and opens payloads. Safe for concurrent use.
type Encryptor struct {
	provider KeyProvider
}

// New builds an Encryptor over the given master-key provider.
func New(provider KeyProvider) *Encryptor { return &Encryptor{provider: provider} }

// Seal encrypts plaintext under a data key generated for this call alone
// and returns the JSON envelope to store.
//
// aad is additional authenticated data bound into the ciphertext but not
// stored in it; callers pass the owning tenant id, so a row copied into
// another organization fails to open rather than decrypting silently. Open
// must be given the identical aad.
func (e *Encryptor) Seal(ctx context.Context, plaintext, aad []byte) (json.RawMessage, error) {
	dataKey := make([]byte, dataKeyLen)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, fmt.Errorf("generate data key: %w", err)
	}

	payload, err := sealAESGCM(dataKey, plaintext, aad)
	if err != nil {
		return nil, err
	}

	wrapped, err := e.provider.Wrap(ctx, dataKey)
	if err != nil {
		return nil, fmt.Errorf("wrap data key: %w", err)
	}

	raw, err := json.Marshal(sealed{
		Version: Version,
		KeyID:   e.provider.KeyID(),
		DataKey: wrapped,
		Payload: payload,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	return raw, nil
}

// Open reverses Seal. Every failure mode collapses to ErrOpen.
func (e *Encryptor) Open(ctx context.Context, raw json.RawMessage, aad []byte) ([]byte, error) {
	plaintext, _, err := e.OpenAndRotate(ctx, raw, aad)
	return plaintext, err
}

// OpenAndRotate opens raw and, when it was sealed under a master key that is
// no longer the provider's current one, also returns a freshly re-sealed
// envelope for the caller to persist. rotated is nil when raw is already
// current, so the common case allocates nothing beyond a plain Open.
//
// Rotation re-seals with a brand-new data key rather than re-wrapping the
// old one under the new master key: same cost, and it retires the old data
// key too, not just the master key that wrapped it.
//
// The caller decides whether and when to persist rotated — a failed write
// is not a failed read, and the next read simply offers the rotation again.
// This is the rotate-on-read half of key rotation; nothing sweeps rows that
// are never read.
func (e *Encryptor) OpenAndRotate(ctx context.Context, raw json.RawMessage, aad []byte) (plaintext []byte, rotated json.RawMessage, err error) {
	var s sealed
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, nil, ErrOpen
	}
	if s.Version != Version {
		return nil, nil, ErrOpen
	}

	dataKey, err := e.provider.Unwrap(ctx, s.KeyID, s.DataKey)
	if err != nil {
		return nil, nil, ErrOpen
	}

	plaintext, err = openAESGCM(dataKey, s.Payload, aad)
	if err != nil {
		return nil, nil, ErrOpen
	}

	if s.KeyID == e.provider.KeyID() {
		return plaintext, nil, nil
	}

	rotated, err = e.Seal(ctx, plaintext, aad)
	if err != nil {
		// Sealing failed but the read already succeeded — surface the
		// plaintext, just without a rotation to offer this time.
		return plaintext, nil, nil
	}
	return plaintext, rotated, nil
}

// sealAESGCM returns nonce || ciphertext.
func sealAESGCM(key, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// openAESGCM reverses sealAESGCM.
func openAESGCM(key, blob, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, ErrOpen
	}
	nonce, ciphertext := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrOpen
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	return cipher.NewGCM(block)
}
