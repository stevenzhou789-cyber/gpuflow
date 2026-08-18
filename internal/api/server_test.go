package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuflow/internal/model"
	"gpuflow/internal/store"
)

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
	state, _ := store.Open("")
	handler := New(state, "test-token")
	handler.images.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "docker" || len(args) < 4 || args[0] != "build" {
			t.Fatalf("unexpected build command: %s %v", name, args)
		}
		return []byte("image built"), nil
	}
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
		if len(images) == 1 && images[0].Status == "ready" {
			if images[0].Command != "python /workspace/smoke.py" || !strings.Contains(images[0].Log, "image built") {
				t.Fatalf("unexpected image: %+v", images[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("image did not become ready: %+v", images)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestJobLifecycleOverHTTP(t *testing.T) {
	state, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
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
	if status := request(t, server, http.MethodPost, "/v1/jobs/"+job.ID+"/status?node_id=local-1", model.JobUpdate{Status: model.JobSucceeded, Output: "done"}, &job); status != http.StatusOK {
		t.Fatalf("success update returned %d", status)
	}
	if job.Status != model.JobSucceeded || job.Output != "done" {
		t.Fatalf("unexpected completed job: %+v", job)
	}
}

func TestDeleteBusyNodeReturnsConflict(t *testing.T) {
	state, _ := store.Open("")
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
