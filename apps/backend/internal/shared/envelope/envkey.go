package envelope

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
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

// DecodeMasterKeys decodes a comma-separated list of base64 master keys —
// the shape of CONNECTOR_MASTER_KEY_PREVIOUS, the retired keys kept for
// decrypt-only use across a rotation. Empty entries (from stray commas or
// surrounding whitespace) are skipped; an empty or all-empty input returns
// nil with no error, since retired keys are optional. A malformed entry's
// error names its 1-based position in the list.
func DecodeMasterKeys(encoded string) ([][]byte, error) {
	var keys [][]byte
	for i, part := range strings.Split(encoded, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, err := DecodeMasterKey(part)
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i+1, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// envKeyID derives the identifier recorded for a master key: a truncated
// hash, enough to tell two master keys apart across a rotation, far too
// little to attack the key itself.
func envKeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return "env:" + hex.EncodeToString(sum[:4])
}

// EnvKeyProvider wraps data keys with a master key supplied through the
// process environment (CONNECTOR_MASTER_KEY). It is the self-hosted and
// development default; see KeyProvider for the managed-KMS path.
//
// It also holds retired master keys (CONNECTOR_MASTER_KEY_PREVIOUS) for
// decrypt-only use, so rows sealed before a rotation still open under the
// rotate-on-read pattern (Encryptor.OpenAndRotate) instead of being bricked
// the moment the primary key changes.
type EnvKeyProvider struct {
	primary   []byte
	primaryID string
	keys      map[string][]byte // keyID -> key, primary included
}

// NewEnvKeyProvider builds a provider from an already-decoded primary master
// key plus zero or more retired master keys kept for decrypt-only use. Wrap
// always uses primary; Unwrap accepts any key in primary or retired.
func NewEnvKeyProvider(primary []byte, retired ...[]byte) (*EnvKeyProvider, error) {
	if len(primary) != MasterKeyLen {
		return nil, ErrMasterKeyLength
	}
	primaryID := envKeyID(primary)
	keys := map[string][]byte{primaryID: primary}
	for _, key := range retired {
		if len(key) != MasterKeyLen {
			return nil, ErrMasterKeyLength
		}
		keys[envKeyID(key)] = key
	}
	return &EnvKeyProvider{primary: primary, primaryID: primaryID, keys: keys}, nil
}

func (p *EnvKeyProvider) KeyID() string { return p.primaryID }

func (p *EnvKeyProvider) Wrap(_ context.Context, dataKey []byte) ([]byte, error) {
	return sealAESGCM(p.primary, dataKey, nil)
}

// Unwrap opens wrapped with the master key identified by keyID. An empty
// keyID (an envelope sealed before key-ID tagging existed — none in
// practice, but handled explicitly) falls back to the primary key.
func (p *EnvKeyProvider) Unwrap(_ context.Context, keyID string, wrapped []byte) ([]byte, error) {
	if keyID == "" {
		keyID = p.primaryID
	}
	key, ok := p.keys[keyID]
	if !ok {
		return nil, ErrUnknownKeyID
	}
	return openAESGCM(key, wrapped, nil)
}

var _ KeyProvider = (*EnvKeyProvider)(nil)
