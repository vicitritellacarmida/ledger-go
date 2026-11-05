package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
	"github.com/vicitritellacarmida/ledger-go/internal/store"
)

// Transport-level error codes. Business error codes come from the domain
// package.
const (
	codeInvalidJSON           = "invalid_json"
	codeBodyTooLarge          = "body_too_large"
	codeIdempotencyRequired   = "idempotency_key_required"
	codeIdempotencyReused     = "idempotency_key_reused"
	codeIdempotencyInProgress = "idempotency_key_in_progress"
	codeRouteNotFound         = "route_not_found"
	codeBusy                  = "service_busy"
	codeInternal              = "internal_error"
)

// ErrorBody is the JSON body of every error response:
//
//	{"error": {"code": "insufficient_funds", "message": "...", "details": {...}}, "request_id": "..."}
type ErrorBody struct {
	Error     ErrorDetail `json:"error"`
	RequestID string      `json:"request_id,omitempty"`
}

// ErrorDetail describes one error.
type ErrorDetail struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// errorObserver is implemented by the idempotency recorder, which needs to
// know the original error (for example to retry after a deadlock).
type errorObserver interface {
	observeError(err error)
}

// statusFor maps a domain error kind to an HTTP status.
func statusFor(kind domain.ErrorKind) int {
	switch kind {
	case domain.KindValidation:
		return http.StatusBadRequest
	case domain.KindNotFound:
		return http.StatusNotFound
	case domain.KindConflict:
		return http.StatusConflict
	case domain.KindRule:
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

func (a *API) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if o, ok := w.(errorObserver); ok {
		o.observeError(err)
	}
	body := ErrorBody{RequestID: requestID(r.Context())}
	var de *domain.Error
	var status int
	switch {
	case errors.As(err, &de):
		status = statusFor(de.Kind)
		body.Error = ErrorDetail{Code: de.Code, Message: de.Message, Details: de.Details}
	case store.IsRetryable(err):
		status = http.StatusServiceUnavailable
		body.Error = ErrorDetail{Code: codeBusy, Message: "the ledger is busy; retry the request with the same Idempotency-Key"}
	default:
		status = http.StatusInternalServerError
		body.Error = ErrorDetail{Code: codeInternal, Message: "internal server error"}
		a.log.ErrorContext(r.Context(), "request failed", "error", err, "method", r.Method, "path", r.URL.Path,
			slog.String("request_id", body.RequestID))
	}
	writeJSON(w, status, body)
}

func (a *API) writeErrorCode(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, status, ErrorBody{
		Error:     ErrorDetail{Code: code, Message: msg},
		RequestID: requestID(r.Context()),
	})
}

// requestError is a transport error with a fixed status.
type requestError struct {
	status int
	code   string
	msg    string
}

func (e *requestError) Error() string { return e.msg }

// decode reads a JSON object into dst. Unknown fields are rejected so that
// typos such as "ammount" fail loudly instead of being ignored. If optional
// is true an empty body is accepted.
func decode(r *http.Request, dst any, optional bool) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return &requestError{http.StatusRequestEntityTooLarge, codeBodyTooLarge, "request body is too large"}
		}
		return &requestError{http.StatusBadRequest, codeInvalidJSON, "could not read request body"}
	}
	if strings.TrimSpace(string(data)) == "" {
		if optional {
			return nil
		}
		return &requestError{http.StatusBadRequest, codeInvalidJSON, "request body must be a JSON object"}
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &requestError{http.StatusBadRequest, codeInvalidJSON, describeJSONError(err)}
	}
	if dec.More() {
		return &requestError{http.StatusBadRequest, codeInvalidJSON, "request body must contain a single JSON object"}
	}
	return nil
}

func describeJSONError(err error) string {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syn):
		return fmt.Sprintf("malformed JSON at byte %d", syn.Offset)
	case errors.As(err, &typ):
		return fmt.Sprintf("field %q must be a %s", typ.Field, typ.Type.String())
	case strings.HasPrefix(err.Error(), "json: unknown field"):
		return strings.TrimPrefix(err.Error(), "json: ")
	}
	return "malformed JSON: " + err.Error()
}

// fail writes err, which may be a *requestError or any other error.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	var re *requestError
	if errors.As(err, &re) {
		if o, ok := w.(errorObserver); ok {
			o.observeError(err)
		}
		a.writeErrorCode(w, r, re.status, re.code, re.msg)
		return
	}
	a.writeError(w, r, err)
}
