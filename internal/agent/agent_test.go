package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuflow/internal/api"
	"gpuflow/internal/model"
	"gpuflow/internal/store"
)

func TestAgentDefaultsToAvailableLogicalCPUs(t *testing.T) {
	agent := New(Config{})
	if agent.cfg.CPUCores != runtime.NumCPU() || agent.cfg.CPUCores < 1 {
		t.Fatalf("unexpected reported CPU capacity: %d", agent.cfg.CPUCores)
	}
}

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

func TestLiveJobLogKeepsLatestOutput(t *testing.T) {
	log := &liveJobLog{}
	_, _ = log.Write([]byte(strings.Repeat("a", 40<<10)))
	_, _ = log.Write([]byte(strings.Repeat("b", 40<<10)))
	output := log.String()
	if len(output) != 64<<10 || !strings.HasSuffix(output, strings.Repeat("b", 40<<10)) {
		t.Fatalf("unexpected retained output: bytes=%d", len(output))
	}
}

func TestDockerGPUSelectorUsesAllocatedDevices(t *testing.T) {
	job := &model.Job{Requirements: model.Requirements{GPUCount: 2}, AllocatedGPUs: []int{1, 3}}
	if selector := dockerGPUSelector(job); selector != `"device=1,3"` {
		t.Fatalf("unexpected selector %q", selector)
	}
}

func TestTickKeepsHeartbeatAliveWhileExecuting(t *testing.T) {
	state := store.NewMemory()
	node, _ := state.RegisterNode(model.Node{ID: "heartbeat-node"})
	job, _ := state.CreateJob(model.JobCreate{Name: "heartbeat", Image: "alpine"})
	_ = state.Schedule(time.Minute)

	var heartbeats atomic.Int32
	apiHandler := api.New(state, "").Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/nodes/"+node.ID+"/heartbeat" {
			heartbeats.Add(1)
		}
		apiHandler.ServeHTTP(w, r)
	}))
	defer server.Close()

	agent := New(Config{Server: server.URL, ID: node.ID, Executor: "mock", HeartbeatInterval: 20 * time.Millisecond})
	if err := agent.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	completed, err := state.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != model.JobSucceeded {
		t.Fatalf("expected completed job, got %+v", completed)
	}
	if heartbeats.Load() < 2 {
		t.Fatalf("expected heartbeats during execution, got %d", heartbeats.Load())
	}
}
