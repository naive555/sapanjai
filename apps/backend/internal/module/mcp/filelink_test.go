package mcp

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---- deriveFileLinkKey ----

func TestFileLink_DeriveFileLinkKey_Deterministic(t *testing.T) {
	master := []byte("a-fixed-32-byte-test-master-key")
	k1 := deriveFileLinkKey(master)
	k2 := deriveFileLinkKey(master)
	if len(k1) == 0 {
		t.Fatal("deriveFileLinkKey returned an empty key for a non-empty master key")
	}
	if string(k1) != string(k2) {
		t.Error("deriveFileLinkKey is not deterministic for the same master key")
	}
}

func TestFileLink_DeriveFileLinkKey_DifferentMasterKeysDiffer(t *testing.T) {
	k1 := deriveFileLinkKey([]byte("master-key-one-aaaaaaaaaaaaaaaaa"))
	k2 := deriveFileLinkKey([]byte("master-key-two-bbbbbbbbbbbbbbbbb"))
	if string(k1) == string(k2) {
		t.Error("deriveFileLinkKey produced the same key for two different master keys")
	}
}

func TestFileLink_DeriveFileLinkKey_EmptyMasterKeyDisablesMinting(t *testing.T) {
	if got := deriveFileLinkKey(nil); got != nil {
		t.Errorf("deriveFileLinkKey(nil) = %v, want nil", got)
	}
	if got := deriveFileLinkKey([]byte{}); got != nil {
		t.Errorf("deriveFileLinkKey([]byte{}) = %v, want nil", got)
	}
}

// ---- SignFileLink / VerifyFileLink round trip ----

// signedLinkFields is what a real caller would recover after parsing
// SignFileLink's URL back apart — used by the tamper tests below so each
// one can flip exactly one field and re-verify.
type signedLinkFields struct {
	orgID, connectorID, userID uuid.UUID
	fileID                     string
	exp                        int64
	sig                        string
}

func parseSignedLink(t *testing.T, link string) signedLinkFields {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse signed link: %v", err)
	}
	segs := strings.Split(strings.TrimPrefix(u.Path, "/mcp/files/"), "/")
	if len(segs) != 2 {
		t.Fatalf("unexpected path %q", u.Path)
	}
	connID, err := uuid.Parse(segs[0])
	if err != nil {
		t.Fatalf("connector id in path: %v", err)
	}
	fileID, err := url.PathUnescape(segs[1])
	if err != nil {
		t.Fatalf("unescape file id: %v", err)
	}
	q := u.Query()
	orgID, err := uuid.Parse(q.Get("org"))
	if err != nil {
		t.Fatalf("org query param: %v", err)
	}
	userID, err := uuid.Parse(q.Get("uid"))
	if err != nil {
		t.Fatalf("uid query param: %v", err)
	}
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil {
		t.Fatalf("exp query param: %v", err)
	}
	return signedLinkFields{
		orgID: orgID, connectorID: connID, userID: userID,
		fileID: fileID, exp: exp, sig: q.Get("sig"),
	}
}

func TestFileLink_SignThenVerify_RoundTrip(t *testing.T) {
	key := deriveFileLinkKey([]byte("test-master-key-32-bytes-long!!"))
	orgID, connID, userID := uuid.New(), uuid.New(), uuid.New()
	expiresAt := time.Now().Add(fileLinkTTL)

	link := SignFileLink(key, "https://api.example.com", orgID, connID, userID, "file-123", expiresAt)
	f := parseSignedLink(t, link)

	if !VerifyFileLink(key, f.orgID, f.connectorID, f.userID, f.fileID, f.exp, f.sig, time.Now()) {
		t.Fatal("VerifyFileLink rejected a freshly signed, unexpired link")
	}
}

