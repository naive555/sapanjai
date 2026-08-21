package googlesheets

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This file is docs/07-sheets-adapter-plan.md step 9's required unit test
// suite. Every test name starts with TestDrive so
// `go test ./internal/adapter/googlesheets/ -run TestDrive` selects exactly
// this file. Only mockSheetsAPI (checker_test.go) is ever mocked here —
// none of these tests touch the network.

// fakeCharger (query_test.go) is reused here: allowUpTo charges succeed,
// every charge after that is denied with a fixed 30s retry-after.

// ---- drive_list_folder / listFolder ----

// TestDrive_ListFolder_RejectsUnallowlistedFolderBeforeAnyCall proves the
// allowlist check runs before any client construction or network call — the
// exported ListFolder entrypoint's own job, since listFolder (the seam
// below it) is never even reached. Uses untouchedAPI so any attempt to
// reach the network fails this test by construction.
func TestDrive_ListFolder_RejectsUnallowlistedFolderBeforeAnyCall(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{DriveFolderIDs: []string{"folder-allowed"}}}

	_, err := listFolderThroughAllowlistCheck(cfg, "folder-not-allowed")
	if !errors.Is(err, ErrFolderNotAllowed) {
		t.Fatalf("err = %v, want ErrFolderNotAllowed", err)
	}
	if got := err.Error(); got == "" {
		t.Fatal("error text is empty")
	}
}

// listFolderThroughAllowlistCheck exercises exactly ListFolder's own
// allowlist gate without needing a real oauth2.TokenSource — it stops
// before newClient is ever called, so it can pass a nil TokenSource safely.
func listFolderThroughAllowlistCheck(cfg *Config, folderID string) (*ListFolderOutput, error) {
	return ListFolder(context.Background(), nil, cfg, uuid.New(), nil, ListFolderInput{FolderID: folderID})
}

// TestDrive_ListFolder_PagingTokenPassthrough proves NextPageToken is
// echoed back exactly as the upstream page reported it.
func TestDrive_ListFolder_PagingTokenPassthrough(t *testing.T) {
	api := &mockSheetsAPI{
		t: t,
		listFiles: func(ctx context.Context, folderID string, pageToken string) (*FilePage, error) {
			if folderID != "folder-a" || pageToken != "page-in" {
				t.Errorf("ListFiles called with (%q, %q), want (folder-a, page-in)", folderID, pageToken)
			}
			return &FilePage{
				Files:         []File{{ID: "f1", Name: "one.csv", MimeType: "text/csv"}},
				NextPageToken: "page-out",
			}, nil
		},
	}

	out, err := listFolder(context.Background(), api, uuid.New(), nil, ListFolderInput{FolderID: "folder-a", PageToken: "page-in"})
	if err != nil {
		t.Fatalf("listFolder: %v", err)
	}
	if out.NextPageToken != "page-out" {
		t.Errorf("NextPageToken = %q, want %q", out.NextPageToken, "page-out")
	}
	if !out.HasMore {
		t.Error("HasMore = false, want true when NextPageToken is non-empty")
	}
	if out.FolderID != "folder-a" {
		t.Errorf("FolderID = %q, want %q", out.FolderID, "folder-a")
	}
	if len(out.Files) != 1 || out.Files[0].FileID != "f1" {
		t.Errorf("Files = %+v, want one file f1", out.Files)
	}
}

// TestDrive_ListFolder_NoNextPageTokenMeansNoMore proves HasMore is false
// when Drive reports no further page and the result is under the cap.
func TestDrive_ListFolder_NoNextPageTokenMeansNoMore(t *testing.T) {
	api := &mockSheetsAPI{
		t: t,
		listFiles: func(ctx context.Context, folderID string, pageToken string) (*FilePage, error) {
			return &FilePage{Files: []File{{ID: "f1"}}}, nil
		},
	}

	out, err := listFolder(context.Background(), api, uuid.New(), nil, ListFolderInput{FolderID: "folder-a"})
	if err != nil {
		t.Fatalf("listFolder: %v", err)
	}
	if out.HasMore {
		t.Error("HasMore = true, want false when Drive reports no NextPageToken and the page is under the cap")
	}
}

// TestDrive_ListFolder_TruncatesOverCapAndSetsHasMore proves the
// maxFolderFiles defensive cap truncates a page and forces HasMore true,
// even when Drive itself reported no NextPageToken.
func TestDrive_ListFolder_TruncatesOverCapAndSetsHasMore(t *testing.T) {
	files := make([]File, maxFolderFiles+10)
	for i := range files {
		files[i] = File{ID: "f", Name: "x"}
	}
	api := &mockSheetsAPI{
		t: t,
		listFiles: func(ctx context.Context, folderID string, pageToken string) (*FilePage, error) {
			return &FilePage{Files: files}, nil // no NextPageToken
		},
	}

	out, err := listFolder(context.Background(), api, uuid.New(), nil, ListFolderInput{FolderID: "folder-a"})
	if err != nil {
		t.Fatalf("listFolder: %v", err)
	}
	if len(out.Files) != maxFolderFiles {
		t.Fatalf("Files = %d entries, want exactly maxFolderFiles (%d)", len(out.Files), maxFolderFiles)
	}
	if !out.HasMore {
		t.Error("HasMore = false, want true once the page is truncated to the cap")
	}
}

// TestDrive_ListFolder_RateLimitExhaustion proves an exhausted bucket
// returns *RateLimitedError and never reaches ListFiles.
func TestDrive_ListFolder_RateLimitExhaustion(t *testing.T) {
	charger := &fakeCharger{allowUpTo: 0}
	api := untouchedAPI(t)

	_, err := listFolder(context.Background(), api, uuid.New(), charger, ListFolderInput{FolderID: "folder-a"})
	var rlErr *RateLimitedError
	if !errors.As(err, &rlErr) {
		t.Fatalf("err = %v, want *RateLimitedError", err)
	}
	if rlErr.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", rlErr.RetryAfter)
	}
	if charger.calls != 1 {
		t.Errorf("charger called %d times, want exactly 1", charger.calls)
	}
}

