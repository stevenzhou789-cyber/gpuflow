package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

func TestAscendDockerOptionsUseDiscoveredPhysicalIDs(t *testing.T) {
	a := New(Config{})
	a.acceleratorBackend = backendAscend
	a.baseline = model.Node{GPUCount: 2, Devices: []model.GPUDevice{
		{Index: 0, UUID: "ASCEND-3", Model: "Ascend 910B", VRAMGB: 64},
		{Index: 1, UUID: "ASCEND-7", Model: "Ascend 910B", VRAMGB: 64},
	}}
	job := &model.Job{Requirements: model.Requirements{GPUCount: 1}, AllocatedGPUs: []int{1}}
	args, err := a.acceleratorDockerArgs(job)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if joined != "--runtime ascend -e ASCEND_VISIBLE_DEVICES=7" {
		t.Fatalf("unexpected Ascend Docker options: %s", joined)
	}
}

func TestAscendBackendRequiresEnterpriseCapability(t *testing.T) {
	a := New(Config{AcceleratorBackend: "ascend"})
	if err := a.selectAcceleratorBackend(context.Background(), edition.Community()); err == nil {
		t.Fatal("Ascend backend was accepted by Community capabilities")
	}
}

func TestAutomaticAcceleratorBackendSelectsDetectedVendor(t *testing.T) {
	descriptor := edition.Community()
	descriptor.Features[edition.FeatureHeterogeneousAccelerators] = true
	tests := []struct {
		name        string
		probe       func(context.Context, string, ...string) ([]byte, error)
		wantBackend string
	}{
		{
			name: "ascend",
			probe: func(_ context.Context, command string, _ ...string) ([]byte, error) {
				if command == "npu-smi" {
					return []byte("0 0 0 Ascend 910B"), nil
				}
				return nil, errors.New("nvidia-smi unavailable")
			},
			wantBackend: backendAscend,
		},
		{
			name: "nvidia",
			probe: func(_ context.Context, command string, _ ...string) ([]byte, error) {
				if command == "nvidia-smi" {
					return []byte("0, GPU-test, NVIDIA H100, 81920, 550.54"), nil
				}
				return nil, errors.New("npu-smi unavailable")
			},
			wantBackend: backendNVIDIA,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := New(Config{AcceleratorBackend: "auto", ProbeCommand: test.probe})
			if err := a.selectAcceleratorBackend(context.Background(), descriptor); err != nil {
				t.Fatal(err)
			}
			if a.acceleratorBackend != test.wantBackend {
				t.Fatalf("automatic backend mismatch: got %q want %q", a.acceleratorBackend, test.wantBackend)
			}
		})
	}
}

