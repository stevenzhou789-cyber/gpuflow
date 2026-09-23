package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"gpuflow/internal/api"
	"gpuflow/internal/artifact"
	"gpuflow/internal/model"
	"gpuflow/internal/store"
	"gpuflow/pkg/edition"
)

// This opt-in test requires an explicitly provisioned, disposable local MinIO
// fixture and an already-present Alpine image. It never uses production S3
// credentials, discovers existing services, pulls an image, or starts MinIO.
func TestDockerResultDeliveryEnforcesHostLimitsAndPublishesResults(t *testing.T) {
	if os.Getenv("GPUFLOW_TEST_DOCKER_RESULTS") != "1" {
		t.Skip("set GPUFLOW_TEST_DOCKER_RESULTS=1 with an isolated GPUFLOW_TEST_S3_ENDPOINT to exercise Docker and S3")
	}
	endpoint := strings.TrimSpace(os.Getenv("GPUFLOW_TEST_S3_ENDPOINT"))
	if endpoint == "" {
		t.Fatal("GPUFLOW_TEST_S3_ENDPOINT must identify a disposable local test fixture")
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil || (host != "localhost" && !net.ParseIP(host).IsLoopback()) {
		t.Fatalf("test S3 endpoint must be an explicit loopback host:port, got %q", endpoint)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const image = "alpine:3.21"
	if output, err := exec.CommandContext(ctx, "docker", "image", "inspect", image).CombinedOutput(); err != nil {
		t.Fatalf("the Docker test requires an already-local %s image: %v: %s", image, err, output)
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	objects, err := artifact.Open(artifact.Config{
		Endpoint: endpoint, AccessKey: "reliability-test", SecretKey: "reliability-test-password",
		Bucket: "gpuflow-reliability-" + hex.EncodeToString(random),
	})
	if err != nil {
		t.Fatalf("open disposable S3 fixture: %v", err)
	}
	state := store.NewMemory()
	server := httptest.NewServer(api.NewWithStores(state, state, objects, "", edition.Community()).Handler())
	defer server.Close()
	const nodeID, session = "docker-results-fixture", "docker-results-session"
	if _, err := state.RegisterNodeSession(model.Node{ID: nodeID, CPUCores: 2, MemoryMiB: 1024, HostResourceLimits: true}, session); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ConfirmNodeCleanupSession(nodeID, session); err != nil {
		t.Fatal(err)
	}
	job, err := state.CreateJob(model.JobCreate{
		Name: "Docker result delivery and cgroup limits", Image: image, TimeoutSeconds: 30,
		Requirements: model.Requirements{CPUCores: 0.5, MemoryMiB: 64},
		Command: []string{"sh", "-ec", `cat /sys/fs/cgroup/cpu.max > "$GPUFLOW_ARTIFACT_DIR/cpu.max"
cat /sys/fs/cgroup/memory.max > "$GPUFLOW_ARTIFACT_DIR/memory.max"
printf 'durable-checkpoint\n' > "$GPUFLOW_ARTIFACT_DIR/checkpoint.txt"
printf 'docker-result-delivery-complete\n'`},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := objects.Delete(cleanup, job.ID); err != nil {
			t.Errorf("delete this test's isolated artifact namespace: %v", err)
		}
	})
	if err := state.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	a := New(Config{
		Server: server.URL, ID: nodeID, Executor: "docker", ArtifactDir: workdir,
		CPUCores: 2, HeartbeatInterval: time.Second, ArtifactUploadTimeout: 20 * time.Second,
	})
	a.session = session
	a.setSessionLeaseDeadline(time.Now().Add(model.AgentSessionFailStopTTL))
	heartbeatCtx, stopHeartbeats := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		a.heartbeatLoop(heartbeatCtx)
	}()
	defer func() {
		stopHeartbeats()
		<-heartbeatDone
	}()
	if err := a.tick(ctx); err != nil {
		t.Fatalf("real Docker execution and result delivery failed: %v", err)
	}
	completed, err := state.GetJob(job.ID)
	if err != nil || completed.Status != model.JobSucceeded || completed.Attempts != 1 {
		t.Fatalf("task did not complete exactly once: %+v, %v", completed, err)
	}
	readResult := func(path string) []byte {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("download %s: HTTP %d, %v: %s", path, response.StatusCode, err, body)
		}
		return body
	}
	archive := readResult("/v1/jobs/" + job.ID + "/artifacts/artifacts.tar.gz")
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	files := make(map[string]string)
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = strings.TrimSpace(string(body))
	}
	if _, err := io.Copy(io.Discard, gz); err != nil {
		t.Fatalf("uploaded archive checksum failed: %v", err)
	}
	if strings.Join(strings.Fields(files["cpu.max"]), " ") != "50000 100000" {
		t.Fatalf("Docker CPU quota was not enforced: %q", files["cpu.max"])
	}
	if files["memory.max"] != "67108864" {
		t.Fatalf("Docker memory limit was not enforced: %q", files["memory.max"])
	}
	if files["checkpoint.txt"] != "durable-checkpoint" {
		t.Fatalf("checkpoint changed during delivery: %q", files["checkpoint.txt"])
	}
	log := readResult("/v1/jobs/" + job.ID + "/logs/full")
	if !bytes.Contains(log, []byte("docker-result-delivery-complete")) {
		t.Fatalf("full Docker execution log was not published: %q", log)
	}
	remaining, err := os.ReadDir(workdir)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("confirmed results did not clean their local workspace: %v, %v", remaining, err)
	}
}