// ---- drive_get_file / getFile ----

// TestDrive_GetFile_RejectsFileWhoseParentsMissAllowlist proves a file that
// exists (fetches successfully) but whose parents don't intersect the
// connector's allowlist is rejected with ErrFileNotAllowed.
func TestDrive_GetFile_RejectsFileWhoseParentsMissAllowlist(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{DriveFolderIDs: []string{"folder-allowed"}}}
	api := &mockSheetsAPI{
		t: t,
		file: func(ctx context.Context, fileID string) (*File, error) {
			return &File{ID: fileID, Name: "secret.csv", Parents: []string{"folder-other"}}, nil
		},
	}

	_, err := getFile(context.Background(), api, cfg, uuid.New(), nil, "file-1")
	if !errors.Is(err, ErrFileNotAllowed) {
		t.Fatalf("err = %v, want ErrFileNotAllowed", err)
	}
	if got := err.Error(); got == "" {
		t.Fatal("error text is empty")
	}
}

// TestDrive_GetFile_AllowsFileWithAllowlistedDirectParent proves a file
// whose *direct* parent is on the allowlist is accepted, and one whose
// allowlisted ancestor is only indirect (a grandparent) is not — the
// "direct parents only, nested subfolders are not traversed" invariant
// ErrFileNotAllowed's doc comment states.
func TestDrive_GetFile_AllowsFileWithAllowlistedDirectParent(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{DriveFolderIDs: []string{"folder-allowed"}}}
	api := &mockSheetsAPI{
		t: t,
		file: func(ctx context.Context, fileID string) (*File, error) {
			return &File{
				ID: fileID, Name: "report.csv", MimeType: "text/csv",
				Parents: []string{"folder-allowed"}, SizeBytes: 1024, ModifiedTime: "2026-08-01T00:00:00Z",
			}, nil
		},
	}

	out, err := getFile(context.Background(), api, cfg, uuid.New(), nil, "file-1")
	if err != nil {
		t.Fatalf("getFile: %v", err)
	}
	if out.FileID != "file-1" || out.Name != "report.csv" || out.MimeType != "text/csv" ||
		out.SizeBytes != 1024 || out.ModifiedTime != "2026-08-01T00:00:00Z" {
		t.Errorf("out = %+v, want the fetched metadata verbatim", out)
	}
}

// TestDrive_GetFile_NestedSubfolderNotTraversed proves a file whose direct
// parent is an *unlisted* subfolder of an allowlisted folder is still
// rejected — the allowlist names direct parents only, never an ancestor
// chain.
func TestDrive_GetFile_NestedSubfolderNotTraversed(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{DriveFolderIDs: []string{"folder-top"}}}
	api := &mockSheetsAPI{
		t: t,
		file: func(ctx context.Context, fileID string) (*File, error) {
			// file-1 actually lives in "folder-sub", a child of the
			// allowlisted "folder-top" — but only folder-sub is its direct
			// parent, and folder-sub itself was never allowlisted.
			return &File{ID: fileID, Parents: []string{"folder-sub"}}, nil
		},
	}

	_, err := getFile(context.Background(), api, cfg, uuid.New(), nil, "file-1")
	if !errors.Is(err, ErrFileNotAllowed) {
		t.Fatalf("err = %v, want ErrFileNotAllowed for a file reachable only through an unlisted subfolder", err)
	}
}

// TestDrive_GetFile_UpstreamErrorBecomesNotFound proves any upstream File()
// failure — a real 404, a revoked share, anything sheetsAPI.File returns an
// error for — collapses to ErrFileNotFound rather than propagating upstream
// error text (which this package never surfaces past its own boundary).
func TestDrive_GetFile_UpstreamErrorBecomesNotFound(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{DriveFolderIDs: []string{"folder-allowed"}}}
	api := &mockSheetsAPI{
		t: t,
		file: func(ctx context.Context, fileID string) (*File, error) {
			return nil, errors.New("googleapi: Error 404: File not found: file-missing")
		},
	}

	_, err := getFile(context.Background(), api, cfg, uuid.New(), nil, "file-missing")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("err = %v, want ErrFileNotFound", err)
	}
}

// TestDrive_GetFile_RateLimitExhaustion proves an exhausted bucket returns
// *RateLimitedError before ever calling File().
func TestDrive_GetFile_RateLimitExhaustion(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{DriveFolderIDs: []string{"folder-allowed"}}}
	charger := &fakeCharger{allowUpTo: 0}
	api := untouchedAPI(t)

	_, err := getFile(context.Background(), api, cfg, uuid.New(), charger, "file-1")
	var rlErr *RateLimitedError
	if !errors.As(err, &rlErr) {
		t.Fatalf("err = %v, want *RateLimitedError", err)
	}
	if rlErr.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", rlErr.RetryAfter)
	}
}

// ---- IsGoogleNativeMimeType ----

func TestDrive_IsGoogleNativeMimeType(t *testing.T) {
	cases := []struct {
		mime string
		want bool
	}{
		{"application/vnd.google-apps.document", true},
		{"application/vnd.google-apps.spreadsheet", true},
		{"application/vnd.google-apps.folder", true},
		{"text/csv", false},
		{"application/pdf", false},
		{"image/png", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsGoogleNativeMimeType(tc.mime); got != tc.want {
			t.Errorf("IsGoogleNativeMimeType(%q) = %v, want %v", tc.mime, got, tc.want)
		}
	}
}
