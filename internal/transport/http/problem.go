// Package httpapi holds the HTTP transport: router, middleware and handlers.
// It translates application errors into RFC 7807 problem responses; no layer
// below transport knows about HTTP status codes.
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

// ProblemBaseURI namespaces problem `type` URIs.
const ProblemBaseURI = "https://proxui.dev/errors/"

// Problem is an RFC 7807 problem details document.
type Problem struct {
	Type      string            `json:"type"`
	Title     string            `json:"title"`
	Status    int               `json:"status"`
	Code      string            `json:"code"`
	Detail    string            `json:"detail,omitempty"`
	RequestID string            `json:"request_id,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
	// Body carries endpoint-specific detail that is useful precisely because
	// the call failed — a connection test's partial results, for instance,
	// where how far it got is more informative than the error itself.
	Body any `json:"body,omitempty"`
}

// WriteProblem renders an RFC 7807 response. code is the machine-readable
// identifier clients switch on (e.g. "rbac.console_denied").
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	WriteProblemFields(w, r, status, code, detail, nil)
}

// WriteProblemFields renders an RFC 7807 response carrying per-field validation errors.
func WriteProblemFields(w http.ResponseWriter, r *http.Request, status int, code, detail string, fields map[string]string) {
	p := Problem{
		Type:      ProblemBaseURI + code,
		Title:     http.StatusText(status),
		Status:    status,
		Code:      code,
		Detail:    detail,
		RequestID: middleware.GetReqID(r.Context()),
		Fields:    fields,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// WriteProblemWithBody renders a problem carrying extra structured detail.
func WriteProblemWithBody(w http.ResponseWriter, r *http.Request, status int, code, detail string, body any) {
	p := Problem{
		Type:      ProblemBaseURI + code,
		Title:     http.StatusText(status),
		Status:    status,
		Code:      code,
		Detail:    detail,
		RequestID: middleware.GetReqID(r.Context()),
		Body:      body,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// WriteJSON renders a successful JSON response.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
