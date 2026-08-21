package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// This file exists because every other test in this package (service_test.go,
// tools_sheets_test.go, tools_drive_test.go) only ever exercised the
// download route's *rejection* paths — a bad signature, an expired exp, no
// query at all. Every one of those is green even if VerifyFileLink were
// broken *closed* in some other way (e.g. a mint-vs-serve mismatch on
// fileId's URL escaping, which would make the very first legitimately
// signed link 404 forever) — none of them prove a validly signed link
// actually gets past verification. This file is package mcp (not mcp_test)
// specifically so it can reach deriveFileLinkKey to build a signing key
// that matches a *Service's own derived one.

// recordingConnectorGetter is a connectorGetter test double that records
// the (organizationID, connectorID) its Get was called with, so a test can
// assert a request reached connector resolution — and, just as important,
// that a rejected request never did.
type recordingConnectorGetter struct {
	getCalled           bool
	gotOrgID, gotConnID uuid.UUID
	err                 error
}

func (r *recordingConnectorGetter) Get(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error) {
	r.getCalled = true
	r.gotOrgID = organizationID
	r.gotConnID = connectorID
	if r.err != nil {
		return db.Connector{}, r.err
	}
	return db.Connector{ID: connectorID, OrganizationID: organizationID}, nil
}

func (r *recordingConnectorGetter) OpenConfig(ctx context.Context, organizationID uuid.UUID, encryptedConfig json.RawMessage) (map[string]any, error) {
	return nil, errors.New("recordingConnectorGetter: OpenConfig not reachable in this test")
}

// testDownloadHandler builds a *Handler wired to a fresh recordingConnectorGetter
// and mounts it on its own echo.Echo, using a minimal error handler (this
// package cannot import internal/server's newErrorHandler — server imports
// mcp, not the reverse) that maps *apperror.Error/*echo.HTTPError to a real
// status code, enough for these tests to assert on.
func testDownloadHandler(t *testing.T, masterKey []byte) (*httptest.Server, *recordingConnectorGetter) {
	t.Helper()

	getter := &recordingConnectorGetter{err: errors.New("stop here — this test only cares whether Get was reached")}
	svc := NewService(getter, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), masterKey)
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))

	e := echo.New()
	e.HTTPErrorHandler = func(err error, c echo.Context) {
		if c.Response().Committed {
			return
		}
		status := http.StatusInternalServerError
		var appErr *apperror.Error
		var httpErr *echo.HTTPError
		switch {
		case errors.As(err, &appErr):
			status, _ = apperror.Resolve(appErr.Code)
		case errors.As(err, &httpErr):
			status = httpErr.Code
		}
		_ = c.NoContent(status)
	}
	h.Register(e.Group("/mcp"), func(next echo.HandlerFunc) echo.HandlerFunc { return next })

	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv, getter
}

// TestHandler_DownloadFile_ValidSignatureReachesConnectorResolution proves
// the positive path every other test in this package skips: a link signed
// by SignFileLink, with the exact key Service derives from masterKey, gets
// past VerifyFileLink and reaches connectorGetter.Get with exactly the
// link's own org and connector id — not some other value, and not skipped
// entirely.
func TestHandler_DownloadFile_ValidSignatureReachesConnectorResolution(t *testing.T) {
	masterKey := []byte("test-master-key-32-bytes-long!!")
	srv, getter := testDownloadHandler(t, masterKey)

	orgID, connID, userID := uuid.New(), uuid.New(), uuid.New()
	key := deriveFileLinkKey(masterKey)
	link := SignFileLink(key, srv.URL, orgID, connID, userID, "file-123", time.Now().Add(fileLinkTTL))

	resp, err := http.Get(link) //nolint:gosec // link is built in-process from a fixed test constant, not user input
	if err != nil {
		t.Fatalf("GET %s: %v", link, err)
	}
	defer resp.Body.Close()

	if !getter.getCalled {
		t.Fatal("connectorGetter.Get was never called — a validly signed link failed verification")
	}
	if getter.gotOrgID != orgID || getter.gotConnID != connID {
		t.Errorf("Get called with (org=%s, conn=%s), want (org=%s, conn=%s)", getter.gotOrgID, getter.gotConnID, orgID, connID)
	}
}

// TestHandler_DownloadFile_EscapedFileIDSurvivesSignAndServeRoundTrip is the
// specific regression this file exists to catch: fileId is URL-path-escaped
// by SignFileLink and unescaped by Echo when serving (Handler.downloadFile's
// c.Param("fileId")) — if those two disagree on the encoding even slightly,
// canonicalFileLinkMessage would be built from a different fileId at verify
// time than at sign time, and every download for a file id containing a
// space, slash, or other reserved character would 404 forever. A file id
// with several such characters is used deliberately, not a plain
// alphanumeric one that would pass even with escaping totally broken.
func TestHandler_DownloadFile_EscapedFileIDSurvivesSignAndServeRoundTrip(t *testing.T) {
	masterKey := []byte("test-master-key-32-bytes-long!!")
	srv, getter := testDownloadHandler(t, masterKey)

	orgID, connID, userID := uuid.New(), uuid.New(), uuid.New()
	const fileID = "café report (draft) #2.pdf"
	key := deriveFileLinkKey(masterKey)
	link := SignFileLink(key, srv.URL, orgID, connID, userID, fileID, time.Now().Add(fileLinkTTL))

	resp, err := http.Get(link) //nolint:gosec // see above
	if err != nil {
		t.Fatalf("GET %s: %v", link, err)
	}
	defer resp.Body.Close()

	if !getter.getCalled {
		t.Fatalf("connectorGetter.Get was never called — fileId %q did not survive the sign->serve round trip", fileID)
	}
}

// TestHandler_DownloadFile_TamperedFileIDNeverReachesConnectorGetter proves
// the other direction: flipping one character of fileId in the served URL
// (while leaving org/uid/exp/sig exactly as originally signed) must fail
// verification and never reach connectorGetter.Get at all — a signature
// that verifies against the wrong resource would be a far worse bug than
// one that never verifies.
func TestHandler_DownloadFile_TamperedFileIDNeverReachesConnectorGetter(t *testing.T) {
	masterKey := []byte("test-master-key-32-bytes-long!!")
	srv, getter := testDownloadHandler(t, masterKey)

	orgID, connID, userID := uuid.New(), uuid.New(), uuid.New()
	key := deriveFileLinkKey(masterKey)
	link := SignFileLink(key, srv.URL, orgID, connID, userID, "file-123", time.Now().Add(fileLinkTTL))

	tampered := strings.Replace(link, "file-123", "file-124", 1)
	if tampered == link {
		t.Fatal("test bug: substitution did not change the link")
	}

	resp, err := http.Get(tampered) //nolint:gosec // see above
	if err != nil {
		t.Fatalf("GET %s: %v", tampered, err)
	}
	defer resp.Body.Close()

	if getter.getCalled {
		t.Fatal("connectorGetter.Get was called for a link whose fileId was tampered with")
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
