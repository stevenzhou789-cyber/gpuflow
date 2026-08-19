package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUploadArtifactContextCanBeCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	defer server.CloseClientConnections()

	path := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := New(server.URL, "").UploadArtifactContext(ctx, "/upload", path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected upload deadline, got %v", err)
	}
}
