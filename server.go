package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// APIError is a JSON error body: {"message": "..."} with an HTTP status.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return e.Message }

// Err builds an API error.
func Err(status int, msg string) *APIError { return &APIError{Status: status, Message: msg} }

// writeJSON answers with a JSON body. Every response carries
// X-GitHub-Request-Id (non-empty) and application/json.
func writeJSON(w http.ResponseWriter, s *Store, status int, v any) {
	text, err := json.Marshal(v)
	if err != nil {
		writeErr(w, s, Err(500, "Internal Server Error"))
		return
	}
	h := w.Header()
	h.Set("content-type", "application/json; charset=utf-8")
	h.Set("X-GitHub-Request-Id", s.NextReqID())
	w.WriteHeader(status)
	_, _ = w.Write(text)
	_, _ = w.Write([]byte("\n"))
}

// writeErr answers with {"message": "..."}.
func writeErr(w http.ResponseWriter, s *Store, err *APIError) {
	writeJSON(w, s, err.Status, map[string]string{"message": err.Message})
}

// readJSON decodes a JSON request body. Malformed JSON -> 400.
func readJSON(r *http.Request, dst any) *APIError {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		return Err(400, "Problems parsing JSON")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil // empty body: leave dst zero
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return Err(400, "Problems parsing JSON")
	}
	return nil
}

// Auth checks the Bearer token. Missing or wrong token -> false (caller
// answers 401). When the process was started without MAKEFUN_SHOP_TOKEN,
// any non-empty Bearer token is accepted (local testing only).
func (s *Store) Auth(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	if tok == "" {
		return false
	}
	if s.token == "" {
		return true
	}
	return tok == s.token
}

// requireAuth answers 401 JSON when the request carries no valid token.
func (s *Store) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.Auth(r) {
		return true
	}
	writeErr(w, s, Err(401, "Requires authentication"))
	return false
}

// notFoundJSON answers 404 {"message": ...} for unknown routes.
func notFoundJSON(w http.ResponseWriter, s *Store) {
	writeErr(w, s, Err(404, "Not Found"))
}

// methodNotAllowed answers 405 JSON.
func methodNotAllowed(w http.ResponseWriter, s *Store) {
	writeErr(w, s, Err(405, "Method Not Allowed"))
}
