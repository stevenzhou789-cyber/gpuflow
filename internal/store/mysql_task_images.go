package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gpuflow/internal/model"

	_ "github.com/go-sql-driver/mysql"
)

type MySQLTaskImageStore struct {
	db *sql.DB
}

func OpenMySQLTaskImageStore(dsn string) (*MySQLTaskImageStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect mysql: %w", err)
	}
	store := &MySQLTaskImageStore{db: db}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *MySQLTaskImageStore) migrate(ctx context.Context) error {
	const schema = `CREATE TABLE IF NOT EXISTS task_images (
  id VARCHAR(64) PRIMARY KEY,
  name VARCHAR(255) NOT NULL UNIQUE,
  runtime VARCHAR(64) NOT NULL,
  base_image VARCHAR(512) NOT NULL,
  filename VARCHAR(255) NOT NULL,
  command_text TEXT NOT NULL,
  status VARCHAR(32) NOT NULL,
  build_log MEDIUMTEXT NOT NULL,
  error_message TEXT NOT NULL,
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  INDEX idx_task_images_status_created (status, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate task_images: %w", err)
	}
	return nil
}

func (s *MySQLTaskImageStore) SaveTaskImage(image model.TaskImage) error {
	const query = `INSERT INTO task_images
  (id, name, runtime, base_image, filename, command_text, status, build_log, error_message, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  id = VALUES(id), name = VALUES(name), runtime = VALUES(runtime), base_image = VALUES(base_image),
  filename = VALUES(filename), command_text = VALUES(command_text), status = VALUES(status),
	build_log = VALUES(build_log), error_message = VALUES(error_message),
	created_at = LEAST(created_at, VALUES(created_at)), updated_at = GREATEST(updated_at, VALUES(updated_at))`
	_, err := s.db.Exec(query, image.ID, image.Name, image.Runtime, image.BaseImage, image.Filename,
		image.Command, image.Status, image.Log, image.Error, image.CreatedAt, image.UpdatedAt)
	if err != nil {
		return fmt.Errorf("save task image: %w", err)
	}
	return nil
}

func (s *MySQLTaskImageStore) ListTaskImages() ([]model.TaskImage, error) {
	const query = `SELECT id, name, runtime, base_image, filename, command_text, status,
  build_log, error_message, created_at, updated_at
FROM task_images ORDER BY created_at DESC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("list task images: %w", err)
	}
	defer rows.Close()
	images := make([]model.TaskImage, 0)
	for rows.Next() {
		var image model.TaskImage
		if err := rows.Scan(&image.ID, &image.Name, &image.Runtime, &image.BaseImage, &image.Filename,
			&image.Command, &image.Status, &image.Log, &image.Error, &image.CreatedAt, &image.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan task image: %w", err)
		}
		images = append(images, image)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task images: %w", err)
	}
	return images, nil
}
