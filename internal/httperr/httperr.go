// Package httperr writes gateway-generated error responses.
//
// Every error the gateway itself produces is an RFC 9457 problem document, so a
// client can tell a gateway rejection from an upstream one without guessing.
package httperr

import (
	"encoding/json"
	"net/http"

	"github.com/rivencove/nina/internal/reqctx"
)

const ContentType = "application/problem+json"

// Problem is an RFC 9457 problem detail.
type Problem struct {
	Type     string `json:"type,omitempty"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
	// Errors carries field-level detail, used by request validation.
	Errors []FieldError `json:"errors,omitempty"`
	// RequestID lets an operator find this exact request in the logs.
	RequestID string `json:"request_id,omitempty"`
}

// FieldError locates one validation failure.
type FieldError struct {
	// In is "path", "query", "header", "cookie" or "body".
	In      string `json:"in"`
	Name    string `json:"name,omitempty"`
	Pointer string `json:"pointer,omitempty"`
	Message string `json:"message"`
}

// Write emits a problem document. It is a no-op if the response has started.
func Write(w http.ResponseWriter, r *http.Request, p Problem) {
	if p.Status == 0 {
		p.Status = http.StatusInternalServerError
	}
	if p.Title == "" {
		p.Title = http.StatusText(p.Status)
	}
	if info := reqctx.From(r.Context()); info != nil {
		p.RequestID = info.RequestID
		p.Instance = info.Template
	}
	body, err := json.Marshal(p)
	if err != nil {
		http.Error(w, http.StatusText(p.Status), p.Status)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ContentType)
	h.Del("Content-Length")
	w.WriteHeader(p.Status)
	_, _ = w.Write(body)
}

// Simple writes a problem with just a status and detail.
func Simple(w http.ResponseWriter, r *http.Request, status int, detail string) {
	Write(w, r, Problem{Status: status, Detail: detail})
}
