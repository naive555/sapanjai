package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/shared/apperror"
)

// newLoggerHarness builds a minimal Echo wired the way New does — custom error
// handler plus request logger — and captures the status the logger recorded.
func newLoggerHarness(t *testing.T, handler echo.HandlerFunc) (*echo.Echo, *int, *string) {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	loggedStatus := new(int)
	loggedURI := new(string)

	e := echo.New()
	e.HTTPErrorHandler = newErrorHandler(log)
	e.Use(requestLoggerWithSink(log, func(status int, uri string) {
		*loggedStatus = status
		*loggedURI = uri
	}))
	e.GET("/probe", handler)

	return e, loggedStatus, loggedURI
}

// TestRequestLoggerStatusMatchesResponse guards a regression that shipped once:
// Echo's RequestLogger reads res.Status before the error handler runs, and its
// built-in fallback only unwraps *echo.HTTPError. Every service returns
// *apperror.Error instead, so without HandleError:true each errored request was
// logged as 200 while the client got the real code.
func TestRequestLoggerStatusMatchesResponse(t *testing.T) {
	tests := []struct {
		name    string
		handler echo.HandlerFunc
		want    int
	}{
		{
			name:    "success",
			handler: func(c echo.Context) error { return c.NoContent(http.StatusOK) },
			want:    http.StatusOK,
		},
		{
			name:    "apperror 401",
			handler: func(c echo.Context) error { return apperror.New(apperror.InvalidCredentials) },
			want:    http.StatusUnauthorized,
		},
		{
			name:    "apperror 409",
			handler: func(c echo.Context) error { return apperror.New(apperror.EmailTaken) },
			want:    http.StatusConflict,
		},
		{
			name:    "apperror 429",
			handler: func(c echo.Context) error { return apperror.New(apperror.TooManyAttempts) },
			want:    http.StatusTooManyRequests,
		},
		{
			name:    "echo.HTTPError 400",
			handler: func(c echo.Context) error { return echo.NewHTTPError(http.StatusBadRequest, "Invalid request body") },
			want:    http.StatusBadRequest,
		},
		{
			name:    "unmapped error 500",
			handler: func(c echo.Context) error { return io.ErrUnexpectedEOF },
			want:    http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, loggedStatus, _ := newLoggerHarness(t, tt.handler)

			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe", nil))

			if rec.Code != tt.want {
				t.Errorf("client status = %d, want %d", rec.Code, tt.want)
			}
			if *loggedStatus != tt.want {
				t.Errorf("logged status = %d, want %d (log disagrees with client)", *loggedStatus, tt.want)
			}
		})
	}
}

// TestRequestLoggerSanitizesURI checks the redaction wiring end-to-end through
// the middleware, not just the SanitizeURI unit.
func TestRequestLoggerSanitizesURI(t *testing.T) {
	e, _, loggedURI := newLoggerHarness(t, func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe?token=SUPERSECRET&userId=abc", nil))

	if strings.Contains(*loggedURI, "SUPERSECRET") {
		t.Errorf("logged URI leaked the token: %s", *loggedURI)
	}
	if !strings.Contains(*loggedURI, "userId=abc") {
		t.Errorf("logged URI dropped a safe param: %s", *loggedURI)
	}
}
