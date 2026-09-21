package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v4"
)

type testValidator struct{ v *validator.Validate }

func (tv *testValidator) Validate(i any) error { return tv.v.Struct(i) }

type testBody struct {
	Confirm string `json:"confirm" validate:"required"`
}

func newCtx(method, body string) echo.Context {
	e := echo.New()
	e.Validator = &testValidator{v: validator.New()}
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	return e.NewContext(req, httptest.NewRecorder())
}

func TestBindAndValidate(t *testing.T) {
	tests := []struct {
		name        string
		fn          func(echo.Context, any) error
		method      string
		body        string
		wantCode    int
		wantMessage string
		wantConfirm string
	}{
		{"bind valid post", BindAndValidate, http.MethodPost, `{"confirm":"acme"}`, 0, "", "acme"},
		{"bind malformed post", BindAndValidate, http.MethodPost, `{"confirm":`, 400, "Invalid request body", ""},
		{"bind missing required", BindAndValidate, http.MethodPost, `{}`, 422, "Validation failed", ""},
		{"bindbody valid delete", BindBodyAndValidate, http.MethodDelete, `{"confirm":"acme"}`, 0, "", "acme"},
		{"bindbody malformed", BindBodyAndValidate, http.MethodDelete, `{"confirm":`, 400, "Invalid request body", ""},
		{"bindbody missing req", BindBodyAndValidate, http.MethodDelete, `{}`, 422, "Validation failed", ""},
		// Pins echo's current behavior: as of v4.15.4 DefaultBinder.Bind
		// gates only query-parameter binding on GET/DELETE/HEAD and calls
		// BindBody either way. Echo has moved this before, so if it moves
		// again it fails here rather than in DELETE
		// /admin/organizations/:orgId. See BindBodyAndValidate's doc comment.
		{"bind valid delete", BindAndValidate, http.MethodDelete, `{"confirm":"acme"}`, 0, "", "acme"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req testBody
			err := tt.fn(newCtx(tt.method, tt.body), &req)

			if tt.wantCode == 0 {
				if err != nil {
					t.Fatalf("expected nil error, got %v", err)
				}
				if req.Confirm != tt.wantConfirm {
					t.Errorf("expected Confirm to be %s, got %s", tt.wantConfirm, req.Confirm)
				}
			} else {
				var httpErr *echo.HTTPError
				if !errors.As(err, &httpErr) {
					t.Fatalf("expected *echo.HTTPError, got %v", err)
				}
				if httpErr.Code != tt.wantCode {
					t.Errorf("expected HTTP error code %d, got %d", tt.wantCode, httpErr.Code)
				}
				if httpErr.Message != tt.wantMessage {
					t.Errorf("expected HTTP error message %s, got %s", tt.wantMessage, httpErr.Message)
				}
			}
		})
	}
}
