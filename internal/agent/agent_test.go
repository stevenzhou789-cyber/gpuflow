package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
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
	"gpuflow/pkg/edition"
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
	var complete bytes.Buffer
	log := &liveJobLog{full: &complete}
	_, _ = log.Write([]byte(strings.Repeat("a", 40<<10)))
	_, _ = log.Write([]byte(strings.Repeat("b", 40<<10)))
	output := log.String()
	if len(output) != 64<<10 || !strings.HasSuffix(output, strings.Repeat("b", 40<<10)) {
		t.Fatalf("unexpected retained output: bytes=%d", len(output))
	}
	if complete.Len() != 80<<10 || !strings.HasPrefix(complete.String(), strings.Repeat("a", 40<<10)) {
		t.Fatalf("complete log was truncated: bytes=%d", complete.Len())
	}
}

func TestDockerGPUSelectorUsesAllocatedDevices(t *testing.T) {
	job := &model.Job{Requirements: model.Requirements{GPUCount: 2}, AllocatedGPUs: []int{1, 3}}
	if selector := dockerGPUSelector(job); selector != `"device=1,3"` {
		t.Fatalf("unexpected selector %q", selector)
	}
}

func TestJobContainerNameIsAttemptScoped(t *testing.T) {
	if got := jobContainerName("job-123", 2); got != "gpuflow-job-job-123-2" {
		t.Fatalf("unexpected attempt container name %q", got)
	}
	if jobContainerName("job-123", 1) == jobContainerName("job-123", 2) {
		t.Fatal("different attempts must not share a container name")
	}
	if got := legacyJobContainerName("job-123"); got != "gpuflow-job-job-123" {
		t.Fatalf("unexpected legacy container name %q", got)
	}
}

func TestRunFailsClosedOnUnavailableOrMalformedCapabilities(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   any
	}{
		{name: "unavailable", status: http.StatusServiceUnavailable, body: map[string]string{"error": "unavailable"}},
		{name: "missing schema and features", status: http.StatusOK, body: map[string]string{"name": "enterprise"}},
		{name: "missing basic scheduler key", status: http.StatusOK, body: edition.Descriptor{SchemaVersion: edition.CapabilitiesSchemaVersion, Name: "enterprise", Features: map[string]bool{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var registrations atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/capabilities" {
					w.WriteHeader(test.status)
					_ = json.NewEncoder(w).Encode(test.body)
					return
				}
				if r.URL.Path == "/v1/nodes/register" {
					registrations.Add(1)
				}
				http.NotFound(w, r)
			}))
			defer server.Close()

			err := New(Config{Server: server.URL, Executor: "mock"}).Run(context.Background())
			if err == nil || (!strings.Contains(err.Error(), "capabilities") && !strings.Contains(err.Error(), "schema")) {
				t.Fatalf("expected capabilities failure, got %v", err)
			}
			if registrations.Load() != 0 {
				t.Fatalf("agent registered after capabilities failure")
			}
		})
	}
}

