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

func TestV110MigrationsOnlyAppendToPublishedCoreSchema(t *testing.T) {
	published := map[string]string{
		"core/0001": "d6958b2c06246bcecb8aedf87b1be9bb20bebabd8fb09223e89faa4f3bc414cf",
		"core/0002": "7808ba697a331f409220f6a2a6ef9cba98bee80ab72a2432cde66d5ac7ae6632",
		"core/0003": "75272ef7383cc9624abf909af3a56fa4579ce50c1d0b9f5a859117a815c8a779",
		"core/0004": "eb3c89cd26b96324b7ad568724d4d7e4208ba671e2e81c45b026b54da026efae",
		"core/0005": "f3a80b47e5a4789f8d6b2ad88164b847dd738fa6884e97a9e83bd2309f8059c3",
		"core/0006": "54f2e6e66c5976ffbf3bbd6c1434316b02085c6517af84def935141a99193570",
		"core/0007": "ef190b3cc41f97ee140c0048b413a5e81fc8438ad7b6c20b811ef824d1ebff36",
		"core/0008": "d89e81cd8189a1532dd25b7f5cfc8faa0e206de08d7220494d037c9384ed2385",
		"core/0009": "b27257e0eca73b959ea6a98ab8bb83d44d99421541d44328e942fc2314581755",
	}
	for _, migration := range coreMigrations {
		if checksum, exists := published[migration.id]; exists && migration.checksum() != checksum {
			t.Fatalf("published migration %s changed checksum: got %s want %s", migration.id, migration.checksum(), checksum)
		}
	}
	if len(coreMigrations) < 13 {
		t.Fatalf("v1.1.0 migrations are missing: %+v", coreMigrations)
	}
	for index, id := range []string{"core/0010", "core/0011", "core/0012", "core/0013"} {
		if coreMigrations[index+9].id != id {
			t.Fatalf("v1.1.0 migrations are not an append-only 0010-0013 sequence: %+v", coreMigrations)
		}
	}
}
