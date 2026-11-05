package migrate

import (
	"testing"
	"testing/fstest"

	"github.com/vicitritellacarmida/ledger-go/migrations"
)

func TestLoadSortsAndChecksums(t *testing.T) {
	fsys := fstest.MapFS{
		"0002_b.sql": {Data: []byte("SELECT 2;")},
		"0001_a.sql": {Data: []byte("SELECT 1;")},
		"notes.txt":  {Data: []byte("ignored")},
	}
	ms, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Version != "0001_a" || ms[1].Version != "0002_b" {
		t.Fatalf("unexpected order: %+v", ms)
	}
	if len(ms[0].Checksum) != 64 || ms[0].Checksum == ms[1].Checksum {
		t.Fatalf("bad checksums: %q %q", ms[0].Checksum, ms[1].Checksum)
	}
}

func TestEmbeddedMigrationsLoad(t *testing.T) {
	ms, err := Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) < 5 {
		t.Fatalf("expected at least 5 embedded migrations, got %d", len(ms))
	}
	if ms[0].Version != "0001_accounts" {
		t.Fatalf("first migration = %s", ms[0].Version)
	}
}
