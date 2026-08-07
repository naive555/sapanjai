// Package envelope implements envelope encryption for secrets stored at
// rest. Each payload is sealed with a freshly generated AES-256 data key,
// and that data key is itself sealed by a KeyProvider holding the master
// key. Only the KeyProvider changes when the master key moves from an
// environment variable to KMS/Vault — no stored envelope, and no caller,
// has to change.
package envelope

import (
	"context"
	"errors"
)

// ErrUnknownKeyID is returned by a provider asked to unwrap under a master
// key it does not hold — the signature of a rotation that dropped a key
// still referenced by stored data. Open collapses this into ErrOpen for
// callers; the sentinel exists so a rotation job can tell "wrong key" apart
// from "corrupt data".
var ErrUnknownKeyID = errors.New("envelope: unknown key id")

// KeyProvider wraps and unwraps data keys with the master key. This is the
// swap point for a managed key service: a KMS/Vault provider implements the
// same three methods with Wrap/Unwrap as remote calls, and nothing else in
// the codebase moves.
type KeyProvider interface {
	// KeyID identifies the master key that wrapped a data key. It is
	// recorded in every envelope so a later rotation can tell which rows
	// still need re-wrapping. It must never contain key material.
	KeyID() string

	// Wrap seals a data key. The returned blob is opaque to callers and
	// carries whatever the provider needs to reverse it (nonce, key
	// version, KMS ciphertext framing, ...).
	Wrap(ctx context.Context, dataKey []byte) ([]byte, error)

	// Unwrap reverses Wrap. keyID is the KeyID recorded in the envelope at
	// seal time. A provider that keeps retired master keys around for
	// decrypt-only use selects on it (ErrUnknownKeyID if it holds none
	// matching); a KMS provider may ignore it, since the KMS ciphertext in
	// wrapped already identifies the key that sealed it.
	Unwrap(ctx context.Context, keyID string, wrapped []byte) ([]byte, error)
}