func TestAutomaticAcceleratorBackendRejectsMixedVendorNode(t *testing.T) {
	descriptor := edition.Community()
	descriptor.Features[edition.FeatureHeterogeneousAccelerators] = true
	a := New(Config{
		AcceleratorBackend: "auto",
		ProbeCommand: func(_ context.Context, command string, _ ...string) ([]byte, error) {
			if command == "npu-smi" {
				return []byte("0 0 0 Ascend 910B"), nil
			}
			return []byte("0, GPU-test, NVIDIA H100, 81920, 550.54"), nil
		},
	})
	if err := a.selectAcceleratorBackend(context.Background(), descriptor); err == nil {
		t.Fatal("automatic backend accepted a mixed-vendor node")
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

type fakeAttemptDocker struct {
	mu               sync.Mutex
	name             string
	containerID      string
	createID         string
	stateFile        string
	createCalls      int
	removed          []string
	blockFirstCreate bool
	createEntered    chan struct{}
	releaseCreate    chan struct{}
	blockOnce        sync.Once
}

func (d *fakeAttemptDocker) command(ctx context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("missing Docker command")
	}
	switch args[0] {
	case "create":
		if d.blockFirstCreate {
			d.blockOnce.Do(func() {
				close(d.createEntered)
				select {
				case <-d.releaseCreate:
				case <-ctx.Done():
				}
			})
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := argumentAfter(args, "--name")
		d.mu.Lock()
		defer d.mu.Unlock()
		d.createCalls++
		if d.containerID != "" {
			return []byte(fmt.Sprintf("Conflict. The container name %q is already in use by container %q", name, d.containerID)), errors.New("name conflict")
		}
		d.name, d.containerID = name, d.createID
		d.syncStateFileLocked()
		return []byte(d.containerID + "\n"), nil
	case "ps":
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.containerID == "" {
			return nil, nil
		}
		return []byte(d.containerID + "\n"), nil
	case "inspect":
		target := args[len(args)-1]
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.containerID == "" || (target != d.containerID && target != d.name) {
			return []byte("No such object"), errors.New("not found")
		}
		return []byte(d.containerID + "\n"), nil
	case "rm":
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, target := range args[2:] {
			if d.containerID != "" && (target == d.containerID || target == d.name) {
				d.removed = append(d.removed, d.containerID)
				d.containerID = ""
				d.syncStateFileLocked()
				return []byte(target + "\n"), nil
			}
		}
		return []byte("No such container"), errors.New("not found")
	case "stop":
		target := args[len(args)-1]
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.containerID == "" || target != d.containerID {
			return []byte("No such container"), errors.New("not found")
		}
		return []byte(target + "\n"), nil
	default:
		return nil, fmt.Errorf("unexpected Docker command %q", strings.Join(args, " "))
	}
}

func (d *fakeAttemptDocker) put(name, containerID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.name, d.containerID = name, containerID
	d.syncStateFileLocked()
}

func (d *fakeAttemptDocker) state() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.containerID
}

func (d *fakeAttemptDocker) wasRemoved(containerID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, removed := range d.removed {
		if removed == containerID {
			return true
		}
	}
	return false
}

func (d *fakeAttemptDocker) syncStateFileLocked() {
	if d.stateFile == "" {
		return
	}
	if d.containerID == "" {
		_ = os.Remove(d.stateFile)
		return
	}
	_ = os.WriteFile(d.stateFile, []byte(d.containerID), 0o600)
}

func argumentAfter(args []string, option string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == option {
			return args[index+1]
		}
	}
	return ""
}

func newAttemptServer(t *testing.T, currentSession, currentToken *atomic.Value, jobID string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/jobs/"+jobID+"/attempt" || r.URL.Query().Get("node_id") != "node" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get(model.HeaderAgentSession) != currentSession.Load().(string) || r.Header.Get(model.HeaderAttemptToken) != currentToken.Load().(string) {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid agent session"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func newFenceTestAgent(serverURL, session string, docker *fakeAttemptDocker, execute func(context.Context, string, ...string) *exec.Cmd) *Agent {
	agent := New(Config{Server: serverURL, ID: "node", Executor: "docker", CleanupCommand: docker.command, ExecuteCommand: execute})
	agent.session = session
	agent.sessionTTL = time.Minute
	agent.setSessionLeaseDeadline(time.Now().Add(time.Minute))
	return agent
}

func fenceTestJob(id string) model.Job {
	return model.Job{ID: id, Image: "work", Attempts: 1, TimeoutSeconds: 30, Requirements: model.Requirements{GPUCount: 1}}
}

func TestDelayedOldCreateAfterTakeoverCleanupNeverStarts(t *testing.T) {
	const jobID, attemptToken = "late-create", "old-attempt"
	oldID := strings.Repeat("a", 64)
	var current, currentToken atomic.Value
	current.Store("old-session")
	currentToken.Store(attemptToken)
	server := newAttemptServer(t, &current, &currentToken, jobID)
	defer server.Close()
	docker := &fakeAttemptDocker{
		createID: oldID, blockFirstCreate: true,
		createEntered: make(chan struct{}), releaseCreate: make(chan struct{}),
	}
	var startCalled atomic.Bool
	oldAgent := newFenceTestAgent(server.URL, "old-session", docker, func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		startCalled.Store(true)
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	})
	dir := t.TempDir()
	job := fenceTestJob(jobID)
	done := make(chan error, 1)
	go func() {
		_, err := oldAgent.execute(context.Background(), &job, attemptToken, dir, filepath.Join(dir, "job.log"))
		done <- err
	}()
	<-docker.createEntered

	// Registration has fenced the old session, but takeover cleanup observes
	// no container because the old daemon request is still in flight.
	current.Store("new-session")
	currentToken.Store("new-attempt")
	newAgent := newFenceTestAgent(server.URL, "new-session", docker, nil)
	if err := newAgent.cleanupManagedContainers(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(docker.releaseCreate)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "validate Docker attempt ownership") {
		t.Fatalf("late stale create was not fenced: %v", err)
	}
	if startCalled.Load() {
		t.Fatal("old Agent started a container created after replacement cleanup")
	}
	if state := docker.state(); state != "" {
		t.Fatalf("stopped stale container was not removed by immutable ID: %s", state)
	}
}

