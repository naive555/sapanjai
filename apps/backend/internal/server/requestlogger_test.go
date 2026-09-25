package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
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

// newJSONLoggerHarness is newLoggerHarness with a JSON handler over a buffer,
// so a test can assert the attributes the request logger emits rather than
// only the status the sink observed.
func newJSONLoggerHarness(t *testing.T) (*echo.Echo, *bytes.Buffer) {
	t.Helper()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	e := echo.New()
	e.HTTPErrorHandler = newErrorHandler(log)
	e.Use(requestLogger(log))

	return e, &buf
}

// logLine returns the decoded "request" record, skipping the separate
// "request failed" line newErrorHandler emits above 500.
func logLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			continue
		}
		if m["msg"] == "request" {
			return m
		}
	}
	t.Fatalf("no request log line in: %s", buf.String())
	return nil
}

func serve(e *echo.Echo, target string) {
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
}

// TestRequestLoggerFields covers what production debugging actually reads: the
// route pattern rather than the filled path, who the caller was, and an error
// code that does not drag the error's message along with it below 500.
func TestRequestLoggerFields(t *testing.T) {
	t.Run("route is the pattern not the path", func(t *testing.T) {
		e, buf := newJSONLoggerHarness(t)
		e.GET("/connectors/:id", func(c echo.Context) error { return c.NoContent(http.StatusNoContent) })
		serve(e, "/connectors/abc-123")

		m := logLine(t, buf)
		if got := m["route"]; got != "/connectors/:id" {
			t.Errorf("route = %v, want /connectors/:id", got)
		}
		// uri stays the filled path: route groups, uri identifies.
		if got := m["uri"]; got != "/connectors/abc-123" {
			t.Errorf("uri = %v, want /connectors/abc-123", got)
		}
	})

	t.Run("auth identity when set", func(t *testing.T) {
		e, buf := newJSONLoggerHarness(t)
		userID, orgID := uuid.New(), uuid.New()
		e.GET("/me", func(c echo.Context) error {
			c.Set("auth.userID", userID)
			c.Set("auth.orgID", orgID)
			return c.NoContent(http.StatusOK)
		})
		serve(e, "/me")

		m := logLine(t, buf)
		if got := m["user_id"]; got != userID.String() {
			t.Errorf("user_id = %v, want %s", got, userID)
		}
		if got := m["org_id"]; got != orgID.String() {
			t.Errorf("org_id = %v, want %s", got, orgID)
		}
	})

	t.Run("no auth identity when unset", func(t *testing.T) {
		e, buf := newJSONLoggerHarness(t)
		e.GET("/public", func(c echo.Context) error { return c.NoContent(http.StatusNoContent) })
		serve(e, "/public")

		m := logLine(t, buf)
		// Absent, not empty: an unauthenticated route must not emit a
		// zero uuid that a log query would count as a real caller.
		if _, ok := m["user_id"]; ok {
			t.Errorf("user_id present on an unauthenticated route: %v", m["user_id"])
		}
		if _, ok := m["org_id"]; ok {
			t.Errorf("org_id present on an unauthenticated route: %v", m["org_id"])
		}
	})

	t.Run("error code without message below 500", func(t *testing.T) {
		e, buf := newJSONLoggerHarness(t)
		e.GET("/missing", func(c echo.Context) error { return apperror.New(apperror.NotFound) })
		serve(e, "/missing")

		m := logLine(t, buf)
		if got := m["status"]; got != float64(http.StatusNotFound) {
			t.Errorf("status = %v, want 404", got)
		}
		if got := m["error_code"]; got != apperror.NotFound {
			t.Errorf("error_code = %v, want %s", got, apperror.NotFound)
		}
		// The message is 5xx-only: a 4xx is already named by its code, and an
		// error string can quote credential-adjacent upstream detail.
		if _, ok := m["error"]; ok {
			t.Errorf("error message logged on a 404: %v", m["error"])
		}
	})
}
