// Command ledger runs the double-entry ledger service.
//
// Usage:
//
//	ledger [serve]        start the HTTP server (default)
//	ledger migrate        apply pending database migrations and exit
//	ledger expire-holds   expire due holds once and exit
//
// Configuration comes from environment variables and an optional .env file
// in the working directory (see .env.example). Use -env to point at another
// file.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vicitritellacarmida/ledger-go/internal/config"
	"github.com/vicitritellacarmida/ledger-go/internal/httpapi"
	"github.com/vicitritellacarmida/ledger-go/internal/migrate"
	"github.com/vicitritellacarmida/ledger-go/internal/service"
	"github.com/vicitritellacarmida/ledger-go/internal/store"
	"github.com/vicitritellacarmida/ledger-go/migrations"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ledger:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("ledger", flag.ContinueOnError)
	envFile := fs.String("env", ".env", "path to an optional .env file")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ledger [-env FILE] [serve|migrate|expire-holds]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	cmd := "serve"
	if fs.NArg() > 0 {
		cmd = fs.Arg(0)
	}

	cfg, err := config.Load(*envFile)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	logger := newLogger(cfg)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	st := store.New(db)
	svc := service.New(st, service.Options{
		Location: cfg.BusinessTimezone, Logger: logger, Currencies: cfg.Currencies,
	})

	switch cmd {
	case "migrate":
		return migrateAndSeed(ctx, db, svc, logger, true)
	case "expire-holds":
		n, err := svc.ExpireDueHolds(ctx, 1000)
		if err != nil {
			return err
		}
		logger.Info("holds expired", "count", n)
		return nil
	case "serve":
		if cfg.MigrateOnStart {
			if err := migrateAndSeed(ctx, db, svc, logger, cfg.SeedChart); err != nil {
				return err
			}
		}
		return serve(ctx, cfg, st, svc, logger)
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// migrateAndSeed applies pending migrations and, if seed is true, creates
// missing system accounts of the default chart.
func migrateAndSeed(ctx context.Context, db *sql.DB, svc *service.Service, logger *slog.Logger, seed bool) error {
	r := &migrate.Runner{DB: db, FS: migrations.FS, Logger: logger}
	applied, err := r.Up(ctx)
	if err != nil {
		return err
	}
	logger.Info("migrations up to date", "applied_now", len(applied))
	if !seed {
		return nil
	}
	n, err := svc.EnsureChart(ctx)
	if err != nil {
		return fmt.Errorf("seed chart of accounts: %w", err)
	}
	logger.Info("chart of accounts ready", "created", n)
	return nil
}

func serve(ctx context.Context, cfg *config.Config, st *store.Store, svc *service.Service, logger *slog.Logger) error {
	api := httpapi.New(svc, st, httpapi.Options{Logger: logger, MaxBodyBytes: cfg.MaxBodyBytes})
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	jobCtx, cancelJobs := context.WithCancel(ctx)
	defer cancelJobs()
	if cfg.HoldExpiryInterval > 0 {
		go svc.RunHoldExpiry(jobCtx, cfg.HoldExpiryInterval)
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.HTTPAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	cancelJobs()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
