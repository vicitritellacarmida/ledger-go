// Package store is the MySQL persistence layer. It maps domain types to rows
// with plain database/sql and owns transaction handling.
//
// Transactions travel in the context: InTx starts one and puts it into the
// context it passes on, and every repository method uses the transaction
// from the context when there is one. This lets the service layer compose
// several repository calls, and lets the HTTP idempotency middleware wrap a
// whole request in one transaction, without passing *sql.Tx around.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
)

// MySQL server error numbers used by the store.
const (
	errDuplicateEntry   = 1062
	errLockWaitTimeout  = 1205
	errDeadlock         = 1213
	maxTxAttempts       = 4
	defaultMaxOpenConns = 25
)

// ErrDuplicate is returned when an insert violates a unique key that the
// caller is expected to handle (for example an idempotency key).
var ErrDuplicate = errors.New("store: duplicate key")

// dbtx is the subset of *sql.DB and *sql.Tx used by repository methods.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type txKey struct{}

// Store gives access to all ledger tables.
type Store struct {
	db *sql.DB
}

// New wraps an open database handle.
func New(db *sql.DB) *Store { return &Store{db: db} }

// DB returns the underlying handle.
func (s *Store) DB() *sql.DB { return s.db }

// Open parses a DSN, forces the settings the ledger depends on and verifies
// the connection:
//   - parseTime=true and loc=UTC, so DATETIME columns map to UTC time.Time;
//   - multiStatements=true, so migration files can hold several statements;
//   - session time_zone '+00:00', so UTC_TIMESTAMP and NOW agree.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_DSN: %w", err)
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.MultiStatements = true
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+00:00'"
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(defaultMaxOpenConns)
	db.SetMaxIdleConns(defaultMaxOpenConns)
	db.SetConnMaxLifetime(5 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to MySQL: %w", err)
	}
	return db, nil
}

func (s *Store) conn(ctx context.Context) dbtx {
	if tx, ok := ctx.Value(txKey{}).(*sql.Tx); ok {
		return tx
	}
	return s.db
}

// InTransaction reports whether ctx already carries a transaction.
func InTransaction(ctx context.Context) bool {
	_, ok := ctx.Value(txKey{}).(*sql.Tx)
	return ok
}

// Begin starts a READ COMMITTED transaction and returns a context carrying
// it. Callers that use Begin directly must Commit or Rollback the returned
// transaction themselves; most code should use InTx instead.
//
// READ COMMITTED matters here: after a transaction waits for a row lock, its
// next plain SELECT must see the rows committed by the transaction it waited
// for (for example earlier refunds of the same payment). Under MySQL's
// default REPEATABLE READ it would still see the older snapshot.
func (s *Store) Begin(ctx context.Context) (context.Context, *sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return ctx, nil, err
	}
	return context.WithValue(ctx, txKey{}, tx), tx, nil
}

// InTx runs fn inside a transaction. If ctx already carries a transaction,
// fn simply joins it and the outer owner decides on commit. Otherwise a new
// transaction is started, committed when fn returns nil, and retried a few
// times with a short random backoff on deadlock or lock wait timeout.
func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if InTransaction(ctx) {
		return fn(ctx)
	}
	var err error
	for attempt := 1; attempt <= maxTxAttempts; attempt++ {
		err = s.runTx(ctx, fn)
		if err == nil || !IsRetryable(err) {
			return err
		}
		backoff := time.Duration(attempt*10+rand.Intn(20)) * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return err
}

func (s *Store) runTx(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	txCtx, tx, err := s.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(txCtx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// IsRetryable reports whether err is a deadlock or lock wait timeout. MySQL
// rolls back the whole transaction on a deadlock, so the safe reaction is to
// run the transaction again from the start.
func IsRetryable(err error) bool {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number == errDeadlock || me.Number == errLockWaitTimeout
	}
	return false
}

func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == errDuplicateEntry
}

func notFound(code, format string, args ...any) *domain.Error {
	return domain.NewError(domain.KindNotFound, code, format, args...)
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func nullInt(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

func ptrInt(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// placeholders returns "?, ?, ?" with n markers.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, n*3)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',', ' ')
		}
		b = append(b, '?')
	}
	return string(b)
}