func TestDelayedOldCreateConflictCannotDeleteNewSessionContainer(t *testing.T) {
	const jobID, attemptToken = "late-conflict", "old-attempt"
	oldID, newID := strings.Repeat("a", 64), strings.Repeat("b", 64)
	var current, currentToken atomic.Value
	current.Store("old-session")
	currentToken.Store(attemptToken)
	server := newAttemptServer(t, &current, &currentToken, jobID)
	defer server.Close()
	docker := &fakeAttemptDocker{
		createID: oldID, blockFirstCreate: true,
		createEntered: make(chan struct{}), releaseCreate: make(chan struct{}),
	}
	oldAgent := newFenceTestAgent(server.URL, "old-session", docker, nil)
	dir := t.TempDir()
	job := fenceTestJob(jobID)
	done := make(chan error, 1)
	go func() {
		_, err := oldAgent.execute(context.Background(), &job, attemptToken, dir, filepath.Join(dir, "job.log"))
		done <- err
	}()
	<-docker.createEntered

	current.Store("new-session")
	currentToken.Store("new-attempt")
	docker.put(jobContainerName(jobID, 1), newID)
	close(docker.releaseCreate)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "validate Docker attempt ownership") {
		t.Fatalf("stale conflict was not rejected: %v", err)
	}
	if state := docker.state(); state != newID {
		t.Fatalf("stale Agent altered replacement container: got %q want %q", state, newID)
	}
	if docker.wasRemoved(newID) {
		t.Fatal("stale Agent removed the replacement session container")
	}
}

func TestCurrentSessionReclaimsConflictingAttemptByImmutableID(t *testing.T) {
	const jobID, attemptToken = "reclaim-conflict", "new-attempt"
	oldID, newID := strings.Repeat("a", 64), strings.Repeat("b", 64)
	var current, currentToken atomic.Value
	current.Store("new-session")
	currentToken.Store(attemptToken)
	server := newAttemptServer(t, &current, &currentToken, jobID)
	defer server.Close()
	docker := &fakeAttemptDocker{createID: newID}
	docker.put(jobContainerName(jobID, 1), oldID)
	var startID atomic.Value
	agent := newFenceTestAgent(server.URL, "new-session", docker, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "docker" && len(args) == 3 && args[0] == "start" && args[1] == "--attach" {
			startID.Store(args[2])
		}
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	})
	dir := t.TempDir()
	job := fenceTestJob(jobID)
	if _, err := agent.execute(context.Background(), &job, attemptToken, dir, filepath.Join(dir, "job.log")); err != nil {
		t.Fatal(err)
	}
	if docker.createCalls != 2 || !docker.wasRemoved(oldID) {
		t.Fatalf("conflict was not reclaimed and retried: creates=%d removed_old=%v", docker.createCalls, docker.wasRemoved(oldID))
	}
	if got, _ := startID.Load().(string); got != newID {
		t.Fatalf("start did not use new immutable ID: got %q want %q", got, newID)
	}
	if state := docker.state(); state != "" {
		t.Fatalf("completed container was not cleaned: %s", state)
	}
}

