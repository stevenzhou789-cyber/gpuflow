package agent

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveArtifacts(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "metrics"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metrics", "result.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, err := archiveArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(bundle)
	file, err := os.Open(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(gz)
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "metrics/result.txt" || string(content) != "ok\n" {
		t.Fatalf("unexpected archive entry %q: %q", header.Name, content)
	}
}

func TestArchiveArtifactsSkipsEmptyDirectory(t *testing.T) {
	bundle, err := archiveArtifacts(t.TempDir())
	if err != nil || bundle != "" {
		t.Fatalf("expected no bundle, got %q, %v", bundle, err)
	}
}
