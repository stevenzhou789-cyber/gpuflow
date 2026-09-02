package store

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gpuflow/internal/model"
)

func TestNodeDetailsRoundTripCleanupPending(t *testing.T) {
	for _, pending := range []bool{false, true} {
		original := model.Node{ID: "pending", SessionEpoch: "session", CleanupPending: pending}
		payload, err := json.Marshal(detailsFromNode(&original))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(payload), `"cleanup_pending":`) {
			t.Fatalf("cleanup readiness was omitted from MySQL details JSON: %s", payload)
		}
		var details nodeDetails
		if err := json.Unmarshal(payload, &details); err != nil {
			t.Fatal(err)
		}
		var restored model.Node
		details.apply(&restored)
		if restored.SessionEpoch != original.SessionEpoch || restored.CleanupPending != pending {
			t.Fatalf("cleanup gate was not preserved in MySQL details JSON: %s", payload)
		}
	}

	var legacy nodeDetails
	if err := json.Unmarshal([]byte(`{"session_epoch":"legacy-session"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	legacyNode := model.Node{}
	legacy.apply(&legacyNode)
	if legacyNode.SessionEpoch != "" || !legacyNode.CleanupPending {
		t.Fatalf("legacy details did not fail closed: %+v", legacyNode)
	}
}

func TestUsageRecordsRoundTripInsideRequirementsJSON(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	job := model.Job{
		Requirements: model.Requirements{GPUCount: 2, Labels: map[string]string{model.LabelAcceleratorVendor: model.VendorHuawei}},
		UsageRecords: []model.AcceleratorUsageRecord{{Attempt: 1, NodeID: "ascend", Vendor: model.VendorHuawei, Model: "Ascend 910B", DeviceCount: 2, UnitPricePerHour: 12.5, AssignedAt: now, StartedAt: &now}},
	}
	payload, err := encodeJobRequirements(&job)
	if err != nil {
		t.Fatal(err)
	}
	var restored model.Job
	if err := decodeJobJSON(&restored, []byte("null"), []byte("null"), payload); err != nil {
		t.Fatal(err)
	}
	if restored.Requirements.GPUCount != 2 || len(restored.UsageRecords) != 1 || restored.UsageRecords[0].UnitPricePerHour != 12.5 || restored.UsageRecords[0].StartedAt == nil {
		t.Fatalf("usage accounting was not persisted: %s %+v", payload, restored)
	}
}

func TestMySQLCoreStatePersistsAcrossReopen(t *testing.T) {
	dsn := os.Getenv("GPUFLOW_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("GPUFLOW_TEST_MYSQL_DSN is not set")
	}
	s, err := OpenMySQLStateStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	var appliedCoreMigrations int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE id LIKE 'core/%'").Scan(&appliedCoreMigrations); err != nil {
		t.Fatal(err)
	}
	if appliedCoreMigrations != len(coreMigrations) {
		t.Fatalf("unexpected core migration count: got %d want %d", appliedCoreMigrations, len(coreMigrations))
	}

	nodeInput := model.Node{ID: "mysql-node", Name: "mysql node", Provider: "local", Pool: "default", GPUModel: "RTX 4090", GPUCount: 1, CPUCores: 16, VRAMGB: 24, Labels: map[string]string{"zone": "lab"}}
	node, err := s.RegisterNode(nodeInput)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateJob(model.JobCreate{Name: "mysql job", Image: "alpine", Command: []string{"echo", "mysql"}, Environment: map[string]string{"MODE": "test"}, Requirements: model.Requirements{GPUCount: 1, Labels: map[string]string{"zone": "lab"}}, MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateJob(job.ID, node.ID, model.JobUpdate{Status: model.JobRunning, Output: "partial"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterNode(nodeInput); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateNodeHealth(node.ID, model.NodeHealthUpdate{Status: "HEALTHY", GPUModel: "RTX 4090", GPUCount: 1, VRAMGB: 24, DriverVersion: "550.54", DockerVersion: "27.1", Devices: []model.GPUDevice{{Index: 0, UUID: "GPU-persisted", Model: "RTX 4090", VRAMGB: 24}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Schedule(time.Minute); err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.Exec(`INSERT INTO nodes (id, name, provider, pool, gpu_model, gpu_count,
  vram_gb, hourly_price, labels_json, busy, current_job, last_heartbeat)
VALUES ('external-node', 'external', 'local', 'default', '', 0, 0, 0, '{}', false, '', ?)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.HeartbeatNode(node.ID); err != nil {
		t.Fatal(err)
	}
	var externalNodes int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM nodes WHERE id = 'external-node'").Scan(&externalNodes); err != nil || externalNodes != 1 {
		t.Fatalf("unchanged external row was removed: count=%d err=%v", externalNodes, err)
	}
	if _, err := s.db.Exec("DELETE FROM nodes WHERE id = 'external-node'"); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	image := model.TaskImage{ID: "mysql-image", Name: "gpuflow-task/mysql-test:v1", Runtime: "shell", BaseImage: "alpine", Filename: "smoke.sh", Status: "ready", Log: "mysql result", CreatedAt: now, UpdatedAt: now}
	if err := s.SaveTaskImage(image); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenMySQLStateStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	persistedJob, err := reopened.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedJob.Status != model.JobAssigned || persistedJob.Attempts != 2 || persistedJob.Recoveries != 1 || persistedJob.Environment["MODE"] != "test" || persistedJob.Requirements.Labels["zone"] != "lab" {
		t.Fatalf("unexpected persisted job: %+v", persistedJob)
	}
	nodes := reopened.ListNodes()
	if len(nodes) != 1 || nodes[0].CurrentJob != job.ID || !nodes[0].Busy || nodes[0].CPUCores != 16 || nodes[0].Labels["zone"] != "lab" || nodes[0].HealthStatus != "HEALTHY" || len(nodes[0].Devices) != 1 || nodes[0].Devices[0].UUID != "GPU-persisted" || nodes[0].SessionEpoch == "" {
		t.Fatalf("unexpected persisted nodes: %+v", nodes)
	}
	images, err := reopened.ListTaskImages()
	if err != nil || len(images) != 1 || images[0].ID != image.ID || images[0].Status != "ready" {
		t.Fatalf("unexpected persisted images: %+v err=%v", images, err)
	}
	if err := reopened.DeleteTaskImage(image.ID); err != nil {
		t.Fatal(err)
	}
	images, err = reopened.ListTaskImages()
	if err != nil || len(images) != 0 {
		t.Fatalf("deleted task image still persisted: %+v err=%v", images, err)
	}

	dispatch, err := reopened.NextJobSession(node.ID, nodes[0].SessionEpoch)
	if err != nil || dispatch == nil || dispatch.AttemptToken == "" {
		t.Fatalf("persisted assignment could not be claimed: %+v err=%v", dispatch, err)
	}
	leaseReopened, err := OpenMySQLStateStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseReopened.db.Close()
	if duplicate, err := leaseReopened.NextJobSession(node.ID, nodes[0].SessionEpoch); err != nil || duplicate != nil {
		t.Fatalf("persisted claim was delivered twice: %+v err=%v", duplicate, err)
	}
	if _, err := leaseReopened.UpdateJobLease(job.ID, node.ID, nodes[0].SessionEpoch, dispatch.AttemptToken, model.JobUpdate{Status: model.JobRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := leaseReopened.UpdateJobLease(job.ID, node.ID, nodes[0].SessionEpoch, dispatch.AttemptToken, model.JobUpdate{Status: model.JobSucceeded}); err != nil {
		t.Fatal(err)
	}
	if err := leaseReopened.BeginJobDeletion(job.ID); err != nil {
		t.Fatal(err)
	}
	deletionReopened, err := OpenMySQLStateStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	deleting, err := deletionReopened.GetJob(job.ID)
	if err != nil || deleting.Status != model.JobDeleting {
		t.Fatalf("deletion marker did not survive reopen: %+v err=%v", deleting, err)
	}
	if err := deletionReopened.DeleteJob(job.ID); err != nil {
		t.Fatal(err)
	}
	deletionReopened.db.Close()

	deletedReopened, err := OpenMySQLStateStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deletedReopened.GetJob(job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted job returned after reopen: %v", err)
	}
	rollbackNode, err := deletedReopened.RegisterNode(model.Node{ID: "rollback-node"})
	if err != nil {
		t.Fatal(err)
	}
	beforeHeartbeat := rollbackNode.LastHeartbeat
	if err := deletedReopened.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := deletedReopened.HeartbeatNode(rollbackNode.ID); !errors.Is(err, ErrPersistence) {
		t.Fatalf("expected persistence error after closing MySQL, got %v", err)
	}
	var afterRollback *model.Node
	for _, candidate := range deletedReopened.ListNodes() {
		if candidate.ID == rollbackNode.ID {
			afterRollback = candidate
		}
	}
	if afterRollback == nil || !afterRollback.LastHeartbeat.Equal(beforeHeartbeat) {
		t.Fatalf("failed MySQL heartbeat was not rolled back: before=%s after=%+v", beforeHeartbeat, afterRollback)
	}
}