func TestCreateThenTakeoverBeforeStartCannotRunRemovedContainer(t *testing.T) {
	const jobID, attemptToken = "takeover-before-start", "old-attempt"
	oldID := strings.Repeat("a", 64)
	dir := t.TempDir()
	stateFile, ranFile := filepath.Join(dir, "container.state"), filepath.Join(dir, "workload.ran")
	var current, currentToken atomic.Value
	current.Store("old-session")
	currentToken.Store(attemptToken)
	server := newAttemptServer(t, &current, &currentToken, jobID)
	defer server.Close()
	docker := &fakeAttemptDocker{createID: oldID, stateFile: stateFile}
	startReady, releaseStart := make(chan struct{}), make(chan struct{})
	oldAgent := newFenceTestAgent(server.URL, "old-session", docker, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "docker" || len(args) != 3 || args[0] != "start" || args[1] != "--attach" || args[2] != oldID {
			return exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
		}
		close(startReady)
		<-releaseStart
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAgentDockerStartHelper$")
		cmd.Env = append(os.Environ(),
			"GO_WANT_AGENT_DOCKER_START_HELPER=1",
			"GPUFLOW_TEST_CONTAINER_STATE="+stateFile,
			"GPUFLOW_TEST_EXPECTED_CONTAINER="+oldID,
			"GPUFLOW_TEST_WORKLOAD_RAN="+ranFile,
		)
		return cmd
	})
	job := fenceTestJob(jobID)
	done := make(chan error, 1)
	go func() {
		_, err := oldAgent.execute(context.Background(), &job, attemptToken, dir, filepath.Join(dir, "job.log"))
		done <- err
	}()
	<-startReady

	// The old container exists and passed the exact attempt check. A new Agent
	// now fences the session and removes that ID before the paused start call.
	current.Store("new-session")
	currentToken.Store("new-attempt")
	newAgent := newFenceTestAgent(server.URL, "new-session", docker, nil)
	if err := newAgent.cleanupManagedContainers(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(releaseStart)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "docker start failed") {
		t.Fatalf("start of removed immutable ID did not fail: %v", err)
	}
	if _, err := os.Stat(ranFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed stale workload unexpectedly ran: %v", err)
	}
}

func TestAgentDockerStartHelper(t *testing.T) {
	if os.Getenv("GO_WANT_AGENT_DOCKER_START_HELPER") != "1" {
		return
	}
	state, err := os.ReadFile(os.Getenv("GPUFLOW_TEST_CONTAINER_STATE"))
	if err == nil && string(state) == os.Getenv("GPUFLOW_TEST_EXPECTED_CONTAINER") {
		_ = os.WriteFile(os.Getenv("GPUFLOW_TEST_WORKLOAD_RAN"), []byte("ran"), 0o600)
		os.Exit(0)
	}
	os.Exit(42)
}

func TestCleanupManagedContainersUsesGPUFlowLabelAndVerifiesEmpty(t *testing.T) {
	var calls []string
	listCalls := 0
	agent := New(Config{
		Executor: "docker",
		CleanupCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
			call := name + " " + strings.Join(args, " ")
			calls = append(calls, call)
			if len(args) > 0 && args[0] == "ps" {
				listCalls++
				if listCalls == 1 {
					return []byte("old-a\nold-b\n"), nil
				}
				return nil, nil
			}
			return []byte("old-a\nold-b\n"), nil
		},
	})
	if err := agent.cleanupManagedContainers(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "\n")
	if len(calls) != 3 || !strings.Contains(calls[0], "ps -aq --filter label=gpuflow.job") || !strings.Contains(calls[1], "rm -f old-a old-b") || !strings.Contains(calls[2], "ps -aq --filter label=gpuflow.job") {
		t.Fatalf("unexpected cleanup commands:\n%s", joined)
	}
}

