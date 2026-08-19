package platform

import (
	"errors"
	"net/http"
	"strings"

	"gpuflow/internal/api"
	"gpuflow/internal/artifact"
	"gpuflow/internal/store"
	"gpuflow/pkg/edition"
)

// NewHandler is the supported composition point for Community and commercial
// distributions. Commercial code can add middleware without importing internal packages.
func NewHandler(mysqlDSN, token string, descriptor edition.Descriptor, artifactConfig artifact.Config) (http.Handler, error) {
	if strings.TrimSpace(mysqlDSN) == "" {
		return nil, errors.New("GPUFLOW_MYSQL_DSN is required")
	}
	if strings.TrimSpace(artifactConfig.Endpoint) == "" {
		return nil, errors.New("GPUFLOW_S3_ENDPOINT is required")
	}
	state, err := store.OpenMySQLStateStore(mysqlDSN)
	if err != nil {
		return nil, err
	}
	artifacts, err := artifact.Open(artifactConfig)
	if err != nil {
		return nil, err
	}
	if !artifacts.Enabled() {
		return nil, errors.New("MinIO/S3 artifact storage is required")
	}
	return api.NewWithStores(state, state, artifacts, token, descriptor).Handler(), nil
}
