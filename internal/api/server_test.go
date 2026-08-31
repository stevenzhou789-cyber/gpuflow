package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuflow/internal/artifact"
	"gpuflow/internal/model"
	"gpuflow/internal/store"
	"gpuflow/pkg/edition"
)

type testImagePublisher struct{}

func (testImagePublisher) Publish(_ context.Context, output io.Writer, image string) (string, error) {
	_, _ = io.WriteString(output, "image pushed")
	return "registry.example.com/" + image, nil
}

func (testImagePublisher) PublishedImageName(image string) string {
	return "registry.example.com/" + image
}

func request(t *testing.T, server *httptest.Server, method, path string, body any, out any) int {
	return requestAsAgent(t, server, method, path, body, out, "")
}

func requestAsAgent(t *testing.T, server *httptest.Server, method, path string, body any, out any, attemptToken string) int {
	return requestWithAgentCredentials(t, server, method, path, body, out, "test-session", attemptToken)
}

func requestWithAgentCredentials(t *testing.T, server *httptest.Server, method, path string, body any, out any, session, attemptToken string) int {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, server.URL+path, &payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set(model.HeaderAgentSession, session)
	}
	if attemptToken != "" {
		req.Header.Set(model.HeaderAttemptToken, attemptToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func confirmNodeReady(t *testing.T, server *httptest.Server, nodeID, session string) {
	t.Helper()
	if status := requestWithAgentCredentials(t, server, http.MethodPost, "/v1/nodes/"+nodeID+"/cleanup-complete", nil, nil, session, ""); status != http.StatusOK {
		t.Fatalf("confirm cleanup for %s returned %d", nodeID, status)
	}
}

func TestBuildTaskImageOverHTTP(t *testing.T) {
	state := store.NewMemory()
	handler := New(state, "test-token")
	handler.images.run = func(_ context.Context, output io.Writer, name string, args ...string) error {
		if name != "docker" || len(args) < 4 || args[0] != "build" {
			t.Fatalf("unexpected build command: %s %v", name, args)
		}
		_, _ = io.WriteString(output, "image built")
		return nil
	}
	handler.images.publisher = testImagePublisher{}
	server := httptest.NewServer(handler.Handler())
	defer server.Close()

	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	_ = writer.WriteField("runtime", "python")
	_ = writer.WriteField("image", "gpuflow-task/smoke:test")
	_ = writer.WriteField("requirements", "requests==2.32.3")
	file, err := writer.CreateFormFile("script", "smoke.py")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("print('hello')\n"))
	_ = writer.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/task-images/build", &payload)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("build returned %d", resp.StatusCode)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		var images []TaskImage
		if status := request(t, server, http.MethodGet, "/v1/task-images", nil, &images); status != http.StatusOK {
			t.Fatalf("list returned %d", status)
		}
		var built *TaskImage
		for index := range images {
			if images[index].Name == "registry.example.com/gpuflow-task/smoke:test" {
				built = &images[index]
				break
			}
		}
		if built != nil && built.Status == "ready" {
			if built.Command != "python /workspace/smoke.py" || !strings.Contains(built.Log, "image built") || !strings.Contains(built.Log, "image pushed") {
				t.Fatalf("unexpected image: %+v", built)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("image did not become ready: %+v", images)
		}
		time.Sleep(10 * time.Millisecond)
	}

	var duplicatePayload bytes.Buffer
	duplicateWriter := multipart.NewWriter(&duplicatePayload)
	_ = duplicateWriter.WriteField("runtime", "python")
	_ = duplicateWriter.WriteField("image", "gpuflow-task/smoke:test")
	duplicateFile, err := duplicateWriter.CreateFormFile("script", "smoke.py")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = duplicateFile.Write([]byte("print('duplicate')\n"))
	_ = duplicateWriter.Close()

	duplicateRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/task-images/build", &duplicatePayload)
	duplicateRequest.Header.Set("Authorization", "Bearer test-token")
	duplicateRequest.Header.Set("Content-Type", duplicateWriter.FormDataContentType())
	duplicateResponse, err := http.DefaultClient.Do(duplicateRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer duplicateResponse.Body.Close()
	if duplicateResponse.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate build returned %d, want %d", duplicateResponse.StatusCode, http.StatusConflict)
	}
}

func TestReserveTaskImageNameIsAtomic(t *testing.T) {
	builder := &ImageBuilder{
		images:    make(map[string]*TaskImage),
		store:     store.NewMemory(),
		publisher: testImagePublisher{},
	}

	const attempts = 8
	start := make(chan struct{})
	results := make(chan error, attempts)
	var workers sync.WaitGroup
	for index := range attempts {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			now := time.Now().UTC()
			results <- builder.reserve(&TaskImage{
				ID:        fmt.Sprintf("img-%d", index),
				Name:      "gpuflow-task/concurrent:v1",
				Status:    "building",
				CreatedAt: now,
				UpdatedAt: now,
			})
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	accepted, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, errTaskImageNameConflict):
			conflicts++
		default:
			t.Fatalf("unexpected reserve error: %v", err)
		}
	}
	if accepted != 1 || conflicts != attempts-1 {
		t.Fatalf("accepted=%d conflicts=%d, want 1 and %d", accepted, conflicts, attempts-1)
	}
}

