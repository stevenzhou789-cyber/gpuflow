package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuflow/internal/model"
	"gpuflow/internal/store"
	"gpuflow/pkg/edition"
)

func boundaryRequest(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &payload)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set(model.HeaderAgentSession, "boundary-session")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func boundaryStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status=%d, want=%d: %s", rec.Code, want, rec.Body.String())
	}
}

func boundaryNodeResponse(t *testing.T, rec *httptest.ResponseRecorder, detailed bool) {
	t.Helper()
	boundaryStatus(t, rec, http.StatusOK)
	var node map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &node); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"devices", "driver_version", "docker_version"} {
		if _, present := node[field]; present != detailed {
			t.Fatalf("field %s present=%v, want=%v: %s", field, present, detailed, rec.Body.String())
		}
	}
	if string(node["gpu_count"]) != "1" || string(node["vram_gb"]) != "24" {
		t.Fatalf("projection removed basic capacity: %s", rec.Body.String())
	}
}

func TestCommunityRejectsDetailedInventoryBeforeChangingState(t *testing.T) {
	for _, field := range []string{"devices", "driver_version", "docker_version"} {
		t.Run(field, func(t *testing.T) {
			state := store.NewMemory()
			handler := New(state, "test-token").Handler()
			payload := map[string]any{"id": "worker", "gpu_count": 1, "vram_gb": 24}
			value := any("version")
			if field == "devices" {
				value = []model.GPUDevice{{Index: 0, UUID: "GPU-test", Model: "L4", VRAMGB: 24}}
			}
			payload[field] = value
			rejected := boundaryRequest(t, handler, http.MethodPost, "/v1/nodes/register", payload)
			boundaryStatus(t, rejected, http.StatusBadRequest)
			if len(state.ListNodes()) != 0 {
				t.Fatal("rejected registration changed state")
			}
			delete(payload, field)
			boundaryNodeResponse(t, boundaryRequest(t, handler, http.MethodPost, "/v1/nodes/register", payload), false)
			boundaryNodeResponse(t, boundaryRequest(t, handler, http.MethodPost, "/v1/nodes/worker/cleanup-complete", nil), false)
			health := map[string]any{"status": "DEGRADED", "reason": "rejected report", field: value}
			boundaryStatus(t, boundaryRequest(t, handler, http.MethodPost, "/v1/nodes/worker/health", health), http.StatusBadRequest)
			nodes := state.ListNodes()
			if len(nodes) != 1 || nodes[0].HealthStatus != "HEALTHY" || nodes[0].HealthReason != "" {
				t.Fatal("rejected health report changed state")
			}
		})
	}
}

