// Package config loads service settings from environment variables, with an
// optional .env file for local development. Real environment variables always
// win over values in the file.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	// Embed the time zone database so BUSINESS_TIMEZONE works on minimal
	// container images without /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
)

// PasswordPlaceholder is the marker used in example DSNs. The service refuses
// to start while the DSN still contains it.
const PasswordPlaceholder = "YOUR_PASSWORD_HERE"

// DefaultDSN is used when DATABASE_DSN is not set. It deliberately contains
// the placeholder so that a missing setting fails loudly at startup.
const DefaultDSN = "root:" + PasswordPlaceholder + "@tcp(127.0.0.1:3306)/ledger?parseTime=true&multiStatements=true"

// Config is the full service configuration.
type Config struct {
	// DatabaseDSN is a go-sql-driver/mysql DSN.
	DatabaseDSN string
	// HTTPAddr is the listen address, for example ":8080".
	HTTPAddr string
	// LogLevel is debug, info, warn or error.
	LogLevel string
	// LogFormat is "json" or "text".
	LogFormat string
	// BusinessTimezone decides which calendar day "today" is, for default
	// effective dates and period close rules.
	BusinessTimezone *time.Location
	// Currencies lists the currencies whose system accounts are seeded.
	Currencies []string
	// MigrateOnStart applies pending migrations when the server starts.
	MigrateOnStart bool
	// SeedChart creates missing system accounts when the server starts.
	SeedChart bool
	// HoldExpiryInterval is how often the background job expires due holds.
	// Zero disables the job.
	HoldExpiryInterval time.Duration
	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout time.Duration
	// MaxBodyBytes limits request body size.
	MaxBodyBytes int64
}

// Load reads the configuration. If envFile is not empty and the file exists,
// its KEY=VALUE lines are applied first for keys that are not already set in
// the environment.
func Load(envFile string) (*Config, error) {
	if envFile != "" {
		if err := loadEnvFile(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", envFile, err)
		}
	}
	return FromLookup(os.LookupEnv)
}

// FromLookup builds a Config from a lookup function. It is separate from Load
// so tests can pass a map instead of touching the process environment.
func FromLookup(lookup func(string) (string, bool)) (*Config, error) {
	get := func(key, def string) string {
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return def
	}
	var errs []error

	cfg := &Config{
		DatabaseDSN: get("DATABASE_DSN", DefaultDSN),
		HTTPAddr:    get("HTTP_ADDR", ":8080"),
		LogLevel:    strings.ToLower(get("LOG_LEVEL", "info")),
		LogFormat:   strings.ToLower(get("LOG_FORMAT", "json")),
	}
	if strings.Contains(cfg.DatabaseDSN, PasswordPlaceholder) {
		errs = append(errs, fmt.Errorf("DATABASE_DSN still contains %s: set DATABASE_DSN in the environment or in .env (see .env.example)", PasswordPlaceholder))
	}
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error, got %q", cfg.LogLevel))
	}
	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		errs = append(errs, fmt.Errorf("LOG_FORMAT must be json or text, got %q", cfg.LogFormat))
	}

	tz := get("BUSINESS_TIMEZONE", "Asia/Jakarta")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		errs = append(errs, fmt.Errorf("BUSINESS_TIMEZONE %q: %w", tz, err))
	}
	cfg.BusinessTimezone = loc

	for _, c := range strings.Split(get("CURRENCIES", "IDR,USD"), ",") {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		if _, err := domain.LookupCurrency(c); err != nil {
			errs = append(errs, fmt.Errorf("CURRENCIES: %w", err))
			continue
		}
		cfg.Currencies = append(cfg.Currencies, c)
	}

	parseBool := func(key string, def bool) bool {
		v := get(key, strconv.FormatBool(def))
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s must be true or false, got %q", key, v))
		}
		return b
	}
	parseDur := func(key, def string) time.Duration {
		v := get(key, def)
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Errorf("%s must be a non-negative duration such as 30s, got %q", key, v))
		}
		return d
	}
	cfg.MigrateOnStart = parseBool("MIGRATE_ON_START", true)
	cfg.SeedChart = parseBool("SEED_CHART", true)
	cfg.HoldExpiryInterval = parseDur("HOLD_EXPIRY_INTERVAL", "30s")
	cfg.ShutdownTimeout = parseDur("SHUTDOWN_TIMEOUT", "10s")

	maxBody := get("MAX_BODY_BYTES", "1048576")
	n, err := strconv.ParseInt(maxBody, 10, 64)
	if err != nil || n <= 0 {
		errs = append(errs, fmt.Errorf("MAX_BODY_BYTES must be a positive integer, got %q", maxBody))
	}
	cfg.MaxBodyBytes = n

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

// loadEnvFile sets variables from a simple .env file. Supported syntax:
// KEY=VALUE, optional "export " prefix, blank lines, # comments, and values
// wrapped in single or double quotes.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	vars, err := parseEnv(bufio.NewScanner(f))
	if err != nil {
		return err
	}
	for k, v := range vars {
		if _, exists := os.LookupEnv(k); !exists {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseEnv(sc *bufio.Scanner) (map[string]string, error) {
	out := make(map[string]string)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if n := len(value); n >= 2 && (value[0] == '"' && value[n-1] == '"' || value[0] == '\'' && value[n-1] == '\'') {
			value = value[1 : n-1]
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", lineNo)
		}
		out[key] = value
	}
	return out, sc.Err()
}
