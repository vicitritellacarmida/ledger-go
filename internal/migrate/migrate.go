// Package migrate is a small, forward-only schema migration runner.
//
// Migrations are SQL files named NNNN_description.sql. They are applied in
// lexical order and recorded in the schema_migrations table together with a
// SHA-256 checksum. If an applied file is later edited, the runner refuses to
// continue, because the database no longer matches the code. A MySQL named
// lock (GET_LOCK) makes concurrent runners, such as several instances
// starting at once, take turns.
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"
)

const lockName = "ledger_schema_migrate"

// Migration is one SQL file.
type Migration struct {
	Version  string // file name without .sql, e.g. "0001_accounts"
	SQL      string
	Checksum string
}

// Load reads every *.sql file at the root of fsys, sorted by name.
func Load(fsys fs.FS) ([]Migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := make([]Migration, 0, len(names))
	for _, name := range names {
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		sum := sha256.Sum256(b)
		out = append(out, Migration{
			Version:  strings.TrimSuffix(path.Base(name), ".sql"),
			SQL:      string(b),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	return out, nil
}

// Runner applies migrations to a database. The connection must allow
// multiple statements per Exec (multiStatements=true in the MySQL DSN).
type Runner struct {
	DB     *sql.DB
	FS     fs.FS
	Logger *slog.Logger
}

// Up applies all pending migrations and returns the versions it applied.
func (r *Runner) Up(ctx context.Context) ([]string, error) {
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	migrations, err := Load(r.FS)
	if err != nil {
		return nil, err
	}

	// GET_LOCK is bound to a session, so all work must use one connection.
	conn, err := r.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 60)", lockName).Scan(&got); err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	if !got.Valid || got.Int64 != 1 {
		return nil, errors.New("could not acquire migration lock within 60s")
	}
	defer func() {
		// Use a fresh context so the lock is released even after cancellation.
		_, _ = conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", lockName)
	}()

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    VARCHAR(255) NOT NULL PRIMARY KEY,
		checksum   CHAR(64)     NOT NULL,
		applied_at DATETIME(6)  NOT NULL
	) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_bin`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(migrations))
	for _, m := range migrations {
		known[m.Version] = true
	}
	for v := range applied {
		if !known[v] {
			return nil, fmt.Errorf("database has migration %s that this binary does not know; refusing to continue", v)
		}
	}

	var done []string
	for _, m := range migrations {
		if sum, ok := applied[m.Version]; ok {
			if sum != m.Checksum {
				return done, fmt.Errorf("migration %s was changed after it was applied (checksum mismatch)", m.Version)
			}
			continue
		}
		start := time.Now()
		// MySQL commits DDL implicitly, so a migration cannot be rolled back
		// as a whole. Keep each file small and focused.
		if _, err := conn.ExecContext(ctx, m.SQL); err != nil {
			return done, fmt.Errorf("apply migration %s: %w", m.Version, err)
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO schema_migrations (version, checksum, applied_at) VALUES (?, ?, UTC_TIMESTAMP(6))",
			m.Version, m.Checksum); err != nil {
			return done, fmt.Errorf("record migration %s: %w", m.Version, err)
		}
		logger.Info("migration applied", "version", m.Version, "duration", time.Since(start).String())
		done = append(done, m.Version)
	}
	return done, nil
}

func appliedVersions(ctx context.Context, conn *sql.Conn) (map[string]string, error) {
	rows, err := conn.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var v, sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, err
		}
		out[v] = sum
	}
	return out, rows.Err()
}
