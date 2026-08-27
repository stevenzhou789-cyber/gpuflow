package platform

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"gpuflow/internal/api"
	"gpuflow/internal/artifact"
	"gpuflow/internal/store"
	"gpuflow/pkg/edition"
)

// ArtifactConfig is the public S3-compatible storage contract used by
// distributions that compose GPUFlow without importing internal packages.
type ArtifactConfig struct {
	Endpoint, AccessKey, SecretKey, Bucket, Region string
	UseSSL                                         bool
}

// ImagePublisher is invoked after a task image is built and returns the image
// reference that remote agents should use.
type ImagePublisher interface {
	Publish(context.Context, io.Writer, string) (string, error)
}

type Config struct {
	MySQLDSN       string
	Token          string
	Descriptor     edition.Descriptor
	Artifacts      ArtifactConfig
	ImagePublisher ImagePublisher
}

// NewHandler is the supported composition point for Community and Enterprise
// distributions. Enterprise code can add middleware without importing internal packages.
func NewHandler(mysqlDSN, token string, descriptor edition.Descriptor, artifactConfig artifact.Config) (http.Handler, error) {
	return NewHandlerWithConfig(Config{
		MySQLDSN: mysqlDSN, Token: token, Descriptor: descriptor,
		Artifacts: ArtifactConfig{
			Endpoint: artifactConfig.Endpoint, AccessKey: artifactConfig.AccessKey,
			SecretKey: artifactConfig.SecretKey, Bucket: artifactConfig.Bucket,
			Region: artifactConfig.Region, UseSSL: artifactConfig.UseSSL,
		},
	})
}

// NewHandlerWithConfig is the stable composition point for Enterprise and
// other distributions. Community continues to use NewHandler unchanged.
func NewHandlerWithConfig(cfg Config) (http.Handler, error) {
	if strings.TrimSpace(cfg.MySQLDSN) == "" {
		return nil, errors.New("GPUFLOW_MYSQL_DSN is required")
	}
	if strings.TrimSpace(cfg.Artifacts.Endpoint) == "" {
		return nil, errors.New("GPUFLOW_S3_ENDPOINT is required")
	}
	if _, err := edition.ParseExpiration(cfg.Descriptor.ExpiresAt); err != nil {
		return nil, err
	}
	if cfg.Descriptor.MaxNodes < 0 || cfg.Descriptor.MaxGPUs < 0 {
		return nil, errors.New("enterprise license capacity cannot be negative")
	}
	state, err := store.OpenMySQLStateStore(cfg.MySQLDSN)
	if err != nil {
		return nil, err
	}
	artifacts, err := artifact.Open(artifact.Config{
		Endpoint: cfg.Artifacts.Endpoint, AccessKey: cfg.Artifacts.AccessKey,
		SecretKey: cfg.Artifacts.SecretKey, Bucket: cfg.Artifacts.Bucket,
		Region: cfg.Artifacts.Region, UseSSL: cfg.Artifacts.UseSSL,
	})
	if err != nil {
		return nil, err
	}
	if !artifacts.Enabled() {
		return nil, errors.New("MinIO/S3 artifact storage is required")
	}
	return api.NewWithStoresAndPublisher(state, state, artifacts, cfg.Token, cfg.Descriptor, cfg.ImagePublisher).Handler(), nil
}