func TestNodeProjectionKeepsStoredInventoryAndEnterpriseResponses(t *testing.T) {
	for _, detailed := range []bool{false, true} {
		name := "community"
		if detailed {
			name = "enterprise"
		}
		t.Run(name, func(t *testing.T) {
			state := store.NewMemory()
			descriptor := edition.Community()
			descriptor.Features[edition.FeaturePerGPUInventory] = detailed
			handler := NewWithEdition(state, "test-token", descriptor).Handler()
			node := model.Node{ID: "retained", GPUModel: "L4", GPUCount: 1, VRAMGB: 24,
				Devices:       []model.GPUDevice{{Index: 0, UUID: "GPU-retained", Model: "L4", VRAMGB: 24}},
				DriverVersion: "driver", DockerVersion: "docker"}
			if detailed {
				boundaryNodeResponse(t, boundaryRequest(t, handler, http.MethodPost, "/v1/nodes/register", node), true)
			} else {
				// Simulate persisted inventory loaded before a Community process
				// starts. Projection must not delete this shared durable state.
				if _, err := state.RegisterNodeSession(node, "boundary-session"); err != nil {
					t.Fatal(err)
				}
			}
			boundaryNodeResponse(t, boundaryRequest(t, handler, http.MethodPost, "/v1/nodes/retained/cleanup-complete", nil), detailed)
			boundaryNodeResponse(t, boundaryRequest(t, handler, http.MethodPost, "/v1/nodes/retained/health", model.NodeHealthUpdate{Status: "DEGRADED", Reason: "runtime failure"}), detailed)
			for _, path := range []string{"/v1/nodes", "/v1/nodes?page=1&page_size=1&q=retained"} {
				rec := boundaryRequest(t, handler, http.MethodGet, path, nil)
				boundaryStatus(t, rec, http.StatusOK)
				var nodes []json.RawMessage
				if path == "/v1/nodes" {
					if err := json.Unmarshal(rec.Body.Bytes(), &nodes); err != nil {
						t.Fatal(err)
					}
				} else {
					var page struct {
						Items []json.RawMessage `json:"items"`
						Total int               `json:"total"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
						t.Fatal(err)
					}
					if page.Total != 1 {
						t.Fatalf("projection changed pagination: %s", rec.Body.String())
					}
					nodes = page.Items
				}
				if len(nodes) != 1 {
					t.Fatalf("unexpected node count: %d", len(nodes))
				}
				single := httptest.NewRecorder()
				single.WriteHeader(http.StatusOK)
				_, _ = single.Write(nodes[0])
				boundaryNodeResponse(t, single, detailed)
			}
			retained := state.ListNodes()[0]
			if len(retained.Devices) != 1 || retained.Devices[0].UUID != "GPU-retained" || retained.DriverVersion != "driver" || retained.DockerVersion != "docker" {
				t.Fatal("HTTP projection modified stored inventory")
			}
			if detailed {
				update := model.NodeHealthUpdate{Status: "HEALTHY", GPUModel: "L4", GPUCount: 1, VRAMGB: 24,
					Devices: node.Devices, DriverVersion: "new-driver", DockerVersion: "new-docker"}
				boundaryNodeResponse(t, boundaryRequest(t, handler, http.MethodPost, "/v1/nodes/retained/health", update), true)
				if state.ListNodes()[0].DriverVersion != "new-driver" {
					t.Fatal("Enterprise inventory update did not persist")
				}
			}
		})
	}
}

func TestCommunityBasicHealthStillStopsAndResumesScheduling(t *testing.T) {
	state := store.NewMemory()
	handler := New(state, "test-token").Handler()
	boundaryNodeResponse(t, boundaryRequest(t, handler, http.MethodPost, "/v1/nodes/register", model.Node{ID: "worker", GPUCount: 1, VRAMGB: 24}), false)
	boundaryNodeResponse(t, boundaryRequest(t, handler, http.MethodPost, "/v1/nodes/worker/cleanup-complete", nil), false)
	boundaryNodeResponse(t, boundaryRequest(t, handler, http.MethodPost, "/v1/nodes/worker/health", model.NodeHealthUpdate{Status: "DEGRADED"}), false)
	created := boundaryRequest(t, handler, http.MethodPost, "/v1/jobs", model.JobCreate{Name: "work", Image: "alpine", Requirements: model.Requirements{GPUCount: 1}})
	boundaryStatus(t, created, http.StatusCreated)
	var job model.Job
	if err := json.Unmarshal(created.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Status != model.JobQueued {
		t.Fatal("degraded node received an assignment")
	}
	boundaryNodeResponse(t, boundaryRequest(t, handler, http.MethodPost, "/v1/nodes/worker/health", model.NodeHealthUpdate{Status: "HEALTHY", GPUCount: 1, VRAMGB: 24}), false)
	assigned, err := state.GetJob(job.ID)
	if err != nil || assigned.Status != model.JobAssigned {
		t.Fatalf("basic health recovery did not restore scheduling: %+v %v", assigned, err)
	}
}

func TestCommunityManagementAPIsAreJSONNotFound(t *testing.T) {
	core := New(store.NewMemory(), "test-token").Handler()
	paths := []string{"/enterprise/v1", "/enterprise/v1/", "/enterprise/v1/license", "/enterprise/v1/projects",
		"/enterprise/v1/projects/default/tokens", "/enterprise/v1/scheduling/decisions", "/enterprise/v1/scheduling/projects",
		"/enterprise/v1/nodes/worker/maintenance", "/enterprise/v1/usage-report", "/enterprise/v1/insights",
		"/enterprise/v1/audit", "/enterprise/v1/registry/credentials", "/enterprise/v1/agent-bootstrap", "/v1/projects"}
	for _, path := range paths {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
			rec := boundaryRequest(t, core, method, path, nil)
			boundaryStatus(t, rec, http.StatusNotFound)
			var failure map[string]string
			if json.Unmarshal(rec.Body.Bytes(), &failure) != nil || failure["error"] == "" {
				t.Fatalf("%s %s did not return a JSON API error: %s", method, path, rec.Body.String())
			}
			if strings.HasPrefix(path, "/enterprise/v1") {
				anonymous := httptest.NewRecorder()
				core.ServeHTTP(anonymous, httptest.NewRequest(method, path, nil))
				boundaryStatus(t, anonymous, http.StatusNotFound)
				if anonymous.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("anonymous %s %s fell through to a page", method, path)
				}
			}
		}
	}
	// The enterprise product owns an outer mux. A core namespace fallback
	// cannot intercept real private routes registered by that product.
	outer := http.NewServeMux()
	outer.HandleFunc("GET /enterprise/v1/license", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	outer.Handle("/", core)
	boundaryStatus(t, boundaryRequest(t, outer, http.MethodGet, "/enterprise/v1/license", nil), http.StatusNoContent)
	boundaryStatus(t, boundaryRequest(t, core, http.MethodGet, "/", nil), http.StatusOK)
}
