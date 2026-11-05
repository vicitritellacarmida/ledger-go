// Package httpapi exposes the ledger over JSON/HTTP using the standard
// library router (Go 1.22 method and wildcard patterns).
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/google/uuid"

	"github.com/vicitritellacarmida/ledger-go/internal/service"
	"github.com/vicitritellacarmida/ledger-go/internal/store"
)

// API holds the dependencies of the HTTP handlers.
type API struct {
	svc          *service.Service
	store        *store.Store
	log          *slog.Logger
	maxBodyBytes int64
}

// Options configures the HTTP layer.
type Options struct {
	Logger       *slog.Logger
	MaxBodyBytes int64
}

// New builds the API.
func New(svc *service.Service, st *store.Store, opts Options) *API {
	a := &API{svc: svc, store: st, log: opts.Logger, maxBodyBytes: opts.MaxBodyBytes}
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.maxBodyBytes <= 0 {
		a.maxBodyBytes = 1 << 20
	}
	return a
}

// Handler returns the root handler with all routes and middleware.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	w := a.idempotent // every write endpoint goes through the idempotency layer

	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("GET /readyz", a.readyz)
	mux.HandleFunc("GET /v1/currencies", a.listCurrencies)

	mux.HandleFunc("POST /v1/accounts", w(a.createAccount))
	mux.HandleFunc("GET /v1/accounts", a.listAccounts)
	mux.HandleFunc("GET /v1/accounts/{code}", a.getAccount)
	mux.HandleFunc("POST /v1/accounts/{code}/status", w(a.changeAccountStatus))
	mux.HandleFunc("GET /v1/accounts/{code}/balance", a.getBalance)
	mux.HandleFunc("GET /v1/accounts/{code}/statement", a.getStatement)
	mux.HandleFunc("GET /v1/accounts/{code}/holds", a.listAccountHolds)

	mux.HandleFunc("POST /v1/entries", w(a.postEntry))
	mux.HandleFunc("GET /v1/entries", a.listEntries)
	mux.HandleFunc("GET /v1/entries/{id}", a.getEntry)
	mux.HandleFunc("POST /v1/entries/{id}/reverse", w(a.reverseEntry))

	mux.HandleFunc("POST /v1/holds", w(a.placeHold))
	mux.HandleFunc("GET /v1/holds/{id}", a.getHold)
	mux.HandleFunc("POST /v1/holds/{id}/capture", w(a.captureHold))
	mux.HandleFunc("POST /v1/holds/{id}/void", w(a.voidHold))
	mux.HandleFunc("POST /v1/holds/expire", w(a.expireHolds))

	mux.HandleFunc("POST /v1/transfers/topup", w(a.topup))
	mux.HandleFunc("POST /v1/transfers/payment", w(a.payment))
	mux.HandleFunc("POST /v1/transfers/payout", w(a.payout))
	mux.HandleFunc("POST /v1/transfers/refund", w(a.refund))

	mux.HandleFunc("GET /v1/reports/trial-balance", a.trialBalance)

	mux.HandleFunc("GET /v1/periods", a.listPeriods)
	mux.HandleFunc("POST /v1/periods/{period}/close", w(a.closePeriod))

	mux.HandleFunc("GET /v1/audit/verify", a.verifyChain)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		a.writeErrorCode(w, r, http.StatusNotFound, codeRouteNotFound, "no route for "+r.Method+" "+r.URL.Path)
	})

	return a.recoverer(a.requestLogger(a.withRequestID(a.limitBody(mux))))
}

type ctxKey int

const requestIDKey ctxKey = iota

var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// withRequestID reuses a caller's X-Request-Id when it looks safe, or
// generates one, and echoes it in the response.
func (a *API) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !safeRequestID.MatchString(id) {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func (a *API) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, a.maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (a *API) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		a.log.InfoContext(r.Context(), "http request",
			"method", r.Method, "path", r.URL.Path, "status", sw.status, "bytes", sw.bytes,
			"duration_ms", time.Since(start).Milliseconds(), "request_id", requestID(r.Context()))
	})
}

func (a *API) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				a.log.ErrorContext(r.Context(), "panic in handler", "panic", p, "stack", string(debug.Stack()))
				a.writeErrorCode(w, r, http.StatusInternalServerError, codeInternal, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *API) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.store.DB().PingContext(ctx); err != nil {
		a.writeErrorCode(w, r, http.StatusServiceUnavailable, "database_unavailable", "database is not reachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
