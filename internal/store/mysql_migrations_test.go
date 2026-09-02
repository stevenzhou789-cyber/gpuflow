package store

import (
	"strings"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
)

func TestValidateAppliedCoreMigrations(t *testing.T) {
	known := []mysqlMigration{
		{id: "core/0001", statement: "CREATE TABLE one (id INT)"},
		{id: "core/0002", statement: "ALTER TABLE one ADD COLUMN name TEXT"},
	}
	valid := map[string]string{
		"core/0001":       known[0].checksum(),
		"enterprise/0001": "owned-by-the-enterprise-migrator",
	}
	if err := validateAppliedCoreMigrations(valid, known); err != nil {
		t.Fatalf("valid and extension-owned migrations were rejected: %v", err)
	}

	for name, applied := range map[string]map[string]string{
		"changed migration": {"core/0001": "different"},
		"newer database":    {"core/9999": "unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateAppliedCoreMigrations(applied, known); err == nil {
				t.Fatal("invalid migration history was accepted")
			}
		})
	}

	if err := validateAppliedCoreMigrations(nil, append(known, known[0])); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate migration definition was accepted: %v", err)
	}
	if err := validateAppliedCoreMigrations(nil, []mysqlMigration{{id: "enterprise/0001", statement: "SELECT 1"}}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("non-core migration definition was accepted: %v", err)
	}
}

func TestMigrationChecksumAndRestartableErrors(t *testing.T) {
	migration := mysqlMigration{statement: "ALTER TABLE jobs ADD COLUMN value INT", allowedErrorCodes: []uint16{1060}}
	if migration.checksum() == "" || migration.checksum() != migration.checksum() {
		t.Fatal("migration checksum is not deterministic")
	}
	changed := migration
	changed.statement += " NOT NULL"
	if migration.checksum() == changed.checksum() {
		t.Fatal("changed SQL retained the old checksum")
	}
	if !migration.allows(&mysqldriver.MySQLError{Number: 1060}) {
		t.Fatal("restartable duplicate-column result was rejected")
	}
	if migration.allows(&mysqldriver.MySQLError{Number: 1061}) {
		t.Fatal("unlisted MySQL error was accepted")
	}
}

func TestCoreMigrationDefinitionsAreAppendOnlyAndValid(t *testing.T) {
	if len(coreMigrations) == 0 {
		t.Fatal("core migration sequence is empty")
	}
	applied := make(map[string]string, len(coreMigrations))
	for _, migration := range coreMigrations {
		if migration.name == "" || migration.appVersion == "" || strings.TrimSpace(migration.statement) == "" {
			t.Fatalf("incomplete migration definition: %+v", migration)
		}
		applied[migration.id] = migration.checksum()
	}
	if err := validateAppliedCoreMigrations(applied, coreMigrations); err != nil {
		t.Fatalf("current migration sequence does not validate itself: %v", err)
	}
}
