package platform

import (
	"strings"
	"testing"

	"gpuflow/internal/artifact"
	"gpuflow/pkg/edition"
)

func TestHandlerRequiresMySQLAndArtifactStorage(t *testing.T) {
	if _, err := NewHandler("", "", edition.Community(), artifact.Config{Endpoint: "minio:9000"}); err == nil || !strings.Contains(err.Error(), "GPUFLOW_MYSQL_DSN") {
		t.Fatalf("expected required MySQL error, got %v", err)
	}
	if _, err := NewHandler("unused-dsn", "", edition.Community(), artifact.Config{}); err == nil || !strings.Contains(err.Error(), "GPUFLOW_S3_ENDPOINT") {
		t.Fatalf("expected required artifact storage error, got %v", err)
	}
}

func TestNewHandlerRejectsMalformedLicenseExpiration(t *testing.T) {
	descriptor := edition.Community()
	descriptor.ExpiresAt = "tomorrow"
	_, err := NewHandlerWithConfig(Config{MySQLDSN: "unused-dsn", Descriptor: descriptor, Artifacts: ArtifactConfig{Endpoint: "minio:9000"}})
	if err == nil || !strings.Contains(err.Error(), "expires_at") {
		t.Fatalf("unexpected error: %v", err)
	}
}
