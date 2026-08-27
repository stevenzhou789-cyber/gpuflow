package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"gpuflow/internal/model"

	mysqldriver "github.com/go-sql-driver/mysql"
)

const mysqlJobsSchema = `CREATE TABLE IF NOT EXISTS jobs (
  id VARCHAR(64) PRIMARY KEY,
  name VARCHAR(255) NOT NULL,
  image VARCHAR(512) NOT NULL,
  command_json JSON NOT NULL,
  environment_json JSON NOT NULL,
  requirements_json JSON NOT NULL,
  strategy VARCHAR(64) NOT NULL,
  timeout_seconds INT NOT NULL,
  max_retries INT NOT NULL,
  attempts INT NOT NULL,
  recoveries INT NOT NULL,
  status VARCHAR(32) NOT NULL,
  assigned_node VARCHAR(64) NOT NULL,
  assigned_session VARCHAR(64) NOT NULL DEFAULT '',
  attempt_token VARCHAR(64) NOT NULL DEFAULT '',
  lease_expires_at DATETIME(6) NULL,
  allocated_gpus_json JSON NULL,
  output MEDIUMTEXT NOT NULL,
  error_message TEXT NOT NULL,
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  started_at DATETIME(6) NULL,
  finished_at DATETIME(6) NULL,
  rerun_of VARCHAR(64) NOT NULL,
  INDEX idx_jobs_status_created (status, created_at),
  INDEX idx_jobs_assigned_node (assigned_node)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

const mysqlNodesSchema = `CREATE TABLE IF NOT EXISTS nodes (
  id VARCHAR(64) PRIMARY KEY,
  name VARCHAR(255) NOT NULL,
  provider VARCHAR(128) NOT NULL,
  pool VARCHAR(128) NOT NULL,
  gpu_model VARCHAR(255) NOT NULL,
  gpu_count INT NOT NULL,
  cpu_cores INT NOT NULL DEFAULT 0,
  vram_gb INT NOT NULL,
  hourly_price DOUBLE NOT NULL,
  labels_json JSON NOT NULL,
  details_json JSON NULL,
  busy BOOLEAN NOT NULL,
  current_job VARCHAR(64) NOT NULL,
  last_heartbeat DATETIME(6) NOT NULL,
  INDEX idx_nodes_pool_heartbeat (pool, last_heartbeat),
  INDEX idx_nodes_current_job (current_job)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

func OpenMySQLStateStore(dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql state: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect mysql state: %w", err)
	}
	for name, schema := range map[string]string{"jobs": mysqlJobsSchema, "nodes": mysqlNodesSchema} {
		if _, err := db.ExecContext(ctx, schema); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrate %s: %w", name, err)
		}
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN allocated_gpus_json JSON NULL AFTER assigned_node"); err != nil {
		var mysqlErr *mysqldriver.MySQLError
		if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1060 {
			_ = db.Close()
			return nil, fmt.Errorf("migrate job GPU allocations: %w", err)
		}
	}
	for _, migration := range []struct {
		statement string
		name      string
	}{
		{"ALTER TABLE jobs ADD COLUMN assigned_session VARCHAR(64) NOT NULL DEFAULT '' AFTER assigned_node", "job assigned session"},
		{"ALTER TABLE jobs ADD COLUMN attempt_token VARCHAR(64) NOT NULL DEFAULT '' AFTER assigned_session", "job attempt token"},
		{"ALTER TABLE jobs ADD COLUMN lease_expires_at DATETIME(6) NULL AFTER attempt_token", "job attempt lease"},
	} {
		if _, err := db.ExecContext(ctx, migration.statement); err != nil {
			var mysqlErr *mysqldriver.MySQLError
			if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1060 {
				_ = db.Close()
				return nil, fmt.Errorf("migrate %s: %w", migration.name, err)
			}
		}
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE nodes ADD COLUMN cpu_cores INT NOT NULL DEFAULT 0 AFTER gpu_count"); err != nil {
		var mysqlErr *mysqldriver.MySQLError
		if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1060 {
			_ = db.Close()
			return nil, fmt.Errorf("migrate node CPU capacity: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE nodes ADD COLUMN details_json JSON NULL AFTER labels_json"); err != nil {
		var mysqlErr *mysqldriver.MySQLError
		if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1060 {
			_ = db.Close()
			return nil, fmt.Errorf("migrate node inventory and health: %w", err)
		}
	}
	if err := (&MySQLTaskImageStore{db: db}).migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, state: snapshot{Jobs: map[string]*model.Job{}, Nodes: map[string]*model.Node{}, TaskImages: map[string]*model.TaskImage{}}}
	if err := s.loadMySQL(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) loadMySQL(ctx context.Context) error {
	const jobsQuery = `SELECT id, name, image, command_json, environment_json, requirements_json,
  strategy, timeout_seconds, max_retries, attempts, recoveries, status, assigned_node,
  assigned_session, attempt_token, lease_expires_at, allocated_gpus_json, output, error_message,
  created_at, updated_at, started_at, finished_at, rerun_of FROM jobs`
	rows, err := s.db.QueryContext(ctx, jobsQuery)
	if err != nil {
		return fmt.Errorf("load jobs: %w", err)
	}
	for rows.Next() {
		var job model.Job
		var commandJSON, environmentJSON, requirementsJSON, allocatedGPUsJSON []byte
		var leaseExpiresAt, startedAt, finishedAt sql.NullTime
		if err := rows.Scan(&job.ID, &job.Name, &job.Image, &commandJSON, &environmentJSON, &requirementsJSON,
			&job.Strategy, &job.TimeoutSeconds, &job.MaxRetries, &job.Attempts, &job.Recoveries, &job.Status,
			&job.AssignedNode, &job.AssignedSession, &job.AttemptToken, &leaseExpiresAt, &allocatedGPUsJSON,
			&job.Output, &job.Error, &job.CreatedAt, &job.UpdatedAt, &startedAt, &finishedAt, &job.RerunOf); err != nil {
			rows.Close()
			return fmt.Errorf("scan job: %w", err)
		}
		if err := decodeJobJSON(&job, commandJSON, environmentJSON, requirementsJSON); err != nil {
			rows.Close()
			return fmt.Errorf("decode job %s: %w", job.ID, err)
		}
		if len(allocatedGPUsJSON) > 0 {
			if err := json.Unmarshal(allocatedGPUsJSON, &job.AllocatedGPUs); err != nil {
				return fmt.Errorf("decode job %s GPU allocations: %w", job.ID, err)
			}
		}
		if startedAt.Valid {
			value := startedAt.Time
			job.StartedAt = &value
		}
		if leaseExpiresAt.Valid {
			value := leaseExpiresAt.Time
			job.LeaseExpiresAt = &value
		}
		if finishedAt.Valid {
			value := finishedAt.Time
			job.FinishedAt = &value
		}
		s.state.Jobs[job.ID] = &job
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate jobs: %w", err)
	}
	rows.Close()

	const nodesQuery = `SELECT id, name, provider, pool, gpu_model, gpu_count, cpu_cores, vram_gb,
  hourly_price, labels_json, details_json, busy, current_job, last_heartbeat FROM nodes`
	rows, err = s.db.QueryContext(ctx, nodesQuery)
	if err != nil {
		return fmt.Errorf("load nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var node model.Node
		var labelsJSON []byte
		var detailsJSON sql.RawBytes
		if err := rows.Scan(&node.ID, &node.Name, &node.Provider, &node.Pool, &node.GPUModel,
			&node.GPUCount, &node.CPUCores, &node.VRAMGB, &node.HourlyPrice, &labelsJSON, &detailsJSON, &node.Busy,
			&node.CurrentJob, &node.LastHeartbeat); err != nil {
			return fmt.Errorf("scan node: %w", err)
		}
		if err := json.Unmarshal(labelsJSON, &node.Labels); err != nil {
			return fmt.Errorf("decode node %s labels: %w", node.ID, err)
		}
		if len(detailsJSON) > 0 {
			var details nodeDetails
			if err := json.Unmarshal(detailsJSON, &details); err != nil {
				return fmt.Errorf("decode node %s details: %w", node.ID, err)
			}
			details.apply(&node)
		}
		s.state.Nodes[node.ID] = &node
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate nodes: %w", err)
	}
	for id := range s.state.Nodes {
		s.refreshNodeUsageLocked(id)
	}
	return nil
}

func decodeJobJSON(job *model.Job, commandJSON, environmentJSON, requirementsJSON []byte) error {
	if err := json.Unmarshal(commandJSON, &job.Command); err != nil {
		return fmt.Errorf("command: %w", err)
	}
	if err := json.Unmarshal(environmentJSON, &job.Environment); err != nil {
		return fmt.Errorf("environment: %w", err)
	}
	if err := json.Unmarshal(requirementsJSON, &job.Requirements); err != nil {
		return fmt.Errorf("requirements: %w", err)
	}
	return nil
}

const upsertJobSQL = `INSERT INTO jobs (id, name, image, command_json, environment_json,
  requirements_json, strategy, timeout_seconds, max_retries, attempts, recoveries, status,
  assigned_node, assigned_session, attempt_token, lease_expires_at, allocated_gpus_json,
  output, error_message, created_at, updated_at, started_at, finished_at, rerun_of)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE name=VALUES(name), image=VALUES(image), command_json=VALUES(command_json),
  environment_json=VALUES(environment_json), requirements_json=VALUES(requirements_json),
  strategy=VALUES(strategy), timeout_seconds=VALUES(timeout_seconds), max_retries=VALUES(max_retries),
  attempts=VALUES(attempts), recoveries=VALUES(recoveries), status=VALUES(status),
  assigned_node=VALUES(assigned_node), assigned_session=VALUES(assigned_session), attempt_token=VALUES(attempt_token),
  lease_expires_at=VALUES(lease_expires_at), allocated_gpus_json=VALUES(allocated_gpus_json), output=VALUES(output), error_message=VALUES(error_message),
  created_at=VALUES(created_at), updated_at=VALUES(updated_at), started_at=VALUES(started_at),
  finished_at=VALUES(finished_at), rerun_of=VALUES(rerun_of)`

const upsertNodeSQL = `INSERT INTO nodes (id, name, provider, pool, gpu_model, gpu_count,
  cpu_cores, vram_gb, hourly_price, labels_json, details_json, busy, current_job, last_heartbeat)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE name=VALUES(name), provider=VALUES(provider), pool=VALUES(pool),
  gpu_model=VALUES(gpu_model), gpu_count=VALUES(gpu_count), cpu_cores=VALUES(cpu_cores), vram_gb=VALUES(vram_gb),
  hourly_price=VALUES(hourly_price), labels_json=VALUES(labels_json), details_json=VALUES(details_json), busy=VALUES(busy),
  current_job=VALUES(current_job), last_heartbeat=VALUES(last_heartbeat)`

type nodeDetails struct {
	Devices         []model.GPUDevice `json:"devices,omitempty"`
	DriverVersion   string            `json:"driver_version,omitempty"`
	DockerVersion   string            `json:"docker_version,omitempty"`
	HealthStatus    string            `json:"health_status,omitempty"`
	HealthReason    string            `json:"health_reason,omitempty"`
	LastHealthCheck *time.Time        `json:"last_health_check,omitempty"`
	SessionEpoch    string            `json:"session_epoch,omitempty"`
}

func detailsFromNode(node *model.Node) nodeDetails {
	return nodeDetails{Devices: node.Devices, DriverVersion: node.DriverVersion, DockerVersion: node.DockerVersion, HealthStatus: node.HealthStatus, HealthReason: node.HealthReason, LastHealthCheck: node.LastHealthCheck, SessionEpoch: node.SessionEpoch}
}

func (details nodeDetails) apply(node *model.Node) {
	node.Devices, node.DriverVersion, node.DockerVersion = details.Devices, details.DriverVersion, details.DockerVersion
	node.HealthStatus, node.HealthReason, node.LastHealthCheck = details.HealthStatus, details.HealthReason, details.LastHealthCheck
	node.SessionEpoch = details.SessionEpoch
}

func (s *Store) saveMySQLChangesLocked(before snapshot) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin state transaction: %w", err)
	}
	defer tx.Rollback()
	for id := range before.Jobs {
		if _, exists := s.state.Jobs[id]; !exists {
			if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", id); err != nil {
				return fmt.Errorf("delete job %s: %w", id, err)
			}
		}
	}
	for id := range before.Nodes {
		if _, exists := s.state.Nodes[id]; !exists {
			if _, err := tx.ExecContext(ctx, "DELETE FROM nodes WHERE id = ?", id); err != nil {
				return fmt.Errorf("delete node %s: %w", id, err)
			}
		}
	}
	for _, job := range s.state.Jobs {
		if previous, exists := before.Jobs[job.ID]; exists && reflect.DeepEqual(previous, job) {
			continue
		}
		commandJSON, err := json.Marshal(job.Command)
		if err != nil {
			return fmt.Errorf("encode job %s command: %w", job.ID, err)
		}
		environmentJSON, err := json.Marshal(job.Environment)
		if err != nil {
			return fmt.Errorf("encode job %s environment: %w", job.ID, err)
		}
		requirementsJSON, err := json.Marshal(job.Requirements)
		if err != nil {
			return fmt.Errorf("encode job %s requirements: %w", job.ID, err)
		}
		allocatedGPUsJSON, err := json.Marshal(job.AllocatedGPUs)
		if err != nil {
			return fmt.Errorf("encode job %s GPU allocations: %w", job.ID, err)
		}
		if _, err := tx.ExecContext(ctx, upsertJobSQL, job.ID, job.Name, job.Image, commandJSON,
			environmentJSON, requirementsJSON, job.Strategy, job.TimeoutSeconds, job.MaxRetries,
			job.Attempts, job.Recoveries, job.Status, job.AssignedNode, job.AssignedSession, job.AttemptToken,
			nullableTime(job.LeaseExpiresAt), allocatedGPUsJSON, job.Output, job.Error,
			job.CreatedAt, job.UpdatedAt, nullableTime(job.StartedAt), nullableTime(job.FinishedAt), job.RerunOf); err != nil {
			return fmt.Errorf("insert job %s: %w", job.ID, err)
		}
	}
	for _, node := range s.state.Nodes {
		if previous, exists := before.Nodes[node.ID]; exists && reflect.DeepEqual(previous, node) {
			continue
		}
		labelsJSON, err := json.Marshal(node.Labels)
		if err != nil {
			return fmt.Errorf("encode node %s labels: %w", node.ID, err)
		}
		detailsJSON, err := json.Marshal(detailsFromNode(node))
		if err != nil {
			return fmt.Errorf("encode node %s details: %w", node.ID, err)
		}
		if _, err := tx.ExecContext(ctx, upsertNodeSQL, node.ID, node.Name, node.Provider, node.Pool,
			node.GPUModel, node.GPUCount, node.CPUCores, node.VRAMGB, node.HourlyPrice, labelsJSON, detailsJSON, node.Busy,
			node.CurrentJob, node.LastHeartbeat); err != nil {
			return fmt.Errorf("insert node %s: %w", node.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit state transaction: %w", err)
	}
	return nil
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}
