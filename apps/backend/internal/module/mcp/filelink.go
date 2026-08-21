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

// This file is docs/07-sheets-adapter-plan.md step 9's signed download
// link: drive_get_file (tools_drive.go) hands back a URL good for
// fileLinkTTL, and Handler.downloadFile (handler.go) verifies it on the
// unauthenticated (no bearer header) GET route it is served from. The
// signature *is* the credential — there is no session, no PAT, nothing else
// standing between an internet-reachable URL and a customer's file bytes,
// which is exactly why every field the download route trusts (org,
// connector, file, actor, expiry) is bound into what gets signed rather
// than merely carried alongside it.

// fileLinkTTL is the hard ceiling docs/06-sheets-adapter.md §4.3 sets on a
// drive_get_file download link: "TTL ≤ 15 minutes." Google Drive has no
// signed-URL feature of its own (unlike, say, S3 presigned URLs), so this
// gateway mints and verifies the link itself — see SignFileLink/
// VerifyFileLink below.
const fileLinkTTL = 15 * time.Minute

// fileLinkKeyInfo is the HKDF "info" label deriveFileLinkKey binds into the
// key it produces — the standard HKDF technique for deriving multiple
// independent keys from one secret without needing multiple secrets, keyed
// by a fixed, purpose-specific string so this key can never collide with a
// key some future feature derives from the same master key for a different
// purpose.
const fileLinkKeyInfo = "sapanjai/mcp-file-link/v1"

// deriveFileLinkKey derives a 32-byte HMAC key for file-link signing from
// masterKey via HKDF-SHA256 (golang.org/x/crypto/hkdf — golang.org/x/crypto
// is already a direct dependency of this module, per go.mod, via
// internal/module/auth's use of golang.org/x/crypto/bcrypt) — deliberately
// never masterKey itself. Two reasons: first, the
// ordinary key-separation argument (a key used for one purpose should never
// double as a key for another; masterKey already wraps every connector's
// envelope-encryption data key, internal/shared/envelope) applies here
// exactly as it would anywhere else two features share a secret. Second,
// and specific to this feature: deriving means a CONNECTOR_MASTER_KEY
// rotation (CLAUDE.md's rotate-on-read ground rule) silently invalidates
// every in-flight file link the moment the new key becomes primary, since
// the derived signing key changes too — but a file link's own TTL is at
// most 15 minutes, so "some links stop verifying mid-rotation" is a
// non-event: the caller just calls drive_get_file again for a fresh one.
// Returns nil when masterKey is empty, which NewService treats as "link
// minting disabled" (docs/07 step 9: "a nil/empty key must disable link
// minting ... never sign with an empty key").
func deriveFileLinkKey(masterKey []byte) []byte {
	if len(masterKey) == 0 {
		return nil
	}
	key := make([]byte, sha256.Size)
	kdf := hkdf.New(sha256.New, masterKey, nil, []byte(fileLinkKeyInfo))
	if _, err := io.ReadFull(kdf, key); err != nil {
		// HKDF-SHA256's expand step can only fail to produce sha256.Size
		// bytes if it were asked for more than 255*sha256.Size bytes total
		// (RFC 5869 §2.3) — unreachable for a single 32-byte read. Fail safe
		// (disable minting) rather than panic on an error this code can
		// never actually hit in practice.
		return nil
	}
	return key
}

// canonicalFileLinkMessage builds the exact newline-separated string
// SignFileLink/VerifyFileLink sign and check — every field the download
// route (Handler.downloadFile) will later trust is bound in here, so
// tampering with any one of them (swapping fileID for a different file
// while keeping everything else, say) invalidates sig. Order and format are
// fixed and must never change without a version bump to the "v1" prefix,
// since that prefix is itself part of what's signed — a v2 verifier could
// then reject a v1 signature outright instead of misinterpreting its bytes
// under new rules.
func canonicalFileLinkMessage(orgID, connectorID, userID uuid.UUID, fileID string, exp int64) string {
	return "v1\n" + orgID.String() + "\n" + connectorID.String() + "\n" + fileID + "\n" + userID.String() + "\n" + strconv.FormatInt(exp, 10)
}

// signFileLinkMessage is the raw HMAC-SHA256-over-msg step shared by
// SignFileLink (produces a sig to embed in a URL) and VerifyFileLink
// (recomputes the expected sig to compare against one). base64.RawURLEncoding
// (no padding, URL-safe alphabet) is used so the result drops straight into
// a query string with no further escaping needed.
func signFileLinkMessage(key []byte, msg string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg)) //nolint:errcheck // hash.Hash.Write never returns an error
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// SignFileLink mints the URL drive_get_file returns for fileID, absolute
// against baseURL (the gateway's own per-request origin — see
// RequestInfo/baseURLFromRequest in handler.go/catalog.go for why this
// can't just be a fixed config value) and bound to orgID/connectorID/userID/
// fileID/expiresAt via canonicalFileLinkMessage. fileID is URL-path-escaped
// when building the path segment here; Echo unescapes it back on the
// serving side (Handler.downloadFile's c.Param("fileId")) before it is fed
// back into canonicalFileLinkMessage for verification, so the two sides
// agree on the same unescaped string despite the URL round trip.
func SignFileLink(key []byte, baseURL string, orgID, connectorID, userID uuid.UUID, fileID string, expiresAt time.Time) string {
	exp := expiresAt.Unix()
	sig := signFileLinkMessage(key, canonicalFileLinkMessage(orgID, connectorID, userID, fileID, exp))
	return fmt.Sprintf("%s/mcp/files/%s/%s?org=%s&uid=%s&exp=%d&sig=%s",
		baseURL, connectorID.String(), url.PathEscape(fileID), orgID.String(), userID.String(), exp, sig)
}

// VerifyFileLink reports whether sig is a valid, unexpired signature (as of
// now) over (orgID, connectorID, userID, fileID, exp) under key, AND that
// exp itself claims no more than fileLinkTTL beyond now. That second check
// is what makes docs/02-api-contract.md's "exp is never honored past 15
// minutes" a fact about this function rather than a fact about the one
// caller (registerDriveGetFile) that happens to always pass
// now.Add(fileLinkTTL) today: without it, the 15-minute ceiling would be
// pure caller discipline, and a bug or a future caller passing a longer
// expiresAt to SignFileLink would silently mint a link this function would
// still call valid, since a not-yet-expired timestamp is not the same
// property as "at most 15 minutes from now." Checked *before* the
// signature comparison — a cryptographically valid signature over an
// out-of-bound exp is still rejected, not trusted because the bytes match.
//
// hmac.Equal is used for the signature comparison — constant-time, so a
// timing side channel can never help an attacker guess a valid signature
// one byte at a time. A zero-length key always fails closed (never
// verifies), matching SignFileLink's own contract of never being called
// with one in the first place (Service.NewService derives the key once; a
// nil/empty ConnectorMasterKey means link minting was disabled and no valid
// link could exist to verify).
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