func TestLiveBuildLogIsVisibleWhileBuilding(t *testing.T) {
	now := time.Now().UTC()
	image := &TaskImage{ID: "img-live-log", Status: "building", CreatedAt: now, UpdatedAt: now}
	builder := &ImageBuilder{images: map[string]*TaskImage{image.ID: image}}
	output := &liveBuildLog{builder: builder, imageID: image.ID}

	if _, err := io.WriteString(output, "Step 1/4 : FROM python:3.12-slim\n"); err != nil {
		t.Fatal(err)
	}
	current, err := builder.Get(image.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(current.Log, "Step 1/4") || current.UpdatedAt.Before(now) {
		t.Fatalf("live build output was not published: %+v", current)
	}
}

func TestTaskImageSearchPaginationAndDeletion(t *testing.T) {
	state := store.NewMemory()
	now := time.Now().UTC()
	alpha := model.TaskImage{ID: "img-alpha", Name: "gpuflow-task/alpha:v1", Runtime: "shell", BaseImage: "alpine", Filename: "alpha.sh", Status: "ready", CreatedAt: now, UpdatedAt: now}
	beta := model.TaskImage{ID: "img-beta", Name: "gpuflow-task/beta:v1", Runtime: "python", BaseImage: "python:3.12", Filename: "beta.py", Status: "ready", CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
	building := model.TaskImage{ID: "img-building", Name: "gpuflow-task/building:v1", Runtime: "shell", BaseImage: "alpine", Filename: "build.sh", Status: "building", CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now}
	for _, image := range []model.TaskImage{alpha, beta} {
		if err := state.SaveTaskImage(image); err != nil {
			t.Fatal(err)
		}
	}
	handler := New(state, "test-token")
	if err := state.SaveTaskImage(building); err != nil {
		t.Fatal(err)
	}
	handler.images.mu.Lock()
	handler.images.images[building.ID] = &building
	handler.images.mu.Unlock()
	removed := make([]string, 0)
	handler.images.remove = func(_ context.Context, image string) ([]byte, error) {
		removed = append(removed, image)
		return nil, nil
	}
	server := httptest.NewServer(handler.Handler())
	defer server.Close()

	var page TaskImagePage
	if status := request(t, server, http.MethodGet, "/v1/task-images?page=1&page_size=1&q=alpha", nil, &page); status != http.StatusOK || page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != alpha.ID {
		t.Fatalf("unexpected image page status=%d page=%+v", status, page)
	}
	job, err := state.CreateJob(model.JobCreate{Name: "uses alpha", Image: alpha.Name})
	if err != nil {
		t.Fatal(err)
	}
	if status := request(t, server, http.MethodDelete, "/v1/task-images/"+alpha.ID, nil, nil); status != http.StatusConflict || len(removed) != 0 {
		t.Fatalf("active image deletion status=%d removed=%v", status, removed)
	}
	if _, err := state.CancelJob(job.ID); err != nil {
		t.Fatal(err)
	}
	if status := request(t, server, http.MethodDelete, "/v1/task-images/"+alpha.ID, nil, nil); status != http.StatusNoContent || len(removed) != 1 || removed[0] != alpha.Name {
		t.Fatalf("image deletion status=%d removed=%v", status, removed)
	}
	if status := request(t, server, http.MethodDelete, "/v1/task-images/"+building.ID, nil, nil); status != http.StatusConflict {
		t.Fatalf("building image deletion returned %d", status)
	}
	images, err := state.ListTaskImages()
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(map[string]string, len(images))
	for _, image := range images {
		statuses[image.ID] = image.Status
	}
	if _, exists := statuses[alpha.ID]; exists {
		t.Fatalf("deleted image still persisted: %+v", images)
	}
	if statuses[beta.ID] != "ready" || statuses[building.ID] != "building" {
		t.Fatalf("remaining test images changed unexpectedly: %+v", images)
	}
}

func TestNodeSearchPaginationOverHTTP(t *testing.T) {
	state := store.NewMemory()
	server := httptest.NewServer(New(state, "test-token").Handler())
	defer server.Close()
	for _, node := range []model.Node{
		{ID: "alpha-node", Name: "Alpha GPU", Provider: "local", Pool: "lab-a", GPUModel: "RTX-4090"},
		{ID: "beta-node", Name: "Beta GPU", Provider: "local", Pool: "lab-b", GPUModel: "A100"},
	} {
		request(t, server, http.MethodPost, "/v1/nodes/register", node, &node)
	}
	var page store.NodePage
	if status := request(t, server, http.MethodGet, "/v1/nodes?page=1&page_size=1&q=4090", nil, &page); status != http.StatusOK || page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != "alpha-node" {
		t.Fatalf("unexpected node page status=%d page=%+v", status, page)
	}
}

func TestEnterpriseNodeCapacityOverHTTP(t *testing.T) {
	state := store.NewMemory()
	descriptor := edition.Community()
	descriptor.Name = "enterprise"
	descriptor.MaxNodes = 2
	descriptor.MaxGPUs = 2
	// A legacy CPU limit may still be present in an older descriptor, but it is ignored.
	descriptor.MaxCPUCores = 1
	server := httptest.NewServer(NewWithEdition(state, "test-token", descriptor).Handler())
	defer server.Close()

	first := model.Node{ID: "first", GPUCount: 1}
	if status := request(t, server, http.MethodPost, "/v1/nodes/register", first, &first); status != http.StatusOK {
		t.Fatalf("first registration returned %d", status)
	}
	second := model.Node{ID: "second", GPUCount: 1, CPUCores: 256}
	if status := request(t, server, http.MethodPost, "/v1/nodes/register", second, &second); status != http.StatusOK {
		t.Fatalf("second registration returned %d", status)
	}
	third := model.Node{ID: "third", GPUCount: 0, CPUCores: 1}
	if status := request(t, server, http.MethodPost, "/v1/nodes/register", third, nil); status != http.StatusForbidden {
		t.Fatalf("node overage returned %d", status)
	}
	first.CPUCores = 512
	if status := request(t, server, http.MethodPost, "/v1/nodes/register", first, &first); status != http.StatusOK {
		t.Fatalf("CPU-only reconnect returned %d", status)
	}
}

func TestEnterpriseNodeCapacityAdmissionIsAtomic(t *testing.T) {
	state := store.NewMemory()
	descriptor := edition.Community()
	descriptor.MaxNodes = 1
	descriptor.MaxGPUs = 1
	server := httptest.NewServer(NewWithEdition(state, "test-token", descriptor).Handler())
	defer server.Close()

	const candidates = 8
	start := make(chan struct{})
	results := make(chan int, candidates)
	errs := make(chan error, candidates)
	var wg sync.WaitGroup
	for i := 0; i < candidates; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			payload, err := json.Marshal(model.Node{ID: fmt.Sprintf("concurrent-%d", index), GPUCount: 1, CPUCores: 1})
			if err != nil {
				errs <- err
				return
			}
			req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/nodes/register", bytes.NewReader(payload))
			if err != nil {
				errs <- err
				return
			}
			req.Header.Set("Authorization", "Bearer test-token")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(model.HeaderAgentSession, fmt.Sprintf("session-%d", index))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err
				return
			}
			_ = resp.Body.Close()
			results <- resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	accepted, rejected := 0, 0
	for status := range results {
		switch status {
		case http.StatusOK:
			accepted++
		case http.StatusForbidden:
			rejected++
		default:
			t.Fatalf("unexpected registration status %d", status)
		}
	}
	if accepted != 1 || rejected != candidates-1 || len(state.ListNodes()) != 1 {
		t.Fatalf("capacity admission was not atomic: accepted=%d rejected=%d nodes=%d", accepted, rejected, len(state.ListNodes()))
	}
}

func TestJobLifecycleOverHTTP(t *testing.T) {
	state := store.NewMemory()
	server := httptest.NewServer(New(state, "test-token").Handler())
	defer server.Close()

	node := model.Node{ID: "local-1", Name: "local-1", Provider: "local", Pool: "development", GPUCount: 1, VRAMGB: 24, HourlyPrice: 2}
	if status := request(t, server, http.MethodPost, "/v1/nodes/register", node, &node); status != http.StatusOK {
		t.Fatalf("register returned %d", status)
	}
	confirmNodeReady(t, server, node.ID, "test-session")

	var job model.Job
	create := model.JobCreate{Name: "smoke", Image: "alpine", Requirements: model.Requirements{GPUCount: 1, MinVRAMGB: 20}}
	if status := request(t, server, http.MethodPost, "/v1/jobs", create, &job); status != http.StatusCreated {
		t.Fatalf("create returned %d", status)
	}

	var dispatch model.AgentJob
	if status := request(t, server, http.MethodPost, "/v1/nodes/local-1/next", nil, &dispatch); status != http.StatusOK {
		t.Fatalf("next returned %d", status)
	}
	job = dispatch.Job
	if job.AssignedNode != "local-1" || job.Status != model.JobAssigned {
		t.Fatalf("unexpected assignment: %+v", job)
	}

	if status := requestAsAgent(t, server, http.MethodPost, "/v1/jobs/"+job.ID+"/status?node_id=local-1", model.JobUpdate{Status: model.JobRunning}, &job, dispatch.AttemptToken); status != http.StatusOK {
		t.Fatalf("running update returned %d", status)
	}
	if status := requestAsAgent(t, server, http.MethodPost, "/v1/jobs/"+job.ID+"/logs?node_id=local-1", model.JobLogUpdate{Output: "epoch 1/10"}, &job, dispatch.AttemptToken); status != http.StatusOK {
		t.Fatalf("live log update returned %d", status)
	}
	if job.Status != model.JobRunning || job.Output != "epoch 1/10" {
		t.Fatalf("live log changed job incorrectly: %+v", job)
	}
	if status := requestAsAgent(t, server, http.MethodPost, "/v1/jobs/"+job.ID+"/status?node_id=local-1", model.JobUpdate{Status: model.JobSucceeded, Output: "done"}, &job, dispatch.AttemptToken); status != http.StatusOK {
		t.Fatalf("success update returned %d", status)
	}
	if job.Status != model.JobSucceeded || job.Output != "done" {
		t.Fatalf("unexpected completed job: %+v", job)
	}
	var artifacts struct {
		Enabled bool  `json:"enabled"`
		Items   []any `json:"items"`
	}
	if status := request(t, server, http.MethodGet, "/v1/jobs/"+job.ID+"/artifacts", nil, &artifacts); status != http.StatusOK {
		t.Fatalf("artifact list returned %d", status)
	}
	if artifacts.Enabled || len(artifacts.Items) != 0 {
		t.Fatalf("unexpected disabled artifact response: %+v", artifacts)
	}
	var rerun model.Job
	if status := request(t, server, http.MethodPost, "/v1/jobs/"+job.ID+"/rerun", nil, &rerun); status != http.StatusCreated || rerun.RerunOf != job.ID {
		t.Fatalf("rerun returned %d: %+v", status, rerun)
	}
	var page store.JobPage
	if status := request(t, server, http.MethodGet, "/v1/jobs?page=1&page_size=1&q=smoke", nil, &page); status != http.StatusOK || page.Total != 2 || len(page.Items) != 1 {
		t.Fatalf("paged list returned %d: %+v", status, page)
	}
	if status := request(t, server, http.MethodDelete, "/v1/jobs/"+job.ID, nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete returned %d", status)
	}
}

func TestAgentCleanupGateOverHTTP(t *testing.T) {
	state := store.NewMemory()
	server := httptest.NewServer(New(state, "test-token").Handler())
	defer server.Close()

	node := model.Node{ID: "cleanup-http", GPUCount: 1, VRAMGB: 24}
	if status := requestWithAgentCredentials(t, server, http.MethodPost, "/v1/nodes/register", node, &node, "session-one", ""); status != http.StatusOK || !node.CleanupPending {
		t.Fatalf("registration did not enter cleanup gate: status=%d node=%+v", status, node)
	}
	var job model.Job
	if status := request(t, server, http.MethodPost, "/v1/jobs", model.JobCreate{Name: "gated", Image: "work", Requirements: model.Requirements{GPUCount: 1}}, &job); status != http.StatusCreated {
		t.Fatalf("create returned %d", status)
	}
	stored, _ := state.GetJob(job.ID)
	if stored.Status != model.JobQueued {
		t.Fatalf("job crossed cleanup gate: %+v", stored)
	}
	if status := requestWithAgentCredentials(t, server, http.MethodPost, "/v1/nodes/cleanup-http/heartbeat", nil, nil, "session-one", ""); status != http.StatusConflict {
		t.Fatalf("heartbeat bypass returned %d", status)
	}
	if status := requestWithAgentCredentials(t, server, http.MethodPost, "/v1/nodes/cleanup-http/next", nil, nil, "session-one", ""); status != http.StatusConflict {
		t.Fatalf("poll bypass returned %d", status)
	}
	if status := requestWithAgentCredentials(t, server, http.MethodPost, "/v1/nodes/cleanup-http/cleanup-complete", nil, nil, "wrong-session", ""); status != http.StatusConflict {
		t.Fatalf("wrong cleanup confirmation returned %d", status)
	}
	confirmNodeReady(t, server, node.ID, "session-one")
	var dispatch model.AgentJob
	if status := requestWithAgentCredentials(t, server, http.MethodPost, "/v1/nodes/cleanup-http/next", nil, &dispatch, "session-one", ""); status != http.StatusOK || dispatch.ID != job.ID {
		t.Fatalf("ready node did not receive job: status=%d dispatch=%+v", status, dispatch)
	}
}

func TestCreateJobValidationOverHTTP(t *testing.T) {
	server := httptest.NewServer(New(store.NewMemory(), "test-token").Handler())
	defer server.Close()
	invalid := []model.JobCreate{
		{Name: "negative GPU", Image: "work", Requirements: model.Requirements{GPUCount: -1}},
		{Name: "too many GPUs", Image: "work", Requirements: model.Requirements{GPUCount: 1025}},
		{Name: "negative VRAM", Image: "work", Requirements: model.Requirements{MinVRAMGB: -1}},
		{Name: "negative price", Image: "work", Requirements: model.Requirements{MaxHourly: -1}},
		{Name: "negative timeout", Image: "work", TimeoutSeconds: -1},
		{Name: "timeout overflow", Image: "work", TimeoutSeconds: int(int64(1) << 31)},
		{Name: "negative retry", Image: "work", MaxRetries: -1},
	}
	for _, input := range invalid {
		if status := request(t, server, http.MethodPost, "/v1/jobs", input, nil); status != http.StatusBadRequest {
			t.Fatalf("invalid job %q returned %d", input.Name, status)
		}
	}
}

func TestDeleteBusyNodeReturnsConflict(t *testing.T) {
	state := store.NewMemory()
	server := httptest.NewServer(New(state, "test-token").Handler())
	defer server.Close()
	node := model.Node{ID: "busy-node", GPUCount: 1, VRAMGB: 24}
	request(t, server, http.MethodPost, "/v1/nodes/register", node, &node)
	confirmNodeReady(t, server, node.ID, "test-session")
	var job model.Job
	request(t, server, http.MethodPost, "/v1/jobs", model.JobCreate{Name: "active", Image: "alpine", Requirements: model.Requirements{GPUCount: 1}}, &job)
	if status := request(t, server, http.MethodDelete, "/v1/nodes/busy-node", nil, nil); status != http.StatusConflict {
		t.Fatalf("expected conflict, got %d", status)
	}
}

type recordingArtifactStore struct {
	artifact.Store
	deleted   bool
	deleteErr error
}

type blockingStagedArtifactStore struct {
	artifact.Store
	mu           sync.Mutex
	canonical    []byte
	staged       []byte
	stageReady   chan struct{}
	releaseStage chan struct{}
	commits      int
	discards     int
}

func (s *blockingStagedArtifactStore) Enabled() bool { return true }

func (s *blockingStagedArtifactStore) Stage(ctx context.Context, _, _ string, input io.Reader, _ int64) (artifact.Staged, error) {
	payload, err := io.ReadAll(input)
	if err != nil {
		return artifact.Staged{}, err
	}
	s.mu.Lock()
	s.staged = append([]byte(nil), payload...)
	s.mu.Unlock()
	close(s.stageReady)
	select {
	case <-s.releaseStage:
		return artifact.Staged{}, nil
	case <-ctx.Done():
		return artifact.Staged{}, ctx.Err()
	}
}

func (s *blockingStagedArtifactStore) Commit(context.Context, artifact.Staged) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits++
	s.canonical = append([]byte(nil), s.staged...)
	return nil
}

