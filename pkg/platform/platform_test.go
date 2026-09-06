package platform

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuflow/internal/artifact"
	"gpuflow/internal/model"
	"gpuflow/internal/store"
	"gpuflow/pkg/edition"
)

func TestHandlerRequiresMySQLAndArtifactStorage(t *testing.T) {
	if _, err := NewHandler("", "", edition.Community(), artifact.Config{Endpoint: "minio:9000"}); err == nil || !strings.Contains(err.Error(), "GPUFLOW_MYSQL_DSN") {
		t.Fatalf("expected required MySQL error, got %v", err)
	}
	if _, err := NewHandler("unused-dsn", "", edition.Community(), artifact.Config{}); err == nil || !strings.Contains(err.Error(), "GPUFLOW_S3_ENDPOINT") {
		t.Fatalf("expected required artifact storage error, got %v", err)
	}
}

func TestRuntimeProvidesExplicitAtomicSchedulingControl(t *testing.T) {
	state := store.NewMemory()
	runtime := &Runtime{handler: http.NotFoundHandler(), state: state}
	var controller SchedulingController = runtime
	if controller == nil || runtime.Handler() == nil {
		t.Fatal("runtime did not expose its supported contracts")
	}
	if err := runtime.UpdateSchedulingLimits(SchedulingLimits{MaxNodes: -1}); err == nil {
		t.Fatal("negative live scheduling limit was accepted")
	}
	if err := runtime.UpdateSchedulingLimits(SchedulingLimits{ExpiresAt: "tomorrow"}); err == nil {
		t.Fatal("malformed live scheduling expiration was accepted")
	}
	if err := runtime.UpdateSchedulingLimits(SchedulingLimits{AcceleratorLimits: map[string]edition.AcceleratorLimit{"huawei": {MaxNodes: 1}}}); err == nil {
		t.Fatal("incomplete accelerator limit was accepted")
	}
	for _, id := range []string{"node-a", "node-b"} {
		if _, err := state.RegisterNode(model.Node{ID: id, GPUCount: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := state.CreateJob(model.JobCreate{Name: "job-" + id, Image: "alpine", Requirements: model.Requirements{GPUCount: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.UpdateSchedulingLimits(SchedulingLimits{MaxNodes: 1}); err != nil {
		t.Fatal(err)
	}
	if err := state.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	assigned := 0
	for _, job := range state.ListJobs() {
		if job.Status == model.JobAssigned {
			assigned++
		}
	}
	if assigned != 1 {
		t.Fatalf("live scheduling limits were not applied atomically: assigned=%d", assigned)
	}
}

func TestRuntimeProvidesProjectController(t *testing.T) {
	state := store.NewMemory()
	runtime := &Runtime{handler: http.NotFoundHandler(), state: state}
	var controller ProjectController = runtime
	project, err := controller.CreateProject(ProjectCreate{ID: "alpha", Name: "Alpha", MaxQueuedJobs: 2, MaxConcurrentJobs: 1, MaxGPUs: 2})
	if err != nil || project.ID != "alpha" {
		t.Fatalf("project controller create failed: %+v err=%v", project, err)
	}
	projects := controller.ListProjects()
	if len(projects) != 2 {
		t.Fatalf("project controller did not include default and alpha: %+v", projects)
	}
	snapshot, err := controller.ProjectQuotaSnapshot("alpha")
	if err != nil || snapshot.MaxQueuedJobs != 2 || snapshot.ProjectID != "alpha" {
		t.Fatalf("project quota snapshot failed: %+v err=%v", snapshot, err)
	}
	disabled := model.ProjectDisabled
	if _, err := controller.UpdateProject("default", ProjectUpdate{Status: &disabled}); !errors.Is(err, ErrProjectConflict) {
		t.Fatalf("controller disabled default project: %v", err)
	}
	if _, err := controller.GetProject("missing"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("controller did not preserve not-found error: %v", err)
	}
}

func TestNewHandlerRejectsMalformedLicenseExpiration(t *testing.T) {
	descriptor := edition.Community()
	descriptor.ExpiresAt = "tomorrow"
	_, err := NewHandlerWithConfig(Config{MySQLDSN: "unused-dsn", Descriptor: descriptor, Artifacts: ArtifactConfig{Endpoint: "minio:9000"}})
	if err == nil || !strings.Contains(err.Error(), "expires_at") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewHandlerRequiresSchemaV2DedicatedProbeImage(t *testing.T) {
	config := Config{MySQLDSN: "unused-dsn", Artifacts: ArtifactConfig{Endpoint: "minio:9000"}}
	legacy := edition.Community()
	legacy.SchemaVersion = 1
	config.Descriptor = legacy
	if _, err := NewHandlerWithConfig(config); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("legacy schema was accepted: %v", err)
	}

	missingProbe := edition.Community()
	missingProbe.Features[edition.FeatureNodeHealth] = true
	missingProbe.ProbeImage = ""
	config.Descriptor = missingProbe
	if _, err := NewHandlerWithConfig(config); err == nil || !strings.Contains(err.Error(), "GPUFLOW_PROBE_IMAGE") {
		t.Fatalf("node health without a dedicated probe image was accepted: %v", err)
	}
}