func TestExecuteDoesNotStartDockerAfterLeaseExpiresDuringCleanup(t *testing.T) {
	var commandCreated atomic.Bool
	agent := New(Config{
		Executor: "docker",
		CleanupCommand: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			time.Sleep(75 * time.Millisecond)
			if len(args) > 0 && args[0] == "inspect" {
				return []byte("No such object"), errors.New("not found")
			}
			return nil, nil
		},
		ExecuteCommand: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			commandCreated.Store(true)
			return exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
		},
	})
	agent.sessionTTL = 25 * time.Millisecond
	agent.setSessionLeaseDeadline(time.Now().Add(agent.sessionTTL))
	dir := t.TempDir()
	job := model.Job{ID: "paused-before-start", Image: "work", Attempts: 1, TimeoutSeconds: 60, Requirements: model.Requirements{GPUCount: 1}}
	if _, err := agent.execute(context.Background(), &job, "attempt", dir, filepath.Join(dir, "job.log")); !errors.Is(err, errAgentSessionLeaseExpired) {
		t.Fatalf("expired worker did not fail closed: %v", err)
	}
	if commandCreated.Load() {
		t.Fatal("Docker command was created after the local session lease expired")
	}
}

func TestRunConfirmsCleanupBeforeHeartbeatAndWorkers(t *testing.T) {
	descriptor := edition.Community()
	var cleanupConfirmed atomic.Bool
	heartbeat := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			_ = json.NewEncoder(w).Encode(descriptor)
		case "/v1/nodes/register":
			var node model.Node
			_ = json.NewDecoder(r.Body).Decode(&node)
			node.CleanupPending = true
			_ = json.NewEncoder(w).Encode(node)
		case "/v1/nodes/cleanup-order/cleanup-complete":
			cleanupConfirmed.Store(true)
			w.WriteHeader(http.StatusOK)
		case "/v1/nodes/cleanup-order/heartbeat":
			if !cleanupConfirmed.Load() {
				t.Error("Agent heartbeated before cleanup confirmation")
			}
			select {
			case heartbeat <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusOK)
		case "/v1/nodes/cleanup-order/next":
			if !cleanupConfirmed.Load() {
				t.Error("Agent worker polled before cleanup confirmation")
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- New(Config{Server: server.URL, ID: "cleanup-order", Executor: "mock", PollInterval: time.Hour, HeartbeatInterval: time.Hour}).Run(ctx)
	}()
	select {
	case <-heartbeat:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Agent did not reach post-cleanup heartbeat")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected Run result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Agent did not stop")
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
		{name: "legacy schema", status: http.StatusOK, body: edition.Descriptor{SchemaVersion: 1, Name: "enterprise", Features: map[string]bool{edition.FeatureBasicScheduler: true}}},
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

func TestRunUsesDedicatedCapabilityProbeImageAndSession(t *testing.T) {
	descriptor := edition.Community()
	descriptor.Name = "enterprise"
	descriptor.AgentImage = "gpuflow-enterprise:test"
	descriptor.ProbeImage = "gpuflow-gpu-probe:test"
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
		ProbeImage:     "ghcr.io/obsolete/probe:v0",
		CleanupCommand: func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		ProbeCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "docker" {
				return nil, errors.New("unexpected native GPU probe")
			}
			if len(args) > 0 && args[0] == "version" {
				return []byte("27.1.0"), nil
			}
			if strings.Contains(strings.Join(args, " "), descriptor.ProbeImage) {
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
	if !probeImageUsed.Load() || agent.cfg.ProbeImage != descriptor.ProbeImage {
		t.Fatalf("dedicated capability probe image was not used: %q", agent.cfg.ProbeImage)
	}
	if agent.cfg.ProbeImage == descriptor.AgentImage {
		t.Fatal("Agent image was incorrectly reused as the GPU probe image")
	}
	if invalidHeaders.Load() != 0 {
		t.Fatalf("invalid agent session headers: %d", invalidHeaders.Load())
	}
}

func TestConfigureProbeImageUsesCapabilityDefault(t *testing.T) {
	descriptor := edition.Community()
	descriptor.ProbeImage = "  registry.example.com/gpuflow/probe:v1  "
	agent := New(Config{})

	if err := agent.configureProbeImage(descriptor); err != nil {
		t.Fatal(err)
	}
	if got, want := agent.cfg.ProbeImage, "registry.example.com/gpuflow/probe:v1"; got != want {
		t.Fatalf("unexpected probe image: got %q want %q", got, want)
	}
}

func TestConfigureProbeImagePreservesExplicitLocalValue(t *testing.T) {
	descriptor := edition.Community()
	descriptor.ProbeImage = "registry.example.com/gpuflow/server-probe:v1"
	agent := New(Config{ProbeImage: "registry.example.com/gpuflow/local-probe:v1"})

	if err := agent.configureProbeImage(descriptor); err != nil {
		t.Fatal(err)
	}
	if got, want := agent.cfg.ProbeImage, "registry.example.com/gpuflow/local-probe:v1"; got != want {
		t.Fatalf("explicit probe image was replaced: got %q want %q", got, want)
	}
}

func TestConfigureProbeImageNodeHealthForcesCapabilityValue(t *testing.T) {
	descriptor := edition.Community()
	descriptor.Features[edition.FeatureNodeHealth] = true
	descriptor.ProbeImage = "  registry.example.com/gpuflow/managed-probe:v1  "
	agent := New(Config{ProbeImage: "registry.example.com/gpuflow/obsolete-probe:v0"})

	if err := agent.configureProbeImage(descriptor); err != nil {
		t.Fatal(err)
	}
	if got, want := agent.cfg.ProbeImage, "registry.example.com/gpuflow/managed-probe:v1"; got != want {
		t.Fatalf("managed probe image was not enforced: got %q want %q", got, want)
	}
}

func TestRunRegistersDegradedNodeWhenDockerGPUProbeFails(t *testing.T) {
	descriptor := edition.Community()
	descriptor.Name = "enterprise"
	descriptor.ProbeImage = "gpuflow-gpu-probe:test"
	descriptor.Features[edition.FeatureNodeHealth] = true
	descriptor.Features[edition.FeaturePerGPUInventory] = true
	registered := make(chan model.Node, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		case "/v1/nodes/degraded-node/next":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	agent := New(Config{
		Server: server.URL, ID: "degraded-node", Executor: "docker", PollInterval: time.Hour,
		CleanupCommand: func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		ProbeCommand: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "nvidia-smi" {
				return []byte("0, GPU-a, NVIDIA L4, 23034, 550.54\n"), nil
			}
			if len(args) > 0 && args[0] == "version" {
				return []byte("27.1.0"), nil
			}
			return []byte("exec /usr/bin/nvidia-smi: no such file or directory"), errors.New("exit status 255")
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	select {
	case node := <-registered:
		if node.HealthStatus != "DEGRADED" || node.GPUCount != 1 || !strings.Contains(node.HealthReason, "container runtime validation failed") {
			t.Fatalf("unexpected degraded registration: %+v", node)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Agent exited before registering its degraded state")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected Run result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Agent did not stop")
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
		case "/v1/nodes/session-node/cleanup-complete":
			w.WriteHeader(http.StatusOK)
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
		case "/v1/nodes/partitioned-node/cleanup-complete":
			w.WriteHeader(http.StatusOK)
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
