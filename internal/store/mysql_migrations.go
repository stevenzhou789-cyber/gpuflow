package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	mysqldriver "github.com/go-sql-driver/mysql"
)

const (
	coreMigrationNamespace = "core/"
	coreMigrationLockName  = "gpuflow-core-schema"
	coreMigrationLockWait  = 10
)

const mysqlMigrationsSchema = `CREATE TABLE IF NOT EXISTS schema_migrations (
  id VARCHAR(191) PRIMARY KEY,
  checksum CHAR(64) NOT NULL,
  applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  app_version VARCHAR(64) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

type mysqlMigration struct {
	id                string
	name              string
	appVersion        string
	statement         string
	allowedErrorCodes []uint16
}

var coreMigrations = []mysqlMigration{
	{id: "core/0001", name: "create jobs", appVersion: "v1.0.20", statement: mysqlJobsSchema},
	{id: "core/0002", name: "create nodes", appVersion: "v1.0.20", statement: mysqlNodesSchema},
	{id: "core/0003", name: "add job GPU allocations", appVersion: "v1.0.20", statement: "ALTER TABLE jobs ADD COLUMN allocated_gpus_json JSON NULL AFTER assigned_node", allowedErrorCodes: []uint16{1060}},
	{id: "core/0004", name: "add job assigned session", appVersion: "v1.0.20", statement: "ALTER TABLE jobs ADD COLUMN assigned_session VARCHAR(64) NOT NULL DEFAULT '' AFTER assigned_node", allowedErrorCodes: []uint16{1060}},
	{id: "core/0005", name: "add job attempt token", appVersion: "v1.0.20", statement: "ALTER TABLE jobs ADD COLUMN attempt_token VARCHAR(64) NOT NULL DEFAULT '' AFTER assigned_session", allowedErrorCodes: []uint16{1060}},
	{id: "core/0006", name: "add job attempt lease", appVersion: "v1.0.20", statement: "ALTER TABLE jobs ADD COLUMN lease_expires_at DATETIME(6) NULL AFTER attempt_token", allowedErrorCodes: []uint16{1060}},
	{id: "core/0007", name: "add node CPU capacity", appVersion: "v1.0.20", statement: "ALTER TABLE nodes ADD COLUMN cpu_cores INT NOT NULL DEFAULT 0 AFTER gpu_count", allowedErrorCodes: []uint16{1060}},
	{id: "core/0008", name: "add node inventory and health", appVersion: "v1.0.20", statement: "ALTER TABLE nodes ADD COLUMN details_json JSON NULL AFTER labels_json", allowedErrorCodes: []uint16{1060}},
	{id: "core/0009", name: "create task images", appVersion: "v1.0.20", statement: mysqlTaskImagesSchema},
}

// runCoreMigrations serializes schema changes across control-plane instances,
// records every successful step, and refuses a changed or newer core schema.
// MySQL DDL auto-commits, so every step is deliberately restartable: if a
// process stops between DDL and recording it, the duplicate-column/table result
// is accepted on the next startup and the migration is then recorded.
func runCoreMigrations(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", coreMigrationLockName, coreMigrationLockWait).Scan(&acquired); err != nil {
		return fmt.Errorf("acquire schema migration lock: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return errors.New("acquire schema migration lock: timed out")
	}
	defer func() {
		var released sql.NullInt64
		_ = conn.QueryRowContext(context.Background(), "SELECT RELEASE_LOCK(?)", coreMigrationLockName).Scan(&released)
	}()

	if _, err := conn.ExecContext(ctx, mysqlMigrationsSchema); err != nil {
		return fmt.Errorf("create schema migrations table: %w", err)
	}
	applied, err := loadAppliedMigrations(ctx, conn)
	if err != nil {
		return err
	}
	if err := validateAppliedCoreMigrations(applied, coreMigrations); err != nil {
		return err
	}
	for _, migration := range coreMigrations {
		if _, ok := applied[migration.id]; ok {
			continue
		}
		if _, err := conn.ExecContext(ctx, migration.statement); err != nil && !migration.allows(err) {
			return fmt.Errorf("apply schema migration %s (%s): %w", migration.id, migration.name, err)
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO schema_migrations (id, checksum, app_version) VALUES (?, ?, ?)",
			migration.id, migration.checksum(), migration.appVersion); err != nil {
			return fmt.Errorf("record schema migration %s: %w", migration.id, err)
		}
	}
	return nil
}

func loadAppliedMigrations(ctx context.Context, conn *sql.Conn) (map[string]string, error) {
	rows, err := conn.QueryContext(ctx, "SELECT id, checksum FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("load schema migrations: %w", err)
	}
	defer rows.Close()
	applied := make(map[string]string)
	for rows.Next() {
		var id, checksum string
		if err := rows.Scan(&id, &checksum); err != nil {
			return nil, fmt.Errorf("scan schema migration: %w", err)
		}
		applied[id] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema migrations: %w", err)
	}
	return applied, nil
}

func validateAppliedCoreMigrations(applied map[string]string, known []mysqlMigration) error {
	checksums := make(map[string]string, len(known))
	for _, migration := range known {
		if !strings.HasPrefix(migration.id, coreMigrationNamespace) {
			return fmt.Errorf("invalid core migration id %q", migration.id)
		}
		if _, exists := checksums[migration.id]; exists {
			return fmt.Errorf("duplicate core migration id %q", migration.id)
		}
		checksums[migration.id] = migration.checksum()
	}
	for id, checksum := range applied {
		if !strings.HasPrefix(id, coreMigrationNamespace) {
			continue
		}
		expected, ok := checksums[id]
		if !ok {
			return fmt.Errorf("database contains unsupported core schema migration %q", id)
		}
		if checksum != expected {
			return fmt.Errorf("checksum mismatch for core schema migration %q", id)
		}
	}
	return nil
}

func (m mysqlMigration) checksum() string {
	sum := sha256.Sum256([]byte(m.statement))
	return hex.EncodeToString(sum[:])
}

func (m mysqlMigration) allows(err error) bool {
	var mysqlErr *mysqldriver.MySQLError
	if !errors.As(err, &mysqlErr) {
		return false
	}
	for _, code := range m.allowedErrorCodes {
		if mysqlErr.Number == code {
			return true
		}
	}
	return false
}