func TestRunUsesCapabilityProbeImageAndSession(t *testing.T) {
	descriptor := edition.Community()
	descriptor.Name = "enterprise"
	descriptor.AgentImage = "gpuflow-enterprise:test"
	descriptor.Features[edition.FeatureNodeHealth] = true
	descriptor.Features[edition.FeaturePerGPUInventory] = true
	registered := make(chan model.Node, 1)
	var session string
	var invalidHeaders atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestSession := r.Header.Get(model.HeaderAgentSession)
		if len(requestSession) != 32 {
			invalidHeaders.Add(1)
		}
		if session == "" {
			session = requestSession
		} else if requestSession != session {
			invalidHeaders.Add(1)
		}
		switch r.URL.Path {
		case "/v1/capabilities":
			_ = json.NewEncoder(w).Encode(descriptor)
		case "/v1/nodes/register":
			var node model.Node
			if err := json.NewDecoder(r.Body).Decode(&node); err != nil {
				t.Errorf("decode registration: %v", err)
			}
			_ = json.NewEncoder(w).Encode(node)
			registered <- node
		case "/v1/nodes/probe-node/next":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	var probeImageUsed atomic.Bool
	agent := New(Config{
		Server: server.URL, ID: "probe-node", Executor: "docker", PollInterval: time.Hour,
		ProbeCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "docker" {
				return nil, errors.New("unexpected native GPU probe")
			}
			if len(args) > 0 && args[0] == "version" {
				return []byte("27.1.0"), nil
			}
			if strings.Contains(strings.Join(args, " "), descriptor.AgentImage) {
				probeImageUsed.Store(true)
			}
			return []byte("0, GPU-a, NVIDIA L4, 23034, 550.54\n"), nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	select {
	case node := <-registered:
		if node.GPUCount != 1 || len(node.Devices) != 1 {
			t.Fatalf("unexpected registered inventory: %+v", node)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not register")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected Run result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not stop")
	}
	if !probeImageUsed.Load() || agent.cfg.ProbeImage != descriptor.AgentImage {
		t.Fatalf("capability agent image was not used: %q", agent.cfg.ProbeImage)
	}
	if invalidHeaders.Load() != 0 {
		t.Fatalf("invalid agent session headers: %d", invalidHeaders.Load())
	}
}

func TestWorkerSupervisorScalesUp(t *testing.T) {
	entered := make(chan struct{}, 4)
	var concurrent, maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(model.HeaderAgentSession) != "session" {
			t.Errorf("missing agent session")
		}
		current := concurrent.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		entered <- struct{}{}
		<-r.Context().Done()
		concurrent.Add(-1)
	}))
	defer server.Close()

	agent := New(Config{Server: server.URL, ID: "node", PollInterval: time.Hour})
	agent.session = "session"
	agent.workerTargets = make(chan int, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		agent.workerSupervisor(ctx, 1)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("initial worker did not poll")
	}
	agent.workerTargets <- 3
	for index := 0; index < 2; index++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("additional worker did not poll")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker supervisor did not stop")
	}
	if maximum.Load() != 3 {
		t.Fatalf("expected three concurrent workers, got %d", maximum.Load())
	}
}

func TestRunFailStopsWhenControlPlaneRejectsSession(t *testing.T) {
	descriptor := edition.Community()
	var heartbeats atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			_ = json.NewEncoder(w).Encode(descriptor)
		case "/v1/nodes/register":
			var node model.Node
			_ = json.NewDecoder(r.Body).Decode(&node)
			_ = json.NewEncoder(w).Encode(node)
		case "/v1/nodes/session-node/heartbeat":
			heartbeats.Add(1)
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid agent session"})
		case "/v1/nodes/session-node/next":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := New(Config{Server: server.URL, ID: "session-node", Executor: "mock", PollInterval: time.Hour, HeartbeatInterval: 10 * time.Millisecond}).Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "agent session rejected") {
		t.Fatalf("expected session fail-stop, got %v", err)
	}
	if heartbeats.Load() == 0 {
		t.Fatal("heartbeat did not exercise session rejection")
	}
}

func TestRunFailStopsWhenHeartbeatLeaseExpires(t *testing.T) {
	descriptor := edition.Community()
	var heartbeats atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			_ = json.NewEncoder(w).Encode(descriptor)
		case "/v1/nodes/register":
			var node model.Node
			_ = json.NewDecoder(r.Body).Decode(&node)
			_ = json.NewEncoder(w).Encode(node)
		case "/v1/nodes/partitioned-node/heartbeat":
			if heartbeats.Add(1) == 1 {
				w.WriteHeader(http.StatusOK)
				return
			}
			http.Error(w, "control plane unavailable", http.StatusServiceUnavailable)
		case "/v1/nodes/partitioned-node/next":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	agent := New(Config{Server: server.URL, ID: "partitioned-node", Executor: "mock", PollInterval: time.Hour, HeartbeatInterval: 10 * time.Millisecond})
	agent.sessionTTL = 60 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := agent.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "session lease expired") {
		t.Fatalf("expected heartbeat lease fail-stop, got %v", err)
	}
}

