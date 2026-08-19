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
