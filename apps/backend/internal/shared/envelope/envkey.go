package envelope

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// MasterKeyLen is the required decoded length of CONNECTOR_MASTER_KEY.
const MasterKeyLen = 32 // AES-256

// ErrMasterKeyLength is returned for a master key of the wrong size.
var ErrMasterKeyLength = errors.New("master key must be 32 bytes")

// DecodeMasterKey decodes and validates a base64 (standard encoding) master
// key. config.Load calls this so a bad key is reported at boot alongside
// every other configuration problem, not on the first request.
func DecodeMasterKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode master key: %w", err)
	}
	if len(key) != MasterKeyLen {
		return nil, ErrMasterKeyLength
	}
	return key, nil
}

// EnvKeyProvider wraps data keys with a master key supplied through the
// process environment (CONNECTOR_MASTER_KEY). It is the self-hosted and
// development default; see KeyProvider for the managed-KMS path.
type EnvKeyProvider struct {
	key   []byte
	keyID string
}

// NewEnvKeyProvider builds a provider from an already-decoded master key.
func NewEnvKeyProvider(key []byte) (*EnvKeyProvider, error) {
	if len(key) != MasterKeyLen {
		return nil, ErrMasterKeyLength
	}
	// A truncated hash of the key: enough to tell two master keys apart
	// across a rotation, far too little to attack the key itself.
	sum := sha256.Sum256(key)
	return &EnvKeyProvider{key: key, keyID: "env:" + hex.EncodeToString(sum[:4])}, nil
}

func (p *EnvKeyProvider) KeyID() string { return p.keyID }

func (p *EnvKeyProvider) Wrap(_ context.Context, dataKey []byte) ([]byte, error) {
	return sealAESGCM(p.key, dataKey, nil)
}

func (p *EnvKeyProvider) Unwrap(_ context.Context, wrapped []byte) ([]byte, error) {
	return openAESGCM(p.key, wrapped, nil)
}

var _ KeyProvider = (*EnvKeyProvider)(nil)
