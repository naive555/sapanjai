package mcp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/hkdf"
)

// Signed download links for drive_get_file (tools_drive.go), verified by
// Handler.downloadFile on an unauthenticated GET route. The signature *is*
// the credential — nothing else stands between an internet-reachable URL
// and a customer's file bytes — so every field the download route trusts
// (org, connector, file, actor, expiry) is bound into what gets signed
// rather than merely carried alongside it.

// fileLinkTTL is the hard ceiling docs/06-sheets-adapter.md §4.3 sets on a
// download link. Drive has no signed-URL feature of its own, so the gateway
// mints and verifies these itself.
const fileLinkTTL = 15 * time.Minute

// fileLinkKeyInfo is the HKDF info label, fixed and purpose-specific so this
// key can never collide with another derived from the same master key.
const fileLinkKeyInfo = "sapanjai/mcp-file-link/v1"

// deriveFileLinkKey derives the 32-byte HMAC signing key from masterKey via
// HKDF-SHA256, never masterKey itself: masterKey already wraps every
// connector's envelope data key, and one key should serve one purpose. A
// CONNECTOR_MASTER_KEY rotation therefore invalidates in-flight links — a
// non-event, since none outlive fileLinkTTL.
//
// Returns nil for an empty masterKey, which NewService treats as "link
// minting disabled" rather than ever signing under an empty key.
func deriveFileLinkKey(masterKey []byte) []byte {
	if len(masterKey) == 0 {
		return nil
	}
	key := make([]byte, sha256.Size)
	kdf := hkdf.New(sha256.New, masterKey, nil, []byte(fileLinkKeyInfo))
	if _, err := io.ReadFull(kdf, key); err != nil {
		// Unreachable for a single 32-byte read (RFC 5869 §2.3); fail safe.
		return nil
	}
	return key
}

// canonicalFileLinkMessage builds the signed string. Order and format are
// fixed: change them only with a bump to the "v1" prefix, which is itself
// signed, so a v2 verifier rejects v1 signatures instead of misreading them.
func canonicalFileLinkMessage(orgID, connectorID, userID uuid.UUID, fileID string, exp int64) string {
	return "v1\n" + orgID.String() + "\n" + connectorID.String() + "\n" + fileID + "\n" + userID.String() + "\n" + strconv.FormatInt(exp, 10)
}

// signFileLinkMessage is the HMAC-SHA256 step shared by SignFileLink and
// VerifyFileLink. RawURLEncoding drops straight into a query string.
func signFileLinkMessage(key []byte, msg string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg)) //nolint:errcheck // hash.Hash.Write never returns an error
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// SignFileLink mints the absolute URL drive_get_file returns for fileID.
// fileID is path-escaped here and unescaped by Echo on the serving side, so
// both ends sign the same string despite the URL round trip.
func SignFileLink(key []byte, baseURL string, orgID, connectorID, userID uuid.UUID, fileID string, expiresAt time.Time) string {
	exp := expiresAt.Unix()
	sig := signFileLinkMessage(key, canonicalFileLinkMessage(orgID, connectorID, userID, fileID, exp))
	return fmt.Sprintf("%s/mcp/files/%s/%s?org=%s&uid=%s&exp=%d&sig=%s",
		baseURL, connectorID.String(), url.PathEscape(fileID), orgID.String(), userID.String(), exp, sig)
}

// VerifyFileLink reports whether sig is a valid, unexpired signature over
// (orgID, connectorID, userID, fileID, exp) under key.
//
// The exp bounds are checked *before* the signature: a cryptographically
// valid signature over an out-of-range exp is still rejected. That makes the
// 15-minute ceiling a property of this function rather than caller
// discipline — otherwise a future caller passing a longer expiresAt would
// silently mint links this function still called valid.
//
// hmac.Equal is constant-time. A zero-length key always fails closed, since
// an empty master key means minting was disabled and no valid link exists.
func VerifyFileLink(key []byte, orgID, connectorID, userID uuid.UUID, fileID string, exp int64, sig string, now time.Time) bool {
	if len(key) == 0 {
		return false
	}
	expTime := time.Unix(exp, 0)
	if !now.Before(expTime) {
		return false // already expired
	}
	if expTime.After(now.Add(fileLinkTTL)) {
		return false // claims a TTL longer than the 15-minute ceiling allows
	}
	want := signFileLinkMessage(key, canonicalFileLinkMessage(orgID, connectorID, userID, fileID, exp))
	return hmac.Equal([]byte(want), []byte(sig))
}
