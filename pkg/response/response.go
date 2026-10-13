package response

import (
	"encoding/json"
	"net/http"
)

// ErrorBody is the standard API error envelope.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail describes a single error.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Details carries machine-readable context a client can act on (for
	// example the plan to upgrade to). Omitted when empty.
	Details map[string]any `json:"details,omitempty"`
}

// JSON writes a successful JSON response.
func JSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// Error writes a standard JSON error envelope.
func Error(w http.ResponseWriter, status int, code, message string) {
	JSON(w, status, ErrorBody{
		Error: ErrorDetail{
			Code:    code,
			Message: message,
		},
	})
}

// ErrorWithDetails is Error plus machine-readable context for the client.
func ErrorWithDetails(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	JSON(w, status, ErrorBody{
		Error: ErrorDetail{
			Code:    code,
			Message: message,
			Details: details,
		},
	})
}
