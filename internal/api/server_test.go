package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuflow/internal/artifact"
	"gpuflow/internal/model"
	"gpuflow/internal/store"
)

type testImagePublisher struct{}

func (testImagePublisher) Publish(_ context.Context, output io.Writer, image string) (string, error) {
	_, _ = io.WriteString(output, "image pushed")
	return "registry.example.com/" + image, nil
}

func request(t *testing.T, server *httptest.Server, method, path string, body any, out any) int {
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

func TestJobLifecycleOverHTTP(t *testing.T) {
	state := store.NewMemory()
	server := httptest.NewServer(New(state, "test-token").Handler())
	defer server.Close()

	node := model.Node{ID: "local-1", Name: "local-1", Provider: "local", Pool: "development", GPUCount: 1, VRAMGB: 24, HourlyPrice: 2}
	if status := request(t, server, http.MethodPost, "/v1/nodes/register", node, &node); status != http.StatusOK {
		t.Fatalf("register returned %d", status)
	}

	var job model.Job
	create := model.JobCreate{Name: "smoke", Image: "alpine", Requirements: model.Requirements{GPUCount: 1, MinVRAMGB: 20}}
	if status := request(t, server, http.MethodPost, "/v1/jobs", create, &job); status != http.StatusCreated {
		t.Fatalf("create returned %d", status)
	}

	if status := request(t, server, http.MethodPost, "/v1/nodes/local-1/next", nil, &job); status != http.StatusOK {
		t.Fatalf("next returned %d", status)
	}
	if job.AssignedNode != "local-1" || job.Status != model.JobAssigned {
		t.Fatalf("unexpected assignment: %+v", job)
	}

	if status := request(t, server, http.MethodPost, "/v1/jobs/"+job.ID+"/status?node_id=local-1", model.JobUpdate{Status: model.JobRunning}, &job); status != http.StatusOK {
		t.Fatalf("running update returned %d", status)
	}
	if status := request(t, server, http.MethodPost, "/v1/jobs/"+job.ID+"/logs?node_id=local-1", model.JobLogUpdate{Output: "epoch 1/10"}, &job); status != http.StatusOK {
		t.Fatalf("live log update returned %d", status)
	}
	if job.Status != model.JobRunning || job.Output != "epoch 1/10" {
		t.Fatalf("live log changed job incorrectly: %+v", job)
	}
	if status := request(t, server, http.MethodPost, "/v1/jobs/"+job.ID+"/status?node_id=local-1", model.JobUpdate{Status: model.JobSucceeded, Output: "done"}, &job); status != http.StatusOK {
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

func TestDeleteBusyNodeReturnsConflict(t *testing.T) {
	state := store.NewMemory()
	server := httptest.NewServer(New(state, "test-token").Handler())
	defer server.Close()
	node := model.Node{ID: "busy-node", GPUCount: 1, VRAMGB: 24}
	request(t, server, http.MethodPost, "/v1/nodes/register", node, &node)
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
