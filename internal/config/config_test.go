package config

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

func lookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestRejectsPlaceholderDSN(t *testing.T) {
	_, err := FromLookup(lookup(nil))
	if err == nil || !strings.Contains(err.Error(), PasswordPlaceholder) {
		t.Fatalf("expected placeholder error, got %v", err)
	}
}

func TestDefaultsAndOverrides(t *testing.T) {
	cfg, err := FromLookup(lookup(map[string]string{
		"DATABASE_DSN":         "ledger:example@tcp(db:3306)/ledger?parseTime=true",
		"HTTP_ADDR":            ":9090",
		"CURRENCIES":           "idr, sgd",
		"HOLD_EXPIRY_INTERVAL": "0s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":9090" || cfg.HoldExpiryInterval != 0 || !cfg.MigrateOnStart {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if len(cfg.Currencies) != 2 || cfg.Currencies[1] != "SGD" {
		t.Fatalf("currencies = %v", cfg.Currencies)
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Fatalf("shutdown timeout = %v", cfg.ShutdownTimeout)
	}
}

func TestInvalidValuesAreReportedTogether(t *testing.T) {
	_, err := FromLookup(lookup(map[string]string{
		"DATABASE_DSN":     "x@tcp(db)/l",
		"LOG_LEVEL":        "loud",
		"MIGRATE_ON_START": "maybe",
		"CURRENCIES":       "IDR,XYZ",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"LOG_LEVEL", "MIGRATE_ON_START", "CURRENCIES"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestParseEnv(t *testing.T) {
	src := `# comment
export A=1
B = "two words"
C='single'
D=value # trailing comment

`
	vars, err := parseEnv(bufio.NewScanner(strings.NewReader(src)))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "two words", "C": "single", "D": "value"}
	for k, v := range want {
		if vars[k] != v {
			t.Errorf("%s = %q, want %q", k, vars[k], v)
		}
	}
	if _, err := parseEnv(bufio.NewScanner(strings.NewReader("NOEQUALS"))); err == nil {
		t.Error("expected error for a line without '='")
	}
}
