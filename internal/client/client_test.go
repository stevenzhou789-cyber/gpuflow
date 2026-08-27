package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRequestHelpersAttachHeaders(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(filePath, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Session") != "session-1" {
			t.Errorf("missing request header on %s", r.URL.Path)
		}
		if r.URL.Path == "/json" {
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}
		if r.URL.Path == "/artifact" {
			if _, _, err := r.FormFile("file"); err != nil {
				t.Errorf("read multipart upload: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	headers := make(http.Header)
	headers.Set("X-Test-Session", "session-1")
	var response map[string]bool
	if _, err := New(server.URL, "").DoWithHeaders(http.MethodPost, "/json", map[string]string{"key": "value"}, &response, headers); err != nil {
		t.Fatal(err)
	}
	if !response["ok"] {
		t.Fatalf("unexpected JSON response: %+v", response)
	}
	if _, err := New(server.URL, "").UploadArtifactWithHeaders("/artifact", filePath, headers); err != nil {
		t.Fatal(err)
	}
}

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
