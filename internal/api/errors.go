package api

import (
	"encoding/json"
	"net/http"
)

// The unified error contract (KEEP ruling): every error surfaces as
//
//	{"error": {"code": ..., "message": ..., "details": ..., "trace_id": ...}}
//
// with the HTTP status derived from the normalized code
// (python api_server.py _ERROR_STATUS_MAP).
type apiError struct {
	Code    string
	Message string
	Details map[string]any
}

// errorStatus maps normalized error codes onto HTTP statuses; unknown
// codes fall back to 500 (python _ERROR_STATUS_MAP.get(code, 500)).
func errorStatus(code string) int {
	switch code {
	case "unauthorized":
		return http.StatusUnauthorized
	case "validation_error":
		return http.StatusUnprocessableEntity
	case "not_found", "source_not_bound":
		return http.StatusNotFound
	case "extract_failed", "all_candidates_failed":
		return http.StatusBadGateway
	case "provider_timeout":
		return http.StatusGatewayTimeout
	case "provider_403":
		return http.StatusForbidden
	case "geo_blocked":
		return http.StatusUnavailableForLegalReasons // 451
	case "conflict":
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// errUnauthorized and friends build the typed API errors used across
// handlers.
func errUnauthorized(message string) *apiError {
	return &apiError{Code: "unauthorized", Message: message}
}

func errValidation(message string, details map[string]any) *apiError {
	return &apiError{Code: "validation_error", Message: message, Details: details}
}

func errNotFound(message string) *apiError {
	return &apiError{Code: "not_found", Message: message}
}

func errInternal(message string) *apiError {
	return &apiError{Code: "internal_error", Message: message}
}

// errorPayload renders the wire shape; details/trace_id stay present as
// null when absent (python emits the keys unconditionally).
type errorPayload struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
	TraceID string         `json:"trace_id"`
}

// writeJSON writes a success payload.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeAPIError renders the unified error contract.
func writeAPIError(w http.ResponseWriter, r *http.Request, e *apiError) {
	writeJSON(w, errorStatus(e.Code), errorPayload{
		Error: errorBody{
			Code:    e.Code,
			Message: e.Message,
			Details: e.Details,
			TraceID: traceIDOf(r),
		},
	})
}