func (s *blockingStagedArtifactStore) Discard(context.Context, artifact.Staged) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discards++
	s.staged = nil
	return nil
}

func (s *blockingStagedArtifactStore) snapshot() (canonical, staged []byte, commits, discards int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.canonical...), append([]byte(nil), s.staged...), s.commits, s.discards
}

type staticLogArtifactStore struct {
	artifact.Store
	content string
}

func TestArtifactUploadTakeoverBeforeCommitKeepsCanonicalObject(t *testing.T) {
	state := store.NewMemory()
	artifacts := &blockingStagedArtifactStore{
		Store:        artifact.Disabled(),
		canonical:    []byte("existing artifact"),
		stageReady:   make(chan struct{}),
		releaseStage: make(chan struct{}),
	}
	server := httptest.NewServer(NewWithStores(state, state, artifacts, "test-token", edition.Community()).Handler())
	defer server.Close()

	node := model.Node{ID: "artifact-fence", GPUCount: 1, VRAMGB: 24}
	if status := requestWithAgentCredentials(t, server, http.MethodPost, "/v1/nodes/register", node, &node, "session-one", ""); status != http.StatusOK {
		t.Fatalf("register returned %d", status)
	}
	confirmNodeReady(t, server, node.ID, "session-one")
	var job model.Job
	if status := request(t, server, http.MethodPost, "/v1/jobs", model.JobCreate{Name: "artifact fence", Image: "work", Requirements: model.Requirements{GPUCount: 1}}, &job); status != http.StatusCreated {
		t.Fatalf("create job returned %d", status)
	}
	var dispatch model.AgentJob
	if status := requestWithAgentCredentials(t, server, http.MethodPost, "/v1/nodes/"+node.ID+"/next", nil, &dispatch, "session-one", ""); status != http.StatusOK {
		t.Fatalf("next returned %d", status)
	}
	if status := requestWithAgentCredentials(t, server, http.MethodPost, "/v1/jobs/"+job.ID+"/status?node_id="+node.ID, model.JobUpdate{Status: model.JobRunning}, nil, "session-one", dispatch.AttemptToken); status != http.StatusOK {
		t.Fatalf("start job returned %d", status)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "artifacts.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("stale replacement")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/jobs/"+job.ID+"/artifacts?node_id="+node.ID, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set(model.HeaderAgentSession, "session-one")
	req.Header.Set(model.HeaderAttemptToken, dispatch.AttemptToken)
	type result struct {
		response *http.Response
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(req)
		resultCh <- result{response: response, err: requestErr}
	}()

	select {
	case <-artifacts.stageReady:
	case <-time.After(5 * time.Second):
		t.Fatal("artifact upload did not reach staging")
	}
	if _, err := state.RegisterNode(model.Node{ID: node.ID, GPUCount: 1, VRAMGB: 24}); err != nil {
		t.Fatalf("take over node during staging: %v", err)
	}
	close(artifacts.releaseStage)

	var upload result
	select {
	case upload = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("artifact upload did not finish")
	}
	if upload.err != nil {
		t.Fatal(upload.err)
	}
	defer upload.response.Body.Close()
	if upload.response.StatusCode != http.StatusConflict {
		responseBody, _ := io.ReadAll(upload.response.Body)
		t.Fatalf("fenced upload returned %d: %s", upload.response.StatusCode, responseBody)
	}
	canonical, staged, commits, discards := artifacts.snapshot()
	if string(canonical) != "existing artifact" || commits != 0 {
		t.Fatalf("fenced upload changed canonical object: canonical=%q commits=%d", canonical, commits)
	}
	if len(staged) != 0 || discards != 1 {
		t.Fatalf("fenced upload was not cleaned up: staged=%q discards=%d", staged, discards)
	}
}

func (s staticLogArtifactStore) Enabled() bool { return true }

func (s staticLogArtifactStore) Open(_ context.Context, _, name string) (io.ReadCloser, artifact.Item, error) {
	if name != "training.log" {
		return nil, artifact.Item{}, errors.New("not found")
	}
	return io.NopCloser(strings.NewReader(s.content)), artifact.Item{Name: name, Size: int64(len(s.content))}, nil
}

func TestDownloadFullJobLog(t *testing.T) {
	state := store.NewMemory()
	job, _ := state.CreateJob(model.JobCreate{Name: "full-log", Image: "alpine"})
	handler := New(state, "test-token")
	handler.artifacts = staticLogArtifactStore{Store: artifact.Disabled(), content: "epoch 1\nepoch 2\n"}
	server := httptest.NewServer(handler.Handler())
	defer server.Close()

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/jobs/"+job.ID+"/logs/full", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "epoch 1\nepoch 2\n" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("unexpected full log response: status=%d content-type=%q body=%q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
}

func (s *recordingArtifactStore) Delete(context.Context, string) error {
	s.deleted = true
	return s.deleteErr
}

func TestDeleteActiveJobKeepsArtifacts(t *testing.T) {
	state := store.NewMemory()
	artifacts := &recordingArtifactStore{Store: artifact.Disabled()}
	handler := New(state, "test-token")
	handler.artifacts = artifacts
	server := httptest.NewServer(handler.Handler())
	defer server.Close()

	var job model.Job
	request(t, server, http.MethodPost, "/v1/jobs", model.JobCreate{Name: "active", Image: "alpine"}, &job)
	if status := request(t, server, http.MethodDelete, "/v1/jobs/"+job.ID, nil, nil); status != http.StatusConflict {
		t.Fatalf("expected conflict, got %d", status)
	}
	if artifacts.deleted {
		t.Fatal("artifacts were deleted before the active job was rejected")
	}
}

func TestDeleteJobCanRetryAfterArtifactFailure(t *testing.T) {
	state := store.NewMemory()
	artifacts := &recordingArtifactStore{Store: artifact.Disabled(), deleteErr: errors.New("object store unavailable")}
	handler := New(state, "test-token")
	handler.artifacts = artifacts
	server := httptest.NewServer(handler.Handler())
	defer server.Close()

	job, _ := state.CreateJob(model.JobCreate{Name: "delete-retry", Image: "alpine"})
	if _, err := state.CancelJob(job.ID); err != nil {
		t.Fatal(err)
	}
	if status := request(t, server, http.MethodDelete, "/v1/jobs/"+job.ID, nil, nil); status != http.StatusBadGateway {
		t.Fatalf("expected artifact failure, got %d", status)
	}
	deleting, err := state.GetJob(job.ID)
	if err != nil || deleting.Status != model.JobDeleting {
		t.Fatalf("job did not retain resumable deletion state: %+v err=%v", deleting, err)
	}
	artifacts.deleteErr = nil
	if status := request(t, server, http.MethodDelete, "/v1/jobs/"+job.ID, nil, nil); status != http.StatusNoContent {
		t.Fatalf("retry delete returned %d", status)
	}
	if _, err := state.GetJob(job.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("job still exists after retry: %v", err)
	}
}

func TestReadableBuildError(t *testing.T) {
	err := errors.New("exit status 1")
	message := readableBuildError([]byte("failed to fetch anonymous token: connection attempt failed"), err)
	if !strings.Contains(message, "镜像加速") {
		t.Fatalf("unexpected network error: %s", message)
	}
	message = readableBuildError([]byte("manifest unknown"), err)
	if !strings.Contains(message, "标签无效") {
		t.Fatalf("unexpected manifest error: %s", message)
	}
}

type failingTaskImageStore struct{}

func (failingTaskImageStore) SaveTaskImage(model.TaskImage) error {
	return errors.New("storage unavailable")
}

func (failingTaskImageStore) ListTaskImages() ([]model.TaskImage, error) {
	return nil, nil
}

func (failingTaskImageStore) DeleteTaskImage(string) error {
	return errors.New("storage unavailable")
}

func TestImageBuildPersistenceFailureIsVisible(t *testing.T) {
	now := time.Now().UTC()
	image := &TaskImage{ID: "img-persist", Name: "gpuflow-task/test:v1", Status: "building", CreatedAt: now, UpdatedAt: now}
	builder := &ImageBuilder{images: map[string]*TaskImage{image.ID: image}, store: failingTaskImageStore{}}
	builder.finish(image.ID, []byte("build completed"), nil)
	got := builder.List()[0]
	if got.Status != "failed" || !strings.Contains(got.Error, "storage unavailable") {
		t.Fatalf("persistence failure was hidden: %+v", got)
	}
}

func TestExpiredLicenseAllowsReconnectButRejectsNewCapacityAndScheduling(t *testing.T) {
	state := store.NewMemory()
	existing := model.Node{ID: "licensed-node", GPUCount: 1, VRAMGB: 24}
	if _, err := state.RegisterNodeSession(existing, "test-session"); err != nil {
		t.Fatal(err)
	}
	descriptor := edition.Community()
	descriptor.Name = "enterprise"
	descriptor.ExpiresAt = time.Now().Add(-time.Minute).Format(time.RFC3339)
	descriptor.MaxNodes, descriptor.MaxGPUs = 0, 0
	server := httptest.NewServer(NewWithEdition(state, "test-token", descriptor).Handler())
	defer server.Close()

	if status := request(t, server, http.MethodPost, "/v1/nodes/register", existing, &existing); status != http.StatusOK {
		t.Fatalf("existing node reconnect returned %d", status)
	}
	confirmNodeReady(t, server, existing.ID, "test-session")
	if status := request(t, server, http.MethodPost, "/v1/nodes/register", model.Node{ID: "new-node", GPUCount: 0}, nil); status != http.StatusForbidden {
		t.Fatalf("new capacity under expired license returned %d", status)
	}
	var job model.Job
	request(t, server, http.MethodPost, "/v1/jobs", model.JobCreate{Name: "expired-job", Image: "work", Requirements: model.Requirements{GPUCount: 1}}, &job)
	stored, _ := state.GetJob(job.ID)
	if stored.Status != model.JobQueued {
		t.Fatalf("expired license scheduled a new job: %+v", stored)
	}
}

func TestHealthInventoryCannotBypassLicenseOverHTTP(t *testing.T) {
	state := store.NewMemory()
	descriptor := edition.Community()
	descriptor.Name = "enterprise"
	descriptor.MaxGPUs = 1
	descriptor.Features[edition.FeatureNodeHealth] = true
	descriptor.Features[edition.FeaturePerGPUInventory] = true
	server := httptest.NewServer(NewWithEdition(state, "test-token", descriptor).Handler())
	defer server.Close()

	for _, id := range []string{"health-a", "health-b"} {
		node := model.Node{ID: id, GPUModel: "none", HealthStatus: "HEALTHY"}
		if status := requestWithAgentCredentials(t, server, http.MethodPost, "/v1/nodes/register", node, &node, "session-"+id, ""); status != http.StatusOK {
			t.Fatalf("register %s returned %d", id, status)
		}
		confirmNodeReady(t, server, id, "session-"+id)
	}
	update := model.NodeHealthUpdate{Status: "HEALTHY", GPUModel: "L4", GPUCount: 1, VRAMGB: 24, Devices: []model.GPUDevice{{Index: 0, UUID: "GPU-one", Model: "L4", VRAMGB: 24}}}
	if status := requestWithAgentCredentials(t, server, http.MethodPost, "/v1/nodes/health-a/health", update, nil, "session-health-a", ""); status != http.StatusOK {
		t.Fatalf("first health expansion returned %d", status)
	}
	update.Devices[0].UUID = "GPU-two"
	if status := requestWithAgentCredentials(t, server, http.MethodPost, "/v1/nodes/health-b/health", update, nil, "session-health-b", ""); status != http.StatusForbidden {
		t.Fatalf("health expansion over license returned %d", status)
	}
	if status := requestWithAgentCredentials(t, server, http.MethodPost, "/v1/nodes/health-b/health", model.NodeHealthUpdate{Status: "DEGRADED", GPUCount: -1}, nil, "session-health-b", ""); status != http.StatusBadRequest {
		t.Fatalf("negative health inventory returned %d", status)
	}
}

func TestAgentSessionAndAttemptHeadersAreEnforcedOverHTTP(t *testing.T) {
	state := store.NewMemory()
	server := httptest.NewServer(New(state, "test-token").Handler())
	defer server.Close()

	node := model.Node{ID: "fenced-http", GPUCount: 1, VRAMGB: 24}
	if status := requestWithAgentCredentials(t, server, http.MethodPost, "/v1/nodes/register", node, &node, "session-one", ""); status != http.StatusOK {
		t.Fatalf("register returned %d", status)
	}
	confirmNodeReady(t, server, node.ID, "session-one")
	var job model.Job
	request(t, server, http.MethodPost, "/v1/jobs", model.JobCreate{Name: "fenced", Image: "work", Requirements: model.Requirements{GPUCount: 1}}, &job)
	var dispatch model.AgentJob
	if status := requestWithAgentCredentials(t, server, http.MethodPost, "/v1/nodes/fenced-http/next", nil, &dispatch, "session-one", ""); status != http.StatusOK {
		t.Fatalf("dispatch returned %d", status)
	}
	statusPath := "/v1/jobs/" + job.ID + "/status?node_id=fenced-http"
	if status := requestWithAgentCredentials(t, server, http.MethodPost, statusPath, model.JobUpdate{Status: model.JobRunning}, nil, "session-one", ""); status != http.StatusPreconditionFailed {
		t.Fatalf("missing attempt token returned %d", status)
	}
	if status := requestWithAgentCredentials(t, server, http.MethodPost, statusPath, model.JobUpdate{Status: model.JobRunning}, nil, "session-two", dispatch.AttemptToken); status != http.StatusConflict {
		t.Fatalf("stale session returned %d", status)
	}
	if status := requestWithAgentCredentials(t, server, http.MethodPost, statusPath, model.JobUpdate{Status: model.JobRunning}, &job, "session-one", dispatch.AttemptToken); status != http.StatusOK {
		t.Fatalf("valid attempt returned %d", status)
	}
	attemptPath := "/v1/jobs/" + job.ID + "/attempt?node_id=fenced-http"
	if status := requestWithAgentCredentials(t, server, http.MethodGet, attemptPath, nil, nil, "session-one", dispatch.AttemptToken); status != http.StatusNoContent {
		t.Fatalf("valid attempt validation returned %d", status)
	}
	if status := requestWithAgentCredentials(t, server, http.MethodGet, attemptPath, nil, nil, "session-one", "wrong"); status != http.StatusPreconditionFailed {
		t.Fatalf("wrong attempt validation returned %d", status)
	}
	if status := requestWithAgentCredentials(t, server, http.MethodGet, attemptPath, nil, nil, "session-two", dispatch.AttemptToken); status != http.StatusConflict {
		t.Fatalf("stale session validation returned %d", status)
	}
	logPath := "/v1/jobs/" + job.ID + "/logs?node_id=fenced-http"
	if status := requestWithAgentCredentials(t, server, http.MethodPost, logPath, model.JobLogUpdate{Output: "late"}, nil, "session-one", "wrong"); status != http.StatusPreconditionFailed {
		t.Fatalf("wrong log attempt returned %d", status)
	}
	if status := requestWithAgentCredentials(t, server, http.MethodPost, logPath, model.JobLogUpdate{Output: "current"}, &job, "session-one", dispatch.AttemptToken); status != http.StatusOK || job.Output != "current" {
		t.Fatalf("valid log update returned %d job=%+v", status, job)
	}
}

func TestCapabilitiesHideCommercialLicenseWithoutAuthentication(t *testing.T) {
	descriptor := edition.Community()
	descriptor.Name, descriptor.LicensedTo = "enterprise", "commercial-customer"
	descriptor.ExpiresAt, descriptor.MaxNodes, descriptor.MaxGPUs = "2030-01-01T00:00:00+08:00", 10, 80
	server := httptest.NewServer(NewWithEdition(store.NewMemory(), "test-token", descriptor).Handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var public map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&public); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"licensed_to", "expires_at", "max_nodes", "max_gpus", "max_cpu_cores"} {
		if _, exposed := public[field]; exposed {
			t.Fatalf("public capabilities exposed %s: %+v", field, public)
		}
	}
}
