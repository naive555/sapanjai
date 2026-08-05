package httpx

// ErrorResponse is the JSON shape returned for every error response,
// mirroring the { message: string } shape used by the source app. Lives
// here (rather than in internal/server, which no handler package imports)
// so Swagger annotations across every module can reference it by a package
// name that's already in scope.
type ErrorResponse struct {
	Message string `json:"message"`
}
