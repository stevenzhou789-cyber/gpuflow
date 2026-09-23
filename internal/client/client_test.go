package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
			body, err := io.ReadAll(r.Body)
			_, params, dispositionErr := mime.ParseMediaType(r.Header.Get("Content-Disposition"))
			if err != nil || string(body) != "artifact" || r.ContentLength != 8 || r.Header.Get("Content-Type") != "application/octet-stream" || dispositionErr != nil || params["filename"] != "artifact.tar.gz" {
				t.Errorf("invalid streaming upload: length=%d disposition=%v body=%q err=%v", r.ContentLength, params, body, err)
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

type artifactRoundTripper func(*http.Request) (*http.Response, error)

func (f artifactRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUploadArtifactEarlyRejectionClosesFile(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "模型 权重.tar.gz")
	if err := os.WriteFile(filePath, []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	var body io.ReadCloser
	c := New("http://upload.invalid", "token")
	c.HTTP.Transport = artifactRoundTripper(func(r *http.Request) (*http.Response, error) {
		body = r.Body
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Disposition"))
		if err != nil || params["filename"] != filepath.Base(filePath) || r.ContentLength != 7 {
			t.Errorf("stream metadata: filename=%q length=%d err=%v", params["filename"], r.ContentLength, err)
		}
		return &http.Response{StatusCode: http.StatusConflict, Status: "409 Conflict", Body: io.NopCloser(strings.NewReader("stale attempt")), Header: make(http.Header)}, nil
	})
	status, err := c.UploadArtifact("/upload", filePath)
	if status != http.StatusConflict || err == nil || !strings.Contains(err.Error(), "stale attempt") {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if _, err := body.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("file retained after server rejection: %v", err)
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
