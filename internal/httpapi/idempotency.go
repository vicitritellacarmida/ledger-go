package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"time"
	"unicode"

	"github.com/vicitritellacarmida/ledger-go/internal/store"
)

const (
	idempotencyHeader = "Idempotency-Key"
	replayedHeader    = "Idempotent-Replayed"
	maxIdemAttempts   = 4
)

// recorder buffers a handler's response so it can be stored with the
// idempotency key before anything reaches the client.
type recorder struct {
	header    http.Header
	status    int
	body      bytes.Buffer
	retryable bool
}

func newRecorder() *recorder { return &recorder{header: http.Header{}} }

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}
func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}
func (r *recorder) observeError(err error) {
	if store.IsRetryable(err) {
		r.retryable = true
	}
}

func (r *recorder) flush(w http.ResponseWriter) {
	for k, v := range r.header {
		w.Header()[k] = v
	}
	w.WriteHeader(r.status)
	_, _ = w.Write(r.body.Bytes())
}

// idempotent wraps a write handler.
//
// Flow for a request with Idempotency-Key K:
//
//  1. Start a transaction and INSERT a placeholder row for K. If another
//     request with K is still running, MySQL makes this INSERT wait on the
//     unique key until that transaction ends.
//  2. If the INSERT hits a duplicate, K was already used: replay the stored
//     response when the request fingerprint matches, otherwise answer 422.
//  3. Otherwise run the handler inside the same transaction (the service
//     joins it through the context), store its response on the row, and
//     commit. Business change and stored response commit together, so a
//     crash can never leave one without the other.
//
// 4xx responses are stored and replayed too, after their partial writes are
// rolled back to a savepoint. 5xx responses are not stored, so the client can
// retry with the same key.
func (a *API) idempotent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(idempotencyHeader)
		if key == "" {
			a.writeErrorCode(w, r, http.StatusBadRequest, codeIdempotencyRequired,
				"the Idempotency-Key header is required on write requests")
			return
		}
		if !validKey(key) {
			a.writeErrorCode(w, r, http.StatusBadRequest, codeIdempotencyRequired,
				"Idempotency-Key must be 1-255 printable ASCII characters")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			a.writeErrorCode(w, r, http.StatusRequestEntityTooLarge, codeBodyTooLarge, "request body is too large")
			return
		}
		fingerprint := requestHash(r.Method, r.URL.Path, body)

		for attempt := 1; attempt <= maxIdemAttempts; attempt++ {
			r.Body = io.NopCloser(bytes.NewReader(body))
			retry := a.runIdempotent(w, r, next, key, fingerprint)
			if !retry {
				return
			}
			time.Sleep(time.Duration(attempt*15+rand.Intn(25)) * time.Millisecond)
		}
		a.writeErrorCode(w, r, http.StatusServiceUnavailable, codeBusy,
			"the ledger is busy; retry the request with the same Idempotency-Key")
	}
}

// runIdempotent performs one attempt and reports whether it should be
// retried (deadlock or lock wait timeout).
func (a *API) runIdempotent(w http.ResponseWriter, r *http.Request, next http.HandlerFunc, key, fingerprint string) (retry bool) {
	ctx, tx, err := a.store.Begin(r.Context())
	if err != nil {
		a.writeError(w, r, err)
		return false
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback()
		}
	}()

	err = a.store.ReserveIdempotencyKey(ctx, &store.IdempotencyRecord{
		Key: key, Method: r.Method, Path: r.URL.Path, RequestHash: fingerprint, CreatedAt: time.Now().UTC(),
	})
	switch {
	case errors.Is(err, store.ErrDuplicate):
		_ = tx.Rollback()
		finished = true
		return a.replay(w, r, key, fingerprint)
	case store.IsRetryable(err):
		return true
	case err != nil:
		a.writeError(w, r, err)
		return false
	}

	if _, err := tx.ExecContext(ctx, "SAVEPOINT before_handler"); err != nil {
		a.writeError(w, r, err)
		return false
	}
	rec := newRecorder()
	next(rec, r.WithContext(ctx))
	if rec.retryable {
		return true
	}
	if rec.status >= 500 {
		rec.flush(w)
		return false
	}
	if rec.status >= 400 {
		if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT before_handler"); err != nil {
			if store.IsRetryable(err) {
				return true
			}
			a.writeError(w, r, err)
			return false
		}
	}
	if err := a.store.CompleteIdempotencyKey(ctx, key, rec.status, rec.body.Bytes()); err != nil {
		if store.IsRetryable(err) {
			return true
		}
		a.writeError(w, r, err)
		return false
	}
	if err := tx.Commit(); err != nil {
		finished = true
		if store.IsRetryable(err) {
			return true
		}
		a.writeError(w, r, err)
		return false
	}
	finished = true
	rec.flush(w)
	return false
}

func (a *API) replay(w http.ResponseWriter, r *http.Request, key, fingerprint string) (retry bool) {
	// Read outside the failed transaction so we see the committed row.
	rec, err := a.store.GetIdempotencyKey(context.WithoutCancel(r.Context()), key)
	if err != nil {
		a.writeError(w, r, err)
		return false
	}
	if rec == nil {
		// The other request rolled back after our insert failed; try again.
		return true
	}
	if rec.RequestHash != fingerprint {
		a.writeErrorCode(w, r, http.StatusUnprocessableEntity, codeIdempotencyReused,
			"this Idempotency-Key was already used with a different request")
		return false
	}
	if rec.ResponseStatus == 0 {
		a.writeErrorCode(w, r, http.StatusConflict, codeIdempotencyInProgress,
			"a request with this Idempotency-Key is still in progress")
		return false
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set(replayedHeader, "true")
	w.WriteHeader(rec.ResponseStatus)
	_, _ = w.Write(rec.ResponseBody)
	return false
}

func validKey(k string) bool {
	if len(k) == 0 || len(k) > 255 {
		return false
	}
	for _, c := range k {
		if c > unicode.MaxASCII || !unicode.IsPrint(c) {
			return false
		}
	}
	return true
}

// requestHash fingerprints a request: method, path and the body in
// canonical JSON form (object keys sorted, insignificant whitespace
// removed). Two bodies that differ only in key order or spacing therefore
// count as the same request. A body that is not valid JSON is hashed as is.
func requestHash(method, path string, body []byte) string {
	canon := body
	if len(bytes.TrimSpace(body)) > 0 {
		var v any
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&v); err == nil {
			if b, err := json.Marshal(v); err == nil {
				canon = b
			}
		}
	} else {
		canon = nil
	}
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(canon)
	return hex.EncodeToString(h.Sum(nil))
}