func TestTerminalUpdateAcknowledgesConcurrentCancellation(t *testing.T) {
	var posted []model.JobStatus
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var update model.JobUpdate
			_ = json.NewDecoder(r.Body).Decode(&update)
			posted = append(posted, update.Status)
			if update.Status == model.JobCanceled {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "job is canceling"})
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(model.Job{ID: "job-cancel-race", Status: model.JobCanceling})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	agent := New(Config{Server: server.URL, ID: "node", Executor: "mock"})
	agent.session = "session"
	job := model.Job{ID: "job-cancel-race", Attempts: 1}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := agent.updateJobStatusUntilAccepted(ctx, &job, "attempt", model.JobUpdate{Status: model.JobSucceeded}); err != nil {
		t.Fatal(err)
	}
	if len(posted) != 2 || posted[0] != model.JobSucceeded || posted[1] != model.JobCanceled {
		t.Fatalf("expected terminal update to converge on cancellation, got %v", posted)
	}
}

func TestHealthLoopPreservesKnownInventoryWhenGPUDisappears(t *testing.T) {
	updates := make(chan model.NodeHealthUpdate, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(model.HeaderAgentSession) != "session" {
			t.Errorf("missing agent session")
		}
		var update model.NodeHealthUpdate
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Errorf("decode health update: %v", err)
		}
		updates <- update
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	agent := New(Config{
		Server: server.URL, ID: "health-node", Executor: "docker", HealthInterval: 10 * time.Millisecond,
		ProbeCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "docker" {
				return []byte("27.1.0"), nil
			}
			return []byte("0, GPU-a, NVIDIA L4, 23034, 550.54\n"), nil
		},
	})
	agent.session = "session"
	agent.baseline = model.Node{
		GPUModel: "NVIDIA L4", GPUCount: 2, VRAMGB: 23,
		Devices: []model.GPUDevice{
			{Index: 0, UUID: "GPU-a", Model: "NVIDIA L4", VRAMGB: 23},
			{Index: 1, UUID: "GPU-b", Model: "NVIDIA L4", VRAMGB: 23},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		agent.healthLoop(ctx)
		close(done)
	}()
	select {
	case update := <-updates:
		if update.Status != "DEGRADED" || update.GPUCount != 2 || len(update.Devices) != 2 || !strings.Contains(update.Reason, "dropped") {
			t.Fatalf("known GPU inventory was not preserved: %+v", update)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("health update was not sent")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("health loop did not stop")
	}
}

func TestTickKeepsHeartbeatAliveWhileExecuting(t *testing.T) {
	state := store.NewMemory()
	node, _ := state.RegisterNode(model.Node{ID: "heartbeat-node"})
	job, _ := state.CreateJob(model.JobCreate{Name: "heartbeat", Image: "alpine"})
	_ = state.Schedule(time.Minute)

	var heartbeats, invalidHeaders atomic.Int32
	apiHandler := api.New(state, "").Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/nodes/"+node.ID+"/") && r.Header.Get(model.HeaderAgentSession) != node.SessionEpoch {
			invalidHeaders.Add(1)
		}
		if strings.HasPrefix(r.URL.Path, "/v1/jobs/"+job.ID) && (r.Header.Get(model.HeaderAgentSession) != node.SessionEpoch || r.Header.Get(model.HeaderAttemptToken) == "") {
			invalidHeaders.Add(1)
		}
		if r.URL.Path == "/v1/nodes/"+node.ID+"/heartbeat" {
			heartbeats.Add(1)
		}
		apiHandler.ServeHTTP(w, r)
	}))
	defer server.Close()

	agent := New(Config{Server: server.URL, ID: node.ID, Executor: "mock", HeartbeatInterval: 20 * time.Millisecond})
	agent.session = node.SessionEpoch
	ctx, cancel := context.WithCancel(context.Background())
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		agent.heartbeatLoop(ctx)
	}()
	if err := agent.tick(ctx); err != nil {
		cancel()
		<-heartbeatDone
		t.Fatal(err)
	}
	cancel()
	<-heartbeatDone
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
	if invalidHeaders.Load() != 0 {
		t.Fatalf("agent omitted session or attempt headers on %d requests", invalidHeaders.Load())
	}
}
