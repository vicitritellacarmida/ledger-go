// Package testutil provides helpers for integration tests that need MySQL.
//
// Integration tests run only when TEST_DATABASE_DSN is set. The database
// name in that DSN must end with "_test"; each test package then uses its own
// database derived from it (for example ledger_test becomes
// ledger_service_test), because `go test ./...` runs packages in parallel.
// The helper drops and recreates that database, so it refuses any name that
// does not end with "_test" to protect real data.
package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/vicitritellacarmida/ledger-go/internal/migrate"
	"github.com/vicitritellacarmida/ledger-go/internal/store"
	"github.com/vicitritellacarmida/ledger-go/migrations"
)

// EnvDSN is the environment variable that enables integration tests.
const EnvDSN = "TEST_DATABASE_DSN"

var (
	once    sync.Once
	sharedD *sql.DB
	initErr error
	safeID  = regexp.MustCompile(`^[a-z0-9_]+$`)
)

// DatabaseName derives the per-package database name and validates it.
func DatabaseName(base, pkg string) (string, error) {
	if !strings.HasSuffix(base, "_test") {
		return "", fmt.Errorf("refusing to use database %q: the name must end with _test because the tests drop and recreate it", base)
	}
	name := strings.TrimSuffix(base, "_test") + "_" + pkg + "_test"
	if !safeID.MatchString(name) {
		return "", fmt.Errorf("database name %q may only contain a-z, 0-9 and _", name)
	}
	return name, nil
}

// DB returns a migrated, empty database for the calling test package, or
// skips the test when TEST_DATABASE_DSN is not set. pkg is a short package
// tag such as "store" or "service". The database is created once per test
// binary; call Reset at the start of each test for a clean state.
func DB(t testing.TB, pkg string) *sql.DB {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skipf("skipping integration test: set %s, for example %s='root@tcp(127.0.0.1:3306)/ledger_test'", EnvDSN, EnvDSN)
	}
	once.Do(func() { sharedD, initErr = setup(dsn, pkg) })
	if initErr != nil {
		t.Fatalf("integration database setup: %v", initErr)
	}
	Reset(t, sharedD)
	return sharedD
}

func setup(dsn, pkg string) (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", EnvDSN, err)
	}
	name, err := DatabaseName(cfg.DBName, pkg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	server := cfg.Clone()
	server.DBName = ""
	admin, err := sql.Open("mysql", server.FormatDSN())
	if err != nil {
		return nil, err
	}
	defer admin.Close()
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+name+"`"); err != nil {
		return nil, fmt.Errorf("drop %s: %w", name, err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		return nil, fmt.Errorf("create %s: %w", name, err)
	}

	cfg.DBName = name
	db, err := store.Open(ctx, cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	r := &migrate.Runner{DB: db, FS: migrations.FS}
	if _, err := r.Up(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", name, err)
	}
	return db, nil
}

// tables lists every table Reset empties, children before parents.
var tables = []string{"postings", "journal_entries", "holds", "accounts", "accounting_periods", "idempotency_keys", "chain_head"}

// Reset empties all ledger tables and restores the chain head row.
// TRUNCATE does not fire the immutability triggers, which is what we want.
func Reset(t testing.TB, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	defer conn.Close()
	stmts := []string{"SET FOREIGN_KEY_CHECKS = 0"}
	for _, tbl := range tables {
		stmts = append(stmts, "TRUNCATE TABLE "+tbl)
	}
	stmts = append(stmts,
		"SET FOREIGN_KEY_CHECKS = 1",
		"INSERT INTO chain_head (id, last_seq, last_hash, updated_at) VALUES (1, 0, REPEAT('0', 64), UTC_TIMESTAMP(6))",
	)
	for _, s := range stmts {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			t.Fatalf("reset: %s: %v", s, err)
		}
	}
}