func TestFileLink_SignFileLink_URLShape(t *testing.T) {
	key := deriveFileLinkKey([]byte("test-master-key-32-bytes-long!!"))
	orgID, connID, userID := uuid.New(), uuid.New(), uuid.New()
	expiresAt := time.Unix(1234567890, 0)

	link := SignFileLink(key, "https://api.example.com", orgID, connID, userID, "a file/with?special&chars", expiresAt)

	wantPrefix := "https://api.example.com/mcp/files/" + connID.String() + "/"
	if !strings.HasPrefix(link, wantPrefix) {
		t.Fatalf("link = %q, want prefix %q", link, wantPrefix)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	if q.Get("org") != orgID.String() {
		t.Errorf("org = %q, want %q", q.Get("org"), orgID.String())
	}
	if q.Get("uid") != userID.String() {
		t.Errorf("uid = %q, want %q", q.Get("uid"), userID.String())
	}
	if q.Get("exp") != "1234567890" {
		t.Errorf("exp = %q, want %q", q.Get("exp"), "1234567890")
	}
	if q.Get("sig") == "" {
		t.Error("sig is empty")
	}
	// The file id must have been path-escaped, not left to corrupt the URL
	// structure (a literal "/" or "?" in the path segment would otherwise
	// change what path/query Echo parses this into).
	fileSeg := strings.TrimPrefix(u.Path, "/mcp/files/"+connID.String()+"/")
	unescaped, err := url.PathUnescape(fileSeg)
	if err != nil {
		t.Fatalf("unescape file segment: %v", err)
	}
	if unescaped != "a file/with?special&chars" {
		t.Errorf("round-tripped file id = %q, want the original", unescaped)
	}
}

// ---- tampering ----

func TestFileLink_Verify_TamperedFieldFails(t *testing.T) {
	key := deriveFileLinkKey([]byte("test-master-key-32-bytes-long!!"))
	orgID, connID, userID := uuid.New(), uuid.New(), uuid.New()
	fileID := "file-123"
	exp := time.Now().Add(fileLinkTTL).Unix()
	sig := signFileLinkMessage(key, canonicalFileLinkMessage(orgID, connID, userID, fileID, exp))

	// Sanity: the untampered fields verify.
	if !VerifyFileLink(key, orgID, connID, userID, fileID, exp, sig, time.Now()) {
		t.Fatal("baseline signature failed to verify")
	}

	cases := []struct {
		name string
		mut  func() (uuid.UUID, uuid.UUID, uuid.UUID, string, int64)
	}{
		{"file id", func() (uuid.UUID, uuid.UUID, uuid.UUID, string, int64) {
			return orgID, connID, userID, "a-different-file", exp
		}},
		{"connector id", func() (uuid.UUID, uuid.UUID, uuid.UUID, string, int64) {
			return orgID, uuid.New(), userID, fileID, exp
		}},
		{"org id", func() (uuid.UUID, uuid.UUID, uuid.UUID, string, int64) {
			return uuid.New(), connID, userID, fileID, exp
		}},
		{"user id", func() (uuid.UUID, uuid.UUID, uuid.UUID, string, int64) {
			return orgID, connID, uuid.New(), fileID, exp
		}},
		{"exp", func() (uuid.UUID, uuid.UUID, uuid.UUID, string, int64) {
			return orgID, connID, userID, fileID, exp + 3600
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, c, u, f, e := tc.mut()
			if VerifyFileLink(key, o, c, u, f, e, sig, time.Now()) {
				t.Errorf("tampering with %s still verified", tc.name)
			}
		})
	}
}

func TestFileLink_Verify_TamperedSignatureFails(t *testing.T) {
	key := deriveFileLinkKey([]byte("test-master-key-32-bytes-long!!"))
	orgID, connID, userID := uuid.New(), uuid.New(), uuid.New()
	exp := time.Now().Add(fileLinkTTL).Unix()
	sig := signFileLinkMessage(key, canonicalFileLinkMessage(orgID, connID, userID, "file-123", exp))

	tampered := sig[:len(sig)-1] + "x"
	if tampered == sig {
		t.Fatal("test bug: tampered signature equals the original")
	}
	if VerifyFileLink(key, orgID, connID, userID, "file-123", exp, tampered, time.Now()) {
		t.Error("a tampered signature still verified")
	}
}

func TestFileLink_Verify_ExpiredFails(t *testing.T) {
	key := deriveFileLinkKey([]byte("test-master-key-32-bytes-long!!"))
	orgID, connID, userID := uuid.New(), uuid.New(), uuid.New()
	fileID := "file-123"
	exp := time.Now().Add(-1 * time.Minute).Unix() // already expired
	sig := signFileLinkMessage(key, canonicalFileLinkMessage(orgID, connID, userID, fileID, exp))

	if VerifyFileLink(key, orgID, connID, userID, fileID, exp, sig, time.Now()) {
		t.Error("an expired link verified")
	}
}

// TestFileLink_Verify_RejectsExpBeyondTTLCeiling proves the 15-minute
// ceiling is enforced by VerifyFileLink itself, not merely by SignFileLink's
// one caller always passing now.Add(fileLinkTTL). sig here is a genuinely
// valid signature over an exp that is not yet expired but claims a TTL well
// past the ceiling — VerifyFileLink must still reject it, since nothing
// about "not yet expired" alone proves "at most 15 minutes from now."
func TestFileLink_Verify_RejectsExpBeyondTTLCeiling(t *testing.T) {
	key := deriveFileLinkKey([]byte("test-master-key-32-bytes-long!!"))
	orgID, connID, userID := uuid.New(), uuid.New(), uuid.New()
	fileID := "file-123"
	now := time.Now()
	exp := now.Add(fileLinkTTL + 5*time.Minute).Unix() // valid-looking, but 20 minutes out
	sig := signFileLinkMessage(key, canonicalFileLinkMessage(orgID, connID, userID, fileID, exp))

	if VerifyFileLink(key, orgID, connID, userID, fileID, exp, sig, now) {
		t.Error("a validly signed link whose exp exceeds the 15-minute TTL ceiling still verified")
	}
}

// TestFileLink_Verify_ExpAtExactlyTheCeilingSucceeds is the boundary
// complement to the test above: an exp of exactly now+fileLinkTTL (what
// SignFileLink itself always produces) must still verify — the ceiling
// check must not be off-by-one in the strict direction and reject a
// legitimately minted link.
func TestFileLink_Verify_ExpAtExactlyTheCeilingSucceeds(t *testing.T) {
	key := deriveFileLinkKey([]byte("test-master-key-32-bytes-long!!"))
	orgID, connID, userID := uuid.New(), uuid.New(), uuid.New()
	fileID := "file-123"
	now := time.Now()
	exp := now.Add(fileLinkTTL).Unix()
	sig := signFileLinkMessage(key, canonicalFileLinkMessage(orgID, connID, userID, fileID, exp))

	if !VerifyFileLink(key, orgID, connID, userID, fileID, exp, sig, now) {
		t.Error("a link with exp exactly at now+fileLinkTTL failed to verify")
	}
}

func TestFileLink_Verify_DifferentKeyFails(t *testing.T) {
	keyA := deriveFileLinkKey([]byte("master-key-a-aaaaaaaaaaaaaaaaaaa"))
	keyB := deriveFileLinkKey([]byte("master-key-b-bbbbbbbbbbbbbbbbbbb"))
	orgID, connID, userID := uuid.New(), uuid.New(), uuid.New()
	fileID := "file-123"
	exp := time.Now().Add(fileLinkTTL).Unix()
	sig := signFileLinkMessage(keyA, canonicalFileLinkMessage(orgID, connID, userID, fileID, exp))

	if VerifyFileLink(keyB, orgID, connID, userID, fileID, exp, sig, time.Now()) {
		t.Error("a signature verified under a different key than it was signed with")
	}
}

func TestFileLink_Verify_EmptyKeyAlwaysFails(t *testing.T) {
	orgID, connID, userID := uuid.New(), uuid.New(), uuid.New()
	exp := time.Now().Add(fileLinkTTL).Unix()
	if VerifyFileLink(nil, orgID, connID, userID, "file-123", exp, "anything", time.Now()) {
		t.Error("VerifyFileLink with a nil key returned true")
	}
}
